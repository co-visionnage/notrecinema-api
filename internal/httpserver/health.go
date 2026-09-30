package httpserver

import (
	"context"
	"net/http"
	"time"

	"notrecinema/api/internal/platform/response"
)

type pinger interface {
	Ping(ctx context.Context) error
}

// Healthz — liveness-проба: никогда не обращается к зависимостям, поэтому
// отвечает только на вопрос «процесс жив?» — ровно то, на чём должно быть
// основано решение о рестарте.
func Healthz(w http.ResponseWriter, r *http.Request) {
	response.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Readyz — readiness-проба: падает, пока база недоступна, чтобы балансировщик
// или оркестратор мог перестать слать сюда трафик, не перезапуская процесс.
func Readyz(db pinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		if err := db.Ping(ctx); err != nil {
			response.JSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
			return
		}
		response.JSON(w, http.StatusOK, map[string]string{"status": "ready"})
	}
}
