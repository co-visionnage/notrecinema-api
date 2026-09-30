package config

import (
	"os"
	"testing"
	"time"
)

func TestLoadRequiresDatabaseURL(t *testing.T) {
	_ = os.Unsetenv("DATABASE_URL")

	if _, err := Load(); err == nil {
		t.Error("Load() без DATABASE_URL должен вернуть ошибку")
	}
}

func TestLoadAppliesDefaultsAndParsesValues(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgresql://user:pass@localhost:5432/db")
	t.Setenv("ALLOWED_ORIGINS", "http://localhost:3000,http://localhost:3001")
	_ = os.Unsetenv("PORT")
	_ = os.Unsetenv("SHUTDOWN_TIMEOUT")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Port != "8080" {
		t.Errorf("Port = %q, want default %q", cfg.Port, "8080")
	}
	if cfg.ShutdownTimeout != 15*time.Second {
		t.Errorf("ShutdownTimeout = %v, want default 15s", cfg.ShutdownTimeout)
	}
	if len(cfg.AllowedOrigins) != 2 || cfg.AllowedOrigins[0] != "http://localhost:3000" {
		t.Errorf("AllowedOrigins = %v, want two parsed origins", cfg.AllowedOrigins)
	}
}
