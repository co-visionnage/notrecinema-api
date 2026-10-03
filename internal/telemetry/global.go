package telemetry

import (
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Метрики, которые пишутся из самых разных пакетов (rate limit, события
// аутентификации, исходящие HTTP-запросы, фоновые задачи, транзакции БД).
// Протаскивать структуру Metrics в каждый конструктор ради одного Inc()
// значило бы изменить десяток сигнатур, поэтому это обычные пакетные
// переменные на реестре по умолчанию, как у самой библиотеки Prometheus.
// Каждая -- в единственном экземпляре на процесс, двойной регистрации нет.
var (
	rateLimitChecks = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "rate_limit_checks_total",
		Help: "Проверки ограничения частоты по группе лимита и результату (allowed/rejected).",
	}, []string{"bucket", "result"})

	authEvents = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "auth_events_total",
		Help: "События аутентификации (вход, регистрация, 2FA, сброс пароля, сессии) по виду и результату.",
	}, []string{"event", "result"})

	outboundRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "outbound_http_requests_total",
		Help: "Исходящие HTTP-запросы к внешним сервисам по хосту и исходу (success/client_error/server_error/error).",
	}, []string{"host", "outcome"})

	outboundDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "outbound_http_request_duration_seconds",
		Help:    "Длительность исходящего HTTP-запроса к внешнему сервису.",
		Buckets: prometheus.DefBuckets,
	}, []string{"host"})

	panicsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "http_panics_total",
		Help: "Паники в обработчиках запросов, перехваченные Recover.",
	})

	jobRuns = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "background_job_runs_total",
		Help: "Запуски фоновых задач по имени и результату.",
	}, []string{"job", "result"})

	jobDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "background_job_duration_seconds",
		Help:    "Длительность одного запуска фоновой задачи.",
		Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1, 5, 15, 60, 300},
	}, []string{"job"})

	jobLastSuccess = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "background_job_last_success_timestamp_seconds",
		Help: "Когда фоновая задача в последний раз завершилась успешно (по этому значению строится алерт «задача давно не работала»).",
	}, []string{"job"})

	dbTransactions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "db_transactions_total",
		Help: "Транзакции под пользовательским контекстом по исходу (commit/rollback).",
	}, []string{"result"})

	dbTransactionDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "db_transaction_duration_seconds",
		Help:    "Длительность транзакции под пользовательским контекстом, включая работу внутри неё.",
		Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	})

	buildInfo = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "build_info",
		Help: "Версия и сборка сервиса (значение всегда 1, данные в метках).",
	}, []string{"service", "version", "go_version"})
)

// SetBuildInfo публикует версию сервиса.
func SetBuildInfo(service, version string) {
	buildInfo.WithLabelValues(service, version, runtime.Version()).Set(1)
}

// bucketLabel оставляет от ключа лимита только фиксированную часть
// ("auth:ip:203.0.113.7" -> "auth:ip"): ключи содержат адреса, почты и id,
// которые нельзя класть в метку.
func bucketLabel(bucketKey string) string {
	parts := strings.SplitN(bucketKey, ":", 3)
	if len(parts) >= 3 {
		return parts[0] + ":" + parts[1]
	}
	return parts[0]
}

// RecordRateLimit учитывает одну проверку ограничения частоты.
func RecordRateLimit(bucketKey string, allowed bool) {
	result := "allowed"
	if !allowed {
		result = "rejected"
	}
	rateLimitChecks.WithLabelValues(bucketLabel(bucketKey), result).Inc()
}

// RecordAuth учитывает событие аутентификации. Значения event и result --
// короткий фиксированный набор из кода (никаких почт и id).
func RecordAuth(event, result string) {
	authEvents.WithLabelValues(event, result).Inc()
}

// RecordPanic учитывает перехваченную панику обработчика.
func RecordPanic() { panicsTotal.Inc() }

// RecordJob учитывает один запуск фоновой задачи.
func RecordJob(job string, started time.Time, err error) {
	result := "success"
	if err != nil {
		result = "failure"
	}
	jobRuns.WithLabelValues(job, result).Inc()
	jobDuration.WithLabelValues(job).Observe(time.Since(started).Seconds())
	if err == nil {
		jobLastSuccess.WithLabelValues(job).SetToCurrentTime()
	}
}

// RecordTransaction учитывает завершённую транзакцию.
func RecordTransaction(started time.Time, committed bool) {
	result := "commit"
	if !committed {
		result = "rollback"
	}
	dbTransactions.WithLabelValues(result).Inc()
	dbTransactionDuration.Observe(time.Since(started).Seconds())
}

type instrumentedTransport struct {
	base http.RoundTripper
}

func (t *instrumentedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	started := time.Now()
	resp, err := t.base.RoundTrip(req)

	outcome := "error"
	if err == nil {
		switch {
		case resp.StatusCode >= 500:
			outcome = "server_error"
		case resp.StatusCode >= 400:
			outcome = "client_error"
		default:
			outcome = "success"
		}
	}
	// Хост берётся из запроса, но набор хостов конечен и задан кодом
	// (kinopoisk, omdb, tmdb, trakt, github, хранилище).
	host := req.URL.Hostname()
	outboundRequests.WithLabelValues(host, outcome).Inc()
	outboundDuration.WithLabelValues(host).Observe(time.Since(started).Seconds())
	return resp, err
}

// InstrumentedClient возвращает HTTP-клиент, который считает свои запросы
// и их длительность по хосту назначения. Таймаут не задаётся: как и у
// http.DefaultClient, срок задаёт контекст запроса.
func InstrumentedClient() *http.Client {
	return &http.Client{Transport: &instrumentedTransport{base: http.DefaultTransport}}
}
