package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image/png"

	"github.com/jackc/pgx/v5"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"notrecinema/api/internal/outbox"
	"notrecinema/api/internal/platform/apperror"
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

// ConfirmTwoFactor — прямой перенос confirmTwoFactorAction.
func (s *Service) ConfirmTwoFactor(ctx context.Context, userID, code string) error {
	return s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		var secret *string
		if err := tx.QueryRow(ctx, `
			SELECT totp_secret FROM public.profiles WHERE id = $1
		`, userID).Scan(&secret); err != nil {
			return err
		}
		if secret == nil {
			return apperror.Invalid("сначала начните настройку — отсканируйте QR-код")
		}
		if !verifyTOTP(code, *secret) {
			return apperror.Invalid("неверный код")
		}

		if _, err := tx.Exec(ctx, `UPDATE public.profiles SET totp_enabled = true WHERE id = $1`, userID); err != nil {
			return err
		}
		return outbox.Insert(ctx, tx, "profile", userID, "security.two_factor_enabled", map[string]string{"userId": userID})
	})
}

// DisableTwoFactor — прямой перенос disableTwoFactorAction: требует
// валидный текущий код, затем полностью стирает секрет (не только флаг).
func (s *Service) DisableTwoFactor(ctx context.Context, userID, code string) error {
	return s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		var secret *string
		if err := tx.QueryRow(ctx, `
			SELECT totp_secret FROM public.profiles WHERE id = $1
		`, userID).Scan(&secret); err != nil {
			return err
		}
		if secret == nil || !verifyTOTP(code, *secret) {
			return apperror.Invalid("неверный код")
		}

		if _, err := tx.Exec(ctx, `
			UPDATE public.profiles SET totp_enabled = false, totp_secret = NULL WHERE id = $1
		`, userID); err != nil {
			return err
		}
		return outbox.Insert(ctx, tx, "profile", userID, "security.two_factor_disabled", map[string]string{"userId": userID})
	})
}

// TwoFactorStatus — прямой перенос 2fa-status/route.ts.
func (s *Service) TwoFactorStatus(ctx context.Context, userID string) (bool, error) {
	var enabled bool
	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT totp_enabled FROM public.profiles WHERE id = $1
		`, userID).Scan(&enabled)
	})
	return enabled, err
}
