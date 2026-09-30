package upload

import (
	"context"
	"io"
	"net/http"
	"strings"

	"notrecinema/api/internal/auth"
	"notrecinema/api/internal/platform/apperror"
	"notrecinema/api/internal/platform/response"
	"notrecinema/api/internal/postgres"
)

const (
	maxImageSize = 5 * 1024 * 1024

	userRateLimitMaxAttempts = 20
	userRateLimitWindowSecs  = 10 * 60
	ipRateLimitMaxAttempts   = 60
	ipRateLimitWindowSecs    = 10 * 60
)

// RegisterRoutes монтирует POST /api/v1/upload/image -- прямой перенос
// notrecinema-app (src/app/api/upload/image/route.ts). db нужен только для
// check_rate_limit (миграция 0016), та же функция, что уже использует
// internal/movies/internal/calendar/internal/seasons.
func RegisterRoutes(mux *http.ServeMux, svc *Service, db *postgres.Pool) {
	mux.HandleFunc("POST /api/v1/upload/image", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())

		if !svc.Configured() {
			response.Error(w, apperror.Invalid("загрузка изображений не настроена"))
			return
		}

		if allowed, err := checkRateLimit(r.Context(), db, "upload:ip:"+clientIP(r), ipRateLimitMaxAttempts, ipRateLimitWindowSecs); err != nil {
			response.Error(w, apperror.Internal("failed to check rate limit", err))
			return
		} else if !allowed {
			response.Error(w, apperror.RateLimited("слишком много загрузок. Попробуйте позже"))
			return
		}
		if allowed, err := checkRateLimit(r.Context(), db, "upload:user:"+user.ID, userRateLimitMaxAttempts, userRateLimitWindowSecs); err != nil {
			response.Error(w, apperror.Internal("failed to check rate limit", err))
			return
		} else if !allowed {
			response.Error(w, apperror.RateLimited("слишком много загрузок. Попробуйте позже"))
			return
		}

		if err := r.ParseMultipartForm(maxImageSize + 1<<20); err != nil {
			response.Error(w, apperror.Invalid("файл не передан"))
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			response.Error(w, apperror.Invalid("файл не передан"))
			return
		}
		defer func() { _ = file.Close() }()

		data, err := io.ReadAll(io.LimitReader(file, maxImageSize+1))
		if err != nil {
			response.Error(w, apperror.Internal("failed to read upload", err))
			return
		}
		if len(data) > maxImageSize {
			response.Error(w, apperror.Invalid("изображение должно быть не больше 5 МБ"))
			return
		}

		image, ok := SniffImageType(data)
		if !ok {
			response.Error(w, apperror.Invalid("можно загружать только изображения (PNG, JPEG, GIF, WebP)"))
			return
		}

		url, err := svc.UploadSeriesImage(r.Context(), data, image)
		if err != nil {
			response.Error(w, apperror.Internal("не удалось загрузить изображение", err))
			return
		}
		response.JSON(w, http.StatusOK, map[string]string{"url": url})
	})
}

func checkRateLimit(ctx context.Context, db *postgres.Pool, bucketKey string, maxAttempts, windowSeconds int) (bool, error) {
	var allowed bool
	err := db.QueryRow(ctx, `SELECT public.check_rate_limit($1, $2, $3)`, bucketKey, maxAttempts, windowSeconds).Scan(&allowed)
	return allowed, err
}

// clientIP -- та же логика, что auth.clientIP (X-Real-Ip, иначе последний
// хоп X-Forwarded-For, иначе "unknown"), продублирована умышленно: пакеты
// не должны тянуть друг друга ради одной функции на пять строк.
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
