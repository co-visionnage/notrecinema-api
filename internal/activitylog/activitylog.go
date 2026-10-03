// Package activitylog пишет и читает family_activity_log — лёгкий журнал
// аудита ("кто добавил сериал", "кто закрыл опрос"). actor_label и
// target_label фиксируются на момент записи (а не читаются через JOIN при
// каждом SELECT), поэтому лог остаётся читаемым даже после того, как
// профиль автора удалён -- см. комментарий в самой миграции
// 0015_family_activity_log.sql.
package activitylog

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

type Entry struct {
	ID          string    `json:"id"`
	ActorLabel  string    `json:"actorLabel"`
	Action      string    `json:"action"`
	TargetLabel *string   `json:"targetLabel,omitempty"`
	Detail      *string   `json:"detail,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
}

// Insert пишет запись в family_activity_log внутри переданной транзакции.
// Вызывается из других доменных пакетов (series, polls, events, families)
// сразу после мутации, которую стоит залогировать -- в той же транзакции,
// чтобы запись в лог и сама мутация коммитились или откатывались вместе.
func Insert(ctx context.Context, tx pgx.Tx, familyID, actorUserID, actorLabel, action string, targetLabel, detail *string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO public.family_activity_log
			(family_id, actor_user_id, actor_label, action, target_label, detail)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, familyID, actorUserID, actorLabel, action, targetLabel, detail)
	return err
}

type Service struct {
	db *postgres.Pool
}

func NewService(db *postgres.Pool) *Service {
	return &Service{db: db}
}

const pageSize = 50

// Page -- страница журнала: pageSize записей и признак, что есть ещё.
type Page struct {
	Entries []Entry `json:"entries"`
	HasMore bool    `json:"hasMore"`
}

func (s *Service) ListForFamily(ctx context.Context, userID, familyID string, offset int) (Page, error) {
	page := Page{Entries: []Entry{}}

	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, actor_label, action, target_label, detail, created_at
			FROM public.family_activity_log
			WHERE family_id = $1
			ORDER BY created_at DESC
			LIMIT $2 OFFSET $3
		`, familyID, pageSize+1, offset)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var e Entry
			if err := rows.Scan(&e.ID, &e.ActorLabel, &e.Action, &e.TargetLabel, &e.Detail, &e.CreatedAt); err != nil {
				return err
			}
			page.Entries = append(page.Entries, e)
		}
		return rows.Err()
	})
	if err != nil {
		return Page{}, apperror.Internal("failed to list activity log", err)
	}

	if len(page.Entries) > pageSize {
		page.HasMore = true
		page.Entries = page.Entries[:pageSize]
	}
	return page, nil
}

func RegisterRoutes(mux *http.ServeMux, svc *Service) {
	mux.HandleFunc("GET /api/v1/families/{familyId}/activity", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())
		familyID := r.PathValue("familyId")

		offset := 0
		if raw := r.URL.Query().Get("offset"); raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
				offset = parsed
			}
		}

		page, err := svc.ListForFamily(r.Context(), user.ID, familyID, offset)
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, page)
	})
}
