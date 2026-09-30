// Package apperror определяет небольшой набор видов ошибок, которые каждый
// хендлер преобразует в HTTP-статус. Доменные пакеты возвращают их вместо
// «сырых» ошибок, чтобы HTTP-слою не приходилось угадывать смысл ошибки
// сервисного уровня.
package apperror

import (
	"errors"
	"fmt"
	"net/http"
)

type Kind string

const (
	KindInvalidInput Kind = "invalid_input"
	KindUnauthorized Kind = "unauthorized"
	KindForbidden    Kind = "forbidden"
	KindNotFound     Kind = "not_found"
	KindConflict     Kind = "conflict"
	KindRateLimited  Kind = "rate_limited"
	KindInternal     Kind = "internal"
)

type Error struct {
	Kind    Kind
	Message string
	cause   error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.cause)
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.cause }

func New(kind Kind, message string) *Error {
	return &Error{Kind: kind, Message: message}
}

func Wrap(kind Kind, message string, cause error) *Error {
	return &Error{Kind: kind, Message: message, cause: cause}
}

func Invalid(message string) *Error      { return New(KindInvalidInput, message) }
func Unauthorized(message string) *Error { return New(KindUnauthorized, message) }
func Forbidden(message string) *Error    { return New(KindForbidden, message) }
func NotFound(message string) *Error     { return New(KindNotFound, message) }
func Conflict(message string) *Error     { return New(KindConflict, message) }
func RateLimited(message string) *Error  { return New(KindRateLimited, message) }
func Internal(message string, cause error) *Error {
	return Wrap(KindInternal, message, cause)
}

// HTTPStatus сопоставляет ошибку с HTTP-статусом. Любая ошибка, которая не
// является *Error (неожиданный случай после recover, баг), считается
// внутренней.
func HTTPStatus(err error) int {
	var appErr *Error
	if errors.As(err, &appErr) {
		switch appErr.Kind {
		case KindInvalidInput:
			return http.StatusBadRequest
		case KindUnauthorized:
			return http.StatusUnauthorized
		case KindForbidden:
			return http.StatusForbidden
		case KindNotFound:
			return http.StatusNotFound
		case KindConflict:
			return http.StatusConflict
		case KindRateLimited:
			return http.StatusTooManyRequests
		default:
			return http.StatusInternalServerError
		}
	}
	return http.StatusInternalServerError
}

// PublicMessage — сообщение, которое безопасно отдать клиенту: внутренние
// ошибки никогда не раскрывают свою причину, остальные отдаются как есть.
func PublicMessage(err error) string {
	var appErr *Error
	if errors.As(err, &appErr) {
		if appErr.Kind == KindInternal {
			return "internal server error"
		}
		return appErr.Message
	}
	return "internal server error"
}
