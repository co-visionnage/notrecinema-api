package auth

import (
	"encoding/json"
	"net/http"

	"notrecinema/api/internal/platform/apperror"
	"notrecinema/api/internal/platform/response"
)

// RegisterProtectedRoutes монтирует роуты 2FA, которым уже нужна активная
// сессия (setup/confirm/disable/status) -- в отличие от RegisterRoutes
// (login/register/logout/verify-2fa), эти рассчитаны на protectedMux, за
// auth.Service.Middleware.
func RegisterProtectedRoutes(mux *http.ServeMux, svc *Service) {
	mux.HandleFunc("POST /api/v1/auth/2fa/setup", func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())

		setup, err := svc.StartTwoFactorSetup(r.Context(), user.ID, user.Email)
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, setup)
	})

	mux.HandleFunc("POST /api/v1/auth/2fa/confirm", func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())

		var req struct {
			Code string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			response.Error(w, apperror.Invalid("invalid request body"))
			return
		}
		if !svc.allowTwoFactorManagement(w, r, user.ID) {
			return
		}

		codes, err := svc.ConfirmTwoFactor(r.Context(), user.ID, req.Code)
		if err != nil {
			response.Error(w, err)
			return
		}
		// Резервные коды показываются ровно один раз -- в этом ответе.
		response.JSON(w, http.StatusOK, map[string]any{"success": true, "backupCodes": codes})
	})

	mux.HandleFunc("POST /api/v1/auth/2fa/backup-codes/regenerate", func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())

		var req struct {
			Code string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			response.Error(w, apperror.Invalid("invalid request body"))
			return
		}
		if !svc.allowTwoFactorManagement(w, r, user.ID) {
			return
		}

		codes, err := svc.RegenerateBackupCodes(r.Context(), user.ID, req.Code)
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, map[string]any{"success": true, "backupCodes": codes})
	})

	mux.HandleFunc("POST /api/v1/auth/2fa/disable", func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())

		var req struct {
			Code string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			response.Error(w, apperror.Invalid("invalid request body"))
			return
		}
		if !svc.allowTwoFactorManagement(w, r, user.ID) {
			return
		}

		if err := svc.DisableTwoFactor(r.Context(), user.ID, req.Code); err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, map[string]bool{"success": true})
	})

	mux.HandleFunc("GET /api/v1/auth/2fa/status", func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())

		enabled, remaining, err := svc.TwoFactorStatus(r.Context(), user.ID)
		if err != nil {
			response.Error(w, apperror.Internal("failed to get two-factor status", err))
			return
		}
		response.JSON(w, http.StatusOK, map[string]any{"enabled": enabled, "backupCodesRemaining": remaining})
	})
}

const (
	twoFactorManageMaxAttempts   = 10
	twoFactorManageWindowSeconds = 15 * 60
)

// allowTwoFactorManagement ограничивает попытки подтвердить/отключить 2FA и
// перевыпустить коды: эти ручки принимают код, значит, их можно
// перебирать, даже имея сессию.
func (s *Service) allowTwoFactorManagement(w http.ResponseWriter, r *http.Request, userID string) bool {
	return s.allow(w, r, "2fa-manage:user:"+userID, twoFactorManageMaxAttempts, twoFactorManageWindowSeconds, "слишком много попыток. Попробуйте позже")
}
