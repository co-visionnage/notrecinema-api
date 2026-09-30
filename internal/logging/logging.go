// Package logging настраивает структурированный логгер на весь процесс.
// Каждая строка лога — это JSON в stdout, чтобы её можно было отправлять
// в систему сбора логов и индексировать без парсера, гадающего формат.
package logging

import (
	"log/slog"
	"os"
)

func New(serviceName string) *slog.Logger {
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: levelFromEnv(),
	})
	return slog.New(handler).With("service", serviceName)
}

func levelFromEnv() slog.Level {
	switch os.Getenv("LOG_LEVEL") {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
