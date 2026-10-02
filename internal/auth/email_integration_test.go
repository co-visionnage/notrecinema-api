//go:build integration

package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pquerna/otp/totp"

	"notrecinema/api/internal/auth"
	"notrecinema/api/internal/postgres"
)

const testPassword = "correct-horse-battery-1!"

func rawToken(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(buf)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// mintToken делает то же, что в проде делает воркер: кладёт в БД хэш токена.
func mintToken(t *testing.T, db *postgres.Pool, userID, kind, raw string, ttl time.Duration) {
	t.Helper()
	if err := db.Exec(context.Background(), `
		SELECT public.create_email_token($1, $2, $3, $4)
	`, userID, kind, sha256Hex(raw), time.Now().Add(ttl)); err != nil {
		t.Fatalf("create_email_token: %v", err)
	}
}

func countEvents(t *testing.T, db *postgres.Pool, userID, eventType string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(context.Background(), `
		SELECT count(*) FROM public.outbox_events WHERE aggregate_id = $1 AND event_type = $2
	`, userID, eventType).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

func isVerified(t *testing.T, db *postgres.Pool, userID string) bool {
	t.Helper()
	var verified bool
	err := db.WithUserContext(context.Background(), userID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT email_verified_at IS NOT NULL FROM public.profiles WHERE id = $1
		`, userID).Scan(&verified)
	})
	if err != nil {
		t.Fatalf("read verified flag: %v", err)
	}
	return verified
}

func sessionCount(t *testing.T, db *postgres.Pool, userID string) int {
	t.Helper()
	var n int
	err := db.WithUserContext(context.Background(), userID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM public.app_sessions WHERE user_id = $1`, userID).Scan(&n)
	})
	if err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	return n
}

func registerUser(t *testing.T, svc *auth.Service, label string) (email string, session auth.Session) {
	t.Helper()
	email = randomEmail(t, label)
	session, err := svc.Register(context.Background(), email, "Test User", testPassword)
	if err != nil {
		t.Fatalf("Register() error: %v", err)
	}
	return email, session
}

func TestRegisterQueuesVerificationEmail(t *testing.T) {
	db := connectOrSkip(t)
	svc := auth.NewService(db, "notre_cinema_session", "development")

	_, session := registerUser(t, svc, "verify-queued")

	if got := countEvents(t, db, session.User.ID, "email.verification_requested"); got != 1 {
		t.Errorf("verification events after register = %d, want 1", got)
	}
	if isVerified(t, db, session.User.ID) {
		t.Error("a freshly registered account must not be verified")
	}
}

func TestVerifyEmailConsumesTokenOnce(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()
	svc := auth.NewService(db, "notre_cinema_session", "development")
	_, session := registerUser(t, svc, "verify-once")

	token := rawToken(t)
	mintToken(t, db, session.User.ID, "verify_email", token, 24*time.Hour)

	if err := svc.VerifyEmail(ctx, token); err != nil {
		t.Fatalf("VerifyEmail() error: %v", err)
	}
	if !isVerified(t, db, session.User.ID) {
		t.Error("account is not verified after VerifyEmail()")
	}
	if err := svc.VerifyEmail(ctx, token); err == nil {
		t.Error("VerifyEmail() accepted an already used token")
	}
}

func TestVerifyEmailRejectsUnknownAndExpiredTokens(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()
	svc := auth.NewService(db, "notre_cinema_session", "development")
	_, session := registerUser(t, svc, "verify-bad")

	if err := svc.VerifyEmail(ctx, rawToken(t)); err == nil {
		t.Error("VerifyEmail() accepted an unknown token")
	}

	expired := rawToken(t)
	mintToken(t, db, session.User.ID, "verify_email", expired, -time.Minute)
	if err := svc.VerifyEmail(ctx, expired); err == nil {
		t.Error("VerifyEmail() accepted an expired token")
	}
	if isVerified(t, db, session.User.ID) {
		t.Error("account became verified through a rejected token")
	}
}

