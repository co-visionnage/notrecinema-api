// Package families реализует вертикальный срез «семьи»: список семей
// текущего пользователя и создание новой. Видимость участников и владение
// проверяются на уровне RLS, а не в коде приложения -- см.
// families_select_member / families_insert_owner в
// database/migrations/0001_initial_schema.sql (репозиторий notrecinema-app).
package families

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"notrecinema/api/internal/activitylog"
	"notrecinema/api/internal/auth"
	"notrecinema/api/internal/outbox"
	"notrecinema/api/internal/platform/apperror"
	"notrecinema/api/internal/platform/ratelimit"
	"notrecinema/api/internal/platform/response"
	"notrecinema/api/internal/postgres"
)

// memberJoinedEvent -- полезная нагрузка события family.member.joined.
// DisplayName попадает сюда, а не вычисляется в notrecinema-worker: имя
// уже под рукой в той же транзакции, и это исключает лишний поход
// воркера в БД просто ради текста уведомления.
type memberJoinedEvent struct {
	FamilyID    string `json:"familyId"`
	UserID      string `json:"userId"`
	Role        string `json:"role"`
	DisplayName string `json:"displayName"`
}

type Family struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	InviteCode string `json:"inviteCode"`
	OwnerID    string `json:"ownerId"`
	// Role и JoinedAt -- членство вызывающего пользователя в этой семье.
	Role     string    `json:"role"`
	JoinedAt time.Time `json:"joinedAt"`
}

type Service struct {
	db *postgres.Pool
}

func NewService(db *postgres.Pool) *Service {
	return &Service{db: db}
}

