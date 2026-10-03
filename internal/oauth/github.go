// Package oauth реализует вход через GitHub -- прямой перенос
// notrecinema-app (src/app/api/auth/github/start и src/app/auth/callback).
// Привязка к существующему аккаунту идёт исключительно по совпадению
// email (через auth.Service.CreateSessionForEmail -> create_profile_session,
// upsert по email) -- отдельной таблицы/колонки "это GitHub-аккаунт" в
// схеме нет, OAuth-аккаунт просто отличается тем, что у него
// password_hash IS NULL.
package oauth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"notrecinema/api/internal/auth"
	"notrecinema/api/internal/telemetry"
)

const stateCookieName = "notre_cinema_github_oauth_state"

type Service struct {
	authSvc      *auth.Service
	httpClient   *http.Client
	clientID     string
	clientSecret string
}

func NewService(authSvc *auth.Service, clientID, clientSecret string) *Service {
	return &Service{authSvc: authSvc, httpClient: telemetry.InstrumentedClient(), clientID: clientID, clientSecret: clientSecret}
}

func (s *Service) Configured() bool {
	return s.clientID != "" && s.clientSecret != ""
}

// requestOrigin -- scheme+host запроса, тот же смысл, что new URL(request.url).origin
// в оригинале. X-Forwarded-Proto приоритетнее r.TLS: сервис почти всегда
// стоит за reverse-proxy, который терминирует TLS сам (см. ту же логику
// доверия заголовкам проксирования, что и clientIP в internal/auth).
func requestOrigin(r *http.Request) string {
	scheme := "http"
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	} else if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	// За прокси фронтенда (Next rewrites) Host запроса -- адрес API, а
	// браузер ходил на домен фронтенда: callback должен вернуться туда.
	if forwarded := r.Header.Get("X-Forwarded-Host"); forwarded != "" {
		host = forwarded
	}
	return fmt.Sprintf("%s://%s", scheme, host)
}

