//go:build integration

// Запуск: DATABASE_URL=... go test -tags integration ./...
package history_test

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

	"notrecinema/api/internal/history"
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

func createTestUser(t *testing.T, ctx context.Context, db *postgres.Pool, label string) string {
	t.Helper()
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}
	token := hex.EncodeToString(tokenBytes)
	hash := sha256.Sum256([]byte(token))
	email := fmt.Sprintf("history-test-%s-%s@example.com", label, token)

	var userID string
	row := db.QueryRow(ctx,
		"SELECT user_id FROM public.create_profile_session($1, $2, $3, $4)",
		email, "History Test "+label, hex.EncodeToString(hash[:]), time.Now().Add(time.Hour),
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
			VALUES ($1, $2, $3) RETURNING id
		`, "History Test Family", ownerID, hex.EncodeToString(tokenBytes)).Scan(&familyID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO public.family_members (family_id, user_id, role) VALUES ($1, $2, 'owner')
		`, familyID, ownerID)
		return err
	})
	if err != nil {
		t.Fatalf("failed to create test family: %v", err)
	}
	return familyID
}

func joinFamily(t *testing.T, ctx context.Context, db *postgres.Pool, familyID, userID string) {
	t.Helper()
	err := db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.family_members (family_id, user_id, role) VALUES ($1, $2, 'member')
		`, familyID, userID)
		return err
	})
	if err != nil {
		t.Fatalf("failed to join family: %v", err)
	}
}

// TestGetWatchHistorySeesWholeFamily проверяет главное, ради чего вообще
// понадобилась миграция 0008: один участник семьи должен видеть, что
// посмотрел другой, а не только свои собственные отметки "watched".
func TestGetWatchHistorySeesWholeFamily(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()

	owner := createTestUser(t, ctx, db, "owner")
	member := createTestUser(t, ctx, db, "member")
	familyID := createTestFamily(t, ctx, db, owner)
	joinFamily(t, ctx, db, familyID, member)

	err := db.WithUserContext(ctx, owner, func(ctx context.Context, tx pgx.Tx) error {
		var seriesID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO public.family_series (family_id, title, created_by)
			VALUES ($1, $2, $3) RETURNING id
		`, familyID, "Watched By Owner", owner).Scan(&seriesID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO public.family_series_status (series_id, user_id, status, rating, watched_at)
			VALUES ($1, $2, 'watched', 4, NOW())
		`, seriesID, owner)
		return err
	})
	if err != nil {
		t.Fatalf("failed to seed watched series: %v", err)
	}

	svc := history.NewService(db)
	page, err := svc.GetWatchHistory(ctx, member, familyID, 0)
	if err != nil {
		t.Fatalf("GetWatchHistory() error: %v", err)
	}

	if len(page.Entries) != 1 {
		t.Fatalf("Entries = %d, want 1: %+v", len(page.Entries), page.Entries)
	}
	entry := page.Entries[0]
	if entry.Title != "Watched By Owner" {
		t.Errorf("Title = %q, want %q", entry.Title, "Watched By Owner")
	}
	if entry.Rating == nil || *entry.Rating != 4 {
		t.Errorf("Rating = %v, want 4", entry.Rating)
	}
	if page.HasMore {
		t.Errorf("HasMore = true, want false")
	}
}
