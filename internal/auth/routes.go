package auth

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"notrecinema/api/internal/platform/apperror"
	"notrecinema/api/internal/platform/response"
)

// Правила и лимиты ниже -- прямой перенос notrecinema-app
// (src/app/api/auth/login/route.ts, verify-2fa/route.ts): те же пороги,
// те же окна, тот же порядок проверок.
var (
	emailPattern = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)
	// \p{L} — любая буква любого алфавита (оригинал проверял только
	// латиницу и кириллицу явными диапазонами; \p{L} строго шире и не
	// меняет исход ни для одного пароля, который проходил там).
	hasLetter  = regexp.MustCompile(`\p{L}`)
	hasDigit   = regexp.MustCompile(`\d`)
	hasSpecial = regexp.MustCompile(`[^\p{L}\d]`)
)

const (
	ipRateLimitMaxAttempts    = 30
	ipRateLimitWindowSeconds  = 15 * 60
	emailRateLimitMaxAttempts = 5
	emailRateLimitWindowSecs  = 15 * 60

	twoFAIPMaxAttempts      = 30
	twoFAIPWindowSeconds    = 15 * 60
	twoFATokenMaxAttempts   = 5
	twoFATokenWindowSeconds = 15 * 60
)

type loginRequest struct {
	Mode            string `json:"mode"`
	Email           string `json:"email"`
	DisplayName     string `json:"displayName"`
	Password        string `json:"password"`
	ConfirmPassword string `json:"confirmPassword"`
	LegalAccepted   bool   `json:"legalAccepted"`
}

type loginResponse struct {
	Success           bool   `json:"success"`
	RequiresTwoFactor bool   `json:"requiresTwoFactor,omitempty"`
	ChallengeToken    string `json:"challengeToken,omitempty"`
}

func writeAuthError(w http.ResponseWriter, status int, message string) {
	response.JSON(w, status, map[string]string{"error": message})
}

