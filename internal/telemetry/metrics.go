package telemetry

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics группирует метрики API, которым нужен общий конструктор (HTTP,
// outbox, внешние провайдеры), чтобы не плодить глобальные переменные по
// пакетам и не отрегистрировать что-то дважды при повторном вызове
// конструктора (в тестах, например). Сквозные метрики, которые пишутся из
// многих пакетов, живут отдельно -- см. global.go.
type Metrics struct {
	HTTPRequestsTotal   *prometheus.CounterVec
	HTTPRequestDuration *prometheus.HistogramVec
	HTTPInFlight        prometheus.Gauge
	HTTPResponseSize    *prometheus.HistogramVec

	OutboxPublishedTotal   prometheus.Counter
	OutboxEventsPublished  *prometheus.CounterVec
	OutboxFailedTotal      prometheus.Counter
	OutboxPending          prometheus.Gauge
	OutboxOldestPendingAge prometheus.Gauge
	OutboxPublishDuration  prometheus.Histogram

	ExternalAPIRequests *prometheus.CounterVec
	ExternalAPIDuration *prometheus.HistogramVec
}

func NewMetrics(registry prometheus.Registerer) *Metrics {
	factory := promauto.With(registry)

	return &Metrics{
		HTTPRequestsTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Количество HTTP-запросов по методу, маршруту и статусу.",
		}, []string{"method", "path", "status"}),

		HTTPRequestDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "Длительность обработки HTTP-запроса.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "path"}),

		HTTPInFlight: factory.NewGauge(prometheus.GaugeOpts{
			Name: "http_requests_in_flight",
			Help: "Сколько HTTP-запросов обрабатывается прямо сейчас.",
		}),

		HTTPResponseSize: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_response_size_bytes",
			Help:    "Размер тела HTTP-ответа.",
			Buckets: prometheus.ExponentialBuckets(100, 4, 8),
		}, []string{"method", "path"}),

		OutboxPublishedTotal: factory.NewCounter(prometheus.CounterOpts{
			Name: "outbox_published_total",
			Help: "Сколько событий outbox успешно опубликовано в NATS.",
		}),

		OutboxEventsPublished: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "outbox_events_published_total",
			Help: "Опубликованные события outbox по типу события.",
		}, []string{"event_type"}),

		OutboxFailedTotal: factory.NewCounter(prometheus.CounterOpts{
			Name: "outbox_failed_total",
			Help: "Сколько попыток публикации события outbox завершились ошибкой.",
		}),

		OutboxPending: factory.NewGauge(prometheus.GaugeOpts{
			Name: "outbox_pending",
			Help: "Сколько строк outbox_events сейчас не опубликовано (снимок на момент последнего опроса).",
		}),

		OutboxOldestPendingAge: factory.NewGauge(prometheus.GaugeOpts{
			Name: "outbox_oldest_pending_age_seconds",
			Help: "Возраст самого старого неопубликованного события outbox (0, если очередь пуста). Растёт, если публикация зависла.",
		}),

		OutboxPublishDuration: factory.NewHistogram(prometheus.HistogramOpts{
			Name:    "outbox_publish_duration_seconds",
			Help:    "Длительность публикации одного события outbox в NATS.",
			Buckets: prometheus.DefBuckets,
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

// unmatchedRoute -- метка для запросов, которые не подошли ни под один
// маршрут (сканеры, опечатки). Сырой путь в метку класть нельзя: на каждый
// выдуманный адрес появился бы новый ряд.
const unmatchedRoute = "unmatched"

type routeKey struct{}

// routeHolder передаёт наружу шаблон маршрута, который определил ServeMux.
// Пишется из обработчика (в другой горутине при http.TimeoutHandler), а
// читается после возврата -- отсюда мьютекс.
type routeHolder struct {
	mu      sync.Mutex
	pattern string
}

// set не затирает конкретный шаблон общим "/": внешний mux сообщает "/" для
// всего, что передаёт внутреннему, а внутренний -- настоящий маршрут.
func (h *routeHolder) set(pattern string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pattern == "" || h.pattern == "/" {
		h.pattern = pattern
	}
}

func (h *routeHolder) get() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pattern
}

// CaptureRoute оборачивает ServeMux и после обработки запоминает шаблон
// маршрута, который тот выбрал (r.Pattern). Оборачивать нужно сам mux,
// без промежуточных middleware: ServeMux пишет Pattern в тот *http.Request,
// который реально до него дошёл, а middleware, делающие r.WithContext,
// создают копию, и внешний код выбранного маршрута не увидел бы.
func CaptureRoute(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		if holder, ok := r.Context().Value(routeKey{}).(*routeHolder); ok && r.Pattern != "" {
			holder.set(r.Pattern)
		}
	})
}

// routeLabel превращает шаблон ServeMux ("GET /api/v1/families/{familyId}")
// в метку без метода (метод идёт отдельной меткой).
func routeLabel(pattern string) string {
	if pattern == "" {
		return unmatchedRoute
	}
	if _, path, found := strings.Cut(pattern, " "); found {
		return path
	}
	return pattern
}

// HTTPMiddleware пишет число, длительность и размер ответа каждого запроса,
// а также сколько их идёт прямо сейчас. Ставится самым внешним, поэтому
// видит и публичные маршруты (логин, отписка), и запросы, оборвавшиеся по
// таймауту или не прошедшие аутентификацию. Маршрут -- шаблон из ServeMux
// (см. CaptureRoute), а не сырой URL: иначе "/api/v1/families/<uuid>" на
// каждый id давал бы отдельный ряд.
func (m *Metrics) HTTPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		holder := &routeHolder{}
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		m.HTTPInFlight.Inc()
		defer m.HTTPInFlight.Dec()

		next.ServeHTTP(recorder, r.WithContext(context.WithValue(r.Context(), routeKey{}, holder)))

		route := routeLabel(holder.get())
		if route == "/metrics" {
			// Скрейп самого Prometheus не должен шуметь в собственных метриках.
			return
		}

		m.HTTPRequestsTotal.WithLabelValues(r.Method, route, strconv.Itoa(recorder.status)).Inc()
		m.HTTPRequestDuration.WithLabelValues(r.Method, route).Observe(time.Since(started).Seconds())
		m.HTTPResponseSize.WithLabelValues(r.Method, route).Observe(float64(recorder.bytes))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if !r.wroteHeader {
		r.status = status
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wroteHeader = true
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Unwrap позволяет http.ResponseController добраться до настоящего
// ResponseWriter (Flush, SetWriteDeadline и т.д.).
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
