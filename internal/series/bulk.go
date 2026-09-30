package series

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"notrecinema/api/internal/activitylog"
	"notrecinema/api/internal/auth"
	"notrecinema/api/internal/outbox"
	"notrecinema/api/internal/platform/apperror"
	"notrecinema/api/internal/platform/response"
)

// maxBulkImportItems -- прямой перенос MAX_BULK_IMPORT_ITEMS.
const maxBulkImportItems = 500

// BulkItem -- прямой аналог SeriesData из notrecinema-app (то, что
// присылает клиент после поиска/Trakt/CSV-импорта и выбора элементов).
type BulkItem struct {
	Title                 string   `json:"title"`
	Genres                []string `json:"genres"`
	Year                  int      `json:"year"`
	ImageURL              *string  `json:"imageUrl"`
	Status                string   `json:"status"` // watched | to-watch
	Rating                *int     `json:"rating"`
	Comment               *string  `json:"comment"`
	TotalSeasons          *int     `json:"totalSeasons"`
	TotalEpisodes         *int     `json:"totalEpisodes"`
	EpisodeRuntimeMinutes *int     `json:"episodeRuntimeMinutes"`
	TrailerURL            *string  `json:"trailerUrl"`
	MediaType             string   `json:"mediaType"`
	ExternalSource        *string  `json:"externalSource"`
	ExternalID            *string  `json:"externalId"`
}

// bulkRecord -- форма строки для jsonb_to_recordset в первом INSERT
// (family_series). Поля совпадают по имени и порядку с recordShape в
// notrecinema-app -- порядок здесь не важен (jsonb_to_recordset матчит по
// имени), но набор колонок должен быть тем же.
type bulkRecord struct {
	Title                 string   `json:"title"`
	Genres                []string `json:"genres"`
	Year                  int      `json:"year"`
	ImageURL              *string  `json:"image_url"`
	Status                string   `json:"status"`
	Rating                *int     `json:"rating"`
	Comment               *string  `json:"comment"`
	TotalSeasons          *int     `json:"total_seasons"`
	TotalEpisodes         *int     `json:"total_episodes"`
	EpisodeRuntimeMinutes *int     `json:"episode_runtime_minutes"`
	TrailerURL            *string  `json:"trailer_url"`
	MediaType             string   `json:"media_type"`
	ExternalSource        *string  `json:"external_source"`
	ExternalID            *string  `json:"external_id"`
}

type insertedSeriesRow struct {
	ID             string
	ExternalSource *string
	ExternalID     *string
}

type statusRecord struct {
	SeriesID string  `json:"series_id"`
	Status   string  `json:"status"`
	Rating   *int    `json:"rating"`
	Comment  *string `json:"comment"`
}

// bulkImportKey — прямой перенос bulkImportKey: "source:externalId", с
// пустыми строками для отсутствующих значений (так что вручную введённые
// элементы без внешней привязки все схлопываются в один и тот же ключ
// ":" -- это ожидаемо, см. matchBulkImportedRowsToItems).
func bulkImportKey(source, externalID *string) string {
	s, id := "", ""
	if source != nil {
		s = *source
	}
	if externalID != nil {
		id = *externalID
	}
	return s + ":" + id
}

// matchBulkImportedRowsToItems — прямой перенос одноимённой функции:
// сопоставляет вставленные строки с исходными items по ключу
// "source:externalId", FIFO-очередью на ключ (несколько элементов с
// одинаковым ключом -- обычное дело для вручную добавленных без внешней
// привязки -- разбираются по одному, а не все схлопываются на последнем).
func matchBulkImportedRowsToItems(insertedRows []insertedSeriesRow, items []BulkItem) []statusRecord {
	itemsByKey := make(map[string][]BulkItem, len(items))
	for _, item := range items {
		key := bulkImportKey(item.ExternalSource, item.ExternalID)
		itemsByKey[key] = append(itemsByKey[key], item)
	}

	statusRows := make([]statusRecord, len(insertedRows))
	for i, row := range insertedRows {
		key := bulkImportKey(row.ExternalSource, row.ExternalID)
		bucket := itemsByKey[key]

		status := "to-watch"
		var rating *int
		var comment *string
		if len(bucket) > 0 {
			source := bucket[0]
			itemsByKey[key] = bucket[1:]
			status = source.Status
			if source.Status == "watched" {
				rating = source.Rating
				comment = source.Comment
			}
		}

		statusRows[i] = statusRecord{SeriesID: row.ID, Status: status, Rating: rating, Comment: comment}
	}
	return statusRows
}

type BulkResult struct {
	AddedCount int `json:"addedCount"`
}