func (s *Service) ListForUser(ctx context.Context, userID string) ([]Family, error) {
	var families []Family

	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT f.id, f.name, f.invite_code, f.owner_id, m.role, m.joined_at
			FROM public.families f
			JOIN public.family_members m ON m.family_id = f.id
			WHERE m.user_id = $1
			ORDER BY f.created_at DESC
		`, userID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var f Family
			if err := rows.Scan(&f.ID, &f.Name, &f.InviteCode, &f.OwnerID, &f.Role, &f.JoinedAt); err != nil {
				return err
			}
			families = append(families, f)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, apperror.Internal("failed to list families", err)
	}

	if families == nil {
		families = []Family{}
	}
	return families, nil
}

func (s *Service) Create(ctx context.Context, userID, name string) (Family, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Family{}, apperror.Invalid("name is required")
	}
	if len(name) > 255 {
		return Family{}, apperror.Invalid("name must be at most 255 characters")
	}

	inviteCode, err := generateInviteCode()
	if err != nil {
		return Family{}, apperror.Internal("failed to generate invite code", err)
	}

	var family Family

	err = s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO public.families (name, owner_id, invite_code)
			VALUES ($1, $2, $3)
			RETURNING id, name, invite_code, owner_id
		`, name, userID, inviteCode)
		family.Role = "owner"
		family.JoinedAt = time.Now()
		if err := row.Scan(&family.ID, &family.Name, &family.InviteCode, &family.OwnerID); err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO public.family_members (family_id, user_id, role)
			VALUES ($1, $2, 'owner')
		`, family.ID, userID); err != nil {
			return err
		}

		displayName, err := lookupDisplayName(ctx, tx, userID)
		if err != nil {
			return err
		}

		// Событие кладём в outbox в этой же транзакции: либо семья, участник
		// и событие о нём закоммитятся вместе, либо не закоммитится ничего.
		return outbox.Insert(ctx, tx, "family", family.ID, "family.member.joined", memberJoinedEvent{
			FamilyID:    family.ID,
			UserID:      userID,
			Role:        "owner",
			DisplayName: displayName,
		})
	})
	if err != nil {
		return Family{}, apperror.Internal("failed to create family", err)
	}

	return family, nil
}

// Join добавляет пользователя в семью по инвайт-коду.
// find_family_by_invite_code -- SECURITY DEFINER (см. миграцию
// 0001_initial_schema.sql): не-участнику нужно найти семью по коду
// раньше, чем обычный SELECT из families (который требует уже быть
// участником) вообще что-то ему покажет.
const (
	joinRateLimitMaxAttempts   = 10
	joinRateLimitWindowSeconds = 10 * 60
)

func (s *Service) Join(ctx context.Context, userID, inviteCode string) (Family, error) {
	inviteCode = strings.TrimSpace(inviteCode)
	if inviteCode == "" {
		return Family{}, apperror.Invalid("inviteCode is required")
	}

	// Перебор шестизначного кода без ограничений был бы возможен; лимит
	// по пользователю (как и в исходной реализации): 10 попыток за 10 минут.
	allowed, err := ratelimit.Check(ctx, s.db, "join-family:user:"+userID, joinRateLimitMaxAttempts, joinRateLimitWindowSeconds)
	if err != nil {
		return Family{}, apperror.Internal("failed to check rate limit", err)
	}
	if !allowed {
		return Family{}, apperror.RateLimited("Слишком много попыток. Попробуйте позже.")
	}

	var family Family
	err = s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT family_id, name, invite_code FROM public.find_family_by_invite_code($1)`, inviteCode)
		family.Role = "member"
		family.JoinedAt = time.Now()
		if err := row.Scan(&family.ID, &family.Name, &family.InviteCode); err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO public.family_members (family_id, user_id, role)
			VALUES ($1, $2, 'member')
		`, family.ID, userID); err != nil {
			return err
		}

		// Теперь, когда участник добавлен, семья видна ему и через обычный
		// RLS-SELECT -- дочитываем owner_id тем же способом, что и везде
		// (не полагаемся на то, что вернула SECURITY DEFINER функция).
		if err := tx.QueryRow(ctx, `SELECT owner_id FROM public.families WHERE id = $1`, family.ID).Scan(&family.OwnerID); err != nil {
			return err
		}

		displayName, err := lookupDisplayName(ctx, tx, userID)
		if err != nil {
			return err
		}

		if err := activitylog.Insert(ctx, tx, family.ID, userID, displayName, "member_joined", nil, nil); err != nil {
			return err
		}

		return outbox.Insert(ctx, tx, "family", family.ID, "family.member.joined", memberJoinedEvent{
			FamilyID:    family.ID,
			UserID:      userID,
			Role:        "member",
			DisplayName: displayName,
		})
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Family{}, apperror.NotFound("family not found for this invite code")
		}
		if postgres.IsUniqueViolation(err) {
			return Family{}, apperror.Conflict("already a member of this family")
		}
		return Family{}, apperror.Internal("failed to join family", err)
	}
	return family, nil
}

// lookupDisplayName возвращает то, что стоит показать в уведомлении:
// display_name, если пользователь его задал, иначе email.
func lookupDisplayName(ctx context.Context, tx pgx.Tx, userID string) (string, error) {
	var displayName *string
	var email string
	if err := tx.QueryRow(ctx, `SELECT display_name, email FROM public.profiles WHERE id = $1`, userID).
		Scan(&displayName, &email); err != nil {
		return "", err
	}
	if displayName != nil && *displayName != "" {
		return *displayName, nil
	}
	return email, nil
}

func generateInviteCode() (string, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // без неоднозначных O/0/I/1
	const length = 8

	buf := make([]byte, length)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i, b := range buf {
		buf[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(buf), nil
}

type Member struct {
	UserID      string    `json:"userId"`
	Email       string    `json:"email"`
	DisplayName *string   `json:"displayName,omitempty"`
	Role        string    `json:"role"`
	JoinedAt    time.Time `json:"joinedAt"`
}

// ListMembers -- прямой перенос getFamilyMembers из notrecinema-app:
// доступ проверяется явным SELECT (не только RLS), владелец первым, дальше
// по joined_at. userID здесь и есть тот самый явный чек: если у RLS в
// принципе не найдётся строки family_members для этого user_id, JOIN ниже
// вернёт пустой список, а не чужих участников.
func (s *Service) ListMembers(ctx context.Context, userID, familyID string) ([]Member, error) {
	var members []Member

	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		var membershipExists bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS(SELECT 1 FROM public.family_members WHERE family_id = $1 AND user_id = $2)
		`, familyID, userID).Scan(&membershipExists); err != nil {
			return err
		}
		if !membershipExists {
			return apperror.NotFound("family not found")
		}

		rows, err := tx.Query(ctx, `
			SELECT member.user_id, profile.email, profile.display_name, member.role, member.joined_at
			FROM public.family_members AS member
			JOIN public.profiles AS profile ON profile.id = member.user_id
			WHERE member.family_id = $1
			ORDER BY CASE WHEN member.role = 'owner' THEN 0 ELSE 1 END, member.joined_at ASC
		`, familyID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var m Member
			if err := rows.Scan(&m.UserID, &m.Email, &m.DisplayName, &m.Role, &m.JoinedAt); err != nil {
				return err
			}
			members = append(members, m)
		}
		return rows.Err()
	})
	if err != nil {
		var appErr *apperror.Error
		if errors.As(err, &appErr) {
			return nil, appErr
		}
		return nil, apperror.Internal("failed to list family members", err)
	}

	if members == nil {
		members = []Member{}
	}
	return members, nil
}