// RegisterRoutes монтирует публичные auth-роуты (логин, регистрация,
// логаут, 2FA-проверка при входе) -- в отличие от остальных доменов, эти
// не требуют существующей сессии, поэтому монтируются на publicMux, не
// protectedMux (см. cmd/api/main.go). RegisterProtectedRoutes -- для
// роутов, которым сессия уже нужна (2FA setup/confirm/disable/status).
func RegisterRoutes(mux *http.ServeMux, svc *Service) {
	mux.HandleFunc("POST /api/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		var req loginRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeAuthError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.Mode == "" {
			req.Mode = "login"
		}
		email := strings.ToLower(strings.TrimSpace(req.Email))

		allowed, err := svc.checkRateLimit(r.Context(), "auth:ip:"+clientIP(r), ipRateLimitMaxAttempts, ipRateLimitWindowSeconds)
		if err != nil {
			writeAuthError(w, http.StatusInternalServerError, "internal server error")
			return
		}
		if !allowed {
			writeAuthError(w, http.StatusTooManyRequests, "слишком много попыток. Попробуйте позже")
			return
		}

		if !req.LegalAccepted {
			writeAuthError(w, http.StatusBadRequest, "нужно принять пользовательское соглашение и правовые документы")
			return
		}
		if email == "" || !emailPattern.MatchString(email) {
			writeAuthError(w, http.StatusBadRequest, "введите корректный email-адрес")
			return
		}
		if len(req.Password) < 8 {
			writeAuthError(w, http.StatusBadRequest, "пароль должен быть не короче 8 символов")
			return
		}
		if !hasLetter.MatchString(req.Password) || !hasDigit.MatchString(req.Password) || !hasSpecial.MatchString(req.Password) {
			writeAuthError(w, http.StatusBadRequest, "пароль должен содержать буквы, цифры и хотя бы один спецсимвол")
			return
		}

		switch req.Mode {
		case "register":
			allowed, err := svc.checkRateLimit(r.Context(), "auth:register-email:"+email, emailRateLimitMaxAttempts, emailRateLimitWindowSecs)
			if err != nil {
				writeAuthError(w, http.StatusInternalServerError, "internal server error")
				return
			}
			if !allowed {
				writeAuthError(w, http.StatusTooManyRequests, "слишком много попыток регистрации для этого email. Попробуйте позже")
				return
			}

			displayName := strings.TrimSpace(req.DisplayName)
			if len([]rune(displayName)) < 2 {
				writeAuthError(w, http.StatusBadRequest, "укажите имя не короче 2 символов")
				return
			}
			if req.Password != req.ConfirmPassword {
				writeAuthError(w, http.StatusBadRequest, "пароль и подтверждение не совпадают")
				return
			}

			session, err := svc.Register(r.Context(), email, displayName, req.Password)
			if err != nil {
				writeAuthError(w, apperror.HTTPStatus(err), apperror.PublicMessage(err))
				return
			}
			svc.SetSessionCookie(w, session.Token, session.ExpiresAt)
			response.JSON(w, http.StatusOK, loginResponse{Success: true})

		default:
			allowed, err := svc.checkRateLimit(r.Context(), "auth:login-email:"+email, emailRateLimitMaxAttempts, emailRateLimitWindowSecs)
			if err != nil {
				writeAuthError(w, http.StatusInternalServerError, "internal server error")
				return
			}
			if !allowed {
				writeAuthError(w, http.StatusTooManyRequests, "слишком много попыток входа для этого email. Попробуйте позже")
				return
			}

			result, err := svc.Login(r.Context(), email, req.Password)
			if err != nil {
				writeAuthError(w, apperror.HTTPStatus(err), apperror.PublicMessage(err))
				return
			}
			if result.RequiresTwoFactor {
				response.JSON(w, http.StatusOK, loginResponse{Success: true, RequiresTwoFactor: true, ChallengeToken: result.ChallengeToken})
				return
			}
			svc.SetSessionCookie(w, result.Session.Token, result.Session.ExpiresAt)
			response.JSON(w, http.StatusOK, loginResponse{Success: true})
		}
	})

	mux.HandleFunc("POST /api/v1/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		cookie, _ := r.Cookie(svc.CookieName())
		if cookie != nil {
			_ = svc.Logout(r.Context(), cookie.Value)
		}
		svc.ClearSessionCookie(w)
		response.JSON(w, http.StatusOK, map[string]bool{"success": true})
	})

	mux.HandleFunc("POST /api/v1/auth/verify-2fa", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ChallengeToken string `json:"challengeToken"`
			Code           string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeAuthError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		req.ChallengeToken = strings.TrimSpace(req.ChallengeToken)
		req.Code = strings.TrimSpace(req.Code)
		if req.ChallengeToken == "" || req.Code == "" {
			writeAuthError(w, http.StatusBadRequest, "challengeToken и code обязательны")
			return
		}

		allowed, err := svc.checkRateLimit(r.Context(), "2fa:ip:"+clientIP(r), twoFAIPMaxAttempts, twoFAIPWindowSeconds)
		if err != nil {
			writeAuthError(w, http.StatusInternalServerError, "internal server error")
			return
		}
		if !allowed {
			writeAuthError(w, http.StatusTooManyRequests, "слишком много попыток. Попробуйте позже")
			return
		}

		// Бакет по хэшу токена, а не по аккаунту -- ограничивает брутфорс
		// TOTP-кода в рамках одной попытки входа, а не самого аккаунта.
		allowed, err = svc.checkRateLimit(r.Context(), "2fa:token:"+hashToken(req.ChallengeToken), twoFATokenMaxAttempts, twoFATokenWindowSeconds)
		if err != nil {
			writeAuthError(w, http.StatusInternalServerError, "internal server error")
			return
		}
		if !allowed {
			writeAuthError(w, http.StatusTooManyRequests, "слишком много попыток. Войдите заново")
			return
		}

		session, err := svc.VerifyTwoFactor(r.Context(), req.ChallengeToken, req.Code)
		if err != nil {
			writeAuthError(w, apperror.HTTPStatus(err), apperror.PublicMessage(err))
			return
		}
		svc.SetSessionCookie(w, session.Token, session.ExpiresAt)
		response.JSON(w, http.StatusOK, loginResponse{Success: true})
	})
}
