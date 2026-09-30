// Package history отдаёт журнал просмотров семьи: кто что посмотрел и
// когда. Прямой перенос getWatchHistory из notrecinema-app
// (src/shared/api/postgres/queries.ts) -- никакой отдельной таблицы нет,
// это агрегат family_series_status (status = 'watched') + family_series +
// profiles. Семейная (а не только своя) видимость этих строк обеспечена
// RLS-политикой family_series_status_select_family из миграции
// 0008_family_stats_visibility_fix.sql.
package history

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"notrecinema/api/internal/auth"
	"notrecinema/api/internal/platform/apperror"
	"notrecinema/api/internal/platform/response"
	"notrecinema/api/internal/postgres"
)

const pageSize = 50

type Entry struct {
	SeriesID  string    `json:"seriesId"`
	Title     string    `json:"title"`
	ImageURL  *string   `json:"imageUrl,omitempty"`
	Rating    *int      `json:"rating,omitempty"`
	WatchedAt time.Time `json:"watchedAt"`
	WatchedBy string    `json:"watchedBy"`
}

type Page struct {
	Entries []Entry `json:"entries"`
	HasMore bool    `json:"hasMore"`
}

type Service struct {
	db *postgres.Pool
}

func NewService(db *postgres.Pool) *Service {
	return &Service{db: db}
}

func (s *Service) GetWatchHistory(ctx context.Context, userID, familyID string, offset int) (Page, error) {
	var page Page

	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT
				series.id, series.title, series.image_url,
				status.rating, COALESCE(status.watched_at, status.updated_at),
				COALESCE(profile.display_name, profile.email)
			FROM public.family_series_status status
			JOIN public.family_series series ON series.id = status.series_id
			JOIN public.profiles profile ON profile.id = status.user_id
			WHERE series.family_id = $1
			  AND status.status = 'watched'
			ORDER BY COALESCE(status.watched_at, status.updated_at) DESC
			LIMIT $2 OFFSET $3
		`, familyID, pageSize+1, offset)
		if err != nil {
			return err
		}
		defer rows.Close()

		var entries []Entry
		for rows.Next() {
			var e Entry
			if err := rows.Scan(&e.SeriesID, &e.Title, &e.ImageURL, &e.Rating, &e.WatchedAt, &e.WatchedBy); err != nil {
				return err
			}
			entries = append(entries, e)
		}
		if err := rows.Err(); err != nil {
			return err
		}

		page.HasMore = len(entries) > pageSize
		if page.HasMore {
			entries = entries[:pageSize]
		}
		page.Entries = entries
		return nil
	})
	if err != nil {
		return Page{}, apperror.Internal("failed to get watch history", err)
	}

	if page.Entries == nil {
		page.Entries = []Entry{}
	}
	return page, nil
}

func RegisterRoutes(mux *http.ServeMux, svc *Service) {
	mux.HandleFunc("GET /api/v1/families/{familyId}/history", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())
		familyID := r.PathValue("familyId")

		offset := 0
		if raw := r.URL.Query().Get("offset"); raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
				offset = parsed
			}
		}

		page, err := svc.GetWatchHistory(r.Context(), user.ID, familyID, offset)
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, page)
	})
}
