package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"notrecinema/api/internal/platform/apperror"
	"notrecinema/api/internal/telemetry"
)

// sessionTTL -- прямой перенос SESSION_TTL_DAYS (30) из notrecinema-app:
// одинаковый TTL для логина, регистрации и сессии, выданной после 2FA.
const sessionTTL = 30 * 24 * time.Hour

// twoFactorChallengeTTL -- прямой перенос TWO_FACTOR_CHALLENGE_TTL_MINUTES.
const twoFactorChallengeTTL = 5 * time.Minute

type Session struct {
	Token     string
	ExpiresAt time.Time
	User      User
}

type LoginResult struct {
	RequiresTwoFactor bool
	ChallengeToken    string
	Session           *Session
}

func generateToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: generate token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// Register — прямой перенос registerUserSession: register_profile_account
// сам проверяет занятость email (RAISE EXCEPTION 'ACCOUNT_ALREADY_EXISTS',
// миграция 0023 -- защита от account takeover через повторную
// регистрацию на чужой email) и создаёт профиль+сессию одной SECURITY
// DEFINER функцией.
func (s *Service) Register(ctx context.Context, email, displayName, password string) (Session, error) {
	token, err := generateToken()
	if err != nil {
		return Session{}, apperror.Internal("failed to generate session token", err)
	}
	passwordHash, err := HashPassword(password)
	if err != nil {
		return Session{}, apperror.Internal("failed to hash password", err)
	}
	expiresAt := time.Now().Add(sessionTTL)

	var user User
	row := s.db.QueryRow(ctx, `
		SELECT user_id, email, display_name
		FROM public.register_profile_account($1, $2, $3, $4, $5)
	`, email, displayName, passwordHash, hashToken(token), expiresAt)

	var dbDisplayName *string
	if err := row.Scan(&user.ID, &user.Email, &dbDisplayName); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Message == "ACCOUNT_ALREADY_EXISTS" {
			telemetry.RecordAuth("register", "duplicate")
			return Session{}, apperror.Conflict("пользователь с таким email уже существует. Войдите в аккаунт")
		}
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			telemetry.RecordAuth("register", "duplicate")
			return Session{}, apperror.Conflict("пользователь с таким email уже существует")
		}
		telemetry.RecordAuth("register", "error")
		return Session{}, apperror.Internal("failed to register account", err)
	}
	if dbDisplayName != nil {
		user.DisplayName = *dbDisplayName
	}

	s.recordSessionMetadata(ctx, token)
	telemetry.RecordAuth("register", "success")
	return Session{Token: token, ExpiresAt: expiresAt, User: user}, nil
}

// Login — прямой перенос loginUserSession. Если у аккаунта включена 2FA,
// сессия не выдаётся сразу: вместо неё создаётся короткоживущий
// challenge-токен (create_two_factor_challenge), и клиент должен
// подтвердить его через VerifyTwoFactor с TOTP-кодом. Сообщение об ошибке
// намеренно одинаковое и для "нет такого email", и для "неверный пароль"
// -- не палим, какой из двух факторов не совпал.
func (s *Service) Login(ctx context.Context, email, password string) (LoginResult, error) {
	var (
		userID, dbEmail string
		dbDisplayName   *string
		passwordHash    *string
		totpEnabled     bool
	)
	row := s.db.QueryRow(ctx, `
		SELECT user_id, email, display_name, password_hash, totp_enabled
		FROM public.get_profile_auth_by_email($1)
	`, email)
	if err := row.Scan(&userID, &dbEmail, &dbDisplayName, &passwordHash, &totpEnabled); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			telemetry.RecordAuth("login", "invalid_credentials")
			return LoginResult{}, apperror.Unauthorized("неверный email или пароль")
		}
		telemetry.RecordAuth("login", "error")
		return LoginResult{}, apperror.Internal("failed to load account", err)
	}

	if passwordHash == nil || !VerifyPassword(password, *passwordHash) {
		telemetry.RecordAuth("login", "invalid_credentials")
		return LoginResult{}, apperror.Unauthorized("неверный email или пароль")
	}

	if totpEnabled {
		challengeToken, err := generateToken()
		if err != nil {
			return LoginResult{}, apperror.Internal("failed to generate challenge token", err)
		}
		expiresAt := time.Now().Add(twoFactorChallengeTTL)
		if err := s.db.Exec(ctx, `
			SELECT public.create_two_factor_challenge($1, $2, $3)
		`, userID, hashToken(challengeToken), expiresAt); err != nil {
			return LoginResult{}, apperror.Internal("failed to create two-factor challenge", err)
		}
		telemetry.RecordAuth("login", "two_factor_required")
		return LoginResult{RequiresTwoFactor: true, ChallengeToken: challengeToken}, nil
	}

	session, err := s.createSessionForProfile(ctx, userID)
	if err != nil {
		telemetry.RecordAuth("login", "error")
		return LoginResult{}, err
	}
	telemetry.RecordAuth("login", "success")
	return LoginResult{Session: &session}, nil
}

