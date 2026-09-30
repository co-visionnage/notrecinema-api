package stats

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"notrecinema/api/internal/platform/apperror"
)

type YearWrapped struct {
	Year              int     `json:"year"`
	TotalHoursWatched int     `json:"totalHoursWatched"`
	TotalWatchedCount int     `json:"totalWatchedCount"`
	TopGenre          *string `json:"topGenre,omitempty"`
	BestMonth         *string `json:"bestMonth,omitempty"`
	TopRatedTitle     *string `json:"topRatedTitle,omitempty"`
}

func (s *Service) GetYearWrapped(ctx context.Context, userID, familyID string, year int) (YearWrapped, error) {
	result := YearWrapped{Year: year}

	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		var totalHours float64
		if err := tx.QueryRow(ctx, `
			SELECT
				COALESCE(SUM(COALESCE(series.total_episodes, 1) * COALESCE(series.episode_runtime_minutes, 45)) / 60.0, 0),
				COUNT(*)
			FROM public.family_series_status status
			JOIN public.family_series series ON series.id = status.series_id
			WHERE series.family_id = $1
				AND status.status = 'watched'
				AND EXTRACT(YEAR FROM COALESCE(status.watched_at, status.updated_at)) = $2
		`, familyID, year).Scan(&totalHours, &result.TotalWatchedCount); err != nil {
			return err
		}
		result.TotalHoursWatched = int(totalHours + 0.5)

		if err := tx.QueryRow(ctx, `
			SELECT genre
			FROM public.family_series series
			JOIN public.family_series_status status ON status.series_id = series.id
			CROSS JOIN LATERAL UNNEST(series.genres) AS genre
			WHERE series.family_id = $1
				AND status.status = 'watched'
				AND EXTRACT(YEAR FROM COALESCE(status.watched_at, status.updated_at)) = $2
			GROUP BY genre
			ORDER BY COUNT(*) DESC
			LIMIT 1
		`, familyID, year).Scan(&result.TopGenre); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		if err := tx.QueryRow(ctx, `
			SELECT TO_CHAR(COALESCE(status.watched_at, status.updated_at), 'YYYY-MM')
			FROM public.family_series_status status
			JOIN public.family_series series ON series.id = status.series_id
			WHERE series.family_id = $1
				AND status.status = 'watched'
				AND EXTRACT(YEAR FROM COALESCE(status.watched_at, status.updated_at)) = $2
			GROUP BY 1
			ORDER BY COUNT(*) DESC
			LIMIT 1
		`, familyID, year).Scan(&result.BestMonth); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		if err := tx.QueryRow(ctx, `
			SELECT series.title
			FROM public.family_series_status status
			JOIN public.family_series series ON series.id = status.series_id
			WHERE series.family_id = $1
				AND status.status = 'watched'
				AND status.rating IS NOT NULL
				AND EXTRACT(YEAR FROM COALESCE(status.watched_at, status.updated_at)) = $2
			ORDER BY status.rating DESC, status.watched_at DESC
			LIMIT 1
		`, familyID, year).Scan(&result.TopRatedTitle); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		return nil
	})
	if err != nil {
		return YearWrapped{}, apperror.Internal("failed to load year wrapped", err)
	}
	return result, nil
}
