// Package auth реализует полный цикл аутентификации: проверку cookie
// сессии (Middleware, как и раньше) и теперь также выпуск самой сессии --
// логин, регистрация, логаут, 2FA. Использует ту же таблицу app_sessions и
// ту же SQL-схему (create_session_for_profile, get_session_user и т.д.),
// что и notrecinema-app (src/shared/api/postgres/auth.ts), поэтому оба
// сервиса продолжают узнавать одну и ту же сессию и один и тот же формат
// хэша пароля (см. password.go) -- независимо от того, какой из них её
// выпустил.
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"

	"notrecinema/api/internal/platform/apperror"
	"notrecinema/api/internal/postgres"
)

type User struct {
	ID          string
	Email       string
	DisplayName string
}

type contextKey int

const userContextKey contextKey = 0

type Service struct {
	db          *postgres.Pool
	cookieName  string
	environment string
}

// environment "production" включает Secure на cookie -- тот же чек
// process.env.NODE_ENV === 'production', что и в notrecinema-app.
func NewService(db *postgres.Pool, cookieName, environment string) *Service {
	return &Service{db: db, cookieName: cookieName, environment: environment}
}

func (s *Service) isProduction() bool {
	return s.environment == "production"
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// sessionUser ищет сессию по хэшу токена. get_session_user объявлена как
// SECURITY DEFINER (см. первую миграцию схемы), поэтому выполняется с
// правами владельца функции и корректно обходит RLS на app_sessions —
// пользовательского контекста для её ограничения ещё нет, именно его
// и устанавливает этот вызов.
func (s *Service) sessionUser(ctx context.Context, token string) (*User, error) {
	row := s.db.QueryRow(ctx, "SELECT user_id, email, display_name FROM public.get_session_user($1::text)", hashToken(token))

	var (
		user        User
		displayName *string
	)
	if err := row.Scan(&user.ID, &user.Email, &displayName); err != nil {
		return nil, err
	}
	if displayName != nil {
		user.DisplayName = *displayName
	}
	return &user, nil
}

// Middleware требует валидную сессию и кладёт найденного пользователя в
// контекст запроса. Хендлеры за ним могут рассчитывать, что UserFromContext
// отработает без паники.
func (s *Service) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(s.cookieName)
		if err != nil || cookie.Value == "" {
			writeUnauthorized(w)
			return
		}

		user, err := s.sessionUser(r.Context(), cookie.Value)
		if err != nil {
			writeUnauthorized(w)
			return
		}

		ctx := context.WithValue(r.Context(), userContextKey, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func writeUnauthorized(w http.ResponseWriter) {
	unauthorized := apperror.Unauthorized("authentication required")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(apperror.HTTPStatus(unauthorized))
	_, _ = w.Write([]byte(`{"error":"authentication required"}`))
}

// UserFromContext паникует, если вызвана вне Middleware -- любой роут,
// который её использует, обязан быть смонтирован за Middleware, поэтому
// отсутствие пользователя — это баг в разводке роутов, а не штатная
// ситуация, которую нужно аккуратно обрабатывать.
func UserFromContext(ctx context.Context) *User {
	user, ok := ctx.Value(userContextKey).(*User)
	if !ok {
		panic("auth: UserFromContext called outside auth.Middleware")
	}
	return user
}
