// Package stats реализует аналитику поверх просмотров семьи: статистика,
// достижения, итоги года, рекомендации. Все четыре -- прямой перенос
// SQL-логики из notrecinema-app
// (src/shared/api/postgres/queries.ts: getFamilyStats,
// getFamilyAchievements, getYearWrapped, getRecommendations), только
// сами запросы и их результат -- перепроверять формулы заново смысла нет,
// они уже отработаны в проде.
package stats

import (
	"context"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"notrecinema/api/internal/auth"
	"notrecinema/api/internal/platform/apperror"
	"notrecinema/api/internal/platform/response"
	"notrecinema/api/internal/postgres"
)

type Service struct {
	db *postgres.Pool
}

func NewService(db *postgres.Pool) *Service {
	return &Service{db: db}
}

type MonthlyHours struct {
	Month string `json:"month"`
	Hours int    `json:"hours"`
}

type GenreCount struct {
	Genre string `json:"genre"`
	Count int    `json:"count"`
}

type FamilyStats struct {
	TotalHoursWatched  int            `json:"totalHoursWatched"`
	TotalWatchedSeries int            `json:"totalWatchedSeries"`
	ByMonth            []MonthlyHours `json:"byMonth"`
	TopGenres          []GenreCount   `json:"topGenres"`
}

func (s *Service) GetFamilyStats(ctx context.Context, userID, familyID string) (FamilyStats, error) {
	var result FamilyStats

	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		var totalHours float64
		var totalSeries int
		if err := tx.QueryRow(ctx, `
			SELECT
				COALESCE(SUM(COALESCE(series.total_episodes, 1) * COALESCE(series.episode_runtime_minutes, 45)) / 60.0, 0),
				COUNT(*)
			FROM public.family_series_status status
			JOIN public.family_series series ON series.id = status.series_id
			WHERE series.family_id = $1 AND status.status = 'watched'
		`, familyID).Scan(&totalHours, &totalSeries); err != nil {
			return err
		}
		result.TotalHoursWatched = int(totalHours + 0.5)
		result.TotalWatchedSeries = totalSeries

		// LIMIT 6 самых последних месяцев, затем разворачиваем в
		// хронологический порядок -- так же, как .toReversed() в TS-версии.
		rows, err := tx.Query(ctx, `
			SELECT
				TO_CHAR(COALESCE(status.watched_at, status.updated_at), 'YYYY-MM') AS month,
				SUM(COALESCE(series.total_episodes, 1) * COALESCE(series.episode_runtime_minutes, 45)) / 60.0 AS hours
			FROM public.family_series_status status
			JOIN public.family_series series ON series.id = status.series_id
			WHERE series.family_id = $1 AND status.status = 'watched'
			GROUP BY month
			ORDER BY month DESC
			LIMIT 6
		`, familyID)
		if err != nil {
			return err
		}
		var byMonth []MonthlyHours
		for rows.Next() {
			var m MonthlyHours
			var hours float64
			if err := rows.Scan(&m.Month, &hours); err != nil {
				rows.Close()
				return err
			}
			m.Hours = int(hours + 0.5)
			byMonth = append(byMonth, m)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for i, j := 0, len(byMonth)-1; i < j; i, j = i+1, j-1 {
			byMonth[i], byMonth[j] = byMonth[j], byMonth[i]
		}
		result.ByMonth = byMonth

		genreRows, err := tx.Query(ctx, `
			SELECT genre, COUNT(*)
			FROM public.family_series series
			JOIN public.family_series_status status ON status.series_id = series.id
			CROSS JOIN LATERAL UNNEST(series.genres) AS genre
			WHERE series.family_id = $1 AND status.status = 'watched'
			GROUP BY genre
			ORDER BY COUNT(*) DESC
			LIMIT 5
		`, familyID)
		if err != nil {
			return err
		}
		defer genreRows.Close()
		var topGenres []GenreCount
		for genreRows.Next() {
			var g GenreCount
			if err := genreRows.Scan(&g.Genre, &g.Count); err != nil {
				return err
			}
			topGenres = append(topGenres, g)
		}
		if err := genreRows.Err(); err != nil {
			return err
		}
		result.TopGenres = topGenres

		return nil
	})
	if err != nil {
		return FamilyStats{}, apperror.Internal("failed to load family stats", err)
	}

	if result.ByMonth == nil {
		result.ByMonth = []MonthlyHours{}
	}
	if result.TopGenres == nil {
		result.TopGenres = []GenreCount{}
	}
	return result, nil
}

func RegisterRoutes(mux *http.ServeMux, svc *Service) {
	mux.HandleFunc("GET /api/v1/families/{familyId}/stats", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())
		result, err := svc.GetFamilyStats(r.Context(), user.ID, r.PathValue("familyId"))
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, result)
	})

	mux.HandleFunc("GET /api/v1/families/{familyId}/achievements", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())
		result, err := svc.GetFamilyAchievements(r.Context(), user.ID, r.PathValue("familyId"))
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, result)
	})

	mux.HandleFunc("GET /api/v1/families/{familyId}/wrapped", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())

		year, err := strconv.Atoi(r.URL.Query().Get("year"))
		if err != nil {
			response.Error(w, apperror.Invalid("year query parameter is required and must be a number"))
			return
		}

		result, err := svc.GetYearWrapped(r.Context(), user.ID, r.PathValue("familyId"), year)
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, result)
	})

	mux.HandleFunc("GET /api/v1/families/{familyId}/recommendations", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())
		result, err := svc.GetRecommendations(r.Context(), user.ID, r.PathValue("familyId"))
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, result)
	})
}
