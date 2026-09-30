// Package series реализует вертикальный срез «сериалы семьи»: сам сериал
// (family_series) и всё, что к нему привязано, -- статус просмотра,
// прогресс по эпизодам, комментарии, реакции. Видимость и права
// проверяются RLS-политиками, привязанными к членству в семье через
// public.is_family_member -- см. database/migrations в notrecinema-schema.
package series

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"notrecinema/api/internal/activitylog"
	"notrecinema/api/internal/auth"
	"notrecinema/api/internal/outbox"
	"notrecinema/api/internal/platform/apperror"
	"notrecinema/api/internal/platform/response"
	"notrecinema/api/internal/postgres"
)

type Series struct {
	ID                    string   `json:"id"`
	FamilyID              string   `json:"familyId"`
	Title                 string   `json:"title"`
	Genres                []string `json:"genres"`
	Year                  *int     `json:"year,omitempty"`
	ImageURL              *string  `json:"imageUrl,omitempty"`
	CreatedBy             string   `json:"createdBy"`
	TotalSeasons          *int     `json:"totalSeasons,omitempty"`
	TotalEpisodes         *int     `json:"totalEpisodes,omitempty"`
	EpisodeRuntimeMinutes *int     `json:"episodeRuntimeMinutes,omitempty"`
}

type Service struct {
	db *postgres.Pool
}

func NewService(db *postgres.Pool) *Service {
	return &Service{db: db}
}

const seriesColumns = `id, family_id, title, genres, year, image_url, created_by, total_seasons, total_episodes, episode_runtime_minutes`

func scanSeries(row pgx.Row) (Series, error) {
	var s Series
	err := row.Scan(&s.ID, &s.FamilyID, &s.Title, &s.Genres, &s.Year, &s.ImageURL, &s.CreatedBy,
		&s.TotalSeasons, &s.TotalEpisodes, &s.EpisodeRuntimeMinutes)
	return s, err
}

type CreateInput struct {
	Title                 string   `json:"title"`
	Genres                []string `json:"genres"`
	Year                  *int     `json:"year"`
	ImageURL              *string  `json:"imageUrl"`
	TotalSeasons          *int     `json:"totalSeasons"`
	TotalEpisodes         *int     `json:"totalEpisodes"`
	EpisodeRuntimeMinutes *int     `json:"episodeRuntimeMinutes"`
}

