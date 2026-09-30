// Package outbox реализует transactional outbox: запись бизнес-данных и
// запись события о ней происходят в одной транзакции Postgres
// (см. Insert), поэтому событие никогда не теряется при падении между
// "записали в БД" и "опубликовали в шину" и никогда не публикуется, если
// транзакция откатилась. Отдельный Poller вычитывает неопубликованные
// строки и публикует их асинхронно, вне пользовательских транзакций.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"notrecinema/api/internal/postgres"
	"notrecinema/api/internal/telemetry"
)

var tracer = otel.Tracer("notrecinema-api/outbox")

// Publisher — всё, что нужно Poller-у от шины сообщений. outbox не знает
// про NATS напрямую, только про этот интерфейс (реализован в
// internal/eventbus), чтобы пакеты не были связаны друг с другом жёстче,
// чем нужно.
type Publisher interface {
	Publish(ctx context.Context, eventType string, data []byte) error
}

// Envelope — формат сообщения на шине. EventID -- это id строки
// outbox_events, по нему потребитель (notrecinema-worker) дедуплицирует
// повторную доставку: JetStream даёт only at-least-once, то есть одно и то
// же сообщение может прийти дважды, и только EventID позволяет отличить
// "уже обработали" от "новое событие". Формат специально совпадает с тем,
// что ждёт notrecinema-worker/internal/consumer -- это единственный
// договор между двумя независимыми Go-модулями, и он зафиксирован здесь.
type Envelope struct {
	EventID   string          `json:"eventId"`
	EventType string          `json:"eventType"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"createdAt"`
}

// Insert кладёt событие в outbox_events внутри переданной транзакции tx.
// Вызывать нужно из того же db.WithUserContext, где происходит основная
// запись -- события никогда не вставляются отдельной транзакцией, это
// свело бы на нет весь смысл паттерна.
func Insert(ctx context.Context, tx pgx.Tx, aggregateType, aggregateID, eventType string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("outbox: marshal payload: %w", err)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO public.outbox_events (aggregate_type, aggregate_id, event_type, payload)
		VALUES ($1, $2, $3, $4)
	`, aggregateType, aggregateID, eventType, data)
	if err != nil {
		return fmt.Errorf("outbox: insert event: %w", err)
	}
	return nil
}

type event struct {
	ID        string
	EventType string
	Payload   []byte
	CreatedAt time.Time
}

// Poller — фоновый цикл, вычитывающий неопубликованные события и
// отправляющий их в шину. Работает вне пользовательского контекста: у
// outbox_events нет RLS, это системная таблица.
type Poller struct {
	db        *postgres.Pool
	publisher Publisher
	logger    *slog.Logger
	metrics   *telemetry.Metrics
	interval  time.Duration
	batchSize int
}

// metrics может быть nil (например, в тестах) -- тогда Poller просто не
// пишет метрики, вместо паники на нулевом указателе.
func NewPoller(db *postgres.Pool, publisher Publisher, logger *slog.Logger, metrics *telemetry.Metrics) *Poller {
	return &Poller{
		db:        db,
		publisher: publisher,
		logger:    logger,
		metrics:   metrics,
		interval:  2 * time.Second,
		batchSize: 100,
	}
}

// Run блокирует вызывающего до отмены ctx, опрашивая outbox_events каждые
// p.interval. Предназначен для запуска в отдельной goroutine.
func (p *Poller) Run(ctx context.Context) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := p.PollOnce(ctx); err != nil {
				p.logger.Error("outbox: ошибка опроса", "error", err)
			}
		}
	}
}

// PollOnce обрабатывает одну пачку событий и возвращает управление -- не
// блокирует, в отличие от Run. Используется и самим Run по тикеру, и
// тестами, которым нужен ровно один детерминированный цикл опроса без
// ожидания таймера. FOR UPDATE SKIP LOCKED делает безопасным запуск
// нескольких реплик API одновременно: каждая реплика подхватывает свою
// порцию строк, не блокируя и не задваивая работу других.
func (p *Poller) PollOnce(ctx context.Context) error {
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT id, event_type, payload, created_at
		FROM public.outbox_events
		WHERE published_at IS NULL
		ORDER BY created_at
		LIMIT $1
		FOR UPDATE SKIP LOCKED
	`, p.batchSize)
	if err != nil {
		return fmt.Errorf("select pending: %w", err)
	}

	var events []event
	for rows.Next() {
		var e event
		if err := rows.Scan(&e.ID, &e.EventType, &e.Payload, &e.CreatedAt); err != nil {
			rows.Close()
			return fmt.Errorf("scan: %w", err)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rows: %w", err)
	}

	for _, e := range events {
		if err := p.publishOne(ctx, tx, e); err != nil {
			return err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	p.recordPending(ctx)
	return nil
}

// recordPending обновляет gauge актуальным числом неопубликованных строк.
// Отдельный лёгкий запрос вне транзакции обработки: партиционировать этот
// COUNT по batchSize смысла нет, а знать реальный размер очереди (а не
// только размер последней выбранной пачки) -- то, ради чего эта метрика
// вообще существует.
func (p *Poller) recordPending(ctx context.Context) {
	if p.metrics == nil {
		return
	}

	var pending int64
	row := p.db.QueryRow(ctx, `SELECT count(*) FROM public.outbox_events WHERE published_at IS NULL`)
	if err := row.Scan(&pending); err != nil {
		p.logger.Warn("outbox: не удалось обновить метрику outbox_pending", "error", err)
		return
	}
	p.metrics.OutboxPending.Set(float64(pending))
}

// publishOne публикует одно событие в отдельном спане: так в Jaeger видно
// каждую публикацию как дочерний спан цикла опроса, с полным trace'ом,
// продолжающимся дальше в notrecinema-worker через заголовки NATS-сообщения.
func (p *Poller) publishOne(ctx context.Context, tx pgx.Tx, e event) error {
	spanCtx, span := tracer.Start(ctx, "outbox.publish",
		trace.WithAttributes(
			attribute.String("event.id", e.ID),
			attribute.String("event.type", e.EventType),
		),
	)
	defer span.End()

	envelope, marshalErr := json.Marshal(Envelope{
		EventID:   e.ID,
		EventType: e.EventType,
		Payload:   e.Payload,
		CreatedAt: e.CreatedAt,
	})
	if marshalErr != nil {
		span.RecordError(marshalErr)
		span.SetStatus(codes.Error, "marshal envelope")
		return fmt.Errorf("marshal envelope for %s: %w", e.ID, marshalErr)
	}

	publishErr := p.publisher.Publish(spanCtx, e.EventType, envelope)
	if publishErr != nil {
		span.RecordError(publishErr)
		span.SetStatus(codes.Error, "publish")

		if p.metrics != nil {
			p.metrics.OutboxFailedTotal.Inc()
		}
		if _, err := tx.Exec(ctx, `
			UPDATE public.outbox_events
			SET attempts = attempts + 1, last_error = $2
			WHERE id = $1
		`, e.ID, publishErr.Error()); err != nil {
			return fmt.Errorf("record failure for %s: %w", e.ID, err)
		}
		p.logger.Warn("outbox: не удалось опубликовать событие, повторим на следующем цикле",
			"event_id", e.ID, "event_type", e.EventType, "error", publishErr)
		return nil
	}

	if _, err := tx.Exec(ctx, `
		UPDATE public.outbox_events SET published_at = NOW() WHERE id = $1
	`, e.ID); err != nil {
		return fmt.Errorf("mark published %s: %w", e.ID, err)
	}

	if p.metrics != nil {
		p.metrics.OutboxPublishedTotal.Inc()
	}
	return nil
}
