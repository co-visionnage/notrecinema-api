//go:build integration

// Запуск: DATABASE_URL=... go test -tags integration ./...
package movies_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"notrecinema/api/internal/auth"
	"notrecinema/api/internal/movieprovider"
	"notrecinema/api/internal/movies"
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

// createAuthenticatedRequest создаёт настоящего пользователя и настоящую
// сессию (create_profile_session -- то же, чем пользуется auth.Middleware
// в проде) и возвращает уже провалидированный этим middleware запрос --
// то есть с *auth.User в контексте, как его увидел бы реальный хендлер.
func createAuthenticatedRequest(t *testing.T, ctx context.Context, db *postgres.Pool, method, target string) *http.Request {
	t.Helper()
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}
	token := hex.EncodeToString(tokenBytes)
	hash := sha256.Sum256([]byte(token))
	email := fmt.Sprintf("movies-test-%s@example.com", token)

	var userID string
	row := db.QueryRow(ctx,
		"SELECT user_id FROM public.create_profile_session($1, $2, $3, $4)",
		email, "Movies Test", hex.EncodeToString(hash[:]), time.Now().Add(time.Hour),
	)
	if err := row.Scan(&userID); err != nil {
		t.Fatalf("failed to create test user: %v", err)
	}

	authService := auth.NewService(db, "notre_cinema_session", "development")
	req := httptest.NewRequest(method, target, nil)
	req.AddCookie(&http.Cookie{Name: "notre_cinema_session", Value: token})

	var authedReq *http.Request
	authService.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authedReq = r
	})).ServeHTTP(httptest.NewRecorder(), req)

	if authedReq == nil {
		t.Fatal("auth middleware did not authenticate the test request")
	}
	return authedReq
}

type stubProvider struct {
	movie movieprovider.Movie
}

func (s stubProvider) Name() string { return "stub" }
func (s stubProvider) GetMovie(ctx context.Context, externalID string) (movieprovider.Movie, error) {
	return s.movie, nil
}

func TestKnownSourceReturnsMovie(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()

	mux := http.NewServeMux()
	movies.RegisterRoutes(mux, db, map[string]movieprovider.Provider{
		"tmdb": stubProvider{movie: movieprovider.Movie{ID: "603", Title: "The Matrix"}},
	})

	req := createAuthenticatedRequest(t, ctx, db, http.MethodGet, "/api/v1/movies/tmdb/603")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func TestRateLimitBlocksAfterTooManyRequests(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()

	mux := http.NewServeMux()
	movies.RegisterRoutes(mux, db, map[string]movieprovider.Provider{
		"tmdb": stubProvider{movie: movieprovider.Movie{ID: "603", Title: "The Matrix"}},
	})

	// Все запросы идут от одного и того же аутентифицированного пользователя
	// (один bucket в rate_limit_buckets) -- лимит 60/10мин должен сработать
	// раньше, чем мы успеем сделать 61 запрос.
	req := createAuthenticatedRequest(t, ctx, db, http.MethodGet, "/api/v1/movies/tmdb/603")

	var lastStatus int
	for i := 0; i < 61; i++ {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		lastStatus = rec.Code
		if lastStatus == http.StatusTooManyRequests {
			break
		}
	}

	if lastStatus != http.StatusTooManyRequests {
		t.Fatalf("после 61 запроса статус = %d, want %d", lastStatus, http.StatusTooManyRequests)
	}
}
