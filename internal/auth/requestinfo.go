package auth

import (
	"context"
	"net/http"
)

// RequestInfo -- откуда пришёл запрос, создавший сессию. Показывается в
// списке сессий («Chrome, 203.0.113.7»), чтобы человек мог узнать свои
// устройства и закрыть чужие.
type RequestInfo struct {
	IP        string
	UserAgent string
}

type requestInfoKey struct{}

// WithRequest кладёт в контекст IP и User-Agent запроса. Хендлеры, после
// которых создаётся сессия (логин, регистрация, 2FA, OAuth), оборачивают
// им контекст; сервис сам запишет эти данные в сессию.
func WithRequest(ctx context.Context, r *http.Request) context.Context {
	ip := clientIP(r)
	if ip == "unknown" {
		ip = ""
	}
	return context.WithValue(ctx, requestInfoKey{}, RequestInfo{IP: ip, UserAgent: r.UserAgent()})
}

func requestInfoFrom(ctx context.Context) (RequestInfo, bool) {
	info, ok := ctx.Value(requestInfoKey{}).(RequestInfo)
	return info, ok
}

// recordSessionMetadata сохраняет IP и User-Agent только что созданной
// сессии. Сбой не должен ломать вход: метаданные нужны для удобства, а не
// для безопасности, поэтому ошибка лишь глотается.
func (s *Service) recordSessionMetadata(ctx context.Context, token string) {
	info, ok := requestInfoFrom(ctx)
	if !ok {
		return
	}
	_ = s.db.Exec(ctx, `SELECT public.set_session_metadata($1, $2, $3)`, hashToken(token), info.IP, info.UserAgent)
}
