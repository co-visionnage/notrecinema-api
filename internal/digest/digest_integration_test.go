//go:build integration

package digest_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"notrecinema/api/internal/digest"
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
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

func verifiedUser(t *testing.T, ctx context.Context, db *postgres.Pool) string {
	t.Helper()
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	token := hex.EncodeToString(raw)
	hash := sha256.Sum256([]byte(token))

	var userID string
	if err := db.QueryRow(ctx,
		"SELECT user_id FROM public.create_profile_session($1, $2, $3, $4)",
		fmt.Sprintf("digest-%s@example.com", token), "Digest Test", hex.EncodeToString(hash[:]), time.Now().Add(time.Hour),
	).Scan(&userID); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE public.profiles SET email_verified_at = NOW() WHERE id = $1`, userID)
		return err
	}); err != nil {
		t.Fatalf("verify email: %v", err)
	}
	return userID
}

func family(t *testing.T, ctx context.Context, db *postgres.Pool, owner string) string {
	t.Helper()
	raw := make([]byte, 4)
	_, _ = rand.Read(raw)

	var familyID string
	if err := db.WithUserContext(ctx, owner, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO public.families (name, owner_id, invite_code) VALUES ('Digest Family', $1, $2) RETURNING id
		`, owner, hex.EncodeToString(raw)).Scan(&familyID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO public.family_members (family_id, user_id, role) VALUES ($1, $2, 'owner')`, familyID, owner)
		return err
	}); err != nil {
		t.Fatalf("create family: %v", err)
	}
	return familyID
}

// uniqueMonday возвращает понедельник 07:00 UTC в далёком году: неделя
// застолбляется в общей БД навсегда, и тесты не должны мешать друг другу
// и прошлым прогонам.
func uniqueMonday(t *testing.T) time.Time {
	t.Helper()
	n, err := rand.Int(rand.Reader, big.NewInt(50000))
	if err != nil {
		t.Fatalf("rand: %v", err)
	}
	day := time.Date(3000, 1, 1, 7, 0, 0, 0, time.UTC).AddDate(0, 0, int(n.Int64())*7)
	for day.Weekday() != time.Monday {
		day = day.AddDate(0, 0, 1)
	}
	return day
}

func digestEventsFor(t *testing.T, ctx context.Context, db *postgres.Pool, userID string) []map[string]any {
	t.Helper()
	rows, err := db.Query(ctx, `
		SELECT payload FROM public.outbox_events
		WHERE event_type = 'email.weekly_digest' AND aggregate_id = $1
	`, userID)
	if err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	defer rows.Close()

	var events []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("payload: %v", err)
		}
		events = append(events, payload)
	}
	return events
}

func runner(db *postgres.Pool) *digest.Runner {
	return digest.NewRunner(db, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Hour, time.Monday, 6)
}

func TestDigestQueuesOneEventPerRecipientAndOnlyOncePerWeek(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()

	member := verifiedUser(t, ctx, db)
	family(t, ctx, db, member)
	monday := uniqueMonday(t)

	if _, err := runner(db).RunOnce(ctx, monday); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}
	events := digestEventsFor(t, ctx, db, member)
	if len(events) != 1 {
		t.Fatalf("events for the member = %d, want 1", len(events))
	}
	if events[0]["userId"] != member || events[0]["since"] == nil || events[0]["until"] == nil {
		t.Errorf("payload = %v", events[0])
	}

	// Тот же день второй раз (следующий тик, перезапуск, второй экземпляр):
	// неделя уже застолблена, повторной рассылки нет.
	queued, err := runner(db).RunOnce(ctx, monday.Add(time.Hour))
	if err != nil || queued != 0 {
		t.Errorf("second RunOnce() = %d, %v; want 0, nil", queued, err)
	}
	if got := len(digestEventsFor(t, ctx, db, member)); got != 1 {
		t.Errorf("events after the second run = %d, want still 1", got)
	}
}

func TestDigestDoesNothingOutsideTheScheduledSlot(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()

	member := verifiedUser(t, ctx, db)
	family(t, ctx, db, member)
	monday := uniqueMonday(t)

	for _, at := range []time.Time{monday.Add(-2 * time.Hour), monday.AddDate(0, 0, 1)} {
		if queued, err := runner(db).RunOnce(ctx, at); err != nil || queued != 0 {
			t.Errorf("RunOnce(%v) = %d, %v; want 0, nil", at, queued, err)
		}
	}
	if got := len(digestEventsFor(t, ctx, db, member)); got != 0 {
		t.Errorf("events = %d, want 0", got)
	}
}

func TestDigestSkipsUnverifiedUsersThoseWithoutFamilyAndOptOuts(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()

	optedOut := verifiedUser(t, ctx, db)
	family(t, ctx, db, optedOut)
	if err := db.WithUserContext(ctx, optedOut, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.notification_preferences (user_id, category, push, email)
			VALUES ($1, 'weekly_digest', false, false)
		`, optedOut)
		return err
	}); err != nil {
		t.Fatalf("opt out: %v", err)
	}

	lonely := verifiedUser(t, ctx, db) // без семьи

	unverified := verifiedUser(t, ctx, db)
	family(t, ctx, db, unverified)
	if err := db.WithUserContext(ctx, unverified, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE public.profiles SET email_verified_at = NULL WHERE id = $1`, unverified)
		return err
	}); err != nil {
		t.Fatalf("unverify: %v", err)
	}

	included := verifiedUser(t, ctx, db)
	family(t, ctx, db, included)

	if _, err := runner(db).RunOnce(ctx, uniqueMonday(t)); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	for name, userID := range map[string]string{"opted out": optedOut, "no family": lonely, "unverified": unverified} {
		if got := len(digestEventsFor(t, ctx, db, userID)); got != 0 {
			t.Errorf("%s user got %d digest events, want 0", name, got)
		}
	}
	if got := len(digestEventsFor(t, ctx, db, included)); got != 1 {
		t.Errorf("included user got %d digest events, want 1", got)
	}
}

func TestWeeklyDigestContentCoversAddedWatchedEventsAndAiring(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()

	owner := verifiedUser(t, ctx, db)
	familyID := family(t, ctx, db, owner)
	until := time.Now().UTC().Truncate(time.Minute).Add(time.Minute)
	since := until.Add(-digest.Period)

	err := db.WithUserContext(ctx, owner, func(ctx context.Context, tx pgx.Tx) error {
		var seriesID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO public.family_series (family_id, title, created_by) VALUES ($1, 'Fresh Show', $2) RETURNING id
		`, familyID, owner).Scan(&seriesID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO public.family_series_status (series_id, user_id, status, rating, watched_at)
			VALUES ($1, $2, 'watched', 4, NOW())
		`, seriesID, owner); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO public.family_watch_events (family_id, created_by, title, scheduled_at)
			VALUES ($1, $2, 'Movie night', NOW() + interval '2 days')
		`, familyID, owner); err != nil {
			return err
		}
		// Старое добавление за окном сводки не попадает.
		if _, err := tx.Exec(ctx, `
			INSERT INTO public.family_series (family_id, title, created_by, created_at)
			VALUES ($1, 'Old Show', $2, NOW() - interval '30 days')
		`, familyID, owner); err != nil {
			return err
		}
		// Сериал, у которого скоро выходит серия и который ещё не досмотрен.
		_, err := tx.Exec(ctx, `
			INSERT INTO public.family_series (family_id, title, created_by, next_episode_air_date, next_episode_label)
			VALUES ($1, 'Airing Show', $2, CURRENT_DATE + 3, 'S2E5')
		`, familyID, owner)
		return err
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	rows, err := db.Query(ctx, `
		SELECT section, title, detail FROM public.get_weekly_digest_system($1, $2, $3)
	`, owner, since, until)
	if err != nil {
		t.Fatalf("get_weekly_digest_system: %v", err)
	}
	defer rows.Close()

	got := map[string][]string{}
	for rows.Next() {
		var section, title, detail string
		if err := rows.Scan(&section, &title, &detail); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[section] = append(got[section], title+"|"+detail)
	}

	has := func(section, title string) bool {
		for _, item := range got[section] {
			if strings.HasPrefix(item, title+"|") {
				return true
			}
		}
		return false
	}
	if !has("added", "Fresh Show") || has("added", "Old Show") {
		t.Errorf("added = %v", got["added"])
	}
	if !has("watched", "Fresh Show") {
		t.Errorf("watched = %v", got["watched"])
	}
	if !has("event", "Movie night") {
		t.Errorf("event = %v", got["event"])
	}
	if !has("airing", "Airing Show") {
		t.Errorf("airing = %v", got["airing"])
	}
}

func TestWeeklyDigestIsEmptyForSomeoneOutsideTheFamily(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()

	owner := verifiedUser(t, ctx, db)
	familyID := family(t, ctx, db, owner)
	if err := db.WithUserContext(ctx, owner, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO public.family_series (family_id, title, created_by) VALUES ($1, 'Private Show', $2)`, familyID, owner)
		return err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	stranger := verifiedUser(t, ctx, db)

	until := time.Now().UTC().Add(time.Minute)
	var count int
	if err := db.QueryRow(ctx, `
		SELECT count(*) FROM public.get_weekly_digest_system($1, $2, $3)
	`, stranger, until.Add(-digest.Period), until).Scan(&count); err != nil {
		t.Fatalf("query: %v", err)
	}
	if count != 0 {
		t.Errorf("a stranger sees %d digest rows of someone else's family", count)
	}
}
