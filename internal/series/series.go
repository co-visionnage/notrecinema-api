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
	"time"

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

	MediaType          string    `json:"mediaType"`
	TrailerURL         *string   `json:"trailerUrl,omitempty"`
	ExternalSource     *string   `json:"externalSource,omitempty"`
	ExternalID         *string   `json:"externalId,omitempty"`
	NextEpisodeAirDate *string   `json:"nextEpisodeAirDate,omitempty"`
	NextEpisodeLabel   *string   `json:"nextEpisodeLabel,omitempty"`
	CreatedAt          time.Time `json:"createdAt"`

	// Статус просмотра вызывающего пользователя (у каждого участника свой).
	// Без записи в family_series_status сериал считается "to-watch".
	Status    string     `json:"status"`
	Rating    *int       `json:"rating,omitempty"`
	Comment   *string    `json:"comment,omitempty"`
	WatchedAt *time.Time `json:"watchedAt,omitempty"`
}

type Service struct {
	db *postgres.Pool
}

func NewService(db *postgres.Pool) *Service {
	return &Service{db: db}
}

// seriesSelect -- общий SELECT: сам сериал плюс статус вызывающего
// пользователя ($1 -- id пользователя, подставляется первым параметром).
const seriesSelect = `
	s.id, s.family_id, s.title, s.genres, s.year, s.image_url, s.created_by,
	s.total_seasons, s.total_episodes, s.episode_runtime_minutes,
	s.media_type, s.trailer_url, s.external_source, s.external_id,
	to_char(s.next_episode_air_date, 'YYYY-MM-DD'), s.next_episode_label, s.created_at,
	COALESCE(st.status, 'to-watch'), st.rating, st.comment, st.watched_at
	FROM public.family_series s
	LEFT JOIN public.family_series_status st ON st.series_id = s.id AND st.user_id = $1`

// seriesReturning -- RETURNING для INSERT/UPDATE: статус нужен только как
// заглушка, реальный подтягивает reload.
const seriesReturning = `id`

func scanSeries(row pgx.Row) (Series, error) {
	var s Series
	err := row.Scan(&s.ID, &s.FamilyID, &s.Title, &s.Genres, &s.Year, &s.ImageURL, &s.CreatedBy,
		&s.TotalSeasons, &s.TotalEpisodes, &s.EpisodeRuntimeMinutes,
		&s.MediaType, &s.TrailerURL, &s.ExternalSource, &s.ExternalID,
		&s.NextEpisodeAirDate, &s.NextEpisodeLabel, &s.CreatedAt,
		&s.Status, &s.Rating, &s.Comment, &s.WatchedAt)
	return s, err
}

// loadSeries читает сериал вместе со статусом пользователя внутри
// уже открытой транзакции.
func loadSeries(ctx context.Context, tx pgx.Tx, userID, seriesID string) (Series, error) {
	return scanSeries(tx.QueryRow(ctx, `SELECT `+seriesSelect+` WHERE s.id = $2`, userID, seriesID))
}

