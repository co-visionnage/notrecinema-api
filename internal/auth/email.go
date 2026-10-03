package auth

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"notrecinema/api/internal/outbox"
	"notrecinema/api/internal/platform/apperror"
	"notrecinema/api/internal/telemetry"
)

// Подтверждение email и сброс пароля. Сами письма отправляет
// notrecinema-worker: API лишь кладёт событие в outbox (либо это делает
// SQL-функция, см. миграцию 0031), а токен создаётся уже воркером в момент
// отправки -- в событии и в NATS его нет. Здесь только потребление токена,
// пришедшего по ссылке из письма.

const errInvalidLink = "ссылка недействительна или истекла. Запросите новую"

// VerifyEmail подтверждает адрес по токену из письма. Токен одноразовый и
// живёт 24 часа (TTL задаёт воркер при создании).
func (s *Service) VerifyEmail(ctx context.Context, token string) error {
	var ok bool
	if err := s.db.QueryRow(ctx, `
		SELECT public.verify_email_with_token($1)
	`, hashToken(token)).Scan(&ok); err != nil {
		return apperror.Internal("failed to verify email", err)
	}
	if !ok {
		telemetry.RecordAuth("email_verify", "invalid_token")
		return apperror.Invalid(errInvalidLink)
	}
	telemetry.RecordAuth("email_verify", "success")
	return nil
}

// RequestEmailVerification просит выслать письмо с подтверждением ещё раз.
// Не пересылает, если адрес уже подтверждён.
func (s *Service) RequestEmailVerification(ctx context.Context, userID string) error {
	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		var verified bool
		if err := tx.QueryRow(ctx, `
			SELECT email_verified_at IS NOT NULL FROM public.profiles WHERE id = $1
		`, userID).Scan(&verified); err != nil {
			return err
		}
		if verified {
			return apperror.Conflict("email уже подтверждён")
		}
		return outbox.Insert(ctx, tx, "profile", userID, "email.verification_requested", map[string]string{"userId": userID})
	})
	if err != nil {
		var appErr *apperror.Error
		if errors.As(err, &appErr) {
			return appErr
		}
		return apperror.Internal("failed to request email verification", err)
	}
	return nil
}

// RequestPasswordReset ставит в очередь письмо со ссылкой на сброс. Ничего
// не сообщает о том, существует ли аккаунт: функция БД молча ничего не
// делает для неизвестного email, а вызывающий хендлер всегда отвечает
// одинаково.
func (s *Service) RequestPasswordReset(ctx context.Context, email string) error {
	if err := s.db.Exec(ctx, `SELECT public.request_password_reset($1)`, email); err != nil {
		return apperror.Internal("failed to request password reset", err)
	}
	telemetry.RecordAuth("password_reset_request", "queued")
	return nil
}

// ResetPassword меняет пароль по токену из письма. Функция БД заодно гасит
// все сессии пользователя и кладёт событие для письма-уведомления.
func (s *Service) ResetPassword(ctx context.Context, token, newPassword string) error {
	passwordHash, err := HashPassword(newPassword)
	if err != nil {
		return apperror.Internal("failed to hash password", err)
	}

	var userID *string
	if err := s.db.QueryRow(ctx, `
		SELECT public.reset_password_with_token($1, $2)
	`, hashToken(token), passwordHash).Scan(&userID); err != nil {
		return apperror.Internal("failed to reset password", err)
	}
	if userID == nil {
		telemetry.RecordAuth("password_reset_confirm", "invalid_token")
		return apperror.Invalid(errInvalidLink)
	}
	telemetry.RecordAuth("password_reset_confirm", "success")
	return nil
}
