// Package telemetry настраивает трейсинг (OpenTelemetry -> OTLP -> Jaeger)
// и HTTP-метрики (Prometheus) для API. Смысл трейсинга именно здесь: один
// запрос проходит несколько процессов (HTTP -> Postgres -> outbox -> NATS
// -> notrecinema-worker), и без сквозного trace ID отладка "почему это
// событие не обработалось" означает вручную сопоставлять логи трёх
// сервисов по времени.
package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
)

// Shutdown останавливает экспортёр спанов, дожидаясь отправки того, что
// накопилось в буфере -- вызывать при graceful shutdown процесса, иначе
// последние секунды трейсов перед остановкой теряются.
type Shutdown func(ctx context.Context) error

// InitTracing поднимает глобальный TracerProvider с OTLP/HTTP-экспортом.
// otlpEndpoint пустым быть не должен -- в docker-compose это адрес
// контейнера Jaeger.
func InitTracing(ctx context.Context, serviceName, otlpEndpoint string) (Shutdown, error) {
	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(otlpEndpoint),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("telemetry: создать OTLP-экспортёр: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(serviceName)),
	)
	if err != nil {
		return nil, fmt.Errorf("telemetry: создать resource: %w", err)
	}

	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)

	otel.SetTracerProvider(tracerProvider)
	// TraceContext -- W3C-стандарт заголовка traceparent, тот же формат
	// используем и для проброса контекста через заголовки сообщений NATS
	// (см. notrecinema-worker/internal/consumer), а не что-то самописное.
	otel.SetTextMapPropagator(propagation.TraceContext{})

	return tracerProvider.Shutdown, nil
}
