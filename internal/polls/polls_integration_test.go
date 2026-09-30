//go:build integration

// Запуск: go test -tags integration ./...
package polls_test

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

	"notrecinema/api/internal/polls"
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
	email := fmt.Sprintf("polls-test-%s@example.com", token)

	var userID string
	row := db.QueryRow(ctx,
		"SELECT user_id FROM public.create_profile_session($1, $2, $3, $4)",
		email, "Polls Test", hex.EncodeToString(hash[:]), time.Now().Add(time.Hour),
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
		`, "Polls Test Family", ownerID, hex.EncodeToString(tokenBytes)).Scan(&familyID); err != nil {
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

func createTestSeries(t *testing.T, ctx context.Context, db *postgres.Pool, ownerID, familyID string) string {
	t.Helper()
	var seriesID string
	err := db.WithUserContext(ctx, ownerID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO public.family_series (family_id, title, created_by)
			VALUES ($1, 'Poll Test Series', $2)
			RETURNING id
		`, familyID, ownerID).Scan(&seriesID)
	})
	if err != nil {
		t.Fatalf("failed to create test series: %v", err)
	}
	return seriesID
}

func TestPollCreateVoteClose(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()

	owner := createTestUser(t, ctx, db)
	voter := createTestUser(t, ctx, db)
	familyID := createTestFamily(t, ctx, db, owner)
	addFamilyMember(t, ctx, db, familyID, voter)
	seriesID := createTestSeries(t, ctx, db, owner, familyID)

	svc := polls.NewService(db)

	poll, err := svc.Create(ctx, owner, familyID, polls.CreateInput{Title: "Сегодня?", SeriesIDs: []string{seriesID}})
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	if len(poll.Options) != 1 {
		t.Fatalf("Create() options = %+v, want 1 option", poll.Options)
	}
	optionID := poll.Options[0].ID

	if err := svc.Vote(ctx, voter, poll.ID, polls.VoteInput{OptionID: optionID}); err != nil {
		t.Fatalf("Vote() error: %v", err)
	}

	listed, err := svc.ListForFamily(ctx, owner, familyID)
	if err != nil {
		t.Fatalf("ListForFamily() error: %v", err)
	}
	if len(listed) != 1 || listed[0].Options[0].Votes != 1 {
		t.Fatalf("ListForFamily() = %+v, want 1 poll с 1 голосом", listed)
	}

	// Голосование за несуществующую опцию должно быть отклонено RLS
	// (family_watch_poll_votes_upsert_own проверяет, что опция реально
	// принадлежит этому опросу -- миграция 0022).
	if err := svc.Vote(ctx, voter, poll.ID, polls.VoteInput{OptionID: "00000000-0000-0000-0000-000000000000"}); err == nil {
		t.Error("Vote() с несуществующей опцией succeeded, want error")
	}

	if err := svc.Close(ctx, owner, poll.ID); err != nil {
		t.Fatalf("Close() error: %v", err)
	}

	// После закрытия голосовать нельзя (миграция 0024).
	if err := svc.Vote(ctx, voter, poll.ID, polls.VoteInput{OptionID: optionID}); err == nil {
		t.Error("Vote() после Close() succeeded, want error -- закрытый опрос должен отклонять голоса")
	}
}

func TestPollCloseRejectsNonCreatorNonOwner(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()

	owner := createTestUser(t, ctx, db)
	member := createTestUser(t, ctx, db)
	familyID := createTestFamily(t, ctx, db, owner)
	addFamilyMember(t, ctx, db, familyID, member)
	seriesID := createTestSeries(t, ctx, db, owner, familyID)

	svc := polls.NewService(db)
	poll, err := svc.Create(ctx, owner, familyID, polls.CreateInput{SeriesIDs: []string{seriesID}})
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	// member -- не создатель опроса и не владелец семьи, закрыть не может
	// (family_watch_polls_update_creator_or_owner).
	if err := svc.Close(ctx, member, poll.ID); err == nil {
		t.Error("Close() рядовым участником succeeded, want error")
	}
}
