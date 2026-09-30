//go:build integration

// Запуск: DATABASE_URL=... go test -tags integration ./...
package export_test

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

	"notrecinema/api/internal/export"
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

func createTestUser(t *testing.T, ctx context.Context, db *postgres.Pool) (string, string) {
	t.Helper()
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}
	token := hex.EncodeToString(tokenBytes)
	hash := sha256.Sum256([]byte(token))
	email := fmt.Sprintf("export-test-%s@example.com", token)

	var userID string
	row := db.QueryRow(ctx,
		"SELECT user_id FROM public.create_profile_session($1, $2, $3, $4)",
		email, "Export Test", hex.EncodeToString(hash[:]), time.Now().Add(time.Hour),
	)
	if err := row.Scan(&userID); err != nil {
		t.Fatalf("failed to create test user: %v", err)
	}
	return userID, email
}

func TestGetExportDataIncludesFamilyAndSeries(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()

	owner, email := createTestUser(t, ctx, db)

	var seriesID string
	err := db.WithUserContext(ctx, owner, func(ctx context.Context, tx pgx.Tx) error {
		tokenBytes := make([]byte, 4)
		_, _ = rand.Read(tokenBytes)

		var familyID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO public.families (name, owner_id, invite_code)
			VALUES ($1, $2, $3) RETURNING id
		`, "Export Test Family", owner, hex.EncodeToString(tokenBytes)).Scan(&familyID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO public.family_members (family_id, user_id, role) VALUES ($1, $2, 'owner')
		`, familyID, owner); err != nil {
			return err
		}

		if err := tx.QueryRow(ctx, `
			INSERT INTO public.family_series (family_id, title, genres, year, media_type, created_by)
			VALUES ($1, 'Export Show', $2, 2024, 'series', $3) RETURNING id
		`, familyID, []string{"drama"}, owner).Scan(&seriesID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO public.family_series_status (series_id, user_id, status, rating, comment)
			VALUES ($1, $2, 'watched', 5, 'great show')
		`, seriesID, owner); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO public.family_series_progress (series_id, user_id, current_season, current_episode)
			VALUES ($1, $2, 2, 5)
		`, seriesID, owner)
		return err
	})
	if err != nil {
		t.Fatalf("failed to seed export data: %v", err)
	}

	svc := export.NewService(db)
	data, err := svc.GetExportData(ctx, owner)
	if err != nil {
		t.Fatalf("GetExportData() error: %v", err)
	}

	if data.Profile.Email != email {
		t.Errorf("Profile.Email = %q, want %q", data.Profile.Email, email)
	}
	if data.Family == nil || data.Family.FamilyName != "Export Test Family" || data.Family.Role != "owner" {
		t.Errorf("Family = %+v, want name=%q role=owner", data.Family, "Export Test Family")
	}
	if len(data.Series) != 1 {
		t.Fatalf("Series = %d entries, want 1: %+v", len(data.Series), data.Series)
	}
	row := data.Series[0]
	if row.Title != "Export Show" || row.Status == nil || *row.Status != "watched" || row.Rating == nil || *row.Rating != 5 {
		t.Errorf("Series[0] = %+v, unexpected values", row)
	}
	if row.CurrentSeason == nil || *row.CurrentSeason != 2 || row.CurrentEpisode == nil || *row.CurrentEpisode != 5 {
		t.Errorf("Series[0] progress = season=%v episode=%v, want 2/5", row.CurrentSeason, row.CurrentEpisode)
	}
}
