package stats

import (
	"context"

	"github.com/jackc/pgx/v5"

	"notrecinema/api/internal/platform/apperror"
)

type Recommendation struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Genres   []string `json:"genres"`
	Year     int      `json:"year"`
	ImageURL *string  `json:"imageUrl,omitempty"`
	Score    int      `json:"score"`
}

// GetRecommendations ранжирует сериалы семьи, ещё не помеченные watched
// (to-watch или вообще без статуса), по совпадению с жанрами, которые эта
// же семья уже оценила на 4-5 -- чем больше "любимых" жанров у сериала,
// тем выше очки. Свежедобавленные сериалы без оценок идут после уже
// заматченных по жанру (score 0, но ORDER BY score DESC, created_at DESC).
func (s *Service) GetRecommendations(ctx context.Context, userID, familyID string) ([]Recommendation, error) {
	var list []Recommendation

	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			WITH favourite_genres AS (
				SELECT genre, COUNT(*) AS weight
				FROM public.family_series series
				JOIN public.family_series_status status ON status.series_id = series.id
				CROSS JOIN LATERAL UNNEST(series.genres) AS genre
				WHERE series.family_id = $1
					AND status.status = 'watched'
					AND status.rating >= 4
				GROUP BY genre
			)
			SELECT
				series.id,
				series.title,
				series.genres,
				series.year,
				series.image_url,
				COALESCE(SUM(favourite_genres.weight), 0) AS score
			FROM public.family_series series
			LEFT JOIN public.family_series_status my_status
				ON my_status.series_id = series.id
			   AND my_status.user_id = $2
			LEFT JOIN LATERAL UNNEST(series.genres) AS series_genre ON true
			LEFT JOIN favourite_genres ON favourite_genres.genre = series_genre
			WHERE series.family_id = $1
				AND (my_status.status IS NULL OR my_status.status = 'to-watch')
			GROUP BY series.id, series.title, series.genres, series.year, series.image_url
			ORDER BY score DESC, series.created_at DESC
			LIMIT 10
		`, familyID, userID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var r Recommendation
			var year *int
			if err := rows.Scan(&r.ID, &r.Title, &r.Genres, &year, &r.ImageURL, &r.Score); err != nil {
				return err
			}
			if year != nil {
				r.Year = *year
			}
			if r.Genres == nil {
				r.Genres = []string{}
			}
			list = append(list, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, apperror.Internal("failed to load recommendations", err)
	}

	if list == nil {
		list = []Recommendation{}
	}
	return list, nil
}
