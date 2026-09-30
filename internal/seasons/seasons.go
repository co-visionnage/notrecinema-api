// Package seasons отслеживает выход новых сезонов для сериалов,
// импортированных из Kinopoisk/OMDb -- прямой перенос check-new-seasons
// (cron) и checkSeriesUpdatesAction (разовая проверка по запросу) из
// notrecinema-app. В отличие от internal/calendar (миграция 0018, дата
// следующего эпизода), здесь отслеживается total_seasons/total_episodes
// (миграция 0010) и есть настоящее уведомление семьи при увеличении
// total_seasons -- не просто сохранение даты.
package seasons

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"notrecinema/api/internal/auth"
	"notrecinema/api/internal/outbox"
	"notrecinema/api/internal/platform/apperror"
	"notrecinema/api/internal/platform/response"
	"notrecinema/api/internal/postgres"
)

const (
	// Тот же бакет и лимит, что и у internal/calendar.CheckNextEpisode --
	// в оригинале обе разовые проверки по внешним API делят одну квоту.
	rateLimitMaxAttempts  = 20
	rateLimitWindowSecond = 600
	refreshConcurrency    = 5
)

type CheckResult struct {
	SeriesID        string `json:"seriesId"`
	Updated         bool   `json:"updated"`
	NewTotalSeasons *int   `json:"newTotalSeasons,omitempty"`
}

type seasonUpdatedPayload struct {
	SeriesID      string  `json:"seriesId"`
	FamilyID      string  `json:"familyId"`
	Title         string  `json:"title"`
	TotalSeasons  int     `json:"totalSeasons"`
	ExcludeUserID *string `json:"excludeUserId,omitempty"`
}

type Service struct {
	db              *postgres.Pool
	httpClient      *http.Client
	kinopoiskAPIKey string
	omdbAPIKey      string
	logger          *slog.Logger
}

func NewService(db *postgres.Pool, kinopoiskAPIKey, omdbAPIKey string, logger *slog.Logger) *Service {
	return &Service{db: db, httpClient: http.DefaultClient, kinopoiskAPIKey: kinopoiskAPIKey, omdbAPIKey: omdbAPIKey, logger: logger}
}

func (s *Service) Configured() bool {
	return s.kinopoiskAPIKey != "" || s.omdbAPIKey != ""
}

type seriesForCheck struct {
	ID             string
	FamilyID       string
	Title          string
	ExternalSource string
	ExternalID     string
	TotalSeasons   *int
}

