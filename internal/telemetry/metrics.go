package telemetry

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics группирует все метрики API в одном месте, чтобы не плодить
// глобальные переменные по пакетам и не забыть отрегистрировать что-то
// дважды при повторном вызове конструктора (в тестах, например).
type Metrics struct {
	HTTPRequestsTotal    *prometheus.CounterVec
	HTTPRequestDuration  *prometheus.HistogramVec
	OutboxPublishedTotal prometheus.Counter
	OutboxFailedTotal    prometheus.Counter
	OutboxPending        prometheus.Gauge
	ExternalAPIRequests  *prometheus.CounterVec
	ExternalAPIDuration  *prometheus.HistogramVec
}

func NewMetrics(registry prometheus.Registerer) *Metrics {
	factory := promauto.With(registry)

	return &Metrics{
		HTTPRequestsTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Количество HTTP-запросов по методу, пути и статусу.",
		}, []string{"method", "path", "status"}),

		HTTPRequestDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "Длительность обработки HTTP-запроса.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "path"}),

		OutboxPublishedTotal: factory.NewCounter(prometheus.CounterOpts{
			Name: "outbox_published_total",
			Help: "Сколько событий outbox успешно опубликовано в NATS.",
		}),

		OutboxFailedTotal: factory.NewCounter(prometheus.CounterOpts{
			Name: "outbox_failed_total",
			Help: "Сколько попыток публикации события outbox завершились ошибкой.",
		}),

		OutboxPending: factory.NewGauge(prometheus.GaugeOpts{
			Name: "outbox_pending",
			Help: "Сколько строк outbox_events сейчас не опубликовано (снимок на момент последнего опроса).",
		}),

		ExternalAPIRequests: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "external_api_requests_total",
			Help: "Запросы к внешним API метаданных фильмов по провайдеру и результату.",
		}, []string{"provider", "outcome"}),

		ExternalAPIDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "external_api_duration_seconds",
			Help:    "Длительность запроса к внешнему API метаданных фильмов.",
			Buckets: prometheus.DefBuckets,
		}, []string{"provider"}),
	}
}

// Handler отдаёт /metrics в формате, который понимает Prometheus.
func (m *Metrics) Handler() http.Handler {
	return promhttp.Handler()
}

// HTTPMiddleware пишет http_requests_total и http_request_duration_seconds
// на каждый запрос. Путь берётся из r.Pattern (заполняется ServeMux начиная
// с Go 1.22), а не из r.URL.Path -- иначе "/api/v1/families" и
// "/api/v1/families/абракадабра" были бы разными метками и кардинальность
// метрики росла бы вместе с количеством разных ID в URL.
func (m *Metrics) HTTPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(recorder, r)

		path := r.Pattern
		if path == "" {
			path = r.URL.Path
		}

		m.HTTPRequestsTotal.WithLabelValues(r.Method, path, strconv.Itoa(recorder.status)).Inc()
		m.HTTPRequestDuration.WithLabelValues(r.Method, path).Observe(time.Since(start).Seconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}