// VerifyTwoFactor — прямой перенос verifyTwoFactorAndCreateSession.
// get_two_factor_challenge специально не консьюмирующая (не удаляет
// строку) -- неверный код можно повторить, пока challenge не истёк или не
// был использован успешно; брутфорс ограничен только rate-limit на уровне
// HTTP-хендлера.
func (s *Service) VerifyTwoFactor(ctx context.Context, challengeToken, code string) (Session, error) {
	challengeTokenHash := hashToken(challengeToken)

	var userID *string
	if err := s.db.QueryRow(ctx, `
		SELECT public.get_two_factor_challenge($1)
	`, challengeTokenHash).Scan(&userID); err != nil {
		return Session{}, apperror.Internal("failed to load two-factor challenge", err)
	}
	if userID == nil {
		telemetry.RecordAuth("two_factor_verify", "expired_challenge")
		return Session{}, apperror.Invalid("сессия входа истекла. Войдите заново")
	}

	var secret *string
	if err := s.db.QueryRow(ctx, `
		SELECT public.get_totp_secret_for_login($1)
	`, *userID).Scan(&secret); err != nil {
		return Session{}, apperror.Internal("failed to load totp secret", err)
	}
	method := "backup_code"
	if isTOTPFormat(code) {
		method = "totp"
	}
	ok, err := s.checkSecondFactor(ctx, *userID, secret, code)
	if err != nil {
		telemetry.RecordAuth("two_factor_verify", method+"_error")
		return Session{}, err
	}
	if !ok {
		telemetry.RecordAuth("two_factor_verify", method+"_invalid")
		return Session{}, apperror.Invalid("неверный код двухфакторной аутентификации")
	}
	telemetry.RecordAuth("two_factor_verify", method+"_success")

	if err := s.db.Exec(ctx, `
		SELECT public.delete_two_factor_challenge($1)
	`, challengeTokenHash); err != nil {
		return Session{}, apperror.Internal("failed to consume two-factor challenge", err)
	}

	return s.createSessionForProfile(ctx, *userID)
}

func (s *Service) createSessionForProfile(ctx context.Context, userID string) (Session, error) {
	token, err := generateToken()
	if err != nil {
		return Session{}, apperror.Internal("failed to generate session token", err)
	}
	expiresAt := time.Now().Add(sessionTTL)

	var user User
	var dbDisplayName *string
	row := s.db.QueryRow(ctx, `
		SELECT user_id, email, display_name
		FROM public.create_session_for_profile($1, $2, $3)
	`, userID, hashToken(token), expiresAt)
	if err := row.Scan(&user.ID, &user.Email, &dbDisplayName); err != nil {
		return Session{}, apperror.Internal("failed to create session", err)
	}
	if dbDisplayName != nil {
		user.DisplayName = *dbDisplayName
	}

	s.recordSessionMetadata(ctx, token)
	return Session{Token: token, ExpiresAt: expiresAt, User: user}, nil
}

// CreateSessionForEmail — прямой перенос createUserSession: upsert-по-email
// без пароля (create_profile_session, та же SQL-функция, на которой уже
// держатся интеграционные тесты этого модуля). Используется там, где
// личность уже подтверждена не паролем, а внешней стороной -- сегодня
// только internal/oauth (GitHub): совпадение по email достаточно, чтобы
// привязаться к существующему аккаунту или завести новый OAuth-only
// (без password_hash).
func (s *Service) CreateSessionForEmail(ctx context.Context, email, displayName string) (Session, error) {
	token, err := generateToken()
	if err != nil {
		return Session{}, apperror.Internal("failed to generate session token", err)
	}
	expiresAt := time.Now().Add(sessionTTL)

	var user User
	var dbDisplayName *string
	row := s.db.QueryRow(ctx, `
		SELECT user_id, email, display_name
		FROM public.create_profile_session($1, $2, $3, $4)
	`, email, displayName, hashToken(token), expiresAt)
	if err := row.Scan(&user.ID, &user.Email, &dbDisplayName); err != nil {
		return Session{}, apperror.Internal("failed to create session", err)
	}
	if dbDisplayName != nil {
		user.DisplayName = *dbDisplayName
	}

	s.recordSessionMetadata(ctx, token)
	return Session{Token: token, ExpiresAt: expiresAt, User: user}, nil
}

// Logout — прямой перенос clearUserSession: удаляет строку app_sessions
// (best-effort, отсутствие токена/сессии не ошибка) и не требует активной
// сессии сама по себе -- вызывающий хендлер читает cookie напрямую.
func (s *Service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	if err := s.db.Exec(ctx, `
		SELECT public.delete_session_by_token($1)
	`, hashToken(token)); err != nil {
		return apperror.Internal("failed to delete session", err)
	}
	return nil
}

// SetSessionCookie/ClearSessionCookie -- прямой перенос setSessionCookie /
// части clearUserSession, отвечающей за cookie. Флаги совпадают с
// notrecinema-app: httpOnly, SameSite=Lax, Secure только в production,
// Path=/, Expires (не MaxAge).
func (s *Service) SetSessionCookie(w http.ResponseWriter, token string, expiresAt time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName,
		Value:    token,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.isProduction(),
		Path:     "/",
		Expires:  expiresAt,
	})
}

func (s *Service) ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName,
		Value:    "",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.isProduction(),
		Path:     "/",
		Expires:  time.Unix(0, 0),
	})
}

// CookieName нужен OAuth-хендлеру (свой state-cookie, но с той же
// Secure-логикой) и хендлеру logout (прочитать текущий токен перед его
// удалением).
func (s *Service) CookieName() string {
	return s.cookieName
}

func (s *Service) IsProduction() bool {
	return s.isProduction()
}