// RemoveMember -- прямой перенос DELETE /api/family/members: только
// владелец может удалять участников, владельца удалить нельзя (для этого
// есть TransferOwnership). В отличие от оригинала, который на любую из
// этих ошибок отвечал общим 500, здесь разные apperror.Kind -- 403/404.
func (s *Service) RemoveMember(ctx context.Context, userID, familyID, targetUserID string) error {
	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		callerRole, err := memberRole(ctx, tx, familyID, userID)
		if err != nil {
			return err
		}
		if callerRole != "owner" {
			return apperror.Forbidden("только владелец семьи может удалять участников")
		}

		targetRole, err := memberRole(ctx, tx, familyID, targetUserID)
		if err != nil {
			return err
		}
		if targetRole == "" {
			return apperror.NotFound("участник не найден")
		}
		if targetRole == "owner" {
			return apperror.Forbidden("нельзя удалить владельца семьи")
		}

		targetLabel, err := lookupDisplayName(ctx, tx, targetUserID)
		if err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `
			DELETE FROM public.family_members WHERE family_id = $1 AND user_id = $2
		`, familyID, targetUserID); err != nil {
			return err
		}

		actorLabel, err := lookupDisplayName(ctx, tx, userID)
		if err != nil {
			return err
		}
		// member_removed -- новое значение action по сравнению с
		// notrecinema-app: там удаление участника в activity log вообще не
		// попадало (FamilyActivityAction не включал такое значение). Здесь
		// это восполнено, а не изменение поведения задним числом.
		return activitylog.Insert(ctx, tx, familyID, userID, actorLabel, "member_removed", &targetLabel, nil)
	})
	if err != nil {
		var appErr *apperror.Error
		if errors.As(err, &appErr) {
			return appErr
		}
		return apperror.Internal("failed to remove family member", err)
	}
	return nil
}

// SetMemberRole -- прямой перенос setMemberRoleAction, но через SQL-функцию
// set_family_member_role (SECURITY DEFINER, миграция 0030), а не через
// прямой UPDATE, который использует оригинал в notrecinema-app. У
// family_members в принципе нет UPDATE-политики RLS (см. комментарий в
// самой миграции) -- обычный UPDATE от имени app_user всегда молча
// затрагивает 0 строк, независимо от прав вызывающего, так что
// setMemberRoleAction в оригинале на практике никогда не работал. role
// ограничен admin/member проверкой внутри той же функции.
func (s *Service) SetMemberRole(ctx context.Context, userID, familyID, targetUserID, role string) error {
	if role != "admin" && role != "member" {
		return apperror.Invalid("role must be 'admin' or 'member'")
	}

	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		var updated bool
		if err := tx.QueryRow(ctx, `
			SELECT public.set_family_member_role($1, $2, $3)
		`, familyID, targetUserID, role).Scan(&updated); err != nil {
			return err
		}
		if !updated {
			return apperror.NotFound("участник не найден")
		}

		targetLabel, err := lookupDisplayName(ctx, tx, targetUserID)
		if err != nil {
			return err
		}
		actorLabel, err := lookupDisplayName(ctx, tx, userID)
		if err != nil {
			return err
		}
		return activitylog.Insert(ctx, tx, familyID, userID, actorLabel, "role_changed", &targetLabel, &role)
	})
	if err != nil {
		var appErr *apperror.Error
		if errors.As(err, &appErr) {
			return appErr
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			return apperror.Forbidden(pgErr.Message)
		}
		return apperror.Internal("failed to change member role", err)
	}
	return nil
}

