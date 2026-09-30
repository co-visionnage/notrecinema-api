//go:build integration

// Запуск: DATABASE_URL=... go test -tags integration ./...
package nudges_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"notrecinema/api/internal/nudges"
	"notrecinema/api/internal/postgres"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

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
	email := fmt.Sprintf("nudges-test-%s@example.com", token)

	var userID string
	row := db.QueryRow(ctx,
		"SELECT user_id FROM public.create_profile_session($1, $2, $3, $4)",
		email, "Nudges Test", hex.EncodeToString(hash[:]), time.Now().Add(time.Hour),
	)
	if err := row.Scan(&userID); err != nil {
		t.Fatalf("failed to create test user: %v", err)
	}
	return userID
}

// seedStaleProgress создаёт сериал "to-watch" с начатым, но давно не
// двигавшимся прогрессом -- ровно то, что get_stale_progress должна найти.
// updated_at на family_series_progress выставляется явно в INSERT: триггер
// touch_updated_at срабатывает только на UPDATE, так что руками заданное
// значение при вставке не перезаписывается.
func seedStaleProgress(t *testing.T, ctx context.Context, db *postgres.Pool, userID string, daysAgo int) (seriesID string) {
	t.Helper()
	err := db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		tokenBytes := make([]byte, 4)
		_, _ = rand.Read(tokenBytes)

		var familyID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO public.families (name, owner_id, invite_code)
			VALUES ($1, $2, $3) RETURNING id
		`, "Nudges Test Family", userID, hex.EncodeToString(tokenBytes)).Scan(&familyID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO public.family_members (family_id, user_id, role) VALUES ($1, $2, 'owner')
		`, familyID, userID); err != nil {
			return err
		}

		if err := tx.QueryRow(ctx, `
			INSERT INTO public.family_series (family_id, title, created_by)
			VALUES ($1, 'Stale Show', $2) RETURNING id
		`, familyID, userID).Scan(&seriesID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO public.family_series_status (series_id, user_id, status)
			VALUES ($1, $2, 'to-watch')
		`, seriesID, userID); err != nil {
			return err
		}
		updatedAt := time.Now().Add(-time.Duration(daysAgo) * 24 * time.Hour)
		_, err := tx.Exec(ctx, `
			INSERT INTO public.family_series_progress (series_id, user_id, current_season, current_episode, updated_at)
			VALUES ($1, $2, 1, 3, $3)
		`, seriesID, userID, updatedAt)
		return err
	})
	if err != nil {
		t.Fatalf("failed to seed stale progress: %v", err)
	}
	return seriesID
}

func outboxEventCount(t *testing.T, ctx context.Context, db *postgres.Pool, seriesID string) int {
	t.Helper()
	var count int
	err := db.QueryRow(ctx, `
		SELECT count(*) FROM public.outbox_events
		WHERE aggregate_id = $1 AND event_type = 'progress.stale'
	`, seriesID).Scan(&count)
	if err != nil {
		t.Fatalf("failed to count outbox events: %v", err)
	}
	return count
}

func TestCheckOnceNotifiesStaleProgressOnce(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()

	user := createTestUser(t, ctx, db)
	seriesID := seedStaleProgress(t, ctx, db, user, 20) // порог по умолчанию в тесте -- 14 дней

	checker := nudges.NewChecker(db, discardLogger(), time.Hour, 14)

	if err := checker.CheckOnce(ctx); err != nil {
		t.Fatalf("CheckOnce() #1 error: %v", err)
	}
	if got := outboxEventCount(t, ctx, db, seriesID); got != 1 {
		t.Fatalf("outbox events after #1 = %d, want 1", got)
	}

	// last_reminded_at теперь свежий -- повторный прогон не должен
	// застолбить ту же строку снова (недельный троттлинг).
	if err := checker.CheckOnce(ctx); err != nil {
		t.Fatalf("CheckOnce() #2 error: %v", err)
	}
	if got := outboxEventCount(t, ctx, db, seriesID); got != 1 {
		t.Fatalf("outbox events after #2 = %d, want still 1 (throttled)", got)
	}
}

func TestCheckOnceIgnoresRecentProgress(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()

	user := createTestUser(t, ctx, db)
	seriesID := seedStaleProgress(t, ctx, db, user, 2) // недостаточно давно для порога в 14 дней

	checker := nudges.NewChecker(db, discardLogger(), time.Hour, 14)
	if err := checker.CheckOnce(ctx); err != nil {
		t.Fatalf("CheckOnce() error: %v", err)
	}
	if got := outboxEventCount(t, ctx, db, seriesID); got != 0 {
		t.Fatalf("outbox events = %d, want 0 (not stale yet)", got)
	}
}

func TestOutboxPayloadShape(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()

	user := createTestUser(t, ctx, db)
	seriesID := seedStaleProgress(t, ctx, db, user, 20)

	checker := nudges.NewChecker(db, discardLogger(), time.Hour, 14)
	if err := checker.CheckOnce(ctx); err != nil {
		t.Fatalf("CheckOnce() error: %v", err)
	}

	var payload []byte
	err := db.QueryRow(ctx, `
		SELECT payload FROM public.outbox_events
		WHERE aggregate_id = $1 AND event_type = 'progress.stale'
	`, seriesID).Scan(&payload)
	if err != nil {
		t.Fatalf("failed to read outbox payload: %v", err)
	}

	var decoded struct {
		SeriesID       string `json:"seriesId"`
		UserID         string `json:"userId"`
		Title          string `json:"title"`
		CurrentSeason  int    `json:"currentSeason"`
		CurrentEpisode int    `json:"currentEpisode"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("failed to unmarshal payload: %v", err)
	}
	if decoded.SeriesID != seriesID || decoded.UserID != user || decoded.Title != "Stale Show" ||
		decoded.CurrentSeason != 1 || decoded.CurrentEpisode != 3 {
		t.Errorf("payload = %+v, unexpected values", decoded)
	}
}
