// Package events реализует запланированные совместные просмотры
// (family_watch_events) с RSVP каждого участника. В отличие от polls
// (что смотреть), это про когда -- см. комментарий в миграции
// 0017_watch_events.sql.
package events

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

type RSVP struct {
	UserID string `json:"userId"`
	Status string `json:"status"`
}

type Event struct {
	ID          string    `json:"id"`
	FamilyID    string    `json:"familyId"`
	SeriesID    *string   `json:"seriesId,omitempty"`
	CreatedBy   string    `json:"createdBy"`
	Title       string    `json:"title"`
	ScheduledAt time.Time `json:"scheduledAt"`
	CreatedAt   time.Time `json:"createdAt"`
	RSVPs       []RSVP    `json:"rsvps"`
}

type Service struct {
	db *postgres.Pool
}

func NewService(db *postgres.Pool) *Service {
	return &Service{db: db}
}

type CreateInput struct {
	Title       string    `json:"title"`
	ScheduledAt time.Time `json:"scheduledAt"`
	SeriesID    *string   `json:"seriesId"`
}

func (s *Service) Create(ctx context.Context, userID, familyID string, in CreateInput) (Event, error) {
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return Event{}, apperror.Invalid("title is required")
	}
	if in.ScheduledAt.IsZero() {
		return Event{}, apperror.Invalid("scheduledAt is required")
	}

	var event Event
	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO public.family_watch_events (family_id, series_id, created_by, title, scheduled_at)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING id, family_id, series_id, created_by, title, scheduled_at, created_at
		`, familyID, in.SeriesID, userID, title, in.ScheduledAt)
		if err := row.Scan(&event.ID, &event.FamilyID, &event.SeriesID, &event.CreatedBy, &event.Title, &event.ScheduledAt, &event.CreatedAt); err != nil {
			return err
		}
		event.RSVPs = []RSVP{}

		if err := activitylog.Insert(ctx, tx, familyID, userID, "участник", "watch_event.created", &event.Title, nil); err != nil {
			return err
		}

		payload := map[string]any{"eventId": event.ID, "familyId": familyID, "createdBy": userID, "title": event.Title, "scheduledAt": event.ScheduledAt}
		return outbox.Insert(ctx, tx, "watch_event", event.ID, "watch_event.created", payload)
	})
	if err != nil {
		if postgres.IsForeignKeyViolation(err) {
			return Event{}, apperror.Invalid("family or series does not exist")
		}
		return Event{}, apperror.Internal("failed to create event", err)
	}
	return event, nil
}

func (s *Service) ListForFamily(ctx context.Context, userID, familyID string) ([]Event, error) {
	var events []Event

	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, family_id, series_id, created_by, title, scheduled_at, created_at
			FROM public.family_watch_events
			WHERE family_id = $1
			ORDER BY scheduled_at ASC
		`, familyID)
		if err != nil {
			return err
		}

		byID := make(map[string]*Event)
		var order []string
		for rows.Next() {
			var e Event
			if err := rows.Scan(&e.ID, &e.FamilyID, &e.SeriesID, &e.CreatedBy, &e.Title, &e.ScheduledAt, &e.CreatedAt); err != nil {
				rows.Close()
				return err
			}
			e.RSVPs = []RSVP{}
			byID[e.ID] = &e
			order = append(order, e.ID)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(order) == 0 {
			return nil
		}

		rsvpRows, err := tx.Query(ctx, `
			SELECT event_id, user_id, status
			FROM public.family_watch_event_rsvps
			WHERE event_id = ANY($1)
		`, order)
		if err != nil {
			return err
		}
		defer rsvpRows.Close()

		for rsvpRows.Next() {
			var eventID string
			var r RSVP
			if err := rsvpRows.Scan(&eventID, &r.UserID, &r.Status); err != nil {
				return err
			}
			if e, ok := byID[eventID]; ok {
				e.RSVPs = append(e.RSVPs, r)
			}
		}
		if err := rsvpRows.Err(); err != nil {
			return err
		}

		for _, id := range order {
			events = append(events, *byID[id])
		}
		return nil
	})
	if err != nil {
		return nil, apperror.Internal("failed to list events", err)
	}
	if events == nil {
		events = []Event{}
	}
	return events, nil
}

type SetRSVPInput struct {
	Status string `json:"status"`
}

func (s *Service) SetRSVP(ctx context.Context, userID, eventID string, in SetRSVPInput) error {
	switch in.Status {
	case "going", "maybe", "no":
	default:
		return apperror.Invalid("status must be one of: going, maybe, no")
	}

	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.family_watch_event_rsvps (event_id, user_id, status)
			VALUES ($1, $2, $3)
			ON CONFLICT (event_id, user_id) DO UPDATE SET status = EXCLUDED.status
		`, eventID, userID, in.Status)
		return err
	})
	if err != nil {
		if postgres.IsForeignKeyViolation(err) {
			return apperror.NotFound("event not found")
		}
		return apperror.Internal("failed to set RSVP", err)
	}
	return nil
}

func (s *Service) Delete(ctx context.Context, userID, eventID string) error {
	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM public.family_watch_events WHERE id = $1`, eventID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apperror.NotFound("event not found")
		}
		return apperror.Internal("failed to delete event", err)
	}
	return nil
}

func RegisterRoutes(mux *http.ServeMux, svc *Service) {
	mux.HandleFunc("POST /api/v1/families/{familyId}/events", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())

		var in CreateInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			response.Error(w, apperror.Invalid("invalid request body"))
			return
		}

		event, err := svc.Create(r.Context(), user.ID, r.PathValue("familyId"), in)
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusCreated, event)
	})

	mux.HandleFunc("GET /api/v1/families/{familyId}/events", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())
		events, err := svc.ListForFamily(r.Context(), user.ID, r.PathValue("familyId"))
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, events)
	})

	mux.HandleFunc("PUT /api/v1/events/{eventId}/rsvp", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())

		var in SetRSVPInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			response.Error(w, apperror.Invalid("invalid request body"))
			return
		}

		if err := svc.SetRSVP(r.Context(), user.ID, r.PathValue("eventId"), in); err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusNoContent, nil)
	})

	mux.HandleFunc("DELETE /api/v1/events/{eventId}", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())
		if err := svc.Delete(r.Context(), user.ID, r.PathValue("eventId")); err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusNoContent, nil)
	})
}
