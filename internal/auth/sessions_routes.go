package auth

import (
	"encoding/json"
	"net/http"
	"time"

	"notrecinema/api/internal/platform/apperror"
	"notrecinema/api/internal/platform/response"
)

const (
	changePasswordMaxAttempts   = 5
	changePasswordWindowSeconds = 15 * 60
)

type sessionResponse struct {
	ID         string    `json:"id"`
	Current    bool      `json:"current"`
	IP         string    `json:"ip,omitempty"`
	UserAgent  string    `json:"userAgent,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	LastSeenAt time.Time `json:"lastSeenAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

// RegisterSessionRoutes монтирует управление сессиями и смену пароля из
// аккаунта. Все роуты требуют активной сессии (protectedMux).
func RegisterSessionRoutes(mux *http.ServeMux, svc *Service) {
	mux.HandleFunc("GET /api/v1/auth/sessions", func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())

		sessions, err := svc.ListSessions(r.Context(), user.ID, user.SessionID)
		if err != nil {
			response.Error(w, err)
			return
		}

		out := make([]sessionResponse, 0, len(sessions))
		for _, s := range sessions {
			out = append(out, sessionResponse{
				ID: s.ID, Current: s.Current, IP: s.IP, UserAgent: s.UserAgent,
				CreatedAt: s.CreatedAt, LastSeenAt: s.LastSeenAt, ExpiresAt: s.ExpiresAt,
			})
		}
		response.JSON(w, http.StatusOK, map[string]any{"sessions": out})
	})

	// Закрыть все сессии, кроме текущей («выйти на всех других устройствах»).
	mux.HandleFunc("DELETE /api/v1/auth/sessions", func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())

		revoked, err := svc.RevokeOtherSessions(r.Context(), user.ID, user.SessionID)
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, map[string]any{"success": true, "revoked": revoked})
	})

	mux.HandleFunc("DELETE /api/v1/auth/sessions/{sessionId}", func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())
		sessionID := r.PathValue("sessionId")

		deleted, err := svc.RevokeSession(r.Context(), user.ID, sessionID)
		if err != nil {
			response.Error(w, err)
			return
		}
		if !deleted {
			response.Error(w, apperror.NotFound("session not found"))
			return
		}
		// Закрыли ту, по которой вошли: это обычный выход.
		if sessionID == user.SessionID {
			svc.ClearSessionCookie(w)
		}
		response.JSON(w, http.StatusOK, map[string]bool{"success": true})
	})

	mux.HandleFunc("POST /api/v1/auth/password/change", func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())

		var req struct {
			CurrentPassword string `json:"currentPassword"`
			NewPassword     string `json:"newPassword"`
			ConfirmPassword string `json:"confirmPassword"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			response.Error(w, apperror.Invalid("invalid request body"))
			return
		}

		if !svc.allow(w, r, "password-change:user:"+user.ID, changePasswordMaxAttempts, changePasswordWindowSeconds, "слишком много попыток. Попробуйте позже") {
			return
		}

		if problem := passwordProblem(req.NewPassword); problem != "" {
			response.Error(w, apperror.Invalid(problem))
			return
		}
		if req.NewPassword != req.ConfirmPassword {
			response.Error(w, apperror.Invalid("пароль и подтверждение не совпадают"))
			return
		}

		revoked, err := svc.ChangePassword(r.Context(), user.ID, user.SessionID, req.CurrentPassword, req.NewPassword)
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, map[string]any{"success": true, "revokedSessions": revoked})
	})
}
