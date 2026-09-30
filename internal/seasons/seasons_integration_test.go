//go:build integration

// Запуск: DATABASE_URL=... go test -tags integration ./...
//
// Как и в calendar_integration_test.go, реальные вызовы Kinopoisk/OMDb
// здесь не тестируются -- lookup.go бьёт по жёстко заданным URL внешних
// API. Эти тесты проверяют то, что не требует сети: RLS-видимость сериала
// и отбраковку сериалов без привязки к внешнему каталогу.
package seasons_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"notrecinema/api/internal/platform/apperror"
	"notrecinema/api/internal/postgres"
	"notrecinema/api/internal/seasons"
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
	email := fmt.Sprintf("seasons-test-%s@example.com", token)

	var userID string
	row := db.QueryRow(ctx,
		"SELECT user_id FROM public.create_profile_session($1, $2, $3, $4)",
		email, "Seasons Test", hex.EncodeToString(hash[:]), time.Now().Add(time.Hour),
	)
	if err := row.Scan(&userID); err != nil {
		t.Fatalf("failed to create test user: %v", err)
	}
	return userID
}

func createFamilyWithSeries(t *testing.T, ctx context.Context, db *postgres.Pool, ownerID string, externalSource, externalID *string) (familyID, seriesID string) {
	t.Helper()
	err := db.WithUserContext(ctx, ownerID, func(ctx context.Context, tx pgx.Tx) error {
		tokenBytes := make([]byte, 4)
		_, _ = rand.Read(tokenBytes)

		if err := tx.QueryRow(ctx, `
			INSERT INTO public.families (name, owner_id, invite_code)
			VALUES ($1, $2, $3) RETURNING id
		`, "Seasons Test Family", ownerID, hex.EncodeToString(tokenBytes)).Scan(&familyID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO public.family_members (family_id, user_id, role) VALUES ($1, $2, 'owner')
		`, familyID, ownerID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO public.family_series (family_id, title, media_type, external_source, external_id, created_by)
			VALUES ($1, 'Season Show', 'series', $2, $3, $4) RETURNING id
		`, familyID, externalSource, externalID, ownerID).Scan(&seriesID)
	})
	if err != nil {
		t.Fatalf("failed to seed family/series: %v", err)
	}
	return familyID, seriesID
}

func TestCheckSeriesUpdatesRejectsUnlinkedSeries(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()

	owner := createTestUser(t, ctx, db)
	_, seriesID := createFamilyWithSeries(t, ctx, db, owner, nil, nil)

	svc := seasons.NewService(db, "fake-kinopoisk-key", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err := svc.CheckSeriesUpdates(ctx, owner, seriesID)
	if err == nil {
		t.Fatal("CheckSeriesUpdates() error = nil, want invalid_input")
	}
	if status := apperror.HTTPStatus(err); status != 400 {
		t.Errorf("HTTPStatus = %d, want 400", status)
	}
}

func TestCheckSeriesUpdatesRejectsUnknownSeriesForUser(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()

	owner := createTestUser(t, ctx, db)
	outsider := createTestUser(t, ctx, db)
	source, id := "kinopoisk", "301"
	_, seriesID := createFamilyWithSeries(t, ctx, db, owner, &source, &id)

	svc := seasons.NewService(db, "fake-kinopoisk-key", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err := svc.CheckSeriesUpdates(ctx, outsider, seriesID)
	if err == nil {
		t.Fatal("CheckSeriesUpdates() error = nil, want not_found (RLS should hide the series)")
	}
	if status := apperror.HTTPStatus(err); status != 404 {
		t.Errorf("HTTPStatus = %d, want 404", status)
	}
}
