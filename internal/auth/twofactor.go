package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image/png"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"notrecinema/api/internal/outbox"
	"notrecinema/api/internal/platform/apperror"
	"notrecinema/api/internal/telemetry"
)

// issuer — прямой перенос 'notre-cinema' из generateTotpQrCode.
const issuer = "notre-cinema"

// verifyTOTP — прямой эквивалент verifyTotpCode(code, secret) из
// notrecinema-app: totp.Validate использует Period=30/Skew=1/Digits=6/
// SHA1, ровно дефолты otplib, которыми и был заведён существующий секрет.
func verifyTOTP(code, secret string) bool {
	return totp.Validate(code, secret)
}

type TwoFactorSetup struct {
	Secret        string `json:"secret"`
	QRCodeDataURL string `json:"qrCodeDataUrl"`
}

// StartTwoFactorSetup — прямой перенос startTwoFactorSetupAction. Блокирует
// повторную настройку, пока 2FA уже включена (иначе приложение
// рассинхронизируется с уже отсканированным QR-кодом другого секрета) --
// сначала нужно её отключить.
func (s *Service) StartTwoFactorSetup(ctx context.Context, userID, email string) (TwoFactorSetup, error) {
	var result TwoFactorSetup

	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		var alreadyEnabled bool
		if err := tx.QueryRow(ctx, `
			SELECT totp_enabled FROM public.profiles WHERE id = $1
		`, userID).Scan(&alreadyEnabled); err != nil {
			return err
		}
		if alreadyEnabled {
			return apperror.Conflict("двухфакторная аутентификация уже включена. Сначала отключите её, чтобы настроить заново")
		}

		key, err := totp.Generate(totp.GenerateOpts{Issuer: issuer, AccountName: email})
		if err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `
			UPDATE public.profiles SET totp_secret = $2 WHERE id = $1
		`, userID, key.Secret()); err != nil {
			return err
		}

		dataURL, err := qrCodeDataURL(key)
		if err != nil {
			return err
		}

		result = TwoFactorSetup{Secret: key.Secret(), QRCodeDataURL: dataURL}
		return nil
	})
	if err != nil {
		var appErr *apperror.Error
		if errors.As(err, &appErr) {
			return TwoFactorSetup{}, appErr
		}
		return TwoFactorSetup{}, apperror.Internal("failed to start two-factor setup", err)
	}
	return result, nil
}

