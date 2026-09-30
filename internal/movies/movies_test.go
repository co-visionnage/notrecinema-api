package movies

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"notrecinema/api/internal/movieprovider"
)

type stubProvider struct {
	movie movieprovider.Movie
}

func (s stubProvider) Name() string { return "stub" }
func (s stubProvider) GetMovie(ctx context.Context, externalID string) (movieprovider.Movie, error) {
	return s.movie, nil
}

// TestUnknownSourceReturns404 не трогает db (nil здесь безопасен): неизвестный
// источник отбраковывается раньше, чем хендлер вообще доходит до
// check_rate_limit или auth.UserFromContext. Остальные пути (известный
// источник, лимит) требуют настоящего пользователя и настоящего Postgres --
// см. movies_integration_test.go.
func TestUnknownSourceReturns404(t *testing.T) {
	mux := http.NewServeMux()
	RegisterRoutes(mux, nil, map[string]movieprovider.Provider{
		"tmdb": stubProvider{movie: movieprovider.Movie{ID: "1", Title: "Test"}},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/movies/omdb/tt123", nil)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d for unconfigured source", rec.Code, http.StatusNotFound)
	}
}
