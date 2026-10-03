package auth

import (
	"context"
	"net/http"
	"strings"

	"notrecinema/api/internal/platform/ratelimit"
)

// checkRateLimit — прямой перенос checkRateLimit из notrecinema-app:
// фиксированное окно поверх той же SQL-функции check_rate_limit (миграция
// 0016), которой уже пользуется internal/calendar и internal/seasons.
func (s *Service) checkRateLimit(ctx context.Context, bucketKey string, maxAttempts, windowSeconds int) (bool, error) {
	return ratelimit.Check(ctx, s.db, bucketKey, maxAttempts, windowSeconds)
}

// clientIP — прямой перенос getClientIp: X-Real-IP (проставляется
// реверс-прокси, заменяет, а не дописывает значение — не подделать
// клиентским заголовком) приоритетнее последнего хопа X-Forwarded-For
// (каждый хоп дописывает свой адрес, так что последний -- тот, что видел
// сам прокси); без обоих заголовков -- "unknown", как и в оригинале, а не
// r.RemoteAddr (за один и тот же reverse-proxy в проде она всегда была бы
// адресом самого прокси).
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
