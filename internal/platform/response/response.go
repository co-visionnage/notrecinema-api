// Package response централизует запись JSON-ответов, чтобы все хендлеры
// отдавали ответы и ошибки в одном и том же формате.
package response

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"notrecinema/api/internal/platform/apperror"
)

type errorBody struct {
	Error string `json:"error"`
}

func JSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if payload == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		slog.Error("response: failed to encode JSON body", "error", err)
	}
}

// Error пишет подходящий статус и безопасное сообщение об ошибке для err.
// Вызывающему коду не нужно знать, как ошибка сопоставляется со статусом.
// Внутренние ошибки (5xx) дополнительно логируются здесь -- клиент видит
// только generic "internal server error" (см. apperror.PublicMessage), а
// без серверного лога настоящая причина была бы вообще нигде не видна.
func Error(w http.ResponseWriter, err error) {
	status := apperror.HTTPStatus(err)
	if status >= http.StatusInternalServerError {
		slog.Error("internal error", "error", err)
	}
	JSON(w, status, errorBody{Error: apperror.PublicMessage(err)})
}
