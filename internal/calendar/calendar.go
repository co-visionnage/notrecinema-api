// Package calendar хранит и обновляет дату следующего эпизода для сериалов,
// импортированных из внешнего каталога (OMDb/IMDb-CSV) -- next_episode_
// air_date/label на family_series, миграция 0018_episode_calendar.sql.
// Прямой перенос notrecinema-app: checkNextEpisodeAction (по требованию
// пользователя, с троттлингом) и cron check-next-episode (массовое
// обновление всех привязанных сериалов). Отдельная фича от season-tracking
// (миграция 0010) -- здесь только одна ближайшая дата/лейбл, не смена
// total_seasons/total_episodes.
package calendar

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"notrecinema/api/internal/auth"
	"notrecinema/api/internal/platform/apperror"
	"notrecinema/api/internal/platform/response"
	"notrecinema/api/internal/postgres"
)

const (
	rateLimitMaxAttempts  = 20
	rateLimitWindowSecond = 600
	refreshConcurrency    = 5
)

type Entry struct {
	SeriesID           string  `json:"seriesId"`
	Title              string  `json:"title"`
	NextEpisodeAirDate *string `json:"nextEpisodeAirDate,omitempty"`
	NextEpisodeLabel   *string `json:"nextEpisodeLabel,omitempty"`
}

type Calendar struct {
	Upcoming  []Entry `json:"upcoming"`
	Checkable []Entry `json:"checkable"`
}

type Service struct {
	db         *postgres.Pool
	httpClient *http.Client
	apiKey     string
	logger     *slog.Logger
}

func NewService(db *postgres.Pool, apiKey string, logger *slog.Logger) *Service {
	return &Service{db: db, httpClient: http.DefaultClient, apiKey: apiKey, logger: logger}
}

func (s *Service) Configured() bool {
	return s.apiKey != ""
}