func TestVerifyEmailRejectsResetToken(t *testing.T) {
	db := connectOrSkip(t)
	svc := auth.NewService(db, "notre_cinema_session", "development")
	_, session := registerUser(t, svc, "verify-wrongkind")

	token := rawToken(t)
	mintToken(t, db, session.User.ID, "reset_password", token, time.Hour)
	if err := svc.VerifyEmail(context.Background(), token); err == nil {
		t.Error("a password-reset token must not verify an email")
	}
}

func TestRequestEmailVerificationOnlyForUnverified(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()
	svc := auth.NewService(db, "notre_cinema_session", "development")
	_, session := registerUser(t, svc, "verify-resend")

	if err := svc.RequestEmailVerification(ctx, session.User.ID); err != nil {
		t.Fatalf("RequestEmailVerification() error: %v", err)
	}
	if got := countEvents(t, db, session.User.ID, "email.verification_requested"); got != 2 {
		t.Errorf("verification events = %d, want 2 (register + resend)", got)
	}

	token := rawToken(t)
	mintToken(t, db, session.User.ID, "verify_email", token, time.Hour)
	if err := svc.VerifyEmail(ctx, token); err != nil {
		t.Fatalf("VerifyEmail() error: %v", err)
	}
	if err := svc.RequestEmailVerification(ctx, session.User.ID); err == nil {
		t.Error("RequestEmailVerification() succeeded for an already verified account")
	}
	if got := countEvents(t, db, session.User.ID, "email.verification_requested"); got != 2 {
		t.Errorf("verification events = %d, want still 2", got)
	}
}

func TestPasswordResetRequestIsSilentForUnknownEmail(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()
	svc := auth.NewService(db, "notre_cinema_session", "development")

	if err := svc.RequestPasswordReset(ctx, randomEmail(t, "nobody")); err != nil {
		t.Errorf("RequestPasswordReset() for unknown email returned error: %v", err)
	}

	email, session := registerUser(t, svc, "reset-known")
	if err := svc.RequestPasswordReset(ctx, "  "+email+"  "); err != nil {
		t.Fatalf("RequestPasswordReset() error: %v", err)
	}
	if got := countEvents(t, db, session.User.ID, "email.password_reset_requested"); got != 1 {
		t.Errorf("reset events = %d, want 1", got)
	}
}

func TestResetPasswordChangesPasswordAndRevokesSessions(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()
	svc := auth.NewService(db, "notre_cinema_session", "development")
	email, session := registerUser(t, svc, "reset-flow")

	if got := sessionCount(t, db, session.User.ID); got == 0 {
		t.Fatal("registration must have created a session")
	}

	token := rawToken(t)
	mintToken(t, db, session.User.ID, "reset_password", token, time.Hour)

	const newPassword = "brand-new-Passw0rd!"
	if err := svc.ResetPassword(ctx, token, newPassword); err != nil {
		t.Fatalf("ResetPassword() error: %v", err)
	}

	if got := sessionCount(t, db, session.User.ID); got != 0 {
		t.Errorf("sessions after reset = %d, want 0 (everyone is logged out)", got)
	}
	if got := countEvents(t, db, session.User.ID, "security.password_changed"); got != 1 {
		t.Errorf("password_changed events = %d, want 1", got)
	}
	if !isVerified(t, db, session.User.ID) {
		t.Error("a successful reset proves mailbox ownership and must verify the email")
	}

	if _, err := svc.Login(ctx, email, testPassword); err == nil {
		t.Error("old password still works after reset")
	}
	if _, err := svc.Login(ctx, email, newPassword); err != nil {
		t.Errorf("new password does not work: %v", err)
	}

	if err := svc.ResetPassword(ctx, token, "another-Passw0rd!"); err == nil {
		t.Error("ResetPassword() accepted an already used token")
	}
}

