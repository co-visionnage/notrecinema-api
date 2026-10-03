//go:build integration

// Запуск: DATABASE_URL=... go test -tags integration ./...
package auth_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"

	"notrecinema/api/internal/auth"
	"notrecinema/api/internal/postgres"
)

func connectOrSkip(t *testing.T) *postgres.Pool {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL не задан, пропускаем интеграционный тест")
	}
	db, err := postgres.Connect(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

func randomEmail(t *testing.T, label string) string {
	t.Helper()
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("failed to generate random suffix: %v", err)
	}
	return fmt.Sprintf("auth-test-%s-%s@example.com", label, hex.EncodeToString(buf))
}

func TestRegisterThenLoginSucceeds(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()
	svc := auth.NewService(db, "notre_cinema_session", "development")

	email := randomEmail(t, "register")
	session, err := svc.Register(ctx, email, "Test User", "correct-horse-battery-1!")
	if err != nil {
		t.Fatalf("Register() error: %v", err)
	}
	if session.User.Email != email {
		t.Errorf("Register() email = %q, want %q", session.User.Email, email)
	}
	if session.Token == "" {
		t.Error("Register() returned empty token")
	}

	// Повторная регистрация тем же email -- ACCOUNT_ALREADY_EXISTS.
	if _, err := svc.Register(ctx, email, "Other Name", "another-Pass1!"); err == nil {
		t.Error("Register() with duplicate email succeeded, want error")
	}

	result, err := svc.Login(ctx, email, "correct-horse-battery-1!")
	if err != nil {
		t.Fatalf("Login() error: %v", err)
	}
	if result.RequiresTwoFactor {
		t.Fatal("Login() requires two-factor for an account without it enabled")
	}
	if result.Session.User.ID != session.User.ID {
		t.Errorf("Login() user ID = %q, want %q", result.Session.User.ID, session.User.ID)
	}
}

func TestLoginRejectsWrongPassword(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()
	svc := auth.NewService(db, "notre_cinema_session", "development")

	email := randomEmail(t, "wrongpass")
	if _, err := svc.Register(ctx, email, "Test User", "correct-horse-battery-1!"); err != nil {
		t.Fatalf("Register() error: %v", err)
	}

	if _, err := svc.Login(ctx, email, "totally-wrong-1!"); err == nil {
		t.Error("Login() with wrong password succeeded, want error")
	}
}

func TestLoginRejectsUnknownEmail(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()
	svc := auth.NewService(db, "notre_cinema_session", "development")

	if _, err := svc.Login(ctx, randomEmail(t, "nosuchuser"), "whatever-Pass1!"); err == nil {
		t.Error("Login() with unknown email succeeded, want error")
	}
}

func TestLogoutDeletesSession(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()
	svc := auth.NewService(db, "notre_cinema_session", "development")

	email := randomEmail(t, "logout")
	session, err := svc.Register(ctx, email, "Test User", "correct-horse-battery-1!")
	if err != nil {
		t.Fatalf("Register() error: %v", err)
	}

	if err := svc.Logout(ctx, session.Token); err != nil {
		t.Fatalf("Logout() error: %v", err)
	}

	// После логаута сам логин снова должен быть возможен (сессия удалена,
	// не аккаунт) -- но повторный логин уже не видит старый токен как валидный.
	var count int
	if err := db.QueryRow(ctx, `
		SELECT count(*) FROM public.app_sessions WHERE user_id = $1
	`, session.User.ID).Scan(&count); err != nil {
		t.Fatalf("failed to count sessions: %v", err)
	}
	if count != 0 {
		t.Errorf("app_sessions count after logout = %d, want 0", count)
	}
}

func TestTwoFactorSetupConfirmLoginDisable(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()
	svc := auth.NewService(db, "notre_cinema_session", "development")

	email := randomEmail(t, "2fa")
	session, err := svc.Register(ctx, email, "Test User", "correct-horse-battery-1!")
	if err != nil {
		t.Fatalf("Register() error: %v", err)
	}
	userID := session.User.ID

	setup, err := svc.StartTwoFactorSetup(ctx, userID, email)
	if err != nil {
		t.Fatalf("StartTwoFactorSetup() error: %v", err)
	}
	if setup.Secret == "" || setup.QRCodeDataURL == "" {
		t.Fatalf("StartTwoFactorSetup() = %+v, want non-empty secret and QR", setup)
	}

	code, err := totp.GenerateCode(setup.Secret, time.Now())
	if err != nil {
		t.Fatalf("failed to generate totp code: %v", err)
	}
	if _, err := svc.ConfirmTwoFactor(ctx, userID, code); err != nil {
		t.Fatalf("ConfirmTwoFactor() error: %v", err)
	}

	enabled, _, err := svc.TwoFactorStatus(ctx, userID)
	if err != nil {
		t.Fatalf("TwoFactorStatus() error: %v", err)
	}
	if !enabled {
		t.Fatal("TwoFactorStatus() = false after ConfirmTwoFactor")
	}

	// Логин теперь должен требовать 2FA, а не сразу выдавать сессию.
	result, err := svc.Login(ctx, email, "correct-horse-battery-1!")
	if err != nil {
		t.Fatalf("Login() error: %v", err)
	}
	if !result.RequiresTwoFactor {
		t.Fatal("Login() does not require two-factor after it was enabled")
	}

	loginCode, err := totp.GenerateCode(setup.Secret, time.Now())
	if err != nil {
		t.Fatalf("failed to generate totp code: %v", err)
	}
	verified, err := svc.VerifyTwoFactor(ctx, result.ChallengeToken, loginCode)
	if err != nil {
		t.Fatalf("VerifyTwoFactor() error: %v", err)
	}
	if verified.User.ID != userID {
		t.Errorf("VerifyTwoFactor() user ID = %q, want %q", verified.User.ID, userID)
	}

	disableCode, err := totp.GenerateCode(setup.Secret, time.Now())
	if err != nil {
		t.Fatalf("failed to generate totp code: %v", err)
	}
	if err := svc.DisableTwoFactor(ctx, userID, disableCode); err != nil {
		t.Fatalf("DisableTwoFactor() error: %v", err)
	}

	enabled, _, err = svc.TwoFactorStatus(ctx, userID)
	if err != nil {
		t.Fatalf("TwoFactorStatus() error: %v", err)
	}
	if enabled {
		t.Fatal("TwoFactorStatus() = true after DisableTwoFactor")
	}
}
