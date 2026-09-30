//go:build integration

// Запуск: DATABASE_URL=... go test -tags integration ./...
//
// Реальный вызов TMDB здесь не тестируется -- calendar/tmdb.go бьёт по
// жёстко заданному tmdbBaseURL, подменить его на httptest-сервер без
// изменения продакшен-кода нельзя. Эти тесты проверяют то, что не требует
// сети: RLS-видимость сериала и отбраковку сериалов без привязки к
// внешнему каталогу -- именно то место, о котором явно предупреждает
// комментарий в миграции 0018 ("must never be exposed to arbitrary
// client-supplied family ids").
package calendar_test

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

	"notrecinema/api/internal/calendar"
	"notrecinema/api/internal/platform/apperror"
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
	email := fmt.Sprintf("calendar-test-%s@example.com", token)

	var userID string
	row := db.QueryRow(ctx,
		"SELECT user_id FROM public.create_profile_session($1, $2, $3, $4)",
		email, "Calendar Test", hex.EncodeToString(hash[:]), time.Now().Add(time.Hour),
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
		`, "Calendar Test Family", ownerID, hex.EncodeToString(tokenBytes)).Scan(&familyID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO public.family_members (family_id, user_id, role) VALUES ($1, $2, 'owner')
		`, familyID, ownerID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO public.family_series (family_id, title, media_type, external_source, external_id, created_by)
			VALUES ($1, 'Calendar Show', 'series', $2, $3, $4) RETURNING id
		`, familyID, externalSource, externalID, ownerID).Scan(&seriesID)
	})
	if err != nil {
		t.Fatalf("failed to seed family/series: %v", err)
	}
	return familyID, seriesID
}

func TestCheckNextEpisodeRejectsUnlinkedSeries(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()

	owner := createTestUser(t, ctx, db)
	_, seriesID := createFamilyWithSeries(t, ctx, db, owner, nil, nil)

	svc := calendar.NewService(db, "fake-tmdb-key", slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err := svc.CheckNextEpisode(ctx, owner, seriesID)
	if err == nil {
		t.Fatal("CheckNextEpisode() error = nil, want invalid_input")
	}
	if kind := apperror.HTTPStatus(err); kind != 400 {
		t.Errorf("HTTPStatus = %d, want 400", kind)
	}
}

func TestCheckNextEpisodeRejectsUnknownSeriesForUser(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()

	owner := createTestUser(t, ctx, db)
	outsider := createTestUser(t, ctx, db)
	source, id := "omdb", "tt1234567"
	_, seriesID := createFamilyWithSeries(t, ctx, db, owner, &source, &id)

	svc := calendar.NewService(db, "fake-tmdb-key", slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err := svc.CheckNextEpisode(ctx, outsider, seriesID)
	if err == nil {
		t.Fatal("CheckNextEpisode() error = nil, want not_found (RLS should hide the series)")
	}
	if status := apperror.HTTPStatus(err); status != 404 {
		t.Errorf("HTTPStatus = %d, want 404", status)
	}
}

func TestGetCalendarSplitsUpcomingAndCheckable(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()

	owner := createTestUser(t, ctx, db)
	source, id := "omdb", "tt7654321"
	familyID, seriesID := createFamilyWithSeries(t, ctx, db, owner, &source, &id)

	svc := calendar.NewService(db, "fake-tmdb-key", slog.New(slog.NewTextHandler(io.Discard, nil)))
	cal, err := svc.GetCalendar(ctx, owner, familyID)
	if err != nil {
		t.Fatalf("GetCalendar() error: %v", err)
	}
	if len(cal.Upcoming) != 0 {
		t.Errorf("Upcoming = %+v, want empty (no air date set yet)", cal.Upcoming)
	}
	if len(cal.Checkable) != 1 || cal.Checkable[0].SeriesID != seriesID {
		t.Errorf("Checkable = %+v, want one entry for %q", cal.Checkable, seriesID)
	}
}