func (s *Service) Create(ctx context.Context, userID, familyID string, in CreateInput) (Series, error) {
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return Series{}, apperror.Invalid("title is required")
	}
	if len(title) > 255 {
		return Series{}, apperror.Invalid("title must be at most 255 characters")
	}
	if in.Genres == nil {
		in.Genres = []string{}
	}

	var created Series
	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO public.family_series
				(family_id, title, genres, year, image_url, created_by, total_seasons, total_episodes, episode_runtime_minutes)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			RETURNING `+seriesColumns, familyID, title, in.Genres, in.Year, in.ImageURL, userID,
			in.TotalSeasons, in.TotalEpisodes, in.EpisodeRuntimeMinutes)

		var err error
		created, err = scanSeries(row)
		if err != nil {
			return err
		}

		targetLabel := created.Title
		if err := activitylog.Insert(ctx, tx, familyID, userID, "участник", "series.added", &targetLabel, nil); err != nil {
			return err
		}

		payload := map[string]any{"seriesId": created.ID, "familyId": familyID, "userId": userID, "title": created.Title}
		return outbox.Insert(ctx, tx, "series", created.ID, "movie.added", payload)
	})
	if err != nil {
		return Series{}, apperror.Internal("failed to create series", err)
	}
	return created, nil
}

func (s *Service) ListForFamily(ctx context.Context, userID, familyID string) ([]Series, error) {
	var list []Series

	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+seriesColumns+`
			FROM public.family_series
			WHERE family_id = $1
			ORDER BY created_at DESC
		`, familyID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			item, err := scanSeries(rows)
			if err != nil {
				return err
			}
			list = append(list, item)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, apperror.Internal("failed to list series", err)
	}

	if list == nil {
		list = []Series{}
	}
	return list, nil
}

func (s *Service) Get(ctx context.Context, userID, seriesID string) (Series, error) {
	var item Series
	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+seriesColumns+` FROM public.family_series WHERE id = $1`, seriesID)
		var err error
		item, err = scanSeries(row)
		return err
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Series{}, apperror.NotFound("series not found")
		}
		return Series{}, apperror.Internal("failed to load series", err)
	}
	return item, nil
}

type UpdateInput struct {
	Title                 *string   `json:"title"`
	Genres                *[]string `json:"genres"`
	Year                  *int      `json:"year"`
	ImageURL              *string   `json:"imageUrl"`
	TotalSeasons          *int      `json:"totalSeasons"`
	TotalEpisodes         *int      `json:"totalEpisodes"`
	EpisodeRuntimeMinutes *int      `json:"episodeRuntimeMinutes"`
}

func (s *Service) Update(ctx context.Context, userID, seriesID string, in UpdateInput) (Series, error) {
	if in.Title != nil {
		trimmed := strings.TrimSpace(*in.Title)
		if trimmed == "" {
			return Series{}, apperror.Invalid("title cannot be blank")
		}
		in.Title = &trimmed
	}

	var updated Series
	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE public.family_series SET
				title = COALESCE($2, title),
				genres = COALESCE($3, genres),
				year = COALESCE($4, year),
				image_url = COALESCE($5, image_url),
				total_seasons = COALESCE($6, total_seasons),
				total_episodes = COALESCE($7, total_episodes),
				episode_runtime_minutes = COALESCE($8, episode_runtime_minutes)
			WHERE id = $1
			RETURNING `+seriesColumns,
			seriesID, in.Title, in.Genres, in.Year, in.ImageURL,
			in.TotalSeasons, in.TotalEpisodes, in.EpisodeRuntimeMinutes)

		var err error
		updated, err = scanSeries(row)
		return err
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Series{}, apperror.NotFound("series not found")
		}
		return Series{}, apperror.Internal("failed to update series", err)
	}
	return updated, nil
}

func (s *Service) Delete(ctx context.Context, userID, seriesID string) error {
	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		var familyID, title string
		row := tx.QueryRow(ctx, `SELECT family_id, title FROM public.family_series WHERE id = $1`, seriesID)
		if err := row.Scan(&familyID, &title); err != nil {
			return err
		}

		tag, err := tx.Exec(ctx, `DELETE FROM public.family_series WHERE id = $1`, seriesID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}

		return activitylog.Insert(ctx, tx, familyID, userID, "участник", "series.removed", &title, nil)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apperror.NotFound("series not found")
		}
		return apperror.Internal("failed to delete series", err)
	}
	return nil
}

func RegisterRoutes(mux *http.ServeMux, svc *Service) {
	mux.HandleFunc("POST /api/v1/families/{familyId}/series", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())
		familyID := r.PathValue("familyId")

		var in CreateInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			response.Error(w, apperror.Invalid("invalid request body"))
			return
		}

		created, err := svc.Create(r.Context(), user.ID, familyID, in)
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusCreated, created)
	})

	mux.HandleFunc("GET /api/v1/families/{familyId}/series", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())
		familyID := r.PathValue("familyId")

		list, err := svc.ListForFamily(r.Context(), user.ID, familyID)
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, list)
	})

	mux.HandleFunc("GET /api/v1/series/{seriesId}", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())
		item, err := svc.Get(r.Context(), user.ID, r.PathValue("seriesId"))
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, item)
	})

	mux.HandleFunc("PATCH /api/v1/series/{seriesId}", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())

		var in UpdateInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			response.Error(w, apperror.Invalid("invalid request body"))
			return
		}

		updated, err := svc.Update(r.Context(), user.ID, r.PathValue("seriesId"), in)
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, updated)
	})

	mux.HandleFunc("DELETE /api/v1/series/{seriesId}", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())
		if err := svc.Delete(r.Context(), user.ID, r.PathValue("seriesId")); err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusNoContent, nil)
	})
}
