// Package digest раз в неделю ставит в очередь письмо-сводку каждому, кто её
// ждёт: по одному outbox-событию email.weekly_digest на человека.
// notrecinema-worker собирает содержимое (что добавили, что посмотрели,
// какие встречи и новые серии впереди) и отправляет письмо -- API письмо
// не рендерит и почтовому сервису не звонит.
//
// Расписание -- тикер внутри процесса, как у internal/nudges: проверка идёт
// каждые полчаса, а рассылка срабатывает один раз в нужный день недели и
// час. Какая неделя уже разослана, хранится в БД (claim_weekly_digest), поэтому
// перезапуск или несколько экземпляров API письма не удваивают.
package digest

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"notrecinema/api/internal/outbox"
	"notrecinema/api/internal/postgres"
	"notrecinema/api/internal/telemetry"
)

// Period -- окно сводки: прошедшие семь дней; "вперёд" (встречи, новые
// серии) считается от его конца.
const Period = 7 * 24 * time.Hour

type weeklyDigestPayload struct {
	UserID string    `json:"userId"`
	Since  time.Time `json:"since"`
	Until  time.Time `json:"until"`
}

type Runner struct {
	db       *postgres.Pool
	logger   *slog.Logger
	interval time.Duration
	weekday  time.Weekday
	hourUTC  int
}

func NewRunner(db *postgres.Pool, logger *slog.Logger, interval time.Duration, weekday time.Weekday, hourUTC int) *Runner {
	return &Runner{db: db, logger: logger, interval: interval, weekday: weekday, hourUTC: hourUTC}
}

// ParseWeekday понимает английские названия дней ("monday", "Mon").
func ParseWeekday(value string) (time.Weekday, error) {
	name := strings.ToLower(strings.TrimSpace(value))
	for day := time.Sunday; day <= time.Saturday; day++ {
		full := strings.ToLower(day.String())
		if name == full || name == full[:3] {
			return day, nil
		}
	}
	return time.Monday, fmt.Errorf("digest: неизвестный день недели %q (нужен, например, monday)", value)
}

// weekKey -- ключ недели по ISO ("2026-W41"): по нему рассылка
// застолбляется в БД.
func weekKey(at time.Time) string {
	year, week := at.UTC().ISOWeek()
	return fmt.Sprintf("%d-W%02d", year, week)
}

// due -- пора ли рассылать: нужный день недели и час уже наступили (час не
// "ровно", а "не раньше": если API был недоступен в 06:00, письма уйдут, как
// только он поднимется в тот же день).
func (r *Runner) due(now time.Time) bool {
	now = now.UTC()
	return now.Weekday() == r.weekday && now.Hour() >= r.hourUTC
}

// Run блокирует вызывающего до отмены ctx. Предназначен для отдельной goroutine.
func (r *Runner) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			started := time.Now()
			queued, err := r.RunOnce(ctx, started)
			if queued > 0 || err != nil {
				telemetry.RecordJob("digest", started, err)
			}
			if err != nil {
				r.logger.Error("digest: ошибка рассылки", "error", err)
			}
		}
	}
}

// RunOnce ставит сводки в очередь, если now -- время рассылки и эта неделя
// ещё не разослана. Возвращает, скольким людям поставлена сводка.
func (r *Runner) RunOnce(ctx context.Context, now time.Time) (int, error) {
	if !r.due(now) {
		return 0, nil
	}

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var claimed bool
	if err := tx.QueryRow(ctx, `SELECT public.claim_weekly_digest($1)`, weekKey(now)).Scan(&claimed); err != nil {
		return 0, err
	}
	if !claimed {
		return 0, nil
	}

	rows, err := tx.Query(ctx, `SELECT user_id FROM public.list_weekly_digest_recipients_system()`)
	if err != nil {
		return 0, err
	}
	var recipients []string
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			rows.Close()
			return 0, err
		}
		recipients = append(recipients, userID)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	until := now.UTC().Truncate(time.Minute)
	since := until.Add(-Period)
	for _, userID := range recipients {
		payload := weeklyDigestPayload{UserID: userID, Since: since, Until: until}
		if err := outbox.Insert(ctx, tx, "user", userID, "email.weekly_digest", payload); err != nil {
			return 0, err
		}
	}

	// Застолбление и события -- одна транзакция: если вставка событий
	// упадёт, неделя не будет считаться разосланной и следующий тик повторит.
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}

	r.logger.Info("digest: сводки поставлены в очередь", "week", weekKey(now), "recipients", len(recipients))
	return len(recipients), nil
}
