//go:build integration

// Запуск: DATABASE_URL=... go test -tags integration ./...
package users_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"notrecinema/api/internal/auth"
	"notrecinema/api/internal/platform/apperror"
	"notrecinema/api/internal/postgres"
	"notrecinema/api/internal/users"
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
	return fmt.Sprintf("users-test-%s-%s@example.com", label, hex.EncodeToString(buf))
}

func TestDeleteAccountRejectsWrongPassword(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()
	authSvc := auth.NewService(db, "notre_cinema_session", "development")
	usersSvc := users.NewService(db)

	session, err := authSvc.Register(ctx, randomEmail(t, "wrongpass"), "Test User", "correct-horse-battery-1!")
	if err != nil {
		t.Fatalf("Register() error: %v", err)
	}

	if err := usersSvc.DeleteAccount(ctx, session.User.ID, "totally-wrong-1!"); err == nil {
		t.Error("DeleteAccount() with wrong password succeeded, want error")
	}
}

func TestDeleteAccountBlocksIfOwnerOfMultiMemberFamily(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()
	authSvc := auth.NewService(db, "notre_cinema_session", "development")
	usersSvc := users.NewService(db)

	owner, err := authSvc.Register(ctx, randomEmail(t, "owner"), "Owner", "correct-horse-battery-1!")
	if err != nil {
		t.Fatalf("Register(owner) error: %v", err)
	}
	memberEmail := randomEmail(t, "member")
	member, err := authSvc.Register(ctx, memberEmail, "Member", "correct-horse-battery-1!")
	if err != nil {
		t.Fatalf("Register(member) error: %v", err)
	}

	inviteCodeBytes := make([]byte, 6)
	if _, err := rand.Read(inviteCodeBytes); err != nil {
		t.Fatalf("failed to generate invite code: %v", err)
	}

	var familyID, inviteCode string
	err = db.WithUserContext(ctx, owner.User.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO public.families (name, owner_id, invite_code)
			VALUES ('Owner Family', $1, $2) RETURNING id, invite_code
		`, owner.User.ID, hex.EncodeToString(inviteCodeBytes)).Scan(&familyID, &inviteCode)
	})
	if err != nil {
		t.Fatalf("failed to create family: %v", err)
	}
	if err := db.WithUserContext(ctx, owner.User.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO public.family_members (family_id, user_id, role) VALUES ($1, $2, 'owner')`, familyID, owner.User.ID)
		return err
	}); err != nil {
		t.Fatalf("failed to add owner membership: %v", err)
	}
	if err := db.WithUserContext(ctx, member.User.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO public.family_members (family_id, user_id, role) VALUES ($1, $2, 'member')`, familyID, member.User.ID)
		return err
	}); err != nil {
		t.Fatalf("failed to add member membership: %v", err)
	}

	err = usersSvc.DeleteAccount(ctx, owner.User.ID, "correct-horse-battery-1!")
	if err == nil {
		t.Fatal("DeleteAccount() for owner of multi-member family succeeded, want blocked")
	}
	if status := apperror.HTTPStatus(err); status != 409 {
		t.Errorf("HTTPStatus = %d, want 409", status)
	}

	// Профиль владельца должен остаться нетронутым -- проверяем через его
	// же RLS-контекст (profiles_select_self требует current_user_id()),
	// обычный db.QueryRow без WithUserContext всегда вернул бы "не найден"
	// независимо от того, реально ли строка удалена.
	var stillExists bool
	if err := db.WithUserContext(ctx, owner.User.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.profiles WHERE id = $1)`, owner.User.ID).Scan(&stillExists)
	}); err != nil {
		t.Fatalf("failed to check profile: %v", err)
	}
	if !stillExists {
		t.Error("owner profile was deleted despite the block")
	}
}

func TestDeleteAccountSucceedsAndCascades(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()
	authSvc := auth.NewService(db, "notre_cinema_session", "development")
	usersSvc := users.NewService(db)

	email := randomEmail(t, "solo")
	session, err := authSvc.Register(ctx, email, "Solo User", "correct-horse-battery-1!")
	if err != nil {
		t.Fatalf("Register() error: %v", err)
	}

	if err := usersSvc.DeleteAccount(ctx, session.User.ID, "correct-horse-battery-1!"); err != nil {
		t.Fatalf("DeleteAccount() error: %v", err)
	}

	// Ни один из двух столбцов (profiles, app_sessions) не проверить
	// напрямую после удаления -- RLS (profiles_select_self,
	// app_sessions_owner_only) без валидного current_user_id() всегда
	// вернул бы "не найдено", независимо от того, удалена строка на самом
	// деле или нет. Чёрным ящиком: аккаунта больше нет, если залогиниться
	// под тем же email/паролем больше нельзя.
	if _, err := authSvc.Login(ctx, email, "correct-horse-battery-1!"); err == nil {
		t.Error("Login() succeeded with the deleted account's credentials, want error")
	}
}

