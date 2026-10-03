// Package postgres оборачивает пул соединений pgx и паттерн транзакций для
// RLS, которого требует схема: любой запрос к таблице с row-level security
// должен выполняться внутри транзакции, которая сначала устанавливает
// app.current_user_id, потому что каждая политика проверяет
// public.current_user_id() (см. database/migrations/0001_initial_schema.sql
// в репозитории notrecinema-app — это единственный источник истины для схемы).
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"notrecinema/api/internal/telemetry"
)

type Pool struct {
	pool *pgxpool.Pool
}

func Connect(ctx context.Context, databaseURL string) (*Pool, error) {
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse config: %w", err)
	}

	// Повторяет страховку пула из Next.js-приложения
	// (src/shared/api/postgres/database.ts): без ограничения на то, сколько
	// может длиться запрос или простаивающая в транзакции сессия, один
	// зависший запрос способен держать соединение вечно и голодом заморить
	// весь пул.
	poolConfig.MaxConns = 20
	poolConfig.MaxConnIdleTime = 30 * time.Second
	poolConfig.ConnConfig.ConnectTimeout = 5 * time.Second
	poolConfig.ConnConfig.Tracer = telemetry.QueryTracer{}

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("postgres: create pool: %w", err)
	}

	return &Pool{pool: pool}, nil
}

// Stat отдаёт состояние пула соединений (для метрик).
func (p *Pool) Stat() *pgxpool.Stat {
	return p.pool.Stat()
}

func (p *Pool) Close() {
	p.pool.Close()
}

func (p *Pool) Ping(ctx context.Context) error {
	return p.pool.Ping(ctx)
}

// Query выполняет запрос без пользовательского контекста. Безопасно только
// для таблиц/функций без row-level security, либо для SECURITY DEFINER
// функций, которые сознательно её обходят (например, поиск сессии, который
// обязан работать ещё до того, как личность пользователя установлена).
func (p *Pool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return p.pool.Query(ctx, sql, args...)
}

func (p *Pool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return p.pool.QueryRow(ctx, sql, args...)
}

// Exec выполняет запрос без пользовательского контекста и без интереса к
// результату -- для SECURITY DEFINER функций, возвращающих void. В отличие
// от Query, сама закрывает за собой соединение: Query здесь легко забыть
// явным Close() у возвращённых pgx.Rows, что молча удерживает соединение
// пула до его протухания по MaxConnIdleTime -- на серии таких вызовов пул
// исчерпывается и всё, что дальше просит соединение, зависает без ошибки.
func (p *Pool) Exec(ctx context.Context, sql string, args ...any) error {
	_, err := p.pool.Exec(ctx, sql, args...)
	return err
}

// Begin открывает транзакцию без пользовательского контекста. Только для
// системных таблиц без RLS (outbox_events, schema_migrations и т.п.) --
// для всего, что защищено RLS-политиками, используйте WithUserContext.
func (p *Pool) Begin(ctx context.Context) (pgx.Tx, error) {
	return p.pool.Begin(ctx)
}

// WithUserContext выполняет fn внутри транзакции, привязанной к userID:
// каждая RLS-политика, вызывающая public.current_user_id() внутри fn,
// увидит userID. Повторяет withUserContext из Next.js-приложения
// (src/shared/api/postgres/database.ts), чтобы оба сервиса применяли
// абсолютно одинаковые правила доступа.
func (p *Pool) WithUserContext(ctx context.Context, userID string, fn func(ctx context.Context, tx pgx.Tx) error) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: begin transaction: %w", err)
	}

	started := time.Now()
	committed := false
	defer func() {
		_ = tx.Rollback(ctx)
		telemetry.RecordTransaction(started, committed)
	}()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.current_user_id', $1, true)", userID); err != nil {
		return fmt.Errorf("postgres: set user context: %w", err)
	}

	if err := fn(ctx, tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit transaction: %w", err)
	}
	committed = true

	return nil
}