// CheckSeriesUpdates — разовая проверка по требованию пользователя. В
// отличие от RefreshAll, исключает самого пользователя из уведомления (он
// и так только что увидел результат на экране) -- тот же выбор, что в
// notrecinema-app.
func (s *Service) CheckSeriesUpdates(ctx context.Context, userID, seriesID string) (CheckResult, error) {
	series, err := s.loadSeriesForCheck(ctx, userID, seriesID)
	if err != nil {
		return CheckResult{}, err
	}

	var allowed bool
	if err := s.db.QueryRow(ctx, `
		SELECT public.check_rate_limit($1, $2, $3)
	`, "season-check:user:"+userID, rateLimitMaxAttempts, rateLimitWindowSecond).Scan(&allowed); err != nil {
		return CheckResult{}, apperror.Internal("failed to check rate limit", err)
	}
	if !allowed {
		return CheckResult{}, apperror.RateLimited("слишком много проверок, попробуйте позже")
	}

	fresh, err := fetchCurrentSeasonInfo(ctx, s.httpClient, s.kinopoiskAPIKey, s.omdbAPIKey, series.ExternalSource, series.ExternalID)
	if err != nil {
		return CheckResult{}, apperror.Internal("failed to query external api", err)
	}
	if fresh == nil || fresh.TotalSeasons == 0 {
		return CheckResult{SeriesID: series.ID}, nil
	}
	if series.TotalSeasons != nil && fresh.TotalSeasons <= *series.TotalSeasons {
		return CheckResult{SeriesID: series.ID}, nil
	}

	err = s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE public.family_series
			SET total_seasons = $2, total_episodes = COALESCE($3, total_episodes)
			WHERE id = $1
		`, series.ID, fresh.TotalSeasons, fresh.TotalEpisodes); err != nil {
			return err
		}
		return outbox.Insert(ctx, tx, "series", series.ID, "season.updated", seasonUpdatedPayload{
			SeriesID:      series.ID,
			FamilyID:      series.FamilyID,
			Title:         series.Title,
			TotalSeasons:  fresh.TotalSeasons,
			ExcludeUserID: &userID,
		})
	})
	if err != nil {
		return CheckResult{}, apperror.Internal("failed to store season update", err)
	}

	totalSeasons := fresh.TotalSeasons
	return CheckResult{SeriesID: series.ID, Updated: true, NewTotalSeasons: &totalSeasons}, nil
}

func (s *Service) loadSeriesForCheck(ctx context.Context, userID, seriesID string) (seriesForCheck, error) {
	var series seriesForCheck
	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		var externalSource, externalID *string
		err := tx.QueryRow(ctx, `
			SELECT id, family_id, title, external_source, external_id, total_seasons
			FROM public.family_series
			WHERE id = $1 AND media_type = 'series'
		`, seriesID).Scan(&series.ID, &series.FamilyID, &series.Title, &externalSource, &externalID, &series.TotalSeasons)
		if err != nil {
			return err
		}
		if externalSource == nil || externalID == nil {
			return apperror.Invalid("сериал не привязан к внешнему источнику — добавлен вручную, а не через импорт")
		}
		series.ExternalSource = *externalSource
		series.ExternalID = *externalID
		return nil
	})
	if err != nil {
		var appErr *apperror.Error
		if errors.As(err, &appErr) {
			return seriesForCheck{}, appErr
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return seriesForCheck{}, apperror.NotFound("сериал не найден")
		}
		return seriesForCheck{}, apperror.Internal("failed to load series", err)
	}
	return series, nil
}

// RefreshAll — массовый эквивалент cron check-new-seasons: проходит по
// всем сериалам с внешней привязкой, не исключая никого из уведомления
// (ExcludeUserID == nil -- никто это не инициировал лично).
func (s *Service) RefreshAll(ctx context.Context) error {
	if !s.Configured() {
		return nil
	}

	rows, err := s.db.Query(ctx, `
		SELECT id, family_id, title, external_source, external_id, total_seasons
		FROM public.get_series_with_external_ids()
	`)
	if err != nil {
		return err
	}
	var all []seriesForCheck
	for rows.Next() {
		var sc seriesForCheck
		if err := rows.Scan(&sc.ID, &sc.FamilyID, &sc.Title, &sc.ExternalSource, &sc.ExternalID, &sc.TotalSeasons); err != nil {
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
			results <- s.refreshOne(ctx, sc)
		}(sc)
	}
	for range all {
		if err := <-results; err != nil {
			s.logger.Warn("seasons: не удалось обновить сериал", "error", err)
		}
	}
	return nil
}

func (s *Service) refreshOne(ctx context.Context, sc seriesForCheck) error {
	fresh, err := fetchCurrentSeasonInfo(ctx, s.httpClient, s.kinopoiskAPIKey, s.omdbAPIKey, sc.ExternalSource, sc.ExternalID)
	if err != nil {
		s.logger.Warn("seasons: ошибка запроса к внешнему api", "series_id", sc.ID, "error", err)
		return nil
	}
	if fresh == nil || fresh.TotalSeasons == 0 {
		return nil
	}
	if sc.TotalSeasons != nil && fresh.TotalSeasons <= *sc.TotalSeasons {
		return nil
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		SELECT public.update_series_season_tracking($1, $2, $3)
	`, sc.ID, fresh.TotalSeasons, fresh.TotalEpisodes); err != nil {
		return err
	}
	if err := outbox.Insert(ctx, tx, "series", sc.ID, "season.updated", seasonUpdatedPayload{
		SeriesID:     sc.ID,
		FamilyID:     sc.FamilyID,
		Title:        sc.Title,
		TotalSeasons: fresh.TotalSeasons,
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RunPeriodicRefresh запускает RefreshAll раз в interval, тот же паттерн,
// что outbox.Poller, nudges.Checker и calendar.RunPeriodicRefresh.
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
				s.logger.Error("seasons: ошибка массового обновления", "error", err)
			}
		}
	}
}

func RegisterRoutes(mux *http.ServeMux, svc *Service) {
	mux.HandleFunc("POST /api/v1/series/{seriesId}/season-updates/check", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())
		seriesID := r.PathValue("seriesId")

		result, err := svc.CheckSeriesUpdates(r.Context(), user.ID, seriesID)
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, result)
	})
}
