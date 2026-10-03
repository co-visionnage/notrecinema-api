//go:build integration

// Запуск: go test -tags integration ./...
package events_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"notrecinema/api/internal/events"
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

func createTestUser(t *testing.T, ctx context.Context, db *postgres.Pool) string {
	t.Helper()
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}
	token := hex.EncodeToString(tokenBytes)
	hash := sha256.Sum256([]byte(token))
	email := fmt.Sprintf("events-test-%s@example.com", token)

	var userID string
	row := db.QueryRow(ctx,
		"SELECT user_id FROM public.create_profile_session($1, $2, $3, $4)",
		email, "Events Test", hex.EncodeToString(hash[:]), time.Now().Add(time.Hour),
	)
	if err := row.Scan(&userID); err != nil {
		t.Fatalf("failed to create test user: %v", err)
	}
	return userID
}

func createTestFamily(t *testing.T, ctx context.Context, db *postgres.Pool, ownerID string) string {
	t.Helper()
	tokenBytes := make([]byte, 4)
	_, _ = rand.Read(tokenBytes)

	var familyID string
	err := db.WithUserContext(ctx, ownerID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO public.families (name, owner_id, invite_code)
			VALUES ($1, $2, $3)
			RETURNING id
		`, "Events Test Family", ownerID, hex.EncodeToString(tokenBytes)).Scan(&familyID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO public.family_members (family_id, user_id, role)
			VALUES ($1, $2, 'owner')
		`, familyID, ownerID)
		return err
	})
	if err != nil {
		t.Fatalf("failed to create test family: %v", err)
	}
	return familyID
}

func addFamilyMember(t *testing.T, ctx context.Context, db *postgres.Pool, familyID, userID string) {
	t.Helper()
	err := db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.family_members (family_id, user_id, role)
			VALUES ($1, $2, 'member')
		`, familyID, userID)
		return err
	})
	if err != nil {
		t.Fatalf("failed to add family member: %v", err)
	}
}

func TestEventCreateRSVPAndDelete(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()

	owner := createTestUser(t, ctx, db)
	attendee := createTestUser(t, ctx, db)
	familyID := createTestFamily(t, ctx, db, owner)
	addFamilyMember(t, ctx, db, familyID, attendee)

	svc := events.NewService(db)

	event, err := svc.Create(ctx, owner, familyID, events.CreateInput{
		Title:       "Киновечер",
		ScheduledAt: time.Now().Add(48 * time.Hour),
	})
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	if err := svc.SetRSVP(ctx, attendee, event.ID, events.SetRSVPInput{Status: "going"}); err != nil {
		t.Fatalf("SetRSVP() error: %v", err)
	}

	list, err := svc.ListForFamily(ctx, owner, familyID)
	if err != nil {
		t.Fatalf("ListForFamily() error: %v", err)
	}
	// Создатель идёт автоматически, плюс отметка участника.
	if len(list) != 1 || len(list[0].RSVPs) != 2 {
		t.Fatalf("ListForFamily() = %+v, want 1 событие с 2 RSVP (создатель и участник)", list)
	}
	for _, rsvp := range list[0].RSVPs {
		if rsvp.Status != "going" || rsvp.DisplayName == "" {
			t.Errorf("RSVP = %+v, want going with a display name", rsvp)
		}
	}

	// attendee не создатель и не владелец семьи -- удалить событие не может.
	if err := svc.Delete(ctx, attendee, event.ID); err == nil {
		t.Error("Delete() участником-не-автором succeeded, want error")
	}

	if err := svc.Delete(ctx, owner, event.ID); err != nil {
		t.Fatalf("Delete() автором error: %v", err)
	}
}

func TestSetRSVPRejectsInvalidStatus(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()

	owner := createTestUser(t, ctx, db)
	familyID := createTestFamily(t, ctx, db, owner)
	svc := events.NewService(db)

	event, err := svc.Create(ctx, owner, familyID, events.CreateInput{
		Title:       "Валидация",
		ScheduledAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	if err := svc.SetRSVP(ctx, owner, event.ID, events.SetRSVPInput{Status: "maybe-not-a-real-status"}); err == nil {
		t.Error("SetRSVP() с невалидным статусом succeeded, want error")
	}
}
