// Package eventreminders за час до начала встречи для просмотра ставит в
// очередь по одному outbox-событию watch_event.reminder на каждого, кому
// нужно напомнить (участники семьи, кроме ответивших «нет»).
// notrecinema-worker превращает его в push и письмо.
//
// Расписания как такового нет: тикер каждую минуту спрашивает БД, какие
// встречи вошли в окно напоминания. Что встреча уже напомнена, хранится в
// БД (claim_event_reminders_system ставит reminded_at тем же UPDATE, что и
// выбирает её), поэтому перезапуск или несколько экземпляров API
// напоминание не удваивают.
package eventreminders

import (
	"context"
	"log/slog"
	"time"

	"notrecinema/api/internal/outbox"
	"notrecinema/api/internal/postgres"
	"notrecinema/api/internal/telemetry"
)

type reminderPayload struct {
	EventID     string    `json:"eventId"`
	FamilyID    string    `json:"familyId"`
	UserID      string    `json:"userId"`
	Title       string    `json:"title"`
	ScheduledAt time.Time `json:"scheduledAt"`
}

type Runner struct {
	db       *postgres.Pool
	logger   *slog.Logger
	interval time.Duration
	lead     time.Duration
}

func NewRunner(db *postgres.Pool, logger *slog.Logger, interval, lead time.Duration) *Runner {
	return &Runner{db: db, logger: logger, interval: interval, lead: lead}
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
			queued, err := r.RunOnce(ctx)
			if queued > 0 || err != nil {
				telemetry.RecordJob("event_reminders", started, err)
			}
			if err != nil {
				r.logger.Error("event_reminders: ошибка", "error", err)
			}
		}
	}
}

// RunOnce забирает встречи, которым пора напомнить, и ставит событие на
// каждого получателя. Возвращает число поставленных событий. Выбор встреч и
// вставка событий -- одна транзакция: если вставка упадёт, встреча не
// считается напомненной и следующий тик повторит.
func (r *Runner) RunOnce(ctx context.Context) (int, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT event_id, family_id, user_id, title, scheduled_at
		FROM public.claim_event_reminders_system(make_interval(secs => $1))
	`, r.lead.Seconds())
	if err != nil {
		return 0, err
	}
	var payloads []reminderPayload
	for rows.Next() {
		var p reminderPayload
		if err := rows.Scan(&p.EventID, &p.FamilyID, &p.UserID, &p.Title, &p.ScheduledAt); err != nil {
			rows.Close()
			return 0, err
		}
		payloads = append(payloads, p)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(payloads) == 0 {
		return 0, nil
	}

	for _, p := range payloads {
		if err := outbox.Insert(ctx, tx, "watch_event", p.EventID, "watch_event.reminder", p); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}

	r.logger.Info("event_reminders: напоминания поставлены в очередь", "recipients", len(payloads))
	return len(payloads), nil
}
