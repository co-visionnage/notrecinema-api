package movieprovider

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"time"

	"github.com/sony/gobreaker/v2"

	"notrecinema/api/internal/telemetry"
)

// ResilientConfig настраивает обёртку. Используйте DefaultResilientConfig()
// как отправную точку и переопределяйте нужные поля -- в отличие от
// заполнения нулевых значений дефолтами внутри NewResilient, это не
// путает "явно ноль" (например, MaxRetries: 0 -- ретраев не будет) с
// "не задано".
type ResilientConfig struct {
	// Timeout — максимум на одну попытку запроса к провайдеру.
	Timeout time.Duration
	// MaxRetries — сколько ДОПОЛНИТЕЛЬНЫХ попыток после первой неудачной.
	MaxRetries int
	// BaseBackoff — база экспоненциального backoff между попытками
	// (умножается на 2^attempt, плюс джиттер, чтобы синхронные ретраи
	// множества запросов не били по провайдеру одной волной).
	BaseBackoff time.Duration
	// BreakerFailureThreshold — доля неудачных запросов (0..1) в скользящем
	// окне из BreakerMinRequests запросов, после которой breaker открывается
	// и следующие вызовы отклоняются немедленно, без похода к провайдеру.
	BreakerFailureThreshold float64
	BreakerMinRequests      uint32
	// BreakerOpenTimeout — сколько breaker остаётся открытым, прежде чем
	// пропустить одну пробную попытку (half-open).
	BreakerOpenTimeout time.Duration
}

// DefaultResilientConfig — разумные значения по умолчанию для вызова
// внешнего API вроде TMDB/OMDB/Kinopoisk.
func DefaultResilientConfig() ResilientConfig {
	return ResilientConfig{
		Timeout:                 3 * time.Second,
		MaxRetries:              2,
		BaseBackoff:             200 * time.Millisecond,
		BreakerFailureThreshold: 0.5,
		BreakerMinRequests:      5,
		BreakerOpenTimeout:      30 * time.Second,
	}
}

// Resilient оборачивает один Provider таймаутом, ретраями с backoff и
// circuit breaker. Сам по себе Provider-агностик -- ничего не знает про
// TMDB/OMDB/Kinopoisk, только про интерфейс Provider.
type Resilient struct {
	inner   Provider
	cfg     ResilientConfig
	breaker *gobreaker.CircuitBreaker[Movie]
	metrics *telemetry.Metrics
}

// metrics может быть nil -- тогда вызовы просто не инструментируются
// (используется в тестах этого пакета, где Prometheus-реестр не нужен).
func NewResilient(inner Provider, cfg ResilientConfig, metrics *telemetry.Metrics) *Resilient {
	breaker := gobreaker.NewCircuitBreaker[Movie](gobreaker.Settings{
		Name: inner.Name(),
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			if counts.Requests < cfg.BreakerMinRequests {
				return false
			}
			failureRatio := float64(counts.TotalFailures) / float64(counts.Requests)
			return failureRatio >= cfg.BreakerFailureThreshold
		},
		Timeout: cfg.BreakerOpenTimeout,
	})

	return &Resilient{inner: inner, cfg: cfg, breaker: breaker, metrics: metrics}
}

func (r *Resilient) Name() string {
	return r.inner.Name()
}

// GetMovie — вызов через circuit breaker; внутри breaker'а живут ретраи с
// таймаутом на каждую попытку. Порядок именно такой (breaker снаружи,
// ретраи внутри): breaker должен видеть каждый ВЫЗОВ GetMovie как одну
// единицу успеха/неудачи, а не каждую отдельную попытку ретрая --
// иначе 3 ретрая одного вызова считались бы breaker'у как 3 независимых
// отказа и открывали бы его втрое быстрее, чем реальная частота отказов.
func (r *Resilient) GetMovie(ctx context.Context, externalID string) (Movie, error) {
	return r.breaker.Execute(func() (Movie, error) {
		return r.getMovieWithRetry(ctx, externalID)
	})
}

func (r *Resilient) getMovieWithRetry(ctx context.Context, externalID string) (Movie, error) {
	var lastErr error

	for attempt := 0; attempt <= r.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			if err := sleepWithContext(ctx, backoffDuration(r.cfg.BaseBackoff, attempt)); err != nil {
				return Movie{}, err
			}
		}

		attemptStart := time.Now()
		attemptCtx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
		movie, err := r.inner.GetMovie(attemptCtx, externalID)
		cancel()

		if r.metrics != nil {
			outcome := "success"
			if err != nil {
				outcome = "failure"
			}
			r.metrics.ExternalAPIRequests.WithLabelValues(r.inner.Name(), outcome).Inc()
			r.metrics.ExternalAPIDuration.WithLabelValues(r.inner.Name()).Observe(time.Since(attemptStart).Seconds())
		}

		if err == nil {
			return movie, nil
		}
		lastErr = err

		// Отмену вызывающим кодом (не наш таймаут, а именно родительский
		// ctx) ретраить бессмысленно -- вызывающему уже не нужен ответ.
		if errors.Is(ctx.Err(), context.Canceled) {
			return Movie{}, lastErr
		}
	}

	return Movie{}, fmt.Errorf("movieprovider: %s: все попытки исчерпаны: %w", r.inner.Name(), lastErr)
}

func backoffDuration(base time.Duration, attempt int) time.Duration {
	exponential := float64(base) * math.Pow(2, float64(attempt-1))
	jitter := rand.Float64() * exponential * 0.25
	return time.Duration(exponential + jitter)
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
