// Package notifications управляет подписками на web push
// (public.push_subscriptions) -- сама отправка живёт в notrecinema-worker
// (internal/webpush, internal/notifications там же), который вычитывает
// их через SECURITY DEFINER функции, минуя RLS этого пакета. Этот пакет
// отвечает только за то, чтобы пользователь мог подписаться/отписаться --
// то же самое, что savePushSubscriptionAction/removePushSubscriptionAction
// в notrecinema-app (src/shared/actions/push-postgres.ts).
package notifications

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5"

	"notrecinema/api/internal/auth"
	"notrecinema/api/internal/platform/apperror"
	"notrecinema/api/internal/platform/response"
	"notrecinema/api/internal/postgres"
)

type Service struct {
	db *postgres.Pool
	// unsubscribeSecret -- HMAC-ключ ссылок отписки (см. preferences.go).
	unsubscribeSecret []byte
}

func NewService(db *postgres.Pool) *Service {
	return &Service{db: db}
}

type SubscribeInput struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// Subscribe делает upsert по UNIQUE(endpoint): тот же браузер может
// переподписаться (например ключи ротировались) без отдельного DELETE.
func (s *Service) Subscribe(ctx context.Context, userID string, in SubscribeInput) error {
	if in.Endpoint == "" || in.Keys.P256dh == "" || in.Keys.Auth == "" {
		return apperror.Invalid("endpoint and keys.p256dh/keys.auth are required")
	}

	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.push_subscriptions (user_id, endpoint, p256dh, auth)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (endpoint) DO UPDATE SET p256dh = EXCLUDED.p256dh, auth = EXCLUDED.auth
		`, userID, in.Endpoint, in.Keys.P256dh, in.Keys.Auth)
		return err
	})
	if err != nil {
		return apperror.Internal("failed to save push subscription", err)
	}
	return nil
}

type UnsubscribeInput struct {
	Endpoint string `json:"endpoint"`
}

func (s *Service) Unsubscribe(ctx context.Context, userID string, in UnsubscribeInput) error {
	if in.Endpoint == "" {
		return apperror.Invalid("endpoint is required")
	}

	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		// RLS (push_subscriptions_owner_only) уже гарантирует, что это
		// удалит только подписку самого userID, даже без WHERE user_id = ...
		_, err := tx.Exec(ctx, `DELETE FROM public.push_subscriptions WHERE endpoint = $1`, in.Endpoint)
		return err
	})
	if err != nil {
		return apperror.Internal("failed to remove push subscription", err)
	}
	return nil
}

func RegisterRoutes(mux *http.ServeMux, svc *Service) {
	mux.HandleFunc("POST /api/v1/notifications/subscriptions", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())

		var in SubscribeInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			response.Error(w, apperror.Invalid("invalid request body"))
			return
		}

		if err := svc.Subscribe(r.Context(), user.ID, in); err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusNoContent, nil)
	})

	mux.HandleFunc("DELETE /api/v1/notifications/subscriptions", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())

		var in UnsubscribeInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			response.Error(w, apperror.Invalid("invalid request body"))
			return
		}

		if err := svc.Unsubscribe(r.Context(), user.ID, in); err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusNoContent, nil)
	})
}