// qrCodeDataURL рендерит QR-код в PNG и кодирует как data URL -- тот же
// формат, что возвращает QRCode.toDataURL в notrecinema-app.
func qrCodeDataURL(key *otp.Key) (string, error) {
	img, err := key.Image(200, 200)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// loadTwoFactorState читает секрет и флаг 2FA пользователя.
func (s *Service) loadTwoFactorState(ctx context.Context, userID string) (secret *string, enabled bool, err error) {
	err = s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT totp_secret, totp_enabled FROM public.profiles WHERE id = $1
		`, userID).Scan(&secret, &enabled)
	})
	return secret, enabled, err
}

func asAppError(err error, fallback string) error {
	var appErr *apperror.Error
	if errors.As(err, &appErr) {
		return appErr
	}
	return apperror.Internal(fallback, err)
}

// ConfirmTwoFactor — перенос confirmTwoFactorAction: подтверждает настройку
// кодом из приложения, включает 2FA и впервые выдаёт резервные коды. Коды
// возвращаются открытым текстом ровно один раз -- дальше в БД только хэши.
func (s *Service) ConfirmTwoFactor(ctx context.Context, userID, code string) ([]string, error) {
	secret, enabled, err := s.loadTwoFactorState(ctx, userID)
	if err != nil {
		return nil, asAppError(err, "failed to load two-factor state")
	}
	if enabled {
		return nil, apperror.Conflict("двухфакторная аутентификация уже включена")
	}
	if secret == nil {
		return nil, apperror.Invalid("сначала начните настройку — отсканируйте QR-код")
	}
	if !isTOTPFormat(code) || !verifyTOTP(strings.TrimSpace(code), *secret) {
		telemetry.RecordAuth("two_factor_enable", "invalid_code")
		return nil, apperror.Invalid("неверный код")
	}

	codes, err := generateBackupCodes()
	if err != nil {
		return nil, apperror.Internal("failed to generate backup codes", err)
	}
	hashes, err := hashBackupCodes(codes)
	if err != nil {
		return nil, apperror.Internal("failed to hash backup codes", err)
	}

	err = s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE public.profiles SET totp_enabled = true WHERE id = $1`, userID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT public.replace_two_factor_backup_codes($1::text[])`, hashes); err != nil {
			return err
		}
		return outbox.Insert(ctx, tx, "profile", userID, "security.two_factor_enabled", map[string]string{"userId": userID})
	})
	if err != nil {
		return nil, asAppError(err, "failed to enable two-factor authentication")
	}
	telemetry.RecordAuth("two_factor_enable", "success")
	return codes, nil
}

// DisableTwoFactor — перенос disableTwoFactorAction: требует валидный
// текущий код (из приложения или резервный -- человек, потерявший телефон и
// вошедший по резервному коду, должен суметь отключить 2FA), затем полностью
// стирает секрет и резервные коды.
func (s *Service) DisableTwoFactor(ctx context.Context, userID, code string) error {
	secret, _, err := s.loadTwoFactorState(ctx, userID)
	if err != nil {
		return asAppError(err, "failed to load two-factor state")
	}

	ok, err := s.checkSecondFactor(ctx, userID, secret, code)
	if err != nil {
		return err
	}
	if !ok {
		telemetry.RecordAuth("two_factor_disable", "invalid_code")
		return apperror.Invalid("неверный код")
	}

	err = s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE public.profiles SET totp_enabled = false, totp_secret = NULL WHERE id = $1
		`, userID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT public.delete_my_two_factor_backup_codes()`); err != nil {
			return err
		}
		return outbox.Insert(ctx, tx, "profile", userID, "security.two_factor_disabled", map[string]string{"userId": userID})
	})
	if err != nil {
		return asAppError(err, "failed to disable two-factor authentication")
	}
	telemetry.RecordAuth("two_factor_disable", "success")
	return nil
}

// RegenerateBackupCodes выпускает новый набор резервных кодов; прежние, в
// том числе неиспользованные, перестают работать. Нужен код подтверждения
// (из приложения или резервный).
func (s *Service) RegenerateBackupCodes(ctx context.Context, userID, code string) ([]string, error) {
	secret, enabled, err := s.loadTwoFactorState(ctx, userID)
	if err != nil {
		return nil, asAppError(err, "failed to load two-factor state")
	}
	if !enabled {
		return nil, apperror.Conflict("двухфакторная аутентификация не включена")
	}

	ok, err := s.checkSecondFactor(ctx, userID, secret, code)
	if err != nil {
		return nil, err
	}
	if !ok {
		telemetry.RecordAuth("backup_codes_regenerate", "invalid_code")
		return nil, apperror.Invalid("неверный код")
	}

	codes, err := generateBackupCodes()
	if err != nil {
		return nil, apperror.Internal("failed to generate backup codes", err)
	}
	hashes, err := hashBackupCodes(codes)
	if err != nil {
		return nil, apperror.Internal("failed to hash backup codes", err)
	}

	err = s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT public.replace_two_factor_backup_codes($1::text[])`, hashes); err != nil {
			return err
		}
		return outbox.Insert(ctx, tx, "profile", userID, "security.backup_codes_regenerated", map[string]string{"userId": userID})
	})
	if err != nil {
		return nil, asAppError(err, "failed to store backup codes")
	}
	telemetry.RecordAuth("backup_codes_regenerate", "success")
	return codes, nil
}

// TwoFactorStatus — перенос 2fa-status/route.ts, плюс сколько резервных
// кодов осталось (чтобы интерфейс мог предупредить, что пора выпустить
// новые).
func (s *Service) TwoFactorStatus(ctx context.Context, userID string) (enabled bool, backupCodesRemaining int, err error) {
	err = s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			SELECT totp_enabled FROM public.profiles WHERE id = $1
		`, userID).Scan(&enabled); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT public.count_my_two_factor_backup_codes()`).Scan(&backupCodesRemaining)
	})
	return enabled, backupCodesRemaining, err
}
