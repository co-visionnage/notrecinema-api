//go:build integration

// Интеграционным тестам нужен настоящий Postgres по адресу $DATABASE_URL
// с уже применённой схемой notrecinema-app (см. docker-compose.yml — сервис
// migrate в нём как раз это и делает).
// Запуск: go test -tags integration ./...
package families_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"notrecinema/api/internal/families"
	"notrecinema/api/internal/platform/apperror"
	"notrecinema/api/internal/postgres"
)

// createTestUser регистрирует профиль напрямую через ту же функцию БД,
// которую Next.js-приложение использует для создания сессий, и возвращает
// ID нового пользователя.
func createTestUser(t *testing.T, ctx context.Context, db *postgres.Pool) string {
	t.Helper()

	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}
	token := hex.EncodeToString(tokenBytes)
	hash := sha256.Sum256([]byte(token))

	email := fmt.Sprintf("integration-%s@example.com", token)
	expiresAt := time.Now().Add(time.Hour)

	var userID string
	row := db.QueryRow(ctx,
		"SELECT user_id FROM public.create_profile_session($1, $2, $3, $4)",
		email, "Integration Test", hex.EncodeToString(hash[:]), expiresAt,
	)
	if err := row.Scan(&userID); err != nil {
		t.Fatalf("failed to create test user: %v", err)
	}
	return userID
}

func TestFamiliesRLSIsolatesUsers(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	db, err := postgres.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer db.Close()

	svc := families.NewService(db)

	owner := createTestUser(t, ctx, db)
	stranger := createTestUser(t, ctx, db)

	created, err := svc.Create(ctx, owner, "  Integration Family  ")
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	if created.Name != "Integration Family" {
		t.Errorf("Create() name = %q, want trimmed %q", created.Name, "Integration Family")
	}

	ownerFamilies, err := svc.ListForUser(ctx, owner)
	if err != nil {
		t.Fatalf("ListForUser(owner) error: %v", err)
	}
	if len(ownerFamilies) != 1 || ownerFamilies[0].ID != created.ID {
		t.Errorf("ListForUser(owner) = %+v, want exactly the created family", ownerFamilies)
	}

	strangerFamilies, err := svc.ListForUser(ctx, stranger)
	if err != nil {
		t.Fatalf("ListForUser(stranger) error: %v", err)
	}
	if len(strangerFamilies) != 0 {
		t.Errorf("ListForUser(stranger) = %+v, want empty -- RLS should hide the owner's family", strangerFamilies)
	}
}

func TestFamiliesCreateRejectsBlankName(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	db, err := postgres.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer db.Close()

	svc := families.NewService(db)
	owner := createTestUser(t, ctx, db)

	if _, err := svc.Create(ctx, owner, "   "); err == nil {
		t.Error("Create() with blank name succeeded, want an error")
	}
}

func TestJoinAddsMemberAndFiresEvent(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	db, err := postgres.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer db.Close()

	svc := families.NewService(db)
	owner := createTestUser(t, ctx, db)
	joiner := createTestUser(t, ctx, db)

	created, err := svc.Create(ctx, owner, "Joinable Family")
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	joined, err := svc.Join(ctx, joiner, created.InviteCode)
	if err != nil {
		t.Fatalf("Join() error: %v", err)
	}
	if joined.ID != created.ID {
		t.Errorf("Join() family ID = %q, want %q", joined.ID, created.ID)
	}

	joinerFamilies, err := svc.ListForUser(ctx, joiner)
	if err != nil {
		t.Fatalf("ListForUser(joiner) error: %v", err)
	}
	if len(joinerFamilies) != 1 || joinerFamilies[0].ID != created.ID {
		t.Errorf("ListForUser(joiner) = %+v, want exactly the joined family", joinerFamilies)
	}

	// Повторный join тем же пользователем -- конфликт (UNIQUE family_id+user_id).
	if _, err := svc.Join(ctx, joiner, created.InviteCode); err == nil {
		t.Error("Join() повторно тем же пользователем succeeded, want error")
	}
}

func TestJoinRejectsUnknownInviteCode(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	db, err := postgres.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer db.Close()

	svc := families.NewService(db)
	user := createTestUser(t, ctx, db)

	if _, err := svc.Join(ctx, user, "NOSUCHCODE"); err == nil {
		t.Error("Join() с несуществующим кодом succeeded, want error")
	}
}

