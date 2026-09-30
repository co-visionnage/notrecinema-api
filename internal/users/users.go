// Package users реализует вертикальный срез «профиль»: всё необходимое,
// чтобы прочитать профиль текущего пользователя, в рамках RLS-политики
// profiles_select_self.
package users

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"notrecinema/api/internal/auth"
	"notrecinema/api/internal/platform/apperror"
	"notrecinema/api/internal/platform/response"
	"notrecinema/api/internal/postgres"
)

type Profile struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"displayName,omitempty"`
	AvatarURL   string `json:"avatarUrl,omitempty"`
}

type Service struct {
	db *postgres.Pool
}

func NewService(db *postgres.Pool) *Service {
	return &Service{db: db}
}

func (s *Service) GetProfile(ctx context.Context, userID string) (Profile, error) {
	var profile Profile

	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		var displayName, avatarURL *string

		row := tx.QueryRow(ctx,
			`SELECT id, email, display_name, avatar_url FROM public.profiles WHERE id = $1`,
			userID,
		)
		if err := row.Scan(&profile.ID, &profile.Email, &displayName, &avatarURL); err != nil {
			return err
		}
		if displayName != nil {
			profile.DisplayName = *displayName
		}
		if avatarURL != nil {
			profile.AvatarURL = *avatarURL
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Profile{}, apperror.NotFound("profile not found")
		}
		return Profile{}, apperror.Internal("failed to load profile", err)
	}

	return profile, nil
}

// DeleteAccount -- прямой перенос deleteAccountAction: пароль
// перепроверяется заново (активной сессии недостаточно -- это
// необратимое действие), затем проверяются ВСЕ семьи, которыми владеет
// пользователь (не только одна -- в одной он может быть просто участником,
// в другой владельцем, мульти-семейное членство поддерживается), и если
// хоть в одной из них есть другие участники, удаление блокируется: иначе
// ON DELETE CASCADE от profiles снёс бы семью вместе со всеми остальными.
// delete_own_profile (SECURITY DEFINER, миграция 0025) сам дополнительно
// скоупит DELETE по current_user_id() -- userID здесь не может быть чужим.
func (s *Service) DeleteAccount(ctx context.Context, userID, password string) error {
	return s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		var passwordHash *string
		if err := tx.QueryRow(ctx, `
			SELECT password_hash FROM public.profiles WHERE id = $1
		`, userID).Scan(&passwordHash); err != nil {
			return err
		}
		if passwordHash == nil || !auth.VerifyPassword(password, *passwordHash) {
			return apperror.Invalid("неверный пароль")
		}

		rows, err := tx.Query(ctx, `
			SELECT family.name
			FROM public.family_members member
			JOIN public.families family ON family.id = member.family_id
			WHERE member.user_id = $1
			  AND member.role = 'owner'
			  AND (
				SELECT COUNT(*) FROM public.family_members other
				WHERE other.family_id = member.family_id
			  ) > 1
		`, userID)
		if err != nil {
			return err
		}
		var blockingFamilies []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return err
			}
			blockingFamilies = append(blockingFamilies, name)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(blockingFamilies) > 0 {
			return apperror.Conflict(fmt.Sprintf(
				"вы владелец семей с другими участниками (%s). Сначала передайте владение в каждой из них, потом удаляйте аккаунт",
				strings.Join(blockingFamilies, ", "),
			))
		}

		_, err = tx.Exec(ctx, `SELECT public.delete_own_profile($1)`, userID)
		return err
	})
}

// RegisterRoutes монтирует роуты users в mux. Каждый роут здесь рассчитан
// на то, что выполняется за auth.Service.Middleware. authSvc нужен только
// DELETE /me -- очистить cookie сессии после удаления профиля (сама
// строка app_sessions каскадно исчезает вместе с profiles).
func RegisterRoutes(mux *http.ServeMux, svc *Service, authSvc *auth.Service) {
	mux.HandleFunc("GET /api/v1/users/me", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())

		profile, err := svc.GetProfile(r.Context(), user.ID)
		if err != nil {
			response.Error(w, err)
			return
		}

		response.JSON(w, http.StatusOK, profile)
	})

	mux.HandleFunc("DELETE /api/v1/users/me", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())

		var req struct {
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			response.Error(w, apperror.Invalid("invalid request body"))
			return
		}

		if err := svc.DeleteAccount(r.Context(), user.ID, req.Password); err != nil {
			response.Error(w, err)
			return
		}
		authSvc.ClearSessionCookie(w)
		response.JSON(w, http.StatusOK, map[string]bool{"success": true})
	})
}
