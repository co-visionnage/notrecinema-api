package series

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"notrecinema/api/internal/auth"
	"notrecinema/api/internal/platform/apperror"
	"notrecinema/api/internal/platform/response"
	"notrecinema/api/internal/postgres"
)

type Comment struct {
	ID        string    `json:"id"`
	SeriesID  string    `json:"seriesId"`
	UserID    string    `json:"userId"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
}

type CreateCommentInput struct {
	Body string `json:"body"`
}

func (s *Service) AddComment(ctx context.Context, userID, seriesID string, in CreateCommentInput) (Comment, error) {
	body := strings.TrimSpace(in.Body)
	if body == "" {
		return Comment{}, apperror.Invalid("body is required")
	}
	if len(body) > 2000 {
		return Comment{}, apperror.Invalid("body must be at most 2000 characters")
	}

	var comment Comment
	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO public.family_series_comments (series_id, user_id, body)
			VALUES ($1, $2, $3)
			RETURNING id, series_id, user_id, body, created_at
		`, seriesID, userID, body)
		return row.Scan(&comment.ID, &comment.SeriesID, &comment.UserID, &comment.Body, &comment.CreatedAt)
	})
	if err != nil {
		if postgres.IsForeignKeyViolation(err) {
			return Comment{}, apperror.NotFound("series not found")
		}
		return Comment{}, apperror.Internal("failed to add comment", err)
	}
	return comment, nil
}

func (s *Service) ListComments(ctx context.Context, userID, seriesID string) ([]Comment, error) {
	var list []Comment
	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, series_id, user_id, body, created_at
			FROM public.family_series_comments
			WHERE series_id = $1
			ORDER BY created_at ASC
		`, seriesID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var c Comment
			if err := rows.Scan(&c.ID, &c.SeriesID, &c.UserID, &c.Body, &c.CreatedAt); err != nil {
				return err
			}
			list = append(list, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, apperror.Internal("failed to list comments", err)
	}
	if list == nil {
		list = []Comment{}
	}
	return list, nil
}

// DeleteComment полагается на RLS-политику
// family_series_comments_delete_author_or_owner: пытаться заранее отличить
// "не автор" от "не владелец семьи" в Go не нужно, DELETE просто не
// затронет строку, если ни одно из условий не выполнено.
func (s *Service) DeleteComment(ctx context.Context, userID, commentID string) error {
	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM public.family_series_comments WHERE id = $1`, commentID)
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
			return apperror.NotFound("comment not found")
		}
		return apperror.Internal("failed to delete comment", err)
	}
	return nil
}

func RegisterCommentRoutes(mux *http.ServeMux, svc *Service) {
	mux.HandleFunc("POST /api/v1/series/{seriesId}/comments", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())

		var in CreateCommentInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			response.Error(w, apperror.Invalid("invalid request body"))
			return
		}

		comment, err := svc.AddComment(r.Context(), user.ID, r.PathValue("seriesId"), in)
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusCreated, comment)
	})

	mux.HandleFunc("GET /api/v1/series/{seriesId}/comments", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())
		list, err := svc.ListComments(r.Context(), user.ID, r.PathValue("seriesId"))
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, list)
	})

	mux.HandleFunc("DELETE /api/v1/comments/{commentId}", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())
		if err := svc.DeleteComment(r.Context(), user.ID, r.PathValue("commentId")); err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusNoContent, nil)
	})
}