// GetCalendar возвращает сериалы семьи с уже известной датой следующего
// эпизода (upcoming, отсортированы по дате) и отдельно те, что можно
// проверить (привязаны к IMDb id, но даты ещё нет) -- то же деление, что
// делал клиент в notrecinema-app, только теперь на стороне сервера.
func (s *Service) GetCalendar(ctx context.Context, userID, familyID string) (Calendar, error) {
	var cal Calendar

	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, title, next_episode_air_date, next_episode_label
			FROM public.family_series
			WHERE family_id = $1 AND next_episode_air_date IS NOT NULL
			ORDER BY next_episode_air_date ASC
		`, familyID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var e Entry
			var airDate *time.Time
			if err := rows.Scan(&e.SeriesID, &e.Title, &airDate, &e.NextEpisodeLabel); err != nil {
				rows.Close()
				return err
			}
			if airDate != nil {
				formatted := airDate.Format("2006-01-02")
				e.NextEpisodeAirDate = &formatted
			}
			cal.Upcoming = append(cal.Upcoming, e)
		}
		if err := rows.Err(); err != nil {
			return err
		}

		checkRows, err := tx.Query(ctx, `
			SELECT id, title
			FROM public.family_series
			WHERE family_id = $1
			  AND next_episode_air_date IS NULL
			  AND external_source IN ('omdb', 'imdb-csv')
			  AND external_id IS NOT NULL
			  AND media_type = 'series'
		`, familyID)
		if err != nil {
			return err
		}
		defer checkRows.Close()
		for checkRows.Next() {
			var e Entry
			if err := checkRows.Scan(&e.SeriesID, &e.Title); err != nil {
				return err
			}
			cal.Checkable = append(cal.Checkable, e)
		}
		return checkRows.Err()
	})
	if err != nil {
		return Calendar{}, apperror.Internal("failed to get calendar", err)
	}

	if cal.Upcoming == nil {
		cal.Upcoming = []Entry{}
	}
	if cal.Checkable == nil {
		cal.Checkable = []Entry{}
	}
	return cal, nil
}

type seriesForCheck struct {
	ID             string
	FamilyID       string
	Title          string
	ExternalSource string
	ExternalID     string
}

// CheckNextEpisode обновляет дату следующего эпизода одного сериала по
// требованию пользователя. RLS внутри WithUserContext гарантирует, что
// сериал вообще виден вызывающему (иначе просто NotFound) -- сама запись
// в БД потом идёт через update_series_next_episode (SECURITY DEFINER), но
// до этого места чужой семьи дойти не может.
func (s *Service) CheckNextEpisode(ctx context.Context, userID, seriesID string) (Entry, error) {
	if !s.Configured() {
		return Entry{}, apperror.Invalid("календарь эпизодов не настроен (нет TMDB_API_KEY)")
	}

	var series seriesForCheck
	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		var externalSource, externalID *string
		err := tx.QueryRow(ctx, `
			SELECT id, family_id, title, external_source, external_id
			FROM public.family_series
			WHERE id = $1 AND media_type = 'series'
		`, seriesID).Scan(&series.ID, &series.FamilyID, &series.Title, &externalSource, &externalID)
		if err != nil {
			return err
		}
		if externalSource == nil || externalID == nil {
			return apperror.Invalid("у сериала нет привязки к внешнему каталогу")
		}
		if *externalSource != "omdb" && *externalSource != "imdb-csv" {
			return apperror.Invalid("календарь доступен только для сериалов, импортированных из OMDb/IMDb")
		}
		series.ExternalSource = *externalSource
		series.ExternalID = *externalID
		return nil
	})
	if err != nil {
		var appErr *apperror.Error
		if errors.As(err, &appErr) {
			return Entry{}, appErr
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return Entry{}, apperror.NotFound("сериал не найден")
		}
		return Entry{}, apperror.Internal("failed to load series", err)
	}

	// Бакет "season-check:user:" -- тот же, что и у internal/seasons
	// (CheckSeriesUpdates): в notrecinema-app обе разовые проверки по
	// внешним API делят одну квоту, а не считаются раздельно.
	var allowed bool
	if err := s.db.QueryRow(ctx, `
		SELECT public.check_rate_limit($1, $2, $3)
	`, "season-check:user:"+userID, rateLimitMaxAttempts, rateLimitWindowSecond).Scan(&allowed); err != nil {
		return Entry{}, apperror.Internal("failed to check rate limit", err)
	}
	if !allowed {
		return Entry{}, apperror.RateLimited("слишком много проверок, попробуйте позже")
	}

	next, err := findNextEpisode(ctx, s.httpClient, s.apiKey, series.ExternalID)
	if err != nil {
		return Entry{}, apperror.Internal("failed to query tmdb", err)
	}

	entry := Entry{SeriesID: series.ID, Title: series.Title}
	if next == nil {
		return entry, nil
	}

	if err := s.updateNextEpisode(ctx, series.ID, next); err != nil {
		return Entry{}, apperror.Internal("failed to store next episode", err)
	}
	entry.NextEpisodeAirDate = &next.AirDate
	entry.NextEpisodeLabel = &next.Label
	return entry, nil
}

func (s *Service) updateNextEpisode(ctx context.Context, seriesID string, next *nextEpisode) error {
	airDate, err := time.Parse("2006-01-02", next.AirDate)
	if err != nil {
		return err
	}

	rows, err := s.db.Query(ctx, `
		SELECT public.update_series_next_episode($1, $2, $3)
	`, seriesID, airDate, next.Label)
	if err != nil {
		return err
	}
	rows.Close()
	return rows.Err()
}

// RefreshAll обновляет next_episode_air_date/label для всех сериалов
// семей, привязанных к OMDb/IMDb-CSV -- массовый эквивалент cron
// check-next-episode. Concurrency ограничена, чтобы не долбить TMDB всеми
// сериалами платформы одновременно.
func (s *Service) RefreshAll(ctx context.Context) error {
	if !s.Configured() {
		return nil
	}

	rows, err := s.db.Query(ctx, `
		SELECT id, family_id, title, external_source, external_id
		FROM public.get_series_for_episode_check()
	`)
	if err != nil {
		return err
	}
	var all []seriesForCheck
	for rows.Next() {
		var sc seriesForCheck
		if err := rows.Scan(&sc.ID, &sc.FamilyID, &sc.Title, &sc.ExternalSource, &sc.ExternalID); err != nil {
			rows.Close()
			return err
		}
		all = append(all, sc)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	sem := make(chan struct{}, refreshConcurrency)
	results := make(chan error, len(all))
	for _, sc := range all {
		sem <- struct{}{}
		go func(sc seriesForCheck) {
			defer func() { <-sem }()
			next, err := findNextEpisode(ctx, s.httpClient, s.apiKey, sc.ExternalID)
			if err != nil {
				s.logger.Warn("calendar: не удалось обновить сериал", "series_id", sc.ID, "error", err)
				results <- nil
				return
			}
			if next == nil {
				results <- nil
				return
			}
			results <- s.updateNextEpisode(ctx, sc.ID, next)
		}(sc)
	}
	for range all {
		if err := <-results; err != nil {
			s.logger.Warn("calendar: не удалось сохранить дату эпизода", "error", err)
		}
	}
	return nil
}

// RunPeriodicRefresh запускает RefreshAll раз в interval, блокируя
// вызывающего до отмены ctx -- предназначен для отдельной goroutine, тот
// же паттерн, что outbox.Poller и nudges.Checker.
func (s *Service) RunPeriodicRefresh(ctx context.Context, interval time.Duration) {
	if !s.Configured() {
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.RefreshAll(ctx); err != nil {
				s.logger.Error("calendar: ошибка массового обновления", "error", err)
			}
		}
	}
}

func RegisterRoutes(mux *http.ServeMux, svc *Service) {
	mux.HandleFunc("GET /api/v1/families/{familyId}/calendar", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())
		familyID := r.PathValue("familyId")

		cal, err := svc.GetCalendar(r.Context(), user.ID, familyID)
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, cal)
	})

	mux.HandleFunc("POST /api/v1/series/{seriesId}/next-episode/check", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())
		seriesID := r.PathValue("seriesId")

		entry, err := svc.CheckNextEpisode(r.Context(), user.ID, seriesID)
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, entry)
	})
}
