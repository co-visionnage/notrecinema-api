package apperror

import (
	"errors"
	"net/http"
	"testing"
)

func TestHTTPStatus(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{Invalid("bad"), http.StatusBadRequest},
		{Unauthorized("no"), http.StatusUnauthorized},
		{Forbidden("no"), http.StatusForbidden},
		{NotFound("no"), http.StatusNotFound},
		{Conflict("no"), http.StatusConflict},
		{Internal("boom", errors.New("cause")), http.StatusInternalServerError},
		{errors.New("plain error"), http.StatusInternalServerError},
	}

	for _, tc := range cases {
		if got := HTTPStatus(tc.err); got != tc.want {
			t.Errorf("HTTPStatus(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}

func TestPublicMessageHidesInternalCause(t *testing.T) {
	err := Internal("failed to load profile", errors.New("connection refused"))

	got := PublicMessage(err)
	if got != "internal server error" {
		t.Errorf("PublicMessage() = %q, want the cause to be hidden", got)
	}
}

func TestPublicMessagePassesThroughDomainMessage(t *testing.T) {
	err := NotFound("family not found")

	if got := PublicMessage(err); got != "family not found" {
		t.Errorf("PublicMessage() = %q, want %q", got, "family not found")
	}
}
