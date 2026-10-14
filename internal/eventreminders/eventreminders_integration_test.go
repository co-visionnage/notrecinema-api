//go:build integration

package eventreminders_test

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
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"notrecinema/api/internal/eventreminders"
	"notrecinema/api/internal/postgres"
)

const lead = time.Hour

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

func newUser(t *testing.T, ctx context.Context, db *postgres.Pool) string {
	t.Helper()
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	token := hex.EncodeToString(raw)
	hash := sha256.Sum256([]byte(token))

	var userID string
	if err := db.QueryRow(ctx,
		"SELECT user_id FROM public.create_profile_session($1, $2, $3, $4)",
		fmt.Sprintf("reminder-%s@example.com", token), "Reminder Test", hex.EncodeToString(hash[:]), time.Now().Add(time.Hour),
	).Scan(&userID); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return userID
}

// newFamily создаёт семью с владельцем и ещё двумя участниками.
func newFamily(t *testing.T, ctx context.Context, db *postgres.Pool) (familyID string, owner, going, declined string) {
	t.Helper()
	owner, going, declined = newUser(t, ctx, db), newUser(t, ctx, db), newUser(t, ctx, db)
	raw := make([]byte, 4)
	_, _ = rand.Read(raw)

	if err := db.WithUserContext(ctx, owner, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO public.families (name, owner_id, invite_code) VALUES ('Reminder Family', $1, $2) RETURNING id
		`, owner, hex.EncodeToString(raw)).Scan(&familyID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO public.family_members (family_id, user_id, role) VALUES ($1, $2, 'owner')`, familyID, owner)
		return err
	}); err != nil {
		t.Fatalf("create family: %v", err)
	}
	for _, member := range []string{going, declined} {
		if err := db.WithUserContext(ctx, member, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO public.family_members (family_id, user_id, role) VALUES ($1, $2, 'member')`, familyID, member)
			return err
		}); err != nil {
			t.Fatalf("join family: %v", err)
		}
	}
	return familyID, owner, going, declined
}

// newEvent создаёт встречу: startsIn -- через сколько она начнётся,
// createdAgo -- как давно её создали.
func newEvent(t *testing.T, ctx context.Context, db *postgres.Pool, familyID, creator string, startsIn, createdAgo time.Duration) string {
	t.Helper()
	var eventID string
	if err := db.WithUserContext(ctx, creator, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO public.family_watch_events (family_id, created_by, title, scheduled_at, created_at)
			VALUES ($1, $2, 'Вечер кино', NOW() + make_interval(secs => $3), NOW() - make_interval(secs => $4))
			RETURNING id
		`, familyID, creator, startsIn.Seconds(), createdAgo.Seconds()).Scan(&eventID)
	}); err != nil {
		t.Fatalf("create event: %v", err)
	}
	return eventID
}

func rsvp(t *testing.T, ctx context.Context, db *postgres.Pool, eventID, userID, status string) {
	t.Helper()
	if err := db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.family_watch_event_rsvps (event_id, user_id, status) VALUES ($1, $2, $3)
			ON CONFLICT (event_id, user_id) DO UPDATE SET status = EXCLUDED.status
		`, eventID, userID, status)
		return err
	}); err != nil {
		t.Fatalf("rsvp: %v", err)
	}
}

func runOnce(t *testing.T, ctx context.Context, db *postgres.Pool) {
	t.Helper()
	runner := eventreminders.NewRunner(db, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Minute, lead)
	if _, err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}
}

// reminded возвращает отсортированных получателей напоминания о встрече.
func reminded(t *testing.T, ctx context.Context, db *postgres.Pool, eventID string) []string {
	t.Helper()
	rows, err := db.Query(ctx, `
		SELECT payload FROM public.outbox_events
		WHERE event_type = 'watch_event.reminder' AND aggregate_id = $1
	`, eventID)
	if err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	defer rows.Close()

	var users []string
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan: %v", err)
		}
		var payload struct {
			EventID  string `json:"eventId"`
			FamilyID string `json:"familyId"`
			UserID   string `json:"userId"`
			Title    string `json:"title"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("payload: %v", err)
		}
		if payload.EventID != eventID || payload.Title != "Вечер кино" || payload.FamilyID == "" {
			t.Errorf("неполный payload: %+v", payload)
		}
		users = append(users, payload.UserID)
	}
	sort.Strings(users)
	return users
}

func TestRemindsEveryoneExceptDeclinedExactlyOnce(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()

	familyID, owner, going, declined := newFamily(t, ctx, db)
	eventID := newEvent(t, ctx, db, familyID, owner, 30*time.Minute, 3*time.Hour)
	rsvp(t, ctx, db, eventID, going, "going")
	rsvp(t, ctx, db, eventID, declined, "no")

	runOnce(t, ctx, db)
	got := reminded(t, ctx, db, eventID)
	want := []string{owner, going}
	sort.Strings(want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("получатели = %v, want %v (ответивший «нет» напоминание не получает)", got, want)
	}

	runOnce(t, ctx, db)
	if again := reminded(t, ctx, db, eventID); len(again) != len(want) {
		t.Errorf("после второго прогона событий %d, want %d: напоминание не должно повторяться", len(again), len(want))
	}
}

func TestDoesNotRemindTooEarlyTooLateOrForFreshEvents(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()

	familyID, owner, _, _ := newFamily(t, ctx, db)
	cases := map[string]string{
		"ещё рано":     newEvent(t, ctx, db, familyID, owner, 3*time.Hour, 24*time.Hour),
		"уже началась": newEvent(t, ctx, db, familyID, owner, -10*time.Minute, 24*time.Hour),
		"создана внутри окна напоминания": newEvent(t, ctx, db, familyID, owner, 30*time.Minute, time.Minute),
	}

	runOnce(t, ctx, db)
	for name, eventID := range cases {
		if got := reminded(t, ctx, db, eventID); len(got) != 0 {
			t.Errorf("%s: напоминания быть не должно, получили %v", name, got)
		}
	}
}