func TestResetPasswordRejectsBadTokens(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()
	svc := auth.NewService(db, "notre_cinema_session", "development")
	email, session := registerUser(t, svc, "reset-bad")

	if err := svc.ResetPassword(ctx, rawToken(t), "brand-new-Passw0rd!"); err == nil {
		t.Error("ResetPassword() accepted an unknown token")
	}

	expired := rawToken(t)
	mintToken(t, db, session.User.ID, "reset_password", expired, -time.Minute)
	if err := svc.ResetPassword(ctx, expired, "brand-new-Passw0rd!"); err == nil {
		t.Error("ResetPassword() accepted an expired token")
	}

	// Токен подтверждения почты не должен сбрасывать пароль.
	verifyKind := rawToken(t)
	mintToken(t, db, session.User.ID, "verify_email", verifyKind, time.Hour)
	if err := svc.ResetPassword(ctx, verifyKind, "brand-new-Passw0rd!"); err == nil {
		t.Error("a verification token must not reset a password")
	}

	if _, err := svc.Login(ctx, email, testPassword); err != nil {
		t.Errorf("original password stopped working after rejected resets: %v", err)
	}
}

func TestNewResetTokenInvalidatesPreviousOne(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()
	svc := auth.NewService(db, "notre_cinema_session", "development")
	_, session := registerUser(t, svc, "reset-rotate")

	first := rawToken(t)
	mintToken(t, db, session.User.ID, "reset_password", first, time.Hour)
	second := rawToken(t)
	mintToken(t, db, session.User.ID, "reset_password", second, time.Hour)

	if err := svc.ResetPassword(ctx, first, "brand-new-Passw0rd!"); err == nil {
		t.Error("the older reset link still works after a newer one was issued")
	}
	if err := svc.ResetPassword(ctx, second, "brand-new-Passw0rd!"); err != nil {
		t.Errorf("the newest reset link does not work: %v", err)
	}
}

func TestTwoFactorChangesQueueSecurityEmails(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()
	svc := auth.NewService(db, "notre_cinema_session", "development")
	_, session := registerUser(t, svc, "2fa-events")

	setup, err := svc.StartTwoFactorSetup(ctx, session.User.ID, session.User.Email)
	if err != nil {
		t.Fatalf("StartTwoFactorSetup() error: %v", err)
	}
	code, err := totp.GenerateCode(setup.Secret, time.Now())
	if err != nil {
		t.Fatalf("GenerateCode() error: %v", err)
	}

	if got := countEvents(t, db, session.User.ID, "security.two_factor_enabled"); got != 0 {
		t.Errorf("enabled events before confirm = %d, want 0", got)
	}
	if err := svc.ConfirmTwoFactor(ctx, session.User.ID, code); err != nil {
		t.Fatalf("ConfirmTwoFactor() error: %v", err)
	}
	if got := countEvents(t, db, session.User.ID, "security.two_factor_enabled"); got != 1 {
		t.Errorf("enabled events = %d, want 1", got)
	}

	if err := svc.DisableTwoFactor(ctx, session.User.ID, "000000"); err == nil {
		t.Error("DisableTwoFactor() accepted a wrong code")
	}
	if got := countEvents(t, db, session.User.ID, "security.two_factor_disabled"); got != 0 {
		t.Errorf("disabled events after a rejected attempt = %d, want 0", got)
	}

	code, err = totp.GenerateCode(setup.Secret, time.Now())
	if err != nil {
		t.Fatalf("GenerateCode() error: %v", err)
	}
	if err := svc.DisableTwoFactor(ctx, session.User.ID, code); err != nil {
		t.Fatalf("DisableTwoFactor() error: %v", err)
	}
	if got := countEvents(t, db, session.User.ID, "security.two_factor_disabled"); got != 1 {
		t.Errorf("disabled events = %d, want 1", got)
	}
}

func TestOAuthSignInMarksEmailVerified(t *testing.T) {
	db := connectOrSkip(t)
	svc := auth.NewService(db, "notre_cinema_session", "development")

	session, err := svc.CreateSessionForEmail(context.Background(), randomEmail(t, "oauth"), "GitHub User")
	if err != nil {
		t.Fatalf("CreateSessionForEmail() error: %v", err)
	}
	if !isVerified(t, db, session.User.ID) {
		t.Error("GitHub only returns verified emails, the account must start verified")
	}
	if got := countEvents(t, db, session.User.ID, "email.verification_requested"); got != 0 {
		t.Errorf("verification events for an OAuth account = %d, want 0", got)
	}
}
