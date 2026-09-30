package series

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"notrecinema/api/internal/auth"
	"notrecinema/api/internal/outbox"
	"notrecinema/api/internal/platform/apperror"
	"notrecinema/api/internal/platform/response"
	"notrecinema/api/internal/postgres"
)

type Status struct {
	SeriesID  string     `json:"seriesId"`
	UserID    string     `json:"userId"`
	Status    string     `json:"status"`
	Rating    *int       `json:"rating,omitempty"`
	Comment   *string    `json:"comment,omitempty"`
	WatchedAt *time.Time `json:"watchedAt,omitempty"`
}

type SetStatusInput struct {
	Status  string  `json:"status"`
	Rating  *int    `json:"rating"`
	Comment *string `json:"comment"`
}

// SetStatus делает upsert по UNIQUE (series_id, user_id) -- у каждого
// участника семьи ровно один статус на сериал, повторная установка просто
// его обновляет. watched_at выставляется в NOW() только при первом
// переходе в 'watched' (COALESCE не трогает уже проставленную дату при
// повторных PUT, например когда меняют только рейтинг).
func (s *Service) SetStatus(ctx context.Context, userID, seriesID string, in SetStatusInput) (Status, error) {
	if in.Status != "watched" && in.Status != "to-watch" {
		return Status{}, apperror.Invalid("status must be 'watched' or 'to-watch'")
	}
	if in.Rating != nil && (*in.Rating < 1 || *in.Rating > 5) {
		return Status{}, apperror.Invalid("rating must be between 1 and 5")
	}

	var result Status
	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		// in.Status уже провалидирован выше (watched/to-watch), поэтому
		// решение "ставить NOW() в watched_at или нет" принимается здесь, в
		// Go, а не через SQL-сравнение параметра со строковым литералом --
		// именно такое сравнение (`$3 = 'watched'`) в связке с тем же $3,
		// используемым и как значение колонки, дало pgx неоднозначность типа
		// параметра (SQLSTATE 42P08, "inconsistent types deduced").
		var watchedAtOnInsert, watchedAtExpr string
		if in.Status == "watched" {
			watchedAtOnInsert = "NOW()"
			watchedAtExpr = "COALESCE(family_series_status.watched_at, NOW())"
		} else {
			watchedAtOnInsert = "NULL"
			watchedAtExpr = "NULL"
		}

		row := tx.QueryRow(ctx, `
			INSERT INTO public.family_series_status (series_id, user_id, status, rating, comment, watched_at)
			VALUES ($1, $2, $3, $4, $5, `+watchedAtOnInsert+`)
			ON CONFLICT (series_id, user_id) DO UPDATE SET
				status = EXCLUDED.status,
				rating = EXCLUDED.rating,
				comment = EXCLUDED.comment,
				watched_at = `+watchedAtExpr+`
			RETURNING series_id, user_id, status, rating, comment, watched_at
		`, seriesID, userID, in.Status, in.Rating, in.Comment)

		if err := row.Scan(&result.SeriesID, &result.UserID, &result.Status, &result.Rating, &result.Comment, &result.WatchedAt); err != nil {
			return err
		}

		var familyID, title string
		if err := tx.QueryRow(ctx, `SELECT family_id, title FROM public.family_series WHERE id = $1`, seriesID).
			Scan(&familyID, &title); err != nil {
			return err
		}

		if in.Status == "watched" {
			payload := map[string]any{"seriesId": seriesID, "familyId": familyID, "userId": userID, "title": title}
			if err := outbox.Insert(ctx, tx, "series", seriesID, "movie.watched", payload); err != nil {
				return err
			}
		}
		if in.Rating != nil {
			payload := map[string]any{"seriesId": seriesID, "familyId": familyID, "userId": userID, "title": title, "rating": *in.Rating}
			if err := outbox.Insert(ctx, tx, "series", seriesID, "movie.rated", payload); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if postgres.IsForeignKeyViolation(err) {
			return Status{}, apperror.NotFound("series not found")
		}
		return Status{}, apperror.Internal("failed to set status", err)
	}
	return result, nil
}

func (s *Service) GetStatus(ctx context.Context, userID, seriesID string) (Status, error) {
	var result Status
	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT series_id, user_id, status, rating, comment, watched_at
			FROM public.family_series_status
			WHERE series_id = $1 AND user_id = $2
		`, seriesID, userID)
		return row.Scan(&result.SeriesID, &result.UserID, &result.Status, &result.Rating, &result.Comment, &result.WatchedAt)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Status{}, apperror.NotFound("status not set")
		}
		return Status{}, apperror.Internal("failed to load status", err)
	}
	return result, nil
}

func (s *Service) DeleteStatus(ctx context.Context, userID, seriesID string) error {
	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM public.family_series_status WHERE series_id = $1 AND user_id = $2`, seriesID, userID)
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
			return apperror.NotFound("status not set")
		}
		return apperror.Internal("failed to delete status", err)
	}
	return nil
}

func RegisterStatusRoutes(mux *http.ServeMux, svc *Service) {
	mux.HandleFunc("PUT /api/v1/series/{seriesId}/status", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())

		var in SetStatusInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			response.Error(w, apperror.Invalid("invalid request body"))
			return
		}

		result, err := svc.SetStatus(r.Context(), user.ID, r.PathValue("seriesId"), in)
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, result)
	})

	mux.HandleFunc("GET /api/v1/series/{seriesId}/status", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())
		result, err := svc.GetStatus(r.Context(), user.ID, r.PathValue("seriesId"))
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, result)
	})

	mux.HandleFunc("DELETE /api/v1/series/{seriesId}/status", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())
		if err := svc.DeleteStatus(r.Context(), user.ID, r.PathValue("seriesId")); err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusNoContent, nil)
	})
}
