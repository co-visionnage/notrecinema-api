//go:build integration

package auth_test

import (
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"notrecinema/api/internal/auth"
)

func TestIPRateLimitIsConfigurable(t *testing.T) {
	db := connectOrSkip(t)
	svc := auth.NewService(db, "notre_cinema_session", "development", auth.WithIPRateLimit(2))
	mux := http.NewServeMux()
	auth.RegisterRoutes(mux, svc)

	// У каждого прогона свой адрес: счётчик живёт в общей БД.
	buf := make([]byte, 2)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand: %v", err)
	}
	ip := fmt.Sprintf("198.51.%d.%d", buf[0], buf[1])

	statuses := make([]int, 3)
	for i := range statuses {
		body := fmt.Sprintf(`{"mode":"login","email":%q,"password":"wrong-password-1!"}`, randomEmail(t, "limit"))
		request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Real-Ip", ip)
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, request)
		statuses[i] = recorder.Code
	}

	if statuses[0] == http.StatusTooManyRequests || statuses[1] == http.StatusTooManyRequests {
		t.Errorf("first two attempts must not be limited, got %v", statuses)
	}
	if statuses[2] != http.StatusTooManyRequests {
		t.Errorf("third attempt = %d, want 429 with a limit of 2 (all: %v)", statuses[2], statuses)
	}
}
