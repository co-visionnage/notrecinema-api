// Package eventbus оборачивает NATS JetStream: единственное место, которое
// знает про имя стрима и то, как из subject'а вида "family.member.joined"
// получить его. Publisher из этого пакета -- то, что использует
// internal/outbox, чтобы доставлять события, накопленные в outbox_events.
package eventbus

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

const (
	// StreamName -- один стрим на весь домен notrecinema: с одним стримом
	// проще гарантировать порядок и настраивать retention, чем плодить
	// стрим на каждый тип события.
	StreamName = "NOTRECINEMA"

	// StreamSubjects -- какие subject'ы стрим забирает себе. ">" -- любое
	// количество токенов после префикса, так что "family.member.joined",
	// "movie.added" и т.д. попадают в один и тот же стрим.
	streamSubjectsPrefix = "notrecinema."
)

type Bus struct {
	conn   *nats.Conn
	stream jetstream.Stream
	js     jetstream.JetStream
}

// Connect подключается к NATS и идемпотентно создаёт (или обновляет) стрим.
// Идемпотентность важна: этот код выполняется при каждом старте процесса,
// а не один раз при деплое.
func Connect(ctx context.Context, url string) (*Bus, error) {
	conn, err := nats.Connect(url,
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("eventbus: connect: %w", err)
	}

	js, err := jetstream.New(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("eventbus: jetstream: %w", err)
	}

	stream, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      StreamName,
		Subjects:  []string{streamSubjectsPrefix + ">"},
		Retention: jetstream.LimitsPolicy,
		MaxAge:    7 * 24 * time.Hour,
		Storage:   jetstream.FileStorage,
	})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("eventbus: create stream: %w", err)
	}

	return &Bus{conn: conn, stream: stream, js: js}, nil
}

func (b *Bus) Close() {
	b.conn.Close()
}

func (b *Bus) JetStream() jetstream.JetStream {
	return b.js
}

// Subject добавляет к типу события общий префикс стрима, чтобы
// "family.member.joined" стало "notrecinema.family.member.joined".
func Subject(eventType string) string {
	return streamSubjectsPrefix + eventType
}

// Publish публикует данные в subject, соответствующий eventType, и ждёт
// подтверждения от JetStream (не fire-and-forget) -- иначе от гарантии
// "outbox доставил -- событие реально в стриме" остаётся только половина.
// Trace-контекст из ctx прокидывается в заголовки NATS-сообщения (W3C
// traceparent), чтобы notrecinema-worker мог продолжить тот же trace, а не
// начинать новый несвязанный с HTTP-запросом, который породил событие.
func (b *Bus) Publish(ctx context.Context, eventType string, data []byte) error {
	msg := nats.NewMsg(Subject(eventType))
	msg.Data = data
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(msg.Header))

	_, err := b.js.PublishMsg(ctx, msg)
	if err != nil {
		return fmt.Errorf("eventbus: publish %s: %w", eventType, err)
	}
	return nil
}
