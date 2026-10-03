// Package nudges находит заброшенные сериалы (статус "to-watch", прогресс
// начат, давно не двигался) и публикует по одному outbox-событию
// progress.stale на каждый -- notrecinema-worker его превращает в push
// конкретному пользователю (internal/handlers.ProgressStale). Прямой
// перенос check-inactive cron из notrecinema-app
// (src/app/api/cron/check-inactive/route.ts), только вместо HTTP-роута,
// который нужно дёргать снаружи по расписанию, здесь тикер внутри
// процесса -- тот же выбор, что уже сделан для outbox.Poller.
//
// Письмо с напоминанием здесь не отправляется: тот же ProgressStale
// обрабатывает notrecinema-worker и шлёт и push, и письмо (internal/mailing).
package nudges

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"notrecinema/api/internal/outbox"
	"notrecinema/api/internal/postgres"
	"notrecinema/api/internal/telemetry"
)

type staleEntry struct {
	SeriesID       string
	UserID         string
	Title          string
	CurrentSeason  int
	CurrentEpisode int
}

type progressStalePayload struct {
	SeriesID       string `json:"seriesId"`
	UserID         string `json:"userId"`
	Title          string `json:"title"`
	CurrentSeason  int    `json:"currentSeason"`
	CurrentEpisode int    `json:"currentEpisode"`
}

type Checker struct {
	db            *postgres.Pool
	logger        *slog.Logger
	interval      time.Duration
	daysThreshold int
}

func NewChecker(db *postgres.Pool, logger *slog.Logger, interval time.Duration, daysThreshold int) *Checker {
	return &Checker{db: db, logger: logger, interval: interval, daysThreshold: daysThreshold}
}

// Run блокирует вызывающего до отмены ctx, вызывая CheckOnce каждые
// c.interval. Предназначен для запуска в отдельной goroutine.
func (c *Checker) Run(ctx context.Context) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			started := time.Now()
			err := c.CheckOnce(ctx)
			telemetry.RecordJob("nudges", started, err)
			if err != nil {
				c.logger.Error("nudges: ошибка проверки", "error", err)
			}
		}
	}
}

// CheckOnce находит застрявшие progress-строки, атомарно застолбляет
// каждую (mark_progress_reminded) и публикует progress.stale только для
// тех, чей claim реально удался -- если строку уже забрал параллельный
// или предыдущий пересекающийся прогон, повторного события не будет.
// get_stale_progress/mark_progress_reminded -- SECURITY DEFINER функции
// (миграции 0011, 0020): проверка идёт без залогиненного пользователя, как
// и у остальных системных cron-задач в этой схеме.
func (c *Checker) CheckOnce(ctx context.Context) error {
	tx, err := c.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT series_id, user_id, title, current_season, current_episode
		FROM public.get_stale_progress($1)
	`, c.daysThreshold)
	if err != nil {
		return err
	}

	var stale []staleEntry
	for rows.Next() {
		var e staleEntry
		if err := rows.Scan(&e.SeriesID, &e.UserID, &e.Title, &e.CurrentSeason, &e.CurrentEpisode); err != nil {
			rows.Close()
			return err
		}
		stale = append(stale, e)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	notified := 0
	for _, e := range stale {
		if err := c.claimAndPublish(ctx, tx, e); err != nil {
			return err
		}
		notified++
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}

	if len(stale) > 0 {
		c.logger.Info("nudges: проверка завершена", "checked", len(stale), "notified", notified)
	}
	return nil
}

func (c *Checker) claimAndPublish(ctx context.Context, tx pgx.Tx, e staleEntry) error {
	// RETURNING отдаёт строку, только если WHERE (та же staleness-проверка,
	// пере-выполненная атомарно внутри UPDATE) реально что-то нашла и
	// обновила -- ErrNoRows здесь означает "кто-то другой уже застолбил
	// эту строку", а не ошибку.
	var claimed bool
	err := tx.QueryRow(ctx, `
		SELECT public.mark_progress_reminded($1, $2)
	`, e.SeriesID, e.UserID).Scan(&claimed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !claimed {
		return nil
	}

	return outbox.Insert(ctx, tx, "series", e.SeriesID, "progress.stale", progressStalePayload(e))
}