// CreateBulk — прямой перенос addSeriesBulkAction. Два отдельных
// client.query() внутри одной транзакции (не WITH ... INSERT ... INSERT):
// RLS-политика family_series_status_insert_owner пересканирует
// family_series, и в пределах одного WITH-запроса siblings не видят
// строки друг друга (общий snapshot/command id) -- она бы спорадически
// отклоняла вставку статуса, хотя строка сериала объективно уже есть.
// Проверено это поведение реально на Postgres при написании оригинала в
// notrecinema-app -- здесь просто сохранена та же структура.
func (s *Service) CreateBulk(ctx context.Context, userID, familyID string, items []BulkItem) (BulkResult, error) {
	if len(items) == 0 {
		return BulkResult{AddedCount: 0}, nil
	}
	if len(items) > maxBulkImportItems {
		return BulkResult{}, apperror.Invalid("слишком много сериалов за один раз (максимум 500)")
	}

	records := make([]bulkRecord, len(items))
	for i, item := range items {
		r := bulkRecord{
			Title: item.Title, Genres: item.Genres, Year: item.Year, ImageURL: item.ImageURL,
			Status: item.Status, TotalSeasons: item.TotalSeasons, TotalEpisodes: item.TotalEpisodes,
			EpisodeRuntimeMinutes: item.EpisodeRuntimeMinutes, TrailerURL: item.TrailerURL,
			MediaType: item.MediaType, ExternalSource: item.ExternalSource, ExternalID: item.ExternalID,
		}
		if item.Status == "watched" {
			r.Rating = item.Rating
			r.Comment = item.Comment
		}
		if r.Genres == nil {
			r.Genres = []string{}
		}
		records[i] = r
	}
	payload, err := json.Marshal(records)
	if err != nil {
		return BulkResult{}, apperror.Internal("failed to marshal bulk payload", err)
	}

	var result BulkResult
	err = s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			INSERT INTO public.family_series (
				family_id, title, genres, year, image_url, created_by,
				total_seasons, total_episodes, episode_runtime_minutes, trailer_url,
				media_type, external_source, external_id
			)
			SELECT
				$2, t.title, t.genres, t.year, t.image_url, $3,
				t.total_seasons, t.total_episodes, t.episode_runtime_minutes, t.trailer_url,
				t.media_type, t.external_source, t.external_id
			FROM jsonb_to_recordset($1::jsonb) AS t(
				title text, genres text[], year int, image_url text,
				status text, rating int, comment text,
				total_seasons int, total_episodes int, episode_runtime_minutes int,
				trailer_url text, media_type text, external_source text, external_id text
			)
			RETURNING id, external_source, external_id
		`, payload, familyID, userID)
		if err != nil {
			return err
		}
		var inserted []insertedSeriesRow
		for rows.Next() {
			var row insertedSeriesRow
			if err := rows.Scan(&row.ID, &row.ExternalSource, &row.ExternalID); err != nil {
				rows.Close()
				return err
			}
			inserted = append(inserted, row)
		}
		if err := rows.Err(); err != nil {
			return err
		}

		statusRows := matchBulkImportedRowsToItems(inserted, items)
		statusPayload, err := json.Marshal(statusRows)
		if err != nil {
			return err
		}

		statusResult, err := tx.Exec(ctx, `
			INSERT INTO public.family_series_status (series_id, user_id, status, rating, comment, watched_at)
			SELECT t.series_id, $2, t.status, t.rating, t.comment,
			       CASE WHEN t.status = 'watched' THEN NOW() END
			FROM jsonb_to_recordset($1::jsonb) AS t(series_id uuid, status text, rating int, comment text)
		`, statusPayload, userID)
		if err != nil {
			return err
		}
		result.AddedCount = int(statusResult.RowsAffected())

		detail := items[0].Title
		if len(items) > 1 {
			detail = pluralDetail(len(items))
		}
		if err := activitylog.Insert(ctx, tx, familyID, userID, "участник", "series.bulk_added", nil, &detail); err != nil {
			return err
		}

		notifyPayload := map[string]any{
			"familyId": familyID, "userId": userID, "addedCount": result.AddedCount,
		}
		return outbox.Insert(ctx, tx, "family", familyID, "series.bulk_added", notifyPayload)
	})
	if err != nil {
		var appErr *apperror.Error
		if errors.As(err, &appErr) {
			return BulkResult{}, appErr
		}
		return BulkResult{}, apperror.Internal("failed to bulk-add series", err)
	}
	return result, nil
}

func pluralDetail(n int) string {
	return strconv.Itoa(n) + " сериалов (импорт списком)"
}

// RegisterBulkRoutes монтирует POST /api/v1/families/{familyId}/series/bulk.
func RegisterBulkRoutes(mux *http.ServeMux, svc *Service) {
	mux.HandleFunc("POST /api/v1/families/{familyId}/series/bulk", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())
		familyID := r.PathValue("familyId")

		var req struct {
			Items []BulkItem `json:"items"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			response.Error(w, apperror.Invalid("invalid request body"))
			return
		}

		result, err := svc.CreateBulk(r.Context(), user.ID, familyID, req.Items)
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, result)
	})
}