func TestDeleteAccountQueuesGoodbyeEmailWithAddress(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()
	authSvc := auth.NewService(db, "notre_cinema_session", "development")
	usersSvc := users.NewService(db)

	email := randomEmail(t, "goodbye")
	session, err := authSvc.Register(ctx, email, "Прощай", "correct-horse-battery-1!")
	if err != nil {
		t.Fatalf("Register() error: %v", err)
	}

	if err := usersSvc.DeleteAccount(ctx, session.User.ID, "correct-horse-battery-1!"); err != nil {
		t.Fatalf("DeleteAccount() error: %v", err)
	}

	// Профиль уже удалён, поэтому адрес и имя обязаны лежать в самом
	// событии -- иначе воркеру письмо отправлять будет некуда.
	var gotEmail, gotName string
	var gotVerified bool
	if err := db.QueryRow(ctx, `
		SELECT payload->>'email', payload->>'displayName', (payload->>'emailVerified')::boolean
		FROM public.outbox_events
		WHERE aggregate_id = $1 AND event_type = 'account.deleted'
	`, session.User.ID).Scan(&gotEmail, &gotName, &gotVerified); err != nil {
		t.Fatalf("account.deleted event not found: %v", err)
	}
	if gotEmail != email || gotName != "Прощай" {
		t.Errorf("event payload = (%q, %q), want (%q, %q)", gotEmail, gotName, email, "Прощай")
	}
	if gotVerified {
		t.Error("a freshly registered account was never verified, the event must say so")
	}
}

func TestDeleteAccountWithWrongPasswordQueuesNothing(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()
	authSvc := auth.NewService(db, "notre_cinema_session", "development")
	usersSvc := users.NewService(db)

	session, err := authSvc.Register(ctx, randomEmail(t, "nodelete"), "Test User", "correct-horse-battery-1!")
	if err != nil {
		t.Fatalf("Register() error: %v", err)
	}
	if err := usersSvc.DeleteAccount(ctx, session.User.ID, "wrong-password-1!"); err == nil {
		t.Fatal("DeleteAccount() with wrong password succeeded")
	}

	var n int
	if err := db.QueryRow(ctx, `
		SELECT count(*) FROM public.outbox_events WHERE aggregate_id = $1 AND event_type = 'account.deleted'
	`, session.User.ID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("account.deleted events after a rejected delete = %d, want 0", n)
	}
}

func TestProfileReportsEmailVerification(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()
	authSvc := auth.NewService(db, "notre_cinema_session", "development")
	usersSvc := users.NewService(db)

	session, err := authSvc.Register(ctx, randomEmail(t, "profile"), "Test User", "correct-horse-battery-1!")
	if err != nil {
		t.Fatalf("Register() error: %v", err)
	}

	profile, err := usersSvc.GetProfile(ctx, session.User.ID)
	if err != nil {
		t.Fatalf("GetProfile() error: %v", err)
	}
	if profile.EmailVerified {
		t.Error("a new password account must start unverified")
	}

	tokenHash := randomEmail(t, "token-hash")
	if err := db.Exec(ctx, `SELECT public.create_email_token($1, 'verify_email', $2, NOW() + interval '1 hour')`, session.User.ID, tokenHash); err != nil {
		t.Fatalf("create_email_token: %v", err)
	}
	var ok bool
	if err := db.QueryRow(ctx, `SELECT public.verify_email_with_token($1)`, tokenHash).Scan(&ok); err != nil || !ok {
		t.Fatalf("verify_email_with_token: ok=%v err=%v", ok, err)
	}

	profile, err = usersSvc.GetProfile(ctx, session.User.ID)
	if err != nil {
		t.Fatalf("GetProfile() error: %v", err)
	}
	if !profile.EmailVerified {
		t.Error("profile still reports unverified after verification")
	}
}