func redirectWithError(w http.ResponseWriter, r *http.Request, target, message string) {
	u, err := url.Parse(target)
	if err != nil {
		u = &url.URL{Path: "/"}
	}
	q := u.Query()
	q.Set("authError", message)
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// RegisterRoutes монтирует GET /api/v1/auth/github/start и
// GET /api/v1/auth/github/callback на publicMux -- ни один из двух не
// требует уже существующей сессии.
func RegisterRoutes(mux *http.ServeMux, svc *Service) {
	mux.HandleFunc("GET /api/v1/auth/github/start", func(w http.ResponseWriter, r *http.Request) {
		if !svc.Configured() {
			http.Error(w, `{"error":"GITHUB_CLIENT_ID is missing"}`, http.StatusInternalServerError)
			return
		}

		origin := requestOrigin(r)

		if r.URL.Query().Get("legalAccepted") != "true" {
			redirectWithError(w, r, origin+"/", "Нужно принять правовые документы")
			return
		}

		stateBytes := make([]byte, 24)
		if _, err := rand.Read(stateBytes); err != nil {
			http.Error(w, `{"error":"failed to generate state"}`, http.StatusInternalServerError)
			return
		}
		state := hex.EncodeToString(stateBytes)

		http.SetCookie(w, &http.Cookie{
			Name:     stateCookieName,
			Value:    state,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Secure:   svc.authSvc.IsProduction(),
			Path:     "/",
			MaxAge:   600,
		})

		authorizeURL := url.URL{Scheme: "https", Host: "github.com", Path: "/login/oauth/authorize"}
		q := authorizeURL.Query()
		q.Set("client_id", svc.clientID)
		q.Set("redirect_uri", origin+"/api/v1/auth/github/callback")
		q.Set("scope", "read:user user:email")
		q.Set("state", state)
		authorizeURL.RawQuery = q.Encode()

		http.Redirect(w, r, authorizeURL.String(), http.StatusFound)
	})

	mux.HandleFunc("GET /api/v1/auth/github/callback", func(w http.ResponseWriter, r *http.Request) {
		origin := requestOrigin(r)
		next := r.URL.Query().Get("next")
		if next == "" {
			next = "/"
		}
		redirectTarget := origin + next

		code := r.URL.Query().Get("code")
		state := r.URL.Query().Get("state")

		storedState := ""
		if cookie, err := r.Cookie(stateCookieName); err == nil {
			storedState = cookie.Value
		}
		http.SetCookie(w, &http.Cookie{
			Name: stateCookieName, Value: "", Path: "/", MaxAge: -1,
		})

		if code == "" || state == "" || storedState == "" || state != storedState {
			redirectWithError(w, r, redirectTarget, "GitHub OAuth state is invalid")
			return
		}
		if !svc.Configured() {
			redirectWithError(w, r, redirectTarget, "GitHub OAuth is not configured")
			return
		}

		session, err := svc.completeLogin(auth.WithRequest(r.Context(), r), code, origin)
		if err != nil {
			telemetry.RecordAuth("oauth_login", "failure")
		} else {
			telemetry.RecordAuth("oauth_login", "success")
		}
		if err != nil {
			redirectWithError(w, r, redirectTarget, err.Error())
			return
		}

		svc.authSvc.SetSessionCookie(w, session.Token, session.ExpiresAt)
		http.Redirect(w, r, redirectTarget, http.StatusFound)
	})
}

type githubTokenResponse struct {
	AccessToken      string `json:"access_token"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

type githubUserResponse struct {
	Login string `json:"login"`
	Name  string `json:"name"`
}

type githubEmailResponse struct {
	Email    string `json:"email"`
	Primary  bool   `json:"primary"`
	Verified bool   `json:"verified"`
}

func (s *Service) completeLogin(ctx context.Context, code, origin string) (auth.Session, error) {
	accessToken, err := s.exchangeCode(ctx, code, origin)
	if err != nil {
		return auth.Session{}, err
	}

	githubUser, err := s.fetchUser(ctx, accessToken)
	if err != nil {
		return auth.Session{}, err
	}

	email := s.resolveVerifiedEmail(ctx, accessToken, githubUser.Login)
	displayName := githubUser.Name
	if displayName == "" {
		displayName = githubUser.Login
	}

	return s.authSvc.CreateSessionForEmail(ctx, email, displayName)
}

func (s *Service) exchangeCode(ctx context.Context, code, origin string) (string, error) {
	body, err := json.Marshal(map[string]string{
		"client_id":     s.clientID,
		"client_secret": s.clientSecret,
		"code":          code,
		"redirect_uri":  origin + "/api/v1/auth/github/callback",
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://github.com/login/oauth/access_token", strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("GitHub authentication failed")
	}
	defer func() { _ = resp.Body.Close() }()

	var payload githubTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("GitHub authentication failed")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || payload.AccessToken == "" {
		if payload.ErrorDescription != "" {
			return "", fmt.Errorf("%s", payload.ErrorDescription)
		}
		if payload.Error != "" {
			return "", fmt.Errorf("%s", payload.Error)
		}
		return "", fmt.Errorf("GitHub token exchange failed")
	}
	return payload.AccessToken, nil
}

func (s *Service) fetchUser(ctx context.Context, accessToken string) (githubUserResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user", nil)
	if err != nil {
		return githubUserResponse{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", "notre-cinema")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return githubUserResponse{}, fmt.Errorf("failed to load GitHub profile")
	}
	defer func() { _ = resp.Body.Close() }()

	var user githubUserResponse
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil || resp.StatusCode != http.StatusOK || user.Login == "" {
		return githubUserResponse{}, fmt.Errorf("failed to load GitHub profile")
	}
	return user, nil
}

// resolveVerifiedEmail — прямой перенос логики выбора email в callback
// notrecinema-app: никогда не доверяет полю email из /user (там может
// лежать непроверенный публичный адрес), смотрит только на /user/emails
// verified=true. Без подтверждённого адреса -- синтетический
// {login}@users.noreply.github.com, тот же, что использует сам GitHub.
func (s *Service) resolveVerifiedEmail(ctx context.Context, accessToken, login string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user/emails", nil)
	if err != nil {
		return noreplyEmail(login)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", "notre-cinema")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return noreplyEmail(login)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return noreplyEmail(login)
	}

	var emails []githubEmailResponse
	if err := json.NewDecoder(resp.Body).Decode(&emails); err != nil {
		return noreplyEmail(login)
	}

	var verifiedFallback string
	for _, e := range emails {
		if e.Primary && e.Verified {
			return e.Email
		}
		if e.Verified && verifiedFallback == "" {
			verifiedFallback = e.Email
		}
	}
	if verifiedFallback != "" {
		return verifiedFallback
	}
	return noreplyEmail(login)
}

func noreplyEmail(login string) string {
	return strings.ToLower(login) + "@users.noreply.github.com"
}
