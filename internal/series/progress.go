package series

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"notrecinema/api/internal/auth"
	"notrecinema/api/internal/platform/apperror"
	"notrecinema/api/internal/platform/response"
	"notrecinema/api/internal/postgres"
)

type Progress struct {
	SeriesID       string `json:"seriesId"`
	UserID         string `json:"userId"`
	CurrentSeason  int    `json:"currentSeason"`
	CurrentEpisode int    `json:"currentEpisode"`
}

// ProgressEntry -- прогресс одного участника по одному сериалу вместе с
// именем (для списков "кто на какой серии").
type ProgressEntry struct {
	SeriesID       string    `json:"seriesId"`
	UserID         string    `json:"userId"`
	DisplayName    string    `json:"displayName"`
	CurrentSeason  int       `json:"currentSeason"`
	CurrentEpisode int       `json:"currentEpisode"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

const progressEntrySelect = `
	SELECT pr.series_id, pr.user_id, COALESCE(p.display_name, p.email), pr.current_season, pr.current_episode, pr.updated_at
	FROM public.family_series_progress pr
	JOIN public.profiles p ON p.id = pr.user_id`

func (s *Service) queryProgressEntries(ctx context.Context, userID, where string, arg any) ([]ProgressEntry, error) {
	list := []ProgressEntry{}
	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, progressEntrySelect+where+` ORDER BY pr.updated_at DESC`, arg)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e ProgressEntry
			if err := rows.Scan(&e.SeriesID, &e.UserID, &e.DisplayName, &e.CurrentSeason, &e.CurrentEpisode, &e.UpdatedAt); err != nil {
				return err
			}
			list = append(list, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, apperror.Internal("failed to list progress", err)
	}
	return list, nil
}

// ListSeriesProgress -- прогресс всех участников по одному сериалу.
func (s *Service) ListSeriesProgress(ctx context.Context, userID, seriesID string) ([]ProgressEntry, error) {
	return s.queryProgressEntries(ctx, userID, ` WHERE pr.series_id = $1`, seriesID)
}

// ListFamilyProgress -- прогресс всех участников по всем сериалам семьи
// одним запросом (главная страница иначе делала бы запрос на сериал).
func (s *Service) ListFamilyProgress(ctx context.Context, userID, familyID string) ([]ProgressEntry, error) {
	return s.queryProgressEntries(ctx, userID,
		` JOIN public.family_series fs ON fs.id = pr.series_id WHERE fs.family_id = $1`, familyID)
}

type SetProgressInput struct {
	CurrentSeason  int `json:"currentSeason"`
	CurrentEpisode int `json:"currentEpisode"`
}

func (s *Service) SetProgress(ctx context.Context, userID, seriesID string, in SetProgressInput) (Progress, error) {
	if in.CurrentSeason < 1 {
		return Progress{}, apperror.Invalid("currentSeason must be at least 1")
	}
	if in.CurrentEpisode < 0 {
		return Progress{}, apperror.Invalid("currentEpisode cannot be negative")
	}

	var result Progress
	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO public.family_series_progress (series_id, user_id, current_season, current_episode)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (series_id, user_id) DO UPDATE SET
				current_season = EXCLUDED.current_season,
				current_episode = EXCLUDED.current_episode
			RETURNING series_id, user_id, current_season, current_episode
		`, seriesID, userID, in.CurrentSeason, in.CurrentEpisode)
		return row.Scan(&result.SeriesID, &result.UserID, &result.CurrentSeason, &result.CurrentEpisode)
	})
	if err != nil {
		if postgres.IsForeignKeyViolation(err) {
			return Progress{}, apperror.NotFound("series not found")
		}
		return Progress{}, apperror.Internal("failed to set progress", err)
	}
	return result, nil
}

func (s *Service) GetProgress(ctx context.Context, userID, seriesID string) (Progress, error) {
	var result Progress
	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT series_id, user_id, current_season, current_episode
			FROM public.family_series_progress
			WHERE series_id = $1 AND user_id = $2
		`, seriesID, userID)
		return row.Scan(&result.SeriesID, &result.UserID, &result.CurrentSeason, &result.CurrentEpisode)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Progress{}, apperror.NotFound("progress not set")
		}
		return Progress{}, apperror.Internal("failed to load progress", err)
	}
	return result, nil
}

func RegisterProgressRoutes(mux *http.ServeMux, svc *Service) {
	mux.HandleFunc("PUT /api/v1/series/{seriesId}/progress", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())

		var in SetProgressInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			response.Error(w, apperror.Invalid("invalid request body"))
			return
		}

		result, err := svc.SetProgress(r.Context(), user.ID, r.PathValue("seriesId"), in)
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, result)
	})

	mux.HandleFunc("GET /api/v1/series/{seriesId}/progress/all", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())
		list, err := svc.ListSeriesProgress(r.Context(), user.ID, r.PathValue("seriesId"))
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, list)
	})

	mux.HandleFunc("GET /api/v1/families/{familyId}/progress", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())
		list, err := svc.ListFamilyProgress(r.Context(), user.ID, r.PathValue("familyId"))
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, list)
	})

	mux.HandleFunc("GET /api/v1/series/{seriesId}/progress", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())
		result, err := svc.GetProgress(r.Context(), user.ID, r.PathValue("seriesId"))
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, result)
	})
}
