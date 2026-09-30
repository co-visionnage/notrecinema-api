// Package polls реализует голосования "что смотрим сегодня"
// (family_watch_polls). Большая часть правил -- на уровне RLS: закрытый
// опрос нельзя проголосовать (миграция 0024), голос можно отдать только за
// опцию, реально принадлежащую этому опросу (миграция 0022), закрыть опрос
// может только автор или владелец семьи. Go-слой их не дублирует, просто
// доверяет БД вернуть ошибку, если что-то из этого нарушено.
package polls

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"notrecinema/api/internal/activitylog"
	"notrecinema/api/internal/auth"
	"notrecinema/api/internal/outbox"
	"notrecinema/api/internal/platform/apperror"
	"notrecinema/api/internal/platform/response"
	"notrecinema/api/internal/postgres"
)

type Option struct {
	ID       string `json:"id"`
	SeriesID string `json:"seriesId"`
	Votes    int    `json:"votes"`
}

type Poll struct {
	ID        string     `json:"id"`
	FamilyID  string     `json:"familyId"`
	CreatedBy string     `json:"createdBy"`
	Title     string     `json:"title"`
	IsOpen    bool       `json:"isOpen"`
	CreatedAt time.Time  `json:"createdAt"`
	ClosedAt  *time.Time `json:"closedAt,omitempty"`
	Options   []Option   `json:"options"`
}

type Service struct {
	db *postgres.Pool
}

func NewService(db *postgres.Pool) *Service {
	return &Service{db: db}
}

type CreateInput struct {
	Title     string   `json:"title"`
	SeriesIDs []string `json:"seriesIds"`
}

func (s *Service) Create(ctx context.Context, userID, familyID string, in CreateInput) (Poll, error) {
	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = "Что смотрим сегодня?"
	}
	if len(in.SeriesIDs) == 0 {
		return Poll{}, apperror.Invalid("at least one seriesId is required")
	}

	var poll Poll
	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO public.family_watch_polls (family_id, created_by, title)
			VALUES ($1, $2, $3)
			RETURNING id, family_id, created_by, title, is_open, created_at, closed_at
		`, familyID, userID, title)
		if err := row.Scan(&poll.ID, &poll.FamilyID, &poll.CreatedBy, &poll.Title, &poll.IsOpen, &poll.CreatedAt, &poll.ClosedAt); err != nil {
			return err
		}

		for _, seriesID := range in.SeriesIDs {
			var opt Option
			opt.SeriesID = seriesID
			row := tx.QueryRow(ctx, `
				INSERT INTO public.family_watch_poll_options (poll_id, series_id)
				VALUES ($1, $2)
				RETURNING id, series_id
			`, poll.ID, seriesID)
			if err := row.Scan(&opt.ID, &opt.SeriesID); err != nil {
				return err
			}
			poll.Options = append(poll.Options, opt)
		}

		if err := activitylog.Insert(ctx, tx, familyID, userID, "участник", "poll.created", &poll.Title, nil); err != nil {
			return err
		}

		payload := map[string]any{"pollId": poll.ID, "familyId": familyID, "createdBy": userID, "title": poll.Title}
		return outbox.Insert(ctx, tx, "poll", poll.ID, "poll.created", payload)
	})
	if err != nil {
		if postgres.IsForeignKeyViolation(err) {
			return Poll{}, apperror.Invalid("family or one of the series does not exist")
		}
		return Poll{}, apperror.Internal("failed to create poll", err)
	}
	return poll, nil
}

func (s *Service) ListForFamily(ctx context.Context, userID, familyID string) ([]Poll, error) {
	var polls []Poll

	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, family_id, created_by, title, is_open, created_at, closed_at
			FROM public.family_watch_polls
			WHERE family_id = $1
			ORDER BY created_at DESC
		`, familyID)
		if err != nil {
			return err
		}

		byID := make(map[string]*Poll)
		var order []string
		for rows.Next() {
			var p Poll
			if err := rows.Scan(&p.ID, &p.FamilyID, &p.CreatedBy, &p.Title, &p.IsOpen, &p.CreatedAt, &p.ClosedAt); err != nil {
				rows.Close()
				return err
			}
			p.Options = []Option{}
			byID[p.ID] = &p
			order = append(order, p.ID)
		}
		if err := rows.Err(); err != nil {
			return err
		}

		if len(order) == 0 {
			return nil
		}

		optRows, err := tx.Query(ctx, `
			SELECT o.id, o.poll_id, o.series_id, COUNT(v.id)
			FROM public.family_watch_poll_options o
			LEFT JOIN public.family_watch_poll_votes v ON v.option_id = o.id
			WHERE o.poll_id = ANY($1)
			GROUP BY o.id, o.poll_id, o.series_id
		`, order)
		if err != nil {
			return err
		}
		defer optRows.Close()

		for optRows.Next() {
			var pollID string
			var opt Option
			if err := optRows.Scan(&opt.ID, &pollID, &opt.SeriesID, &opt.Votes); err != nil {
				return err
			}
			if p, ok := byID[pollID]; ok {
				p.Options = append(p.Options, opt)
			}
		}
		if err := optRows.Err(); err != nil {
			return err
		}

		for _, id := range order {
			polls = append(polls, *byID[id])
		}
		return nil
	})
	if err != nil {
		return nil, apperror.Internal("failed to list polls", err)
	}
	if polls == nil {
		polls = []Poll{}
	}
	return polls, nil
}

