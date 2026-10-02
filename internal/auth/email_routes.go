package auth

import (
	"encoding/json"
	"net/http"
	"strings"

	"notrecinema/api/internal/platform/apperror"
	"notrecinema/api/internal/platform/response"
)

const (
	verifyIPMaxAttempts   = 30
	verifyIPWindowSeconds = 15 * 60

	resendUserMaxAttempts   = 3
	resendUserWindowSeconds = 60 * 60

	resetRequestIPMaxAttempts     = 10
	resetRequestIPWindowSeconds   = 15 * 60
	resetRequestEmailMaxAttempts  = 3
	resetRequestEmailWindowSecond = 60 * 60

	resetConfirmIPMaxAttempts     = 10
	resetConfirmIPWindowSeconds   = 15 * 60
	resetConfirmTokenMaxAttempts  = 5
	resetConfirmTokenWindowSecond = 15 * 60
)

// RegisterEmailRoutes монтирует публичные роуты подтверждения email и
// сброса пароля: по ссылке из письма сессии у человека ещё (или уже) нет.
// Фронтенд открывает свои страницы /verify-email?token=... и
// /reset-password?token=..., а те дергают эти эндпоинты.
func RegisterEmailRoutes(mux *http.ServeMux, svc *Service) {
	mux.HandleFunc("POST /api/v1/auth/verify-email", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeAuthError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		req.Token = strings.TrimSpace(req.Token)
		if req.Token == "" {
			writeAuthError(w, http.StatusBadRequest, "token обязателен")
			return
		}

		if !svc.allow(w, r, "verify-email:ip:"+clientIP(r), verifyIPMaxAttempts, verifyIPWindowSeconds, "слишком много попыток. Попробуйте позже") {
			return
		}

		if err := svc.VerifyEmail(r.Context(), req.Token); err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, map[string]bool{"success": true})
	})

	mux.HandleFunc("POST /api/v1/auth/password-reset/request", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Email string `json:"email"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeAuthError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		email := strings.ToLower(strings.TrimSpace(req.Email))
		if email == "" || !emailPattern.MatchString(email) {
			writeAuthError(w, http.StatusBadRequest, "введите корректный email-адрес")
			return
		}

		if !svc.allow(w, r, "pwreset-request:ip:"+clientIP(r), resetRequestIPMaxAttempts, resetRequestIPWindowSeconds, "слишком много попыток. Попробуйте позже") {
			return
		}
		if !svc.allow(w, r, "pwreset-request:email:"+email, resetRequestEmailMaxAttempts, resetRequestEmailWindowSecond, "слишком много запросов для этого email. Попробуйте позже") {
			return
		}

		if err := svc.RequestPasswordReset(r.Context(), email); err != nil {
			response.Error(w, err)
			return
		}
		// Ответ одинаков для существующего и несуществующего аккаунта.
		response.JSON(w, http.StatusOK, map[string]bool{"success": true})
	})

	mux.HandleFunc("POST /api/v1/auth/password-reset/confirm", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Token           string `json:"token"`
			NewPassword     string `json:"newPassword"`
			ConfirmPassword string `json:"confirmPassword"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeAuthError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		req.Token = strings.TrimSpace(req.Token)
		if req.Token == "" {
			writeAuthError(w, http.StatusBadRequest, "token обязателен")
			return
		}

		if !svc.allow(w, r, "pwreset-confirm:ip:"+clientIP(r), resetConfirmIPMaxAttempts, resetConfirmIPWindowSeconds, "слишком много попыток. Попробуйте позже") {
			return
		}
		if !svc.allow(w, r, "pwreset-confirm:token:"+hashToken(req.Token), resetConfirmTokenMaxAttempts, resetConfirmTokenWindowSecond, "слишком много попыток. Запросите новую ссылку") {
			return
		}

		if problem := passwordProblem(req.NewPassword); problem != "" {
			writeAuthError(w, http.StatusBadRequest, problem)
			return
		}
		if req.NewPassword != req.ConfirmPassword {
			writeAuthError(w, http.StatusBadRequest, "пароль и подтверждение не совпадают")
			return
		}

		if err := svc.ResetPassword(r.Context(), req.Token, req.NewPassword); err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, map[string]bool{"success": true})
	})
}

// RegisterProtectedEmailRoutes -- повторная отправка письма с
// подтверждением (нужна активная сессия).
func RegisterProtectedEmailRoutes(mux *http.ServeMux, svc *Service) {
	mux.HandleFunc("POST /api/v1/auth/verify-email/resend", func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())

		if !svc.allow(w, r, "verify-email-resend:user:"+user.ID, resendUserMaxAttempts, resendUserWindowSeconds, "письмо уже отправлялось недавно. Попробуйте позже") {
			return
		}

		if err := svc.RequestEmailVerification(r.Context(), user.ID); err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, map[string]bool{"success": true})
	})
}

// allow проверяет rate-limit и сам пишет ответ 429/500, если запрос нужно
// остановить. Возвращает true, если можно продолжать.
func (s *Service) allow(w http.ResponseWriter, r *http.Request, bucketKey string, maxAttempts, windowSeconds int, tooManyMessage string) bool {
	allowed, err := s.checkRateLimit(r.Context(), bucketKey, maxAttempts, windowSeconds)
	if err != nil {
		response.Error(w, apperror.Internal("rate limit check failed", err))
		return false
	}
	if !allowed {
		writeAuthError(w, http.StatusTooManyRequests, tooManyMessage)
		return false
	}
	return true
}