// TransferOwnership -- прямой перенос transferFamilyOwnershipAction: вся
// проверка и сама передача живут в transfer_family_ownership (SECURITY
// DEFINER, миграция 0006), Go только логирует результат в activity log.
func (s *Service) TransferOwnership(ctx context.Context, userID, familyID, newOwnerUserID string) error {
	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT public.transfer_family_ownership($1, $2)`, familyID, newOwnerUserID); err != nil {
			return err
		}

		targetLabel, err := lookupDisplayName(ctx, tx, newOwnerUserID)
		if err != nil {
			return err
		}
		actorLabel, err := lookupDisplayName(ctx, tx, userID)
		if err != nil {
			return err
		}
		return activitylog.Insert(ctx, tx, familyID, userID, actorLabel, "ownership_transferred", &targetLabel, nil)
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			return apperror.Forbidden(pgErr.Message)
		}
		return apperror.Internal("failed to transfer family ownership", err)
	}
	return nil
}

// memberRole возвращает роль userID в familyID, "" если он не участник.
func memberRole(ctx context.Context, tx pgx.Tx, familyID, userID string) (string, error) {
	var role string
	err := tx.QueryRow(ctx, `
		SELECT role FROM public.family_members WHERE family_id = $1 AND user_id = $2
	`, familyID, userID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return role, nil
}

type createRequest struct {
	Name string `json:"name"`
}

type joinRequest struct {
	InviteCode string `json:"inviteCode"`
}

type setRoleRequest struct {
	Role string `json:"role"`
}

type transferOwnershipRequest struct {
	NewOwnerUserID string `json:"newOwnerUserId"`
}

// RegisterRoutes монтирует роуты families в mux. Каждый роут здесь
// рассчитан на то, что выполняется за auth.Service.Middleware.
func RegisterRoutes(mux *http.ServeMux, svc *Service) {
	mux.HandleFunc("GET /api/v1/families", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())

		families, err := svc.ListForUser(r.Context(), user.ID)
		if err != nil {
			response.Error(w, err)
			return
		}

		response.JSON(w, http.StatusOK, families)
	})

	mux.HandleFunc("POST /api/v1/families", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())

		var req createRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			response.Error(w, apperror.Invalid("invalid request body"))
			return
		}

		family, err := svc.Create(r.Context(), user.ID, req.Name)
		if err != nil {
			response.Error(w, err)
			return
		}

		response.JSON(w, http.StatusCreated, family)
	})

	mux.HandleFunc("POST /api/v1/families/join", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())

		var req joinRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			response.Error(w, apperror.Invalid("invalid request body"))
			return
		}

		family, err := svc.Join(r.Context(), user.ID, req.InviteCode)
		if err != nil {
			response.Error(w, err)
			return
		}

		response.JSON(w, http.StatusOK, family)
	})

	mux.HandleFunc("GET /api/v1/families/{familyId}/members", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())
		familyID := r.PathValue("familyId")

		members, err := svc.ListMembers(r.Context(), user.ID, familyID)
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, members)
	})

	mux.HandleFunc("DELETE /api/v1/families/{familyId}/members/{userId}", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())
		familyID := r.PathValue("familyId")
		targetUserID := r.PathValue("userId")

		if err := svc.RemoveMember(r.Context(), user.ID, familyID, targetUserID); err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, map[string]bool{"success": true})
	})

	mux.HandleFunc("PUT /api/v1/families/{familyId}/members/{userId}/role", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())
		familyID := r.PathValue("familyId")
		targetUserID := r.PathValue("userId")

		var req setRoleRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			response.Error(w, apperror.Invalid("invalid request body"))
			return
		}

		if err := svc.SetMemberRole(r.Context(), user.ID, familyID, targetUserID, req.Role); err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, map[string]bool{"success": true})
	})

	mux.HandleFunc("POST /api/v1/families/{familyId}/transfer-ownership", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())
		familyID := r.PathValue("familyId")

		var req transferOwnershipRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			response.Error(w, apperror.Invalid("invalid request body"))
			return
		}

		if err := svc.TransferOwnership(r.Context(), user.ID, familyID, req.NewOwnerUserID); err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, map[string]bool{"success": true})
	})
}
