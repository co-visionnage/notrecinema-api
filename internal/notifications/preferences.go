package notifications

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"notrecinema/api/internal/auth"
	"notrecinema/api/internal/platform/apperror"
	"notrecinema/api/internal/platform/ratelimit"
	"notrecinema/api/internal/platform/response"
	"notrecinema/api/internal/telemetry"
)

// Настройки уведомлений: для каждой категории событий отдельно push и
// отдельно почта. Нет строки в notification_preferences -- действуют
// значения по умолчанию (push везде включён, почта только там, где письма
// и были). Те же значения зашиты в SQL (notification_default, миграция
// 0037): воркер решает, кого уведомлять, там же, в БД, и список обязан
// совпадать. Совпадение проверяет интеграционный тест.

// Category -- тип события, от которого можно отказаться.
type Category struct {
	Key string
	// EmailSupported -- рассылаются ли по этой категории письма вообще.
	// Включить почту для категории без писем нельзя: тумблер ничего бы не
	// делал и вводил в заблуждение.
	EmailSupported bool
	// EmailDefault -- включена ли почта по умолчанию.
	EmailDefault bool
}

// Порядок -- порядок показа в настройках.
var Categories = []Category{
	{Key: "series_added", EmailSupported: true, EmailDefault: true},
	{Key: "season_update", EmailSupported: true, EmailDefault: true},
	{Key: "progress_reminder", EmailSupported: true, EmailDefault: true},
	{Key: "series_watched"},
	{Key: "series_rated"},
	{Key: "poll"},
	{Key: "watch_event"},
	{Key: "member_joined"},
}

func categoryByKey(key string) (Category, bool) {
	for _, c := range Categories {
		if c.Key == key {
			return c, true
		}
	}
	return Category{}, false
}

type Preference struct {
	Category       string `json:"category"`
	Push           bool   `json:"push"`
	Email          bool   `json:"email"`
	EmailSupported bool   `json:"emailSupported"`
}

type PreferenceInput struct {
	Category string `json:"category"`
	Push     bool   `json:"push"`
	Email    bool   `json:"email"`
}

