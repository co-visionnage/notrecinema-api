package imports

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"notrecinema/api/internal/platform/apperror"
	"notrecinema/api/internal/platform/ratelimit"
	"notrecinema/api/internal/platform/response"
	"notrecinema/api/internal/postgres"
	"notrecinema/api/internal/telemetry"
)

const (
	rateLimitMaxAttempts  = 20
	rateLimitWindowSecond = 600
)

type Service struct {
	db              *postgres.Pool
	httpClient      *http.Client
	kinopoiskAPIKey string
	omdbAPIKey      string
	traktClientID   string
}

func NewService(db *postgres.Pool, kinopoiskAPIKey, omdbAPIKey, traktClientID string) *Service {
	return &Service{
		db:              db,
		httpClient:      telemetry.InstrumentedClient(),
		kinopoiskAPIKey: kinopoiskAPIKey,
		omdbAPIKey:      omdbAPIKey,
		traktClientID:   traktClientID,
	}
}

// Search — прямой перенос GET /api/import/search: параллельно опрашивает
// Kinopoisk и OMDb, конкатенирует результаты (Kinopoisk первым).
func (s *Service) Search(ctx context.Context, query string) ([]ImportedSeries, error) {
	var kinopoiskResults, omdbResults []ImportedSeries
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		kinopoiskResults, _ = s.searchKinopoisk(ctx, query)
	}()
	go func() {
		defer wg.Done()
		omdbResults, _ = s.searchOmdb(ctx, query)
	}()
	wg.Wait()

	results := make([]ImportedSeries, 0, len(kinopoiskResults)+len(omdbResults))
	results = append(results, kinopoiskResults...)
	results = append(results, omdbResults...)
	return results, nil
}

func (s *Service) checkRateLimit(ctx context.Context, bucketKey string) (bool, error) {
	return ratelimit.Check(ctx, s.db, bucketKey, rateLimitMaxAttempts, rateLimitWindowSecond)
}

func clientIP(r *http.Request) string {
	if realIP := strings.TrimSpace(r.Header.Get("X-Real-Ip")); realIP != "" {
		return realIP
	}
	if forwardedFor := r.Header.Get("X-Forwarded-For"); forwardedFor != "" {
		hops := strings.Split(forwardedFor, ",")
		return strings.TrimSpace(hops[len(hops)-1])
	}
	return "unknown"
}

// RegisterRoutes монтирует GET /api/v1/import/search, GET
// /api/v1/import/trakt и POST /api/v1/import/csv -- все три смонтированы
// на protectedMux (требуют сессию), в отличие от оригинала в
// notrecinema-app, где эти роуты вообще не проверяли авторизацию (сам
// импорт ничего не пишет в БД, только возвращает список кандидатов,
// поэтому в монолите авторизация не требовалась) -- здесь проверка есть
// просто потому, что все остальные роуты API уже её требуют, а не потому
// что кандидаты сами по себе секретны.
func RegisterRoutes(mux *http.ServeMux, svc *Service) {
	mux.HandleFunc("GET /api/v1/import/search", func(w http.ResponseWriter, r *http.Request) {
		allowed, err := svc.checkRateLimit(r.Context(), "import:ip:"+clientIP(r))
		if err != nil {
			response.Error(w, apperror.Internal("failed to check rate limit", err))
			return
		}
		if !allowed {
			response.Error(w, apperror.RateLimited("слишком много запросов на импорт. Попробуйте позже"))
			return
		}

		query := strings.TrimSpace(r.URL.Query().Get("query"))
		if query == "" {
			response.Error(w, apperror.Invalid("query is required"))
			return
		}
		if svc.kinopoiskAPIKey == "" && svc.omdbAPIKey == "" {
			response.JSON(w, http.StatusNotImplemented, map[string]string{"error": "импорт не настроен: нет KINOPOISK_API_KEY или OMDB_API_KEY"})
			return
		}

		results, err := svc.Search(r.Context(), query)
		if err != nil {
			response.Error(w, apperror.Internal("failed to search", err))
			return
		}
		response.JSON(w, http.StatusOK, map[string]any{"results": results})
	})

	mux.HandleFunc("GET /api/v1/import/trakt", func(w http.ResponseWriter, r *http.Request) {
		allowed, err := svc.checkRateLimit(r.Context(), "import:ip:"+clientIP(r))
		if err != nil {
			response.Error(w, apperror.Internal("failed to check rate limit", err))
			return
		}
		if !allowed {
			response.Error(w, apperror.RateLimited("слишком много запросов на импорт. Попробуйте позже"))
			return
		}

		username := strings.TrimSpace(r.URL.Query().Get("username"))
		if username == "" {
			response.Error(w, apperror.Invalid("username is required"))
			return
		}
		if svc.traktClientID == "" {
			response.JSON(w, http.StatusNotImplemented, map[string]string{"error": "импорт из Trakt не настроен: нет TRAKT_CLIENT_ID"})
			return
		}

		results, err := svc.importTraktWatchlist(r.Context(), username)
		if err != nil {
			response.Error(w, apperror.Internal("failed to import trakt watchlist", err))
			return
		}
		response.JSON(w, http.StatusOK, map[string]any{"results": results})
	})

	mux.HandleFunc("POST /api/v1/import/csv", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			CSV string `json:"csv"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			response.Error(w, apperror.Invalid("invalid request body"))
			return
		}
		results := ParseIMDbWatchlistCSV(req.CSV)
		response.JSON(w, http.StatusOK, map[string]any{"results": results})
	})
}