type CreateInput struct {
	Title                 string   `json:"title"`
	Genres                []string `json:"genres"`
	Year                  *int     `json:"year"`
	ImageURL              *string  `json:"imageUrl"`
	TotalSeasons          *int     `json:"totalSeasons"`
	TotalEpisodes         *int     `json:"totalEpisodes"`
	EpisodeRuntimeMinutes *int     `json:"episodeRuntimeMinutes"`

	MediaType      *string `json:"mediaType"`
	TrailerURL     *string `json:"trailerUrl"`
	ExternalSource *string `json:"externalSource"`
	ExternalID     *string `json:"externalId"`

	// Status/Rating/Comment -- необязательный стартовый статус создателя
	// ("watched" при добавлении уже просмотренного).
	Status  *string `json:"status"`
	Rating  *int    `json:"rating"`
	Comment *string `json:"comment"`
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
	mediaType := "series"
	if in.MediaType != nil && *in.MediaType != "" {
		if *in.MediaType != "series" && *in.MediaType != "movie" {
			return Series{}, apperror.Invalid("mediaType must be 'series' or 'movie'")
		}
		mediaType = *in.MediaType
	}
	status := "to-watch"
	if in.Status != nil && *in.Status != "" {
		if *in.Status != "watched" && *in.Status != "to-watch" {
			return Series{}, apperror.Invalid("status must be 'watched' or 'to-watch'")
		}
		status = *in.Status
	}
	if in.Rating != nil && (*in.Rating < 1 || *in.Rating > 5) {
		return Series{}, apperror.Invalid("rating must be between 1 and 5")
	}

	var created Series
	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		var newID string
		err := tx.QueryRow(ctx, `
			INSERT INTO public.family_series
				(family_id, title, genres, year, image_url, created_by, total_seasons, total_episodes,
				 episode_runtime_minutes, media_type, trailer_url, external_source, external_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
			RETURNING `+seriesReturning, familyID, title, in.Genres, in.Year, in.ImageURL, userID,
			in.TotalSeasons, in.TotalEpisodes, in.EpisodeRuntimeMinutes,
			mediaType, in.TrailerURL, in.ExternalSource, in.ExternalID).Scan(&newID)
		if err != nil {
			return err
		}

		if status == "watched" {
			if _, err := tx.Exec(ctx, `
				INSERT INTO public.family_series_status (series_id, user_id, status, rating, comment, watched_at)
				VALUES ($1, $2, 'watched', $3, $4, NOW())`, newID, userID, in.Rating, in.Comment); err != nil {
				return err
			}
		}

		created, err = loadSeries(ctx, tx, userID, newID)
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
			SELECT `+seriesSelect+`
			WHERE s.family_id = $2
			ORDER BY s.created_at DESC
		`, userID, familyID)
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
		var err error
		item, err = loadSeries(ctx, tx, userID, seriesID)
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
	TrailerURL            *string   `json:"trailerUrl"`
	MediaType             *string   `json:"mediaType"`

	// Rating/Comment относятся к статусу вызывающего пользователя, а не к
	// самому сериалу: меняются в его строке family_series_status.
	Rating  *int    `json:"rating"`
	Comment *string `json:"comment"`
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
		var id string
		err := tx.QueryRow(ctx, `
			UPDATE public.family_series SET
				title = COALESCE($2, title),
				genres = COALESCE($3, genres),
				year = COALESCE($4, year),
				image_url = COALESCE($5, image_url),
				total_seasons = COALESCE($6, total_seasons),
				total_episodes = COALESCE($7, total_episodes),
				episode_runtime_minutes = COALESCE($8, episode_runtime_minutes),
				trailer_url = COALESCE($9, trailer_url),
				media_type = COALESCE($10, media_type)
			WHERE id = $1
			RETURNING `+seriesReturning,
			seriesID, in.Title, in.Genres, in.Year, in.ImageURL,
			in.TotalSeasons, in.TotalEpisodes, in.EpisodeRuntimeMinutes,
			in.TrailerURL, in.MediaType).Scan(&id)
		if err != nil {
			return err
		}

		if in.Rating != nil || in.Comment != nil {
			// Оценка и комментарий живут в статусе пользователя; если строки
			// ещё нет, создаётся "to-watch" с этими полями.
			if _, err := tx.Exec(ctx, `
				INSERT INTO public.family_series_status (series_id, user_id, status, rating, comment)
				VALUES ($1, $2, 'to-watch', $3, $4)
				ON CONFLICT (series_id, user_id) DO UPDATE SET
					rating = COALESCE($3, family_series_status.rating),
					comment = COALESCE($4, family_series_status.comment)`,
				seriesID, userID, in.Rating, in.Comment); err != nil {
				return err
			}
		}

		updated, err = loadSeries(ctx, tx, userID, seriesID)
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
