package series

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5"

	"notrecinema/api/internal/auth"
	"notrecinema/api/internal/platform/apperror"
	"notrecinema/api/internal/platform/response"
	"notrecinema/api/internal/postgres"
)

type Reaction struct {
	SeriesID string `json:"seriesId"`
	UserID   string `json:"userId"`
	Emoji    string `json:"emoji"`
}

type AddReactionInput struct {
	Emoji string `json:"emoji"`
}

// AddReaction идемпотентна по UNIQUE (series_id, user_id, emoji): повторная
// простановка той же реакции тем же пользователем -- не ошибка, а no-op.
func (s *Service) AddReaction(ctx context.Context, userID, seriesID string, in AddReactionInput) (Reaction, error) {
	if in.Emoji == "" {
		return Reaction{}, apperror.Invalid("emoji is required")
	}
	if len([]rune(in.Emoji)) > 8 {
		return Reaction{}, apperror.Invalid("emoji is too long")
	}

	reaction := Reaction{SeriesID: seriesID, UserID: userID, Emoji: in.Emoji}
	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.family_series_reactions (series_id, user_id, emoji)
			VALUES ($1, $2, $3)
			ON CONFLICT (series_id, user_id, emoji) DO NOTHING
		`, seriesID, userID, in.Emoji)
		return err
	})
	if err != nil {
		if postgres.IsForeignKeyViolation(err) {
			return Reaction{}, apperror.NotFound("series not found")
		}
		return Reaction{}, apperror.Internal("failed to add reaction", err)
	}
	return reaction, nil
}

func (s *Service) RemoveReaction(ctx context.Context, userID, seriesID, emoji string) error {
	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			DELETE FROM public.family_series_reactions
			WHERE series_id = $1 AND user_id = $2 AND emoji = $3
		`, seriesID, userID, emoji)
		return err
	})
	if err != nil {
		return apperror.Internal("failed to remove reaction", err)
	}
	return nil
}

func (s *Service) ListReactions(ctx context.Context, userID, seriesID string) ([]Reaction, error) {
	var list []Reaction
	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT series_id, user_id, emoji
			FROM public.family_series_reactions
			WHERE series_id = $1
			ORDER BY created_at ASC
		`, seriesID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var r Reaction
			if err := rows.Scan(&r.SeriesID, &r.UserID, &r.Emoji); err != nil {
				return err
			}
			list = append(list, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, apperror.Internal("failed to list reactions", err)
	}
	if list == nil {
		list = []Reaction{}
	}
	return list, nil
}

func RegisterReactionRoutes(mux *http.ServeMux, svc *Service) {
	mux.HandleFunc("POST /api/v1/series/{seriesId}/reactions", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())

		var in AddReactionInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			response.Error(w, apperror.Invalid("invalid request body"))
			return
		}

		reaction, err := svc.AddReaction(r.Context(), user.ID, r.PathValue("seriesId"), in)
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusCreated, reaction)
	})

	mux.HandleFunc("GET /api/v1/series/{seriesId}/reactions", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())
		list, err := svc.ListReactions(r.Context(), user.ID, r.PathValue("seriesId"))
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, list)
	})

	mux.HandleFunc("DELETE /api/v1/series/{seriesId}/reactions/{emoji}", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())
		if err := svc.RemoveReaction(r.Context(), user.ID, r.PathValue("seriesId"), r.PathValue("emoji")); err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusNoContent, nil)
	})
}
