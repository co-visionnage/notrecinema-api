package movieprovider

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// chaosProvider — специально нестабильный Provider для тестов: имитирует
// реальные сбои внешнего API (медленный ответ, случайная ошибка), про
// которые и написан этот пакет. failUntilAttempt -- сколько первых вызовов
// подряд должны провалиться, прежде чем провайдер начнёт отвечать успешно.
type chaosProvider struct {
	calls            atomic.Int32
	failUntilAttempt int32
	delay            time.Duration
	alwaysFail       bool
}

func (c *chaosProvider) Name() string { return "chaos" }

func (c *chaosProvider) GetMovie(ctx context.Context, externalID string) (Movie, error) {
	n := c.calls.Add(1)

	if c.delay > 0 {
		select {
		case <-time.After(c.delay):
		case <-ctx.Done():
			return Movie{}, ctx.Err()
		}
	}

	if c.alwaysFail || n <= c.failUntilAttempt {
		return Movie{}, errors.New("chaos: искусственный сбой провайдера")
	}
	return Movie{ID: externalID, Title: "Успех", Source: "chaos"}, nil
}

func fastTestConfig() ResilientConfig {
	return ResilientConfig{
		Timeout:                 50 * time.Millisecond,
		MaxRetries:              3,
		BaseBackoff:             1 * time.Millisecond,
		BreakerFailureThreshold: 0.5,
		BreakerMinRequests:      3,
		BreakerOpenTimeout:      50 * time.Millisecond,
	}
}

func TestResilientRetriesUntilSuccess(t *testing.T) {
	chaos := &chaosProvider{failUntilAttempt: 2}
	r := NewResilient(chaos, fastTestConfig(), nil)

	movie, err := r.GetMovie(context.Background(), "123")
	if err != nil {
		t.Fatalf("GetMovie() error = %v, want success after retries", err)
	}
	if movie.Title != "Успех" {
		t.Errorf("GetMovie() = %+v, want успешный результат", movie)
	}
	if chaos.calls.Load() != 3 {
		t.Errorf("provider called %d раз(а), want 3 (2 неудачных + 1 успешная)", chaos.calls.Load())
	}
}

func TestResilientTimeoutCancelsSlowCall(t *testing.T) {
	chaos := &chaosProvider{delay: 200 * time.Millisecond}
	cfg := fastTestConfig()
	cfg.MaxRetries = 0
	r := NewResilient(chaos, cfg, nil)

	start := time.Now()
	_, err := r.GetMovie(context.Background(), "123")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("GetMovie() succeeded, want timeout error")
	}
	if elapsed > 150*time.Millisecond {
		t.Errorf("GetMovie() заняло %v, ожидался обрыв по таймауту ~%v", elapsed, cfg.Timeout)
	}
}

func TestResilientCircuitBreakerOpensAfterFailures(t *testing.T) {
	chaos := &chaosProvider{alwaysFail: true}
	cfg := fastTestConfig()
	cfg.MaxRetries = 0
	r := NewResilient(chaos, cfg, nil)

	// Достаточно вызовов, чтобы набрать BreakerMinRequests неудач и
	// открыть breaker.
	for i := 0; i < int(cfg.BreakerMinRequests); i++ {
		_, _ = r.GetMovie(context.Background(), "123")
	}

	callsBeforeOpen := chaos.calls.Load()

	// Ещё один вызов: если breaker открыт, до провайдера он не дойдёт.
	_, err := r.GetMovie(context.Background(), "123")
	if err == nil {
		t.Fatal("GetMovie() succeeded, want circuit breaker open error")
	}
	if chaos.calls.Load() != callsBeforeOpen {
		t.Errorf("provider был вызван ещё раз при открытом breaker'е: %d -> %d",
			callsBeforeOpen, chaos.calls.Load())
	}
}

func TestFallbackUsesSecondProviderWhenFirstFails(t *testing.T) {
	failing := &chaosProvider{alwaysFail: true}
	working := &chaosProvider{}

	fb := NewFallback(failing, working)

	movie, err := fb.GetMovie(context.Background(), "123")
	if err != nil {
		t.Fatalf("GetMovie() error = %v, want fallback to succeed", err)
	}
	if movie.Title != "Успех" {
		t.Errorf("GetMovie() = %+v, want результат от второго провайдера", movie)
	}
	if failing.calls.Load() != 1 || working.calls.Load() != 1 {
		t.Errorf("ожидался ровно один вызов каждого провайдера, получили failing=%d working=%d",
			failing.calls.Load(), working.calls.Load())
	}
}

func TestFallbackFailsWhenAllProvidersFail(t *testing.T) {
	fb := NewFallback(&chaosProvider{alwaysFail: true}, &chaosProvider{alwaysFail: true})

	if _, err := fb.GetMovie(context.Background(), "123"); err == nil {
		t.Fatal("GetMovie() succeeded, want error when every provider fails")
	}
}
