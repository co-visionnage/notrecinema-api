// Package movies подключает internal/movieprovider к HTTP: получить
// метаданные фильма/сериала по ID конкретного источника.
//
// Здесь сознательно НЕТ Fallback-цепочки между источниками: TMDB, OMDB и
// Kinopoisk используют разные, несовместимые пространства ID (числовой
// TMDB ID, "ttXXXXXXX" IMDB ID у OMDB, числовой Kinopoisk ID) -- один и
// тот же externalID у разных провайдеров означает разные фильмы или
// вообще ничего. Резилиентность (timeout/retry/circuit breaker) при этом
// работает для каждого источника по отдельности через movieprovider.Resilient.
// movieprovider.Fallback остаётся полезен там, где источники ВЗАИМОЗАМЕНЯЕМЫ
// по одному и тому же ID (например несколько зеркал одного API) -- это не
// тот случай.
package movies

import (
	"net/http"

	"notrecinema/api/internal/auth"
	"notrecinema/api/internal/movieprovider"
	"notrecinema/api/internal/platform/apperror"
	"notrecinema/api/internal/platform/ratelimit"
	"notrecinema/api/internal/platform/response"
	"notrecinema/api/internal/postgres"
)

const (
	rateLimitMaxAttempts  = 60
	rateLimitWindowSecond = 600
)

// RegisterRoutes монтирует GET /api/v1/movies/{source}/{externalId}.
// providers -- источники, реально сконфигурированные (с непустым API-ключом);
// источник без ключа просто не регистрируется, и запрос к нему отвечает 404,
// а не падает с ошибкой авторизации у внешнего API.
//
// db нужен только для check_rate_limit (миграция 0016, та же функция, что
// уже использует internal/calendar) -- без лимита один пользователь мог бы
// расходовать чужую квоту TMDB/OMDb/Kinopoisk на весь проект через этот
// прокси, а Resilient (таймаут/ретраи/circuit breaker) защищает только от
// сбоев самого внешнего API, не от объёма запросов к нему.
func RegisterRoutes(mux *http.ServeMux, db *postgres.Pool, providers map[string]movieprovider.Provider) {
	mux.HandleFunc("GET /api/v1/movies/{source}/{externalId}", func(w http.ResponseWriter, r *http.Request) {
		source := r.PathValue("source")
		provider, ok := providers[source]
		if !ok {
			response.Error(w, apperror.NotFound("unknown or unconfigured movie source: "+source))
			return
		}

		user := auth.UserFromContext(r.Context())
		allowed, err := ratelimit.Check(r.Context(), db, "movies:user:"+user.ID, rateLimitMaxAttempts, rateLimitWindowSecond)
		if err != nil {
			response.Error(w, apperror.Internal("failed to check rate limit", err))
			return
		}
		if !allowed {
			response.Error(w, apperror.RateLimited("слишком много запросов, попробуйте позже"))
			return
		}

		movie, err := provider.GetMovie(r.Context(), r.PathValue("externalId"))
		if err != nil {
			response.Error(w, apperror.Internal("failed to fetch movie", err))
			return
		}
		response.JSON(w, http.StatusOK, movie)
	})
}
