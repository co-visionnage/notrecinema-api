//go:build integration

// Интеграционным тестам нужен настоящий Postgres по адресу $DATABASE_URL с
// уже применённой схемой notrecinema-app (см. docker-compose.yml).
// Запуск: go test -tags integration ./...
package series_test

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

	"notrecinema/api/internal/postgres"
	"notrecinema/api/internal/series"
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
	email := fmt.Sprintf("series-test-%s@example.com", token)

	var userID string
	row := db.QueryRow(ctx,
		"SELECT user_id FROM public.create_profile_session($1, $2, $3, $4)",
		email, "Series Test", hex.EncodeToString(hash[:]), time.Now().Add(time.Hour),
	)
	if err := row.Scan(&userID); err != nil {
		t.Fatalf("failed to create test user: %v", err)
	}
	return userID
}

// createTestFamily создаёт семью и добавляет owner как участника напрямую
// через SQL (не через internal/families) -- пакеты остаются независимыми
// друг от друга в тестах, как и в остальном коде. Обе вставки идут внутри
// WithUserContext, иначе RLS-политики (owner_id = current_user_id()) их
// просто отклонят -- current_user_id() без установленного
// app.current_user_id возвращает NULL.
func createTestFamily(t *testing.T, ctx context.Context, db *postgres.Pool, ownerID string) string {
	t.Helper()

	tokenBytes := make([]byte, 4)
	_, _ = rand.Read(tokenBytes)
	inviteCode := hex.EncodeToString(tokenBytes)

	var familyID string
	err := db.WithUserContext(ctx, ownerID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO public.families (name, owner_id, invite_code)
			VALUES ($1, $2, $3)
			RETURNING id
		`, "Series Test Family", ownerID, inviteCode).Scan(&familyID); err != nil {
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

func TestSeriesLifecycleAndStatusAndProgress(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()

	owner := createTestUser(t, ctx, db)
	familyID := createTestFamily(t, ctx, db, owner)
	svc := series.NewService(db)

	created, err := svc.Create(ctx, owner, familyID, series.CreateInput{
		Title:  "  Interstellar  ",
		Genres: []string{"sci-fi"},
	})
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	if created.Title != "Interstellar" {
		t.Errorf("Create() title = %q, want trimmed %q", created.Title, "Interstellar")
	}

	list, err := svc.ListForFamily(ctx, owner, familyID)
	if err != nil {
		t.Fatalf("ListForFamily() error: %v", err)
	}
	if len(list) != 1 || list[0].ID != created.ID {
		t.Errorf("ListForFamily() = %+v, want exactly the created series", list)
	}

	status, err := svc.SetStatus(ctx, owner, created.ID, series.SetStatusInput{Status: "watched", Rating: intPtr(5)})
	if err != nil {
		t.Fatalf("SetStatus() error: %v", err)
	}
	if status.WatchedAt == nil {
		t.Error("SetStatus(watched) did not set watchedAt")
	}
	firstWatchedAt := *status.WatchedAt

	// Повторная установка watched не должна перезаписывать исходный watchedAt.
	status, err = svc.SetStatus(ctx, owner, created.ID, series.SetStatusInput{Status: "watched", Rating: intPtr(4)})
	if err != nil {
		t.Fatalf("SetStatus() (второй раз) error: %v", err)
	}
	if !status.WatchedAt.Equal(firstWatchedAt) {
		t.Errorf("SetStatus() перезаписал watchedAt: было %v, стало %v", firstWatchedAt, *status.WatchedAt)
	}
	if status.Rating == nil || *status.Rating != 4 {
		t.Errorf("SetStatus() rating = %v, want 4", status.Rating)
	}

	progress, err := svc.SetProgress(ctx, owner, created.ID, series.SetProgressInput{CurrentSeason: 2, CurrentEpisode: 3})
	if err != nil {
		t.Fatalf("SetProgress() error: %v", err)
	}
	if progress.CurrentSeason != 2 || progress.CurrentEpisode != 3 {
		t.Errorf("SetProgress() = %+v, want season=2 episode=3", progress)
	}
}

func TestSeriesInvisibleToNonMember(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()

	owner := createTestUser(t, ctx, db)
	stranger := createTestUser(t, ctx, db)
	familyID := createTestFamily(t, ctx, db, owner)
	svc := series.NewService(db)

	if _, err := svc.Create(ctx, owner, familyID, series.CreateInput{Title: "Приватный сериал"}); err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	list, err := svc.ListForFamily(ctx, stranger, familyID)
	if err != nil {
		t.Fatalf("ListForFamily(stranger) error: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("ListForFamily(stranger) = %+v, want empty -- RLS должна скрыть сериал чужой семьи", list)
	}
}

func TestCommentsAndReactions(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()

	owner := createTestUser(t, ctx, db)
	member := createTestUser(t, ctx, db)
	familyID := createTestFamily(t, ctx, db, owner)
	addFamilyMember(t, ctx, db, familyID, member)

	svc := series.NewService(db)
	created, err := svc.Create(ctx, owner, familyID, series.CreateInput{Title: "Комментируемый сериал"})
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	comment, err := svc.AddComment(ctx, member, created.ID, series.CreateCommentInput{Body: "Отлично!"})
	if err != nil {
		t.Fatalf("AddComment() error: %v", err)
	}

	comments, err := svc.ListComments(ctx, owner, created.ID)
	if err != nil {
		t.Fatalf("ListComments() error: %v", err)
	}
	if len(comments) != 1 || comments[0].ID != comment.ID {
		t.Errorf("ListComments() = %+v, want exactly the added comment", comments)
	}

	// Автор не-владелец может удалить свой комментарий.
	if err := svc.DeleteComment(ctx, member, comment.ID); err != nil {
		t.Fatalf("DeleteComment() (автором) error: %v", err)
	}

	if _, err := svc.AddReaction(ctx, member, created.ID, series.AddReactionInput{Emoji: "🔥"}); err != nil {
		t.Fatalf("AddReaction() error: %v", err)
	}
	// Повторная реакция тем же пользователем тем же эмодзи -- не ошибка (ON CONFLICT DO NOTHING).
	if _, err := svc.AddReaction(ctx, member, created.ID, series.AddReactionInput{Emoji: "🔥"}); err != nil {
		t.Fatalf("AddReaction() (повторно) error: %v", err)
	}

	reactions, err := svc.ListReactions(ctx, owner, created.ID)
	if err != nil {
		t.Fatalf("ListReactions() error: %v", err)
	}
	if len(reactions) != 1 {
		t.Errorf("ListReactions() = %+v, want ровно одну реакцию (дубликат не должен был создать вторую строку)", reactions)
	}
}

func intPtr(v int) *int { return &v }

func strPtr(s string) *string { return &s }

// Тип, трейлер и внешние id сохраняются; стартовый статус "watched" создаёт
// строку статуса создателя; у второго участника тот же сериал остаётся
// "to-watch": статус персональный, а не общий.
func TestSeriesExtendedFieldsAndPerUserStatus(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()

	owner := createTestUser(t, ctx, db)
	other := createTestUser(t, ctx, db)
	familyID := createTestFamily(t, ctx, db, owner)
	addFamilyMember(t, ctx, db, familyID, other)
	svc := series.NewService(db)

	created, err := svc.Create(ctx, owner, familyID, series.CreateInput{
		Title:          "Dune",
		MediaType:      strPtr("movie"),
		TrailerURL:     strPtr("https://example.com/trailer"),
		ExternalSource: strPtr("tmdb"),
		ExternalID:     strPtr("438631"),
		Status:         strPtr("watched"),
		Rating:         intPtr(4),
		Comment:        strPtr("круто"),
	})
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	if created.MediaType != "movie" || created.TrailerURL == nil || created.ExternalID == nil || *created.ExternalID != "438631" {
		t.Errorf("extended fields lost: %+v", created)
	}
	if created.Status != "watched" || created.Rating == nil || *created.Rating != 4 || created.WatchedAt == nil {
		t.Errorf("creator status = %+v, want watched/4 with watchedAt", created)
	}
	if created.CreatedAt.IsZero() {
		t.Error("createdAt is empty")
	}

	list, err := svc.ListForFamily(ctx, other, familyID)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListForFamily(other) = %v, %v", list, err)
	}
	if list[0].Status != "to-watch" || list[0].Rating != nil {
		t.Errorf("other member sees the creator's status: %+v", list[0])
	}

	// Рейтинг через PATCH пишется в статус вызывающего, не создателя.
	updated, err := svc.Update(ctx, other, created.ID, series.UpdateInput{Rating: intPtr(2), Comment: strPtr("так себе")})
	if err != nil {
		t.Fatalf("Update() error: %v", err)
	}
	if updated.Rating == nil || *updated.Rating != 2 || updated.Status != "to-watch" {
		t.Errorf("update by other = %+v", updated)
	}
	again, err := svc.Get(ctx, owner, created.ID)
	if err != nil || again.Rating == nil || *again.Rating != 4 {
		t.Errorf("owner's rating changed by someone else's PATCH: %+v, %v", again, err)
	}

	if _, err := svc.Create(ctx, owner, familyID, series.CreateInput{Title: "X", MediaType: strPtr("book")}); err == nil {
		t.Error("an unknown mediaType was accepted")
	}
}

// Комментарий отдаётся с именем автора и спойлер-пометкой; прогресс всех
// участников -- одним списком с именами, и по сериалу, и по всей семье.
func TestCommentAuthorSpoilerAndProgressLists(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()

	owner := createTestUser(t, ctx, db)
	other := createTestUser(t, ctx, db)
	familyID := createTestFamily(t, ctx, db, owner)
	addFamilyMember(t, ctx, db, familyID, other)
	svc := series.NewService(db)

	created, err := svc.Create(ctx, owner, familyID, series.CreateInput{Title: "Severance"})
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	comment, err := svc.AddComment(ctx, other, created.ID, series.CreateCommentInput{
		Body: "ого", SpoilerSeason: intPtr(2), SpoilerEpisode: intPtr(5),
	})
	if err != nil {
		t.Fatalf("AddComment() error: %v", err)
	}
	if comment.AuthorName == "" || comment.SpoilerSeason == nil || *comment.SpoilerSeason != 2 || *comment.SpoilerEpisode != 5 {
		t.Errorf("comment = %+v, want author name and spoiler 2/5", comment)
	}
	if _, err := svc.AddComment(ctx, other, created.ID, series.CreateCommentInput{Body: "x", SpoilerEpisode: intPtr(3)}); err == nil {
		t.Error("a spoiler episode without a season was accepted")
	}

	for _, user := range []string{owner, other} {
		if _, err := svc.SetProgress(ctx, user, created.ID, series.SetProgressInput{CurrentSeason: 1, CurrentEpisode: 3}); err != nil {
			t.Fatalf("SetProgress() error: %v", err)
		}
	}
	bySeries, err := svc.ListSeriesProgress(ctx, owner, created.ID)
	if err != nil || len(bySeries) != 2 || bySeries[0].DisplayName == "" || bySeries[0].UpdatedAt.IsZero() {
		t.Errorf("ListSeriesProgress() = %+v, %v", bySeries, err)
	}
	byFamily, err := svc.ListFamilyProgress(ctx, owner, familyID)
	if err != nil || len(byFamily) != 2 {
		t.Errorf("ListFamilyProgress() = %+v, %v", byFamily, err)
	}
}
