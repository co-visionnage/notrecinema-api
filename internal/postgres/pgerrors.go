package postgres

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// Коды ошибок Postgres: https://www.postgresql.org/docs/current/errcodes-appendix.html
const (
	sqlStateUniqueViolation     = "23505"
	sqlStateForeignKeyViolation = "23503"
)

// IsUniqueViolation сообщает, что операция упала на UNIQUE-ограничении --
// обычно означает "такая связка уже существует", а не настоящую ошибку.
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == sqlStateUniqueViolation
}

// IsForeignKeyViolation сообщает, что операция ссылается на
// несуществующую строку (например, series_id, которого нет) -- полезно
// отличать от произвольной внутренней ошибки, чтобы вернуть 404, а не 500.
func IsForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == sqlStateForeignKeyViolation
}