type VoteInput struct {
	OptionID string `json:"optionId"`
}

func (s *Service) Vote(ctx context.Context, userID, pollID string, in VoteInput) error {
	if in.OptionID == "" {
		return apperror.Invalid("optionId is required")
	}

	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.family_watch_poll_votes (poll_id, option_id, user_id)
			VALUES ($1, $2, $3)
			ON CONFLICT (poll_id, user_id) DO UPDATE SET option_id = EXCLUDED.option_id
		`, pollID, in.OptionID, userID)
		return err
	})
	if err != nil {
		// RLS отклоняет INSERT/UPDATE целиком (не даёт узнать, чего именно
		// не хватило -- опрос закрыт, опция не та, или опроса вовсе нет),
		// поэтому здесь честнее сообщить общее "нельзя", чем гадать 404 или 403.
		if postgres.IsForeignKeyViolation(err) {
			return apperror.Invalid("poll or option does not exist")
		}
		return apperror.Forbidden("vote rejected -- poll may be closed or option invalid")
	}
	return nil
}

// Close закрывает опрос. RLS (family_watch_polls_update_creator_or_owner)
// разрешает UPDATE только автору опроса или владельцу семьи -- если под
// строку не подходит никто из них, UPDATE просто не изменит ни одной
// строки, и это возвращается как 404.
func (s *Service) Close(ctx context.Context, userID, pollID string) error {
	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		var familyID, title string
		tag, err := tx.Exec(ctx, `
			UPDATE public.family_watch_polls SET is_open = false, closed_at = NOW()
			WHERE id = $1
		`, pollID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}

		if err := tx.QueryRow(ctx, `SELECT family_id, title FROM public.family_watch_polls WHERE id = $1`, pollID).
			Scan(&familyID, &title); err != nil {
			return err
		}

		if err := activitylog.Insert(ctx, tx, familyID, userID, "участник", "poll.closed", &title, nil); err != nil {
			return err
		}

		payload := map[string]any{"pollId": pollID, "familyId": familyID, "closedBy": userID, "title": title}
		return outbox.Insert(ctx, tx, "poll", pollID, "poll.closed", payload)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apperror.NotFound("poll not found")
		}
		return apperror.Internal("failed to close poll", err)
	}
	return nil
}

func RegisterRoutes(mux *http.ServeMux, svc *Service) {
	mux.HandleFunc("POST /api/v1/families/{familyId}/polls", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())

		var in CreateInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			response.Error(w, apperror.Invalid("invalid request body"))
			return
		}

		poll, err := svc.Create(r.Context(), user.ID, r.PathValue("familyId"), in)
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusCreated, poll)
	})

	mux.HandleFunc("GET /api/v1/families/{familyId}/polls", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())
		polls, err := svc.ListForFamily(r.Context(), user.ID, r.PathValue("familyId"))
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, polls)
	})

	mux.HandleFunc("POST /api/v1/polls/{pollId}/votes", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())

		var in VoteInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			response.Error(w, apperror.Invalid("invalid request body"))
			return
		}

		if err := svc.Vote(r.Context(), user.ID, r.PathValue("pollId"), in); err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusNoContent, nil)
	})

	mux.HandleFunc("POST /api/v1/polls/{pollId}/close", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())
		if err := svc.Close(r.Context(), user.ID, r.PathValue("pollId")); err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusNoContent, nil)
	})
}
