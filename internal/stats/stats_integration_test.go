//go:build integration

// Запуск: go test -tags integration ./...
package stats_test

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
	"notrecinema/api/internal/stats"
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
	email := fmt.Sprintf("stats-test-%s@example.com", token)

	var userID string
	row := db.QueryRow(ctx,
		"SELECT user_id FROM public.create_profile_session($1, $2, $3, $4)",
		email, "Stats Test", hex.EncodeToString(hash[:]), time.Now().Add(time.Hour),
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
		`, "Stats Test Family", ownerID, hex.EncodeToString(tokenBytes)).Scan(&familyID); err != nil {
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

// createWatchedSeries создаёт сериал, помечает его watched с заданным
// рейтингом и жанром, и выставляет watched_at на конкретную дату --
// без явного управления датой все просмотры попадали бы "сегодня" и тесты
// на разбивку по месяцам/годам ничего бы не проверяли.
func createWatchedSeries(t *testing.T, ctx context.Context, db *postgres.Pool, ownerID, familyID, title, genre string, rating int, watchedAt time.Time) string {
	t.Helper()
	var seriesID string
	err := db.WithUserContext(ctx, ownerID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO public.family_series (family_id, title, genres, created_by, total_episodes, episode_runtime_minutes)
			VALUES ($1, $2, $3, $4, 10, 60)
			RETURNING id
		`, familyID, title, []string{genre}, ownerID).Scan(&seriesID); err != nil {
			return err
		}

		_, err := tx.Exec(ctx, `
			INSERT INTO public.family_series_status (series_id, user_id, status, rating, watched_at)
			VALUES ($1, $2, 'watched', $3, $4)
		`, seriesID, ownerID, rating, watchedAt)
		return err
	})
	if err != nil {
		t.Fatalf("failed to create watched series: %v", err)
	}
	return seriesID
}

func TestFamilyStatsComputesHoursAndGenres(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()

	owner := createTestUser(t, ctx, db)
	familyID := createTestFamily(t, ctx, db, owner)

	// 10 эпизодов * 60 минут = 600 минут = 10 часов, ровно один сериал.
	createWatchedSeries(t, ctx, db, owner, familyID, "Show One", "drama", 5, time.Now())

	svc := stats.NewService(db)
	result, err := svc.GetFamilyStats(ctx, owner, familyID)
	if err != nil {
		t.Fatalf("GetFamilyStats() error: %v", err)
	}

	if result.TotalHoursWatched != 10 {
		t.Errorf("TotalHoursWatched = %d, want 10", result.TotalHoursWatched)
	}
	if result.TotalWatchedSeries != 1 {
		t.Errorf("TotalWatchedSeries = %d, want 1", result.TotalWatchedSeries)
	}
	if len(result.TopGenres) != 1 || result.TopGenres[0].Genre != "drama" || result.TopGenres[0].Count != 1 {
		t.Errorf("TopGenres = %+v, want [{drama 1}]", result.TopGenres)
	}
}

func TestFamilyAchievementsUnlocksFirstWatch(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()

	owner := createTestUser(t, ctx, db)
	familyID := createTestFamily(t, ctx, db, owner)
	createWatchedSeries(t, ctx, db, owner, familyID, "Show One", "drama", 4, time.Now())

	svc := stats.NewService(db)
	result, err := svc.GetFamilyAchievements(ctx, owner, familyID)
	if err != nil {
		t.Fatalf("GetFamilyAchievements() error: %v", err)
	}

	if result.TotalWatchedCount != 1 {
		t.Errorf("TotalWatchedCount = %d, want 1", result.TotalWatchedCount)
	}

	var firstWatch *stats.Achievement
	for i := range result.Achievements {
		if result.Achievements[i].ID == "first-watch" {
			firstWatch = &result.Achievements[i]
		}
	}
	if firstWatch == nil {
		t.Fatal("достижение first-watch отсутствует в списке")
	}
	if !firstWatch.Unlocked {
		t.Error("first-watch.Unlocked = false, want true после одного просмотра")
	}

	var watched10 *stats.Achievement
	for i := range result.Achievements {
		if result.Achievements[i].ID == "watched-10" {
			watched10 = &result.Achievements[i]
		}
	}
	if watched10 == nil {
		t.Fatal("достижение watched-10 отсутствует в списке")
	}
	if watched10.Unlocked {
		t.Error("watched-10.Unlocked = true, want false после всего одного просмотра")
	}
}

func TestYearWrappedFiltersToRequestedYear(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()

	owner := createTestUser(t, ctx, db)
	familyID := createTestFamily(t, ctx, db, owner)

	createWatchedSeries(t, ctx, db, owner, familyID, "Old Show", "comedy", 5,
		time.Date(2024, time.March, 1, 0, 0, 0, 0, time.UTC))
	createWatchedSeries(t, ctx, db, owner, familyID, "New Show", "drama", 3,
		time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC))

	svc := stats.NewService(db)

	wrapped2026, err := svc.GetYearWrapped(ctx, owner, familyID, 2026)
	if err != nil {
		t.Fatalf("GetYearWrapped(2026) error: %v", err)
	}
	if wrapped2026.TotalWatchedCount != 1 {
		t.Errorf("2026: TotalWatchedCount = %d, want 1 (только New Show)", wrapped2026.TotalWatchedCount)
	}
	if wrapped2026.TopRatedTitle == nil || *wrapped2026.TopRatedTitle != "New Show" {
		t.Errorf("2026: TopRatedTitle = %v, want New Show", wrapped2026.TopRatedTitle)
	}

	wrapped2024, err := svc.GetYearWrapped(ctx, owner, familyID, 2024)
	if err != nil {
		t.Fatalf("GetYearWrapped(2024) error: %v", err)
	}
	if wrapped2024.TotalWatchedCount != 1 {
		t.Errorf("2024: TotalWatchedCount = %d, want 1 (только Old Show)", wrapped2024.TotalWatchedCount)
	}
}

func TestRecommendationsExcludeWatchedAndRankByFavouriteGenre(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()

	owner := createTestUser(t, ctx, db)
	familyID := createTestFamily(t, ctx, db, owner)

	// Высоко оценённая драма делает "drama" любимым жанром семьи.
	createWatchedSeries(t, ctx, db, owner, familyID, "Loved Drama", "drama", 5, time.Now())

	svc := stats.NewService(db)

	// Ещё не просмотренный сериал того же жанра -- кандидат в рекомендации.
	err := db.WithUserContext(ctx, owner, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.family_series (family_id, title, genres, created_by)
			VALUES ($1, 'Another Drama', $2, $3)
		`, familyID, []string{"drama"}, owner)
		return err
	})
	if err != nil {
		t.Fatalf("failed to create candidate series: %v", err)
	}

	recommendations, err := svc.GetRecommendations(ctx, owner, familyID)
	if err != nil {
		t.Fatalf("GetRecommendations() error: %v", err)
	}

	var found bool
	for _, r := range recommendations {
		if r.Title == "Loved Drama" {
			t.Error("уже просмотренный сериал попал в рекомендации")
		}
		if r.Title == "Another Drama" {
			found = true
			if r.Score < 1 {
				t.Errorf("Another Drama score = %d, want >= 1 (совпадает с любимым жанром)", r.Score)
			}
		}
	}
	if !found {
		t.Error("Another Drama не найдена в рекомендациях")
	}
}
