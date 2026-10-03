// Package ratelimit -- единственное место, где API обращается к SQL-функции
// check_rate_limit (миграция 0016): фиксированное окно, счётчик в Postgres,
// общий для всех реплик. До выделения пакета тот же запрос был скопирован
// в восьми местах, и учесть отказы в метриках пришлось бы в каждом.
package ratelimit

import (
	"context"

	"github.com/jackc/pgx/v5"

	"notrecinema/api/internal/telemetry"
)

// Querier -- то, что нужно от БД; *postgres.Pool удовлетворяет.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Check увеличивает счётчик группы bucketKey и сообщает, укладывается ли
// вызывающий в maxAttempts за windowSeconds. Вызывать нужно до самой
// работы, а не после. Каждая проверка попадает в rate_limit_checks_total.
func Check(ctx context.Context, db Querier, bucketKey string, maxAttempts, windowSeconds int) (bool, error) {
	var allowed bool
	if err := db.QueryRow(ctx, `SELECT public.check_rate_limit($1, $2, $3)`, bucketKey, maxAttempts, windowSeconds).Scan(&allowed); err != nil {
		return false, err
	}

	telemetry.RecordRateLimit(bucketKey, allowed)
	return allowed, nil
}
