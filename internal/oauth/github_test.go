package oauth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"notrecinema/api/internal/auth"
)

func TestStartRejectsWithoutLegalAccepted(t *testing.T) {
	authSvc := auth.NewService(nil, "notre_cinema_session", "development")
	svc := NewService(authSvc, "client-id", "client-secret")

	mux := http.NewServeMux()
	RegisterRoutes(mux, svc)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/github/start", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d (redirect)", rec.Code, http.StatusFound)
	}
	loc := rec.Header().Get("Location")
	if loc == "" {
		t.Fatal("no Location header on redirect")
	}
}

func TestStartRejectsWithoutConfig(t *testing.T) {
	authSvc := auth.NewService(nil, "notre_cinema_session", "development")
	svc := NewService(authSvc, "", "")

	mux := http.NewServeMux()
	RegisterRoutes(mux, svc)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/github/start?legalAccepted=true", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestCallbackRejectsInvalidState(t *testing.T) {
	authSvc := auth.NewService(nil, "notre_cinema_session", "development")
	svc := NewService(authSvc, "client-id", "client-secret")

	mux := http.NewServeMux()
	RegisterRoutes(mux, svc)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/github/callback?code=abc&state=mismatched", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d (redirect)", rec.Code, http.StatusFound)
	}
	loc := rec.Header().Get("Location")
	if loc == "" {
		t.Fatal("no Location header on redirect")
	}
}

func TestStartConfigured(t *testing.T) {
	authSvc := auth.NewService(nil, "notre_cinema_session", "development")
	svc := NewService(authSvc, "client-id", "client-secret")
	if !svc.Configured() {
		t.Error("Configured() = false, want true when both client id/secret set")
	}
}