// GetPreferences возвращает настройки по всем категориям: сохранённые
// значения, а где их нет -- значения по умолчанию.
func (s *Service) GetPreferences(ctx context.Context, userID string) ([]Preference, error) {
	stored := map[string]PreferenceInput{}

	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT category, push, email FROM public.notification_preferences WHERE user_id = $1
		`, userID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var p PreferenceInput
			if err := rows.Scan(&p.Category, &p.Push, &p.Email); err != nil {
				return err
			}
			stored[p.Category] = p
		}
		return rows.Err()
	})
	if err != nil {
		return nil, apperror.Internal("failed to load notification preferences", err)
	}

	result := make([]Preference, 0, len(Categories))
	for _, c := range Categories {
		pref := Preference{Category: c.Key, Push: true, Email: c.EmailDefault, EmailSupported: c.EmailSupported}
		if saved, ok := stored[c.Key]; ok {
			pref.Push = saved.Push
			pref.Email = saved.Email
		}
		result = append(result, pref)
	}
	return result, nil
}

// SetPreferences сохраняет настройки перечисленных категорий (остальные не
// трогает). Неизвестная категория и почта для категории без писем --
// ошибка, а не молчаливая запись, которая ничего не меняет.
func (s *Service) SetPreferences(ctx context.Context, userID string, inputs []PreferenceInput) error {
	if len(inputs) == 0 {
		return apperror.Invalid("нет настроек для сохранения")
	}
	seen := map[string]bool{}
	for _, in := range inputs {
		category, ok := categoryByKey(in.Category)
		if !ok {
			return apperror.Invalid("неизвестная категория уведомлений: " + in.Category)
		}
		if seen[in.Category] {
			return apperror.Invalid("категория указана дважды: " + in.Category)
		}
		seen[in.Category] = true
		if in.Email && !category.EmailSupported {
			return apperror.Invalid("по этой категории письма не рассылаются: " + in.Category)
		}
	}

	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		for _, in := range inputs {
			if _, err := tx.Exec(ctx, `
				INSERT INTO public.notification_preferences (user_id, category, push, email)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (user_id, category)
				DO UPDATE SET push = EXCLUDED.push, email = EXCLUDED.email, updated_at = NOW()
			`, userID, in.Category, in.Push, in.Email); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return apperror.Internal("failed to save notification preferences", err)
	}
	return nil
}

// Отписка по ссылке из письма. Токен подписан HMAC-ключом, общим для API и
// воркера (UNSUBSCRIBE_SECRET): воркер подписывает ссылку при отправке, API
// проверяет. Формат -- единственный договор между двумя модулями, он
// продублирован в notrecinema-worker/internal/unsubscribe и сверяется
// тестом с общим тестовым вектором:
//
//	token = base64url(userId + "|" + category) + "." + base64url(HMAC-SHA256(secret, base64url(userId + "|" + category)))
//
// Токен не содержит срока действия: отписка должна работать по любому,
// даже очень старому письму, и она безвредна -- отключает только почту
// одной категории.

// WithUnsubscribeSecret включает отписку по ссылке. Без ключа
// UnsubscribeEmail отвечает ошибкой настройки.
func (s *Service) WithUnsubscribeSecret(secret string) *Service {
	s.unsubscribeSecret = []byte(secret)
	return s
}

func signUnsubscribePayload(secret []byte, payload string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// SignUnsubscribeToken нужен тестам (подпись в проде ставит воркер).
func SignUnsubscribeToken(secret, userID, category string) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(userID + "|" + category))
	return payload + "." + signUnsubscribePayload([]byte(secret), payload)
}

func (s *Service) parseUnsubscribeToken(token string) (userID, category string, ok bool) {
	payload, signature, found := strings.Cut(strings.TrimSpace(token), ".")
	if !found || payload == "" || signature == "" {
		return "", "", false
	}

	expected := signUnsubscribePayload(s.unsubscribeSecret, payload)
	if !hmac.Equal([]byte(signature), []byte(expected)) {
		return "", "", false
	}

	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return "", "", false
	}
	userID, category, found = strings.Cut(string(raw), "|")
	if !found || userID == "" {
		return "", "", false
	}
	if _, known := categoryByKey(category); !known {
		return "", "", false
	}
	return userID, category, true
}

// UnsubscribeEmail выключает почту одной категории по токену из письма.
// Возвращает категорию, чтобы страница могла сказать, от чего отписали.
func (s *Service) UnsubscribeEmail(ctx context.Context, token string) (string, error) {
	if len(s.unsubscribeSecret) == 0 {
		return "", apperror.Internal("unsubscribe is not configured", nil)
	}

	userID, category, ok := s.parseUnsubscribeToken(token)
	if !ok {
		telemetry.RecordAuth("unsubscribe", "invalid_token")
		return "", apperror.Invalid("ссылка для отписки недействительна")
	}

	if err := s.db.Exec(ctx, `SELECT public.set_email_preference_system($1, $2, false)`, userID, category); err != nil {
		return "", apperror.Internal("failed to unsubscribe", err)
	}
	telemetry.RecordAuth("unsubscribe", category)
	return category, nil
}

const (
	unsubscribeIPMax           = 30
	unsubscribeIPWindowSeconds = 15 * 60
)

// RegisterPreferenceRoutes монтирует настройки (за сессией) и публичную
// отписку по ссылке.
func RegisterPreferenceRoutes(protectedMux, publicMux *http.ServeMux, svc *Service) {
	protectedMux.HandleFunc("GET /api/v1/notification-preferences", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())

		prefs, err := svc.GetPreferences(r.Context(), user.ID)
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, map[string]any{"preferences": prefs})
	})

	protectedMux.HandleFunc("PUT /api/v1/notification-preferences", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())

		var req struct {
			Preferences []PreferenceInput `json:"preferences"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			response.Error(w, apperror.Invalid("invalid request body"))
			return
		}

		if err := svc.SetPreferences(r.Context(), user.ID, req.Preferences); err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, map[string]bool{"success": true})
	})

	// POST, а не GET: ссылку в письме сканеры и антивирусы открывают сами,
	// и отписка от этого срабатывала бы без человека. POST по этой же
	// ссылке -- это и есть RFC 8058 (List-Unsubscribe-Post: One-Click):
	// почтовый клиент шлёт POST, тело игнорируется, токен берётся из
	// query-параметра. Страница на фронтенде шлёт тот же POST по кнопке.
	publicMux.HandleFunc("POST /api/v1/unsubscribe", func(w http.ResponseWriter, r *http.Request) {
		allowed, err := ratelimit.Check(r.Context(), svc.db, "unsubscribe:ip:"+requestIP(r), unsubscribeIPMax, unsubscribeIPWindowSeconds)
		if err != nil {
			response.Error(w, apperror.Internal("rate limit check failed", err))
			return
		}
		if !allowed {
			response.Error(w, apperror.RateLimited("слишком много попыток. Попробуйте позже"))
			return
		}

		token := r.URL.Query().Get("token")
		if token == "" {
			var body struct {
				Token string `json:"token"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
				token = body.Token
			}
		}

		category, err := svc.UnsubscribeEmail(r.Context(), token)
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, map[string]any{"success": true, "category": category})
	})
}

func requestIP(r *http.Request) string {
	if realIP := strings.TrimSpace(r.Header.Get("X-Real-Ip")); realIP != "" {
		return realIP
	}
	if forwardedFor := r.Header.Get("X-Forwarded-For"); forwardedFor != "" {
		hops := strings.Split(forwardedFor, ",")
		return strings.TrimSpace(hops[len(hops)-1])
	}
	return "unknown"
}
