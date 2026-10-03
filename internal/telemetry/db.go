package telemetry

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	dbQueries = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "db_queries_total",
		Help: "Запросы к Postgres по операции (select/insert/update/delete/function/other) и исходу (ok/error/canceled).",
	}, []string{"operation", "result"})

	dbQueryDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "db_query_duration_seconds",
		Help:    "Длительность запроса к Postgres по операции.",
		Buckets: []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
	}, []string{"operation"})
)

type queryStartKey struct{}

type queryStart struct {
	at        time.Time
	operation string
}

// QueryTracer считает каждый запрос к Postgres (pgx.QueryTracer): число и
// длительность по виду операции. Текст запроса в метку не попадает -- его
// кардинальность неограничена, остаётся только класс операции.
type QueryTracer struct{}

func (QueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, queryStartKey{}, queryStart{at: time.Now(), operation: operationOf(data.SQL)})
}

func (QueryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	started, ok := ctx.Value(queryStartKey{}).(queryStart)
	if !ok {
		return
	}

	result := "ok"
	switch {
	case data.Err == nil:
	case errors.Is(data.Err, context.Canceled), errors.Is(data.Err, context.DeadlineExceeded):
		result = "canceled"
	default:
		result = "error"
	}
	dbQueries.WithLabelValues(started.operation, result).Inc()
	dbQueryDuration.WithLabelValues(started.operation).Observe(time.Since(started.at).Seconds())
}

// operationOf определяет класс запроса по его началу. Вызов SQL-функции
// схемы (SELECT public.fn(...)) вынесен в отдельный класс "function": почти
// вся бизнес-логика живёт в функциях, и отличать её от обычных SELECT
// полезно при разборе нагрузки.
func operationOf(sql string) string {
	trimmed := strings.TrimSpace(sql)
	for strings.HasPrefix(trimmed, "--") {
		newline := strings.IndexByte(trimmed, '\n')
		if newline < 0 {
			return "other"
		}
		trimmed = strings.TrimSpace(trimmed[newline+1:])
	}

	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return "other"
	}

	switch word := strings.ToUpper(fields[0]); word {
	case "SELECT":
		if len(fields) > 1 && strings.HasPrefix(strings.ToLower(fields[1]), "public.") {
			return "function"
		}
		return "select"
	case "INSERT", "UPDATE", "DELETE":
		return strings.ToLower(word)
	case "WITH":
		return "select"
	case "BEGIN", "COMMIT", "ROLLBACK":
		return "transaction"
	default:
		return "other"
	}
}

// PoolCollector отдаёт состояние пула соединений pgx на каждый скрейп.
type PoolCollector struct {
	stat func() *pgxpool.Stat

	connections         *prometheus.Desc
	maxConnections      *prometheus.Desc
	acquires            *prometheus.Desc
	emptyAcquires       *prometheus.Desc
	canceledAcquires    *prometheus.Desc
	acquireWait         *prometheus.Desc
	newConnections      *prometheus.Desc
	destroyedConnection *prometheus.Desc
}

func NewPoolCollector(stat func() *pgxpool.Stat) *PoolCollector {
	return &PoolCollector{
		stat: stat,
		connections: prometheus.NewDesc("db_pool_connections",
			"Соединения пула по состоянию (acquired/idle/constructing).", []string{"state"}, nil),
		maxConnections: prometheus.NewDesc("db_pool_max_connections",
			"Максимальный размер пула соединений.", nil, nil),
		acquires: prometheus.NewDesc("db_pool_acquires_total",
			"Сколько раз соединение брали из пула.", nil, nil),
		emptyAcquires: prometheus.NewDesc("db_pool_empty_acquires_total",
			"Сколько раз пришлось ждать соединение: пул был исчерпан (главный признак нехватки соединений).", nil, nil),
		canceledAcquires: prometheus.NewDesc("db_pool_canceled_acquires_total",
			"Сколько ожиданий соединения оборвалось по контексту.", nil, nil),
		acquireWait: prometheus.NewDesc("db_pool_acquire_wait_seconds_total",
			"Суммарное время ожидания соединения из пула.", nil, nil),
		newConnections: prometheus.NewDesc("db_pool_new_connections_total",
			"Сколько соединений с Postgres открыто за всё время.", nil, nil),
		destroyedConnection: prometheus.NewDesc("db_pool_destroyed_connections_total",
			"Сколько соединений закрыто пулом по причине (max_lifetime/max_idle).", []string{"reason"}, nil),
	}
}

func (c *PoolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.connections
	ch <- c.maxConnections
	ch <- c.acquires
	ch <- c.emptyAcquires
	ch <- c.canceledAcquires
	ch <- c.acquireWait
	ch <- c.newConnections
	ch <- c.destroyedConnection
}

func (c *PoolCollector) Collect(ch chan<- prometheus.Metric) {
	stat := c.stat()
	if stat == nil {
		return
	}

	ch <- prometheus.MustNewConstMetric(c.connections, prometheus.GaugeValue, float64(stat.AcquiredConns()), "acquired")
	ch <- prometheus.MustNewConstMetric(c.connections, prometheus.GaugeValue, float64(stat.IdleConns()), "idle")
	ch <- prometheus.MustNewConstMetric(c.connections, prometheus.GaugeValue, float64(stat.ConstructingConns()), "constructing")
	ch <- prometheus.MustNewConstMetric(c.maxConnections, prometheus.GaugeValue, float64(stat.MaxConns()))
	ch <- prometheus.MustNewConstMetric(c.acquires, prometheus.CounterValue, float64(stat.AcquireCount()))
	ch <- prometheus.MustNewConstMetric(c.emptyAcquires, prometheus.CounterValue, float64(stat.EmptyAcquireCount()))
	ch <- prometheus.MustNewConstMetric(c.canceledAcquires, prometheus.CounterValue, float64(stat.CanceledAcquireCount()))
	ch <- prometheus.MustNewConstMetric(c.acquireWait, prometheus.CounterValue, stat.AcquireDuration().Seconds())
	ch <- prometheus.MustNewConstMetric(c.newConnections, prometheus.CounterValue, float64(stat.NewConnsCount()))
	ch <- prometheus.MustNewConstMetric(c.destroyedConnection, prometheus.CounterValue, float64(stat.MaxLifetimeDestroyCount()), "max_lifetime")
	ch <- prometheus.MustNewConstMetric(c.destroyedConnection, prometheus.CounterValue, float64(stat.MaxIdleDestroyCount()), "max_idle")
}
