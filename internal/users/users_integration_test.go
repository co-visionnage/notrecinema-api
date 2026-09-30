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