func connectOrSkip(t *testing.T) *postgres.Pool {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	db, err := postgres.Connect(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

func TestListMembersOwnerFirstThenByJoinedAt(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()
	svc := families.NewService(db)

	owner := createTestUser(t, ctx, db)
	member := createTestUser(t, ctx, db)
	outsider := createTestUser(t, ctx, db)

	created, err := svc.Create(ctx, owner, "Members Family")
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	if _, err := svc.Join(ctx, member, created.InviteCode); err != nil {
		t.Fatalf("Join() error: %v", err)
	}

	members, err := svc.ListMembers(ctx, owner, created.ID)
	if err != nil {
		t.Fatalf("ListMembers(owner) error: %v", err)
	}
	if len(members) != 2 || members[0].Role != "owner" || members[0].UserID != owner {
		t.Fatalf("ListMembers() = %+v, want owner first", members)
	}
	if members[1].UserID != member || members[1].Role != "member" {
		t.Fatalf("ListMembers()[1] = %+v, want the joined member", members[1])
	}

	if _, err := svc.ListMembers(ctx, outsider, created.ID); err == nil {
		t.Error("ListMembers(outsider) succeeded, want not_found -- outsider isn't a member")
	}
}

func TestRemoveMemberOnlyOwnerCanRemoveNonOwner(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()
	svc := families.NewService(db)

	owner := createTestUser(t, ctx, db)
	member := createTestUser(t, ctx, db)

	created, err := svc.Create(ctx, owner, "Remove Family")
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	if _, err := svc.Join(ctx, member, created.InviteCode); err != nil {
		t.Fatalf("Join() error: %v", err)
	}

	// Не-владелец не может удалять.
	if err := svc.RemoveMember(ctx, member, created.ID, member); err == nil {
		t.Error("RemoveMember(member, self) succeeded, want forbidden -- only the owner can remove")
	} else if apperror.HTTPStatus(err) != 403 {
		t.Errorf("HTTPStatus = %d, want 403", apperror.HTTPStatus(err))
	}

	// Владельца удалить нельзя -- даже самим владельцем.
	if err := svc.RemoveMember(ctx, owner, created.ID, owner); err == nil {
		t.Error("RemoveMember(owner, owner) succeeded, want forbidden -- can't remove the owner")
	}

	if err := svc.RemoveMember(ctx, owner, created.ID, member); err != nil {
		t.Fatalf("RemoveMember(owner, member) error: %v", err)
	}

	members, err := svc.ListMembers(ctx, owner, created.ID)
	if err != nil {
		t.Fatalf("ListMembers() error: %v", err)
	}
	if len(members) != 1 {
		t.Errorf("ListMembers() after removal = %+v, want only the owner left", members)
	}
}

func TestSetMemberRoleRejectsOwnerRoleAndNonOwnerCaller(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()
	svc := families.NewService(db)

	owner := createTestUser(t, ctx, db)
	member := createTestUser(t, ctx, db)

	created, err := svc.Create(ctx, owner, "Role Family")
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	if _, err := svc.Join(ctx, member, created.InviteCode); err != nil {
		t.Fatalf("Join() error: %v", err)
	}

	if err := svc.SetMemberRole(ctx, owner, created.ID, member, "owner"); err == nil {
		t.Error("SetMemberRole(..., \"owner\") succeeded, want invalid_input")
	}

	if err := svc.SetMemberRole(ctx, member, created.ID, member, "admin"); err == nil {
		t.Error("SetMemberRole(member, self) succeeded, want forbidden -- only the owner can change roles")
	}

	if err := svc.SetMemberRole(ctx, owner, created.ID, member, "admin"); err != nil {
		t.Fatalf("SetMemberRole(owner, member, admin) error: %v", err)
	}

	members, err := svc.ListMembers(ctx, owner, created.ID)
	if err != nil {
		t.Fatalf("ListMembers() error: %v", err)
	}
	var found bool
	for _, m := range members {
		if m.UserID == member {
			found = true
			if m.Role != "admin" {
				t.Errorf("member role = %q, want admin", m.Role)
			}
		}
	}
	if !found {
		t.Fatal("member not found in ListMembers() after role change")
	}
}

func TestTransferOwnershipMovesRoles(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()
	svc := families.NewService(db)

	owner := createTestUser(t, ctx, db)
	member := createTestUser(t, ctx, db)

	created, err := svc.Create(ctx, owner, "Transfer Family")
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	if _, err := svc.Join(ctx, member, created.InviteCode); err != nil {
		t.Fatalf("Join() error: %v", err)
	}

	if err := svc.TransferOwnership(ctx, owner, created.ID, member); err != nil {
		t.Fatalf("TransferOwnership() error: %v", err)
	}

	members, err := svc.ListMembers(ctx, member, created.ID)
	if err != nil {
		t.Fatalf("ListMembers() error: %v", err)
	}
	roles := map[string]string{}
	for _, m := range members {
		roles[m.UserID] = m.Role
	}
	if roles[member] != "owner" {
		t.Errorf("new owner role = %q, want owner", roles[member])
	}
	if roles[owner] != "member" {
		t.Errorf("former owner role = %q, want member", roles[owner])
	}

	// Бывший владелец больше не может передавать владение -- он уже не owner.
	if err := svc.TransferOwnership(ctx, owner, created.ID, member); err == nil {
		t.Error("TransferOwnership() by former owner succeeded, want error")
	}
}
