// Package export отдаёт пользователю выгрузку его собственных данных
// (профиль, семья, список сериалов) в JSON или CSV -- прямой перенос
// GET /api/export/data из notrecinema-app
// (src/app/api/export/data/route.ts). Скоуп -- per-user, не per-family:
// все три запроса идут по user_id вызывающего, RLS ничего дополнительно
// не сужает и не должна -- это его собственные данные.
package export

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"notrecinema/api/internal/auth"
	"notrecinema/api/internal/platform/apperror"
	"notrecinema/api/internal/platform/response"
	"notrecinema/api/internal/postgres"
)

type Profile struct {
	Email       string    `json:"email"`
	DisplayName *string   `json:"displayName"`
	CreatedAt   time.Time `json:"createdAt"`
}

type Family struct {
	FamilyName string    `json:"familyName"`
	Role       string    `json:"role"`
	JoinedAt   time.Time `json:"joinedAt"`
}

type SeriesRow struct {
	Title          string     `json:"title"`
	Year           *int       `json:"year"`
	Genres         []string   `json:"genres"`
	MediaType      string     `json:"mediaType"`
	Status         *string    `json:"status"`
	Rating         *int       `json:"rating"`
	Comment        *string    `json:"comment"`
	WatchedAt      *time.Time `json:"watchedAt"`
	CurrentSeason  *int       `json:"currentSeason"`
	CurrentEpisode *int       `json:"currentEpisode"`
}

type Data struct {
	Profile Profile     `json:"profile"`
	Family  *Family     `json:"family"`
	Series  []SeriesRow `json:"series"`
}

type Service struct {
	db *postgres.Pool
}

func NewService(db *postgres.Pool) *Service {
	return &Service{db: db}
}

func (s *Service) GetExportData(ctx context.Context, userID string) (Data, error) {
	var data Data

	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			SELECT email, display_name, created_at FROM public.profiles WHERE id = $1
		`, userID).Scan(&data.Profile.Email, &data.Profile.DisplayName, &data.Profile.CreatedAt); err != nil {
			return err
		}

		var fam Family
		err := tx.QueryRow(ctx, `
			SELECT family.name, member.role, member.joined_at
			FROM public.family_members member
			JOIN public.families family ON family.id = member.family_id
			WHERE member.user_id = $1
			LIMIT 1
		`, userID).Scan(&fam.FamilyName, &fam.Role, &fam.JoinedAt)
		switch {
		case err == nil:
			data.Family = &fam
		case errors.Is(err, pgx.ErrNoRows):
			// у пользователя может не быть семьи -- допустимо, оставляем nil
		default:
			return err
		}

		rows, err := tx.Query(ctx, `
			SELECT
				series.title, series.year, series.genres, series.media_type,
				status.status, status.rating, status.comment, status.watched_at,
				progress.current_season, progress.current_episode
			FROM public.family_series_status status
			JOIN public.family_series series ON series.id = status.series_id
			LEFT JOIN public.family_series_progress progress
				ON progress.series_id = series.id
			   AND progress.user_id = status.user_id
			WHERE status.user_id = $1
			ORDER BY series.title ASC
		`, userID)
		if err != nil {
			return err
		}
		defer rows.Close()

		var series []SeriesRow
		for rows.Next() {
			var row SeriesRow
			if err := rows.Scan(
				&row.Title, &row.Year, &row.Genres, &row.MediaType,
				&row.Status, &row.Rating, &row.Comment, &row.WatchedAt,
				&row.CurrentSeason, &row.CurrentEpisode,
			); err != nil {
				return err
			}
			series = append(series, row)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if series == nil {
			series = []SeriesRow{}
		}
		data.Series = series
		return nil
	})
	if err != nil {
		return Data{}, apperror.Internal("failed to get export data", err)
	}
	return data, nil
}

func toCSV(w http.ResponseWriter, series []SeriesRow) error {
	writer := csv.NewWriter(w)
	header := []string{
		"title", "year", "genres", "media_type", "status", "rating",
		"comment", "watched_at", "current_season", "current_episode",
	}
	if err := writer.Write(header); err != nil {
		return err
	}

	for _, row := range series {
		genres := ""
		for i, g := range row.Genres {
			if i > 0 {
				genres += "; "
			}
			genres += g
		}

		record := []string{
			row.Title,
			intPtrToString(row.Year),
			genres,
			row.MediaType,
			stringPtrToString(row.Status),
			intPtrToString(row.Rating),
			stringPtrToString(row.Comment),
			timePtrToString(row.WatchedAt),
			intPtrToString(row.CurrentSeason),
			intPtrToString(row.CurrentEpisode),
		}
		if err := writer.Write(record); err != nil {
			return err
		}
	}

	writer.Flush()
	return writer.Error()
}

func intPtrToString(v *int) string {
	if v == nil {
		return ""
	}
	return strconv.Itoa(*v)
}

func stringPtrToString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func timePtrToString(v *time.Time) string {
	if v == nil {
		return ""
	}
	return v.Format(time.RFC3339)
}

func RegisterRoutes(mux *http.ServeMux, svc *Service) {
	mux.HandleFunc("GET /api/v1/export/data", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())

		data, err := svc.GetExportData(r.Context(), user.ID)
		if err != nil {
			response.Error(w, err)
			return
		}

		if r.URL.Query().Get("format") == "csv" {
			w.Header().Set("Content-Type", "text/csv; charset=utf-8")
			w.Header().Set("Content-Disposition", `attachment; filename="my-data.csv"`)
			w.WriteHeader(http.StatusOK)
			if err := toCSV(w, data.Series); err != nil {
				return
			}
			return
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="my-data.json"`)
		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(data)
	})
}
