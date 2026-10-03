package families

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"notrecinema/api/internal/activitylog"
	"notrecinema/api/internal/auth"
	"notrecinema/api/internal/outbox"
	"notrecinema/api/internal/platform/apperror"
	"notrecinema/api/internal/platform/ratelimit"
	"notrecinema/api/internal/platform/response"
)

// Приглашение в семью по email. Владелец или админ вводит адрес, человеку
// уходит письмо со ссылкой. Письмо отправляет notrecinema-worker: API
// только создаёт строку приглашения и кладёт событие в outbox, а токен
// рождается у воркера в момент отправки (в outbox и NATS его нет, как и у
// подтверждения email). Токен одноразовый, живёт 7 дней.

const (
	maxPendingInvitationsPerFamily = 20
	invitesPerUserMax              = 20
	invitesPerUserWindowSeconds    = 24 * 60 * 60
	resendsPerInvitationMax        = 3
	resendWindowSeconds            = 60 * 60
	previewIPMax                   = 30
	previewIPWindowSeconds         = 15 * 60
)

var invitationEmailPattern = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)

type Invitation struct {
	ID        string     `json:"id"`
	Email     string     `json:"email"`
	CreatedAt time.Time  `json:"createdAt"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

type InvitationPreview struct {
	FamilyName  string `json:"familyName"`
	InviterName string `json:"inviterName"`
}

type invitationRequestedEvent struct {
	InvitationID string `json:"invitationId"`
	FamilyID     string `json:"familyId"`
}

func hashInvitationToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Service) rateLimit(ctx context.Context, bucketKey string, maxAttempts, windowSeconds int) (bool, error) {
	return ratelimit.Check(ctx, s.db, bucketKey, maxAttempts, windowSeconds)
}

// CreateInvitation приглашает адрес в семью. Повторное приглашение того же
// адреса не плодит строки: обновляет существующее и отправляет письмо ещё
// раз. Права проверяет RLS (менеджер семьи: владелец или админ).
func (s *Service) CreateInvitation(ctx context.Context, userID, familyID, email string) (Invitation, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || !invitationEmailPattern.MatchString(email) {
		return Invitation{}, apperror.Invalid("введите корректный email-адрес")
	}

	allowed, err := s.rateLimit(ctx, "invite:user:"+userID, invitesPerUserMax, invitesPerUserWindowSeconds)
	if err != nil {
		return Invitation{}, apperror.Internal("rate limit check failed", err)
	}
	if !allowed {
		return Invitation{}, apperror.RateLimited("слишком много приглашений за сутки. Попробуйте завтра")
	}

	var invitation Invitation
	err = s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		var isManager bool
		if err := tx.QueryRow(ctx, `SELECT public.is_family_manager($1)`, familyID).Scan(&isManager); err != nil {
			return err
		}
		if !isManager {
			return apperror.Forbidden("приглашать в семью могут владелец и администраторы")
		}

		var alreadyMember bool
		if err := tx.QueryRow(ctx, `SELECT public.is_family_member_email($1, $2)`, familyID, email).Scan(&alreadyMember); err != nil {
			return err
		}
		if alreadyMember {
			return apperror.Conflict("этот человек уже состоит в семье")
		}

		var pending int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM public.family_invitations
			WHERE family_id = $1 AND accepted_at IS NULL AND LOWER(email) <> $2
		`, familyID, email).Scan(&pending); err != nil {
			return err
		}
		if pending >= maxPendingInvitationsPerFamily {
			return apperror.Conflict("слишком много неотвеченных приглашений: отзовите ненужные")
		}

		if err := tx.QueryRow(ctx, `
			INSERT INTO public.family_invitations (family_id, email, invited_by)
			VALUES ($1, $2, $3)
			ON CONFLICT (family_id, LOWER(email)) WHERE accepted_at IS NULL
			DO UPDATE SET invited_by = EXCLUDED.invited_by, created_at = NOW()
			RETURNING id, email, created_at
		`, familyID, email, userID).Scan(&invitation.ID, &invitation.Email, &invitation.CreatedAt); err != nil {
			return err
		}

		return outbox.Insert(ctx, tx, "family", familyID, "family.invitation_requested",
			invitationRequestedEvent{InvitationID: invitation.ID, FamilyID: familyID})
	})
	if err != nil {
		var appErr *apperror.Error
		if errors.As(err, &appErr) {
			return Invitation{}, appErr
		}
		return Invitation{}, apperror.Internal("failed to create invitation", err)
	}
	return invitation, nil
}

// ListInvitations возвращает неотвеченные приглашения семьи (видны только
// менеджерам: так устроена RLS-политика).
func (s *Service) ListInvitations(ctx context.Context, userID, familyID string) ([]Invitation, error) {
	invitations := []Invitation{}
	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, email, created_at, expires_at
			FROM public.family_invitations
			WHERE family_id = $1 AND accepted_at IS NULL
			ORDER BY created_at DESC
		`, familyID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var inv Invitation
			if err := rows.Scan(&inv.ID, &inv.Email, &inv.CreatedAt, &inv.ExpiresAt); err != nil {
				return err
			}
			invitations = append(invitations, inv)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, apperror.Internal("failed to list invitations", err)
	}
	return invitations, nil
}

// RevokeInvitation отзывает неотвеченное приглашение. Токен из уже
// отправленного письма после этого перестаёт работать (строки нет).
func (s *Service) RevokeInvitation(ctx context.Context, userID, familyID, invitationID string) error {
	var deleted bool
	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			DELETE FROM public.family_invitations
			WHERE id = $1 AND family_id = $2 AND accepted_at IS NULL
		`, invitationID, familyID)
		deleted = tag.RowsAffected() > 0
		return err
	})
	if err != nil {
		return apperror.Internal("failed to revoke invitation", err)
	}
	if !deleted {
		return apperror.NotFound("invitation not found")
	}
	return nil
}

// ResendInvitation отправляет письмо с приглашением ещё раз (новый токен,
// срок снова 7 дней).
func (s *Service) ResendInvitation(ctx context.Context, userID, familyID, invitationID string) error {
	// Сначала права и существование, потом лимит: иначе посторонний мог бы
	// выбирать чужой лимит отправок, дёргая ручку с чужим id.
	var exists bool
	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM public.family_invitations
				WHERE id = $1 AND family_id = $2 AND accepted_at IS NULL
			)
		`, invitationID, familyID).Scan(&exists)
	})
	if err != nil {
		return apperror.Internal("failed to load invitation", err)
	}
	if !exists {
		return apperror.NotFound("invitation not found")
	}

	allowed, err := s.rateLimit(ctx, "invite-resend:"+invitationID, resendsPerInvitationMax, resendWindowSeconds)
	if err != nil {
		return apperror.Internal("rate limit check failed", err)
	}
	if !allowed {
		return apperror.RateLimited("письмо уже отправлялось недавно. Попробуйте позже")
	}

	err = s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		return outbox.Insert(ctx, tx, "family", familyID, "family.invitation_requested",
			invitationRequestedEvent{InvitationID: invitationID, FamilyID: familyID})
	})
	if err != nil {
		return apperror.Internal("failed to resend invitation", err)
	}
	return nil
}

// AcceptInvitation принимает приглашение от имени вошедшего пользователя.
func (s *Service) AcceptInvitation(ctx context.Context, userID, token string) (Family, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return Family{}, apperror.Invalid("token is required")
	}

	family := Family{Role: "member", JoinedAt: time.Now()}
	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		var familyName string
		if err := tx.QueryRow(ctx, `
			SELECT family_id, family_name FROM public.accept_family_invitation($1)
		`, hashInvitationToken(token)).Scan(&family.ID, &familyName); err != nil {
			return err
		}
		family.Name = familyName

		// После вступления семья видна обычным RLS-путём.
		if err := tx.QueryRow(ctx, `
			SELECT invite_code, owner_id FROM public.families WHERE id = $1
		`, family.ID).Scan(&family.InviteCode, &family.OwnerID); err != nil {
			return err
		}

		// Запись в журнал -- только если участник добавлен именно сейчас
		// (joined_at проставлен NOW() этой же транзакции), а не уже состоял.
		var justJoined bool
		if err := tx.QueryRow(ctx, `
			SELECT joined_at = NOW() FROM public.family_members WHERE family_id = $1 AND user_id = $2
		`, family.ID, userID).Scan(&justJoined); err != nil {
			return err
		}
		if !justJoined {
			return nil
		}
		displayName, err := lookupDisplayName(ctx, tx, userID)
		if err != nil {
			return err
		}
		return activitylog.Insert(ctx, tx, family.ID, userID, displayName, "member_joined", nil, nil)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Family{}, apperror.Invalid("приглашение недействительно или истекло")
		}
		return Family{}, apperror.Internal("failed to accept invitation", err)
	}
	return family, nil
}

// PreviewInvitation показывает, куда зовут, до входа в аккаунт.
func (s *Service) PreviewInvitation(ctx context.Context, token string) (InvitationPreview, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return InvitationPreview{}, apperror.Invalid("token is required")
	}

	var preview InvitationPreview
	err := s.db.QueryRow(ctx, `
		SELECT family_name, inviter_name FROM public.preview_family_invitation($1)
	`, hashInvitationToken(token)).Scan(&preview.FamilyName, &preview.InviterName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return InvitationPreview{}, apperror.NotFound("приглашение недействительно или истекло")
		}
		return InvitationPreview{}, apperror.Internal("failed to preview invitation", err)
	}
	return preview, nil
}

// RegisterInvitationRoutes монтирует роуты приглашений: управление и приём
// -- за сессией (protectedMux), предпросмотр -- публичный (по ссылке из
// письма человек может быть ещё не вошёл).
func RegisterInvitationRoutes(protectedMux, publicMux *http.ServeMux, svc *Service) {
	protectedMux.HandleFunc("POST /api/v1/families/{familyId}/invitations", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())

		var req struct {
			Email string `json:"email"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			response.Error(w, apperror.Invalid("invalid request body"))
			return
		}

		invitation, err := svc.CreateInvitation(r.Context(), user.ID, r.PathValue("familyId"), req.Email)
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusCreated, invitation)
	})

	protectedMux.HandleFunc("GET /api/v1/families/{familyId}/invitations", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())

		invitations, err := svc.ListInvitations(r.Context(), user.ID, r.PathValue("familyId"))
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, invitations)
	})

	protectedMux.HandleFunc("DELETE /api/v1/families/{familyId}/invitations/{invitationId}", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())

		if err := svc.RevokeInvitation(r.Context(), user.ID, r.PathValue("familyId"), r.PathValue("invitationId")); err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, map[string]bool{"success": true})
	})

	protectedMux.HandleFunc("POST /api/v1/families/{familyId}/invitations/{invitationId}/resend", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())

		if err := svc.ResendInvitation(r.Context(), user.ID, r.PathValue("familyId"), r.PathValue("invitationId")); err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, map[string]bool{"success": true})
	})

	protectedMux.HandleFunc("POST /api/v1/invitations/accept", func(w http.ResponseWriter, r *http.Request) {
		user := auth.UserFromContext(r.Context())

		var req struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			response.Error(w, apperror.Invalid("invalid request body"))
			return
		}

		family, err := svc.AcceptInvitation(r.Context(), user.ID, req.Token)
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, family)
	})

	publicMux.HandleFunc("GET /api/v1/invitations/preview", func(w http.ResponseWriter, r *http.Request) {
		allowed, err := svc.rateLimit(r.Context(), "invite-preview:ip:"+requestIP(r), previewIPMax, previewIPWindowSeconds)
		if err != nil {
			response.Error(w, apperror.Internal("rate limit check failed", err))
			return
		}
		if !allowed {
			response.Error(w, apperror.RateLimited("слишком много попыток. Попробуйте позже"))
			return
		}

		preview, err := svc.PreviewInvitation(r.Context(), r.URL.Query().Get("token"))
		if err != nil {
			response.Error(w, err)
			return
		}
		response.JSON(w, http.StatusOK, preview)
	})
}

// requestIP -- та же логика, что auth.clientIP: X-Real-IP (его заменяет
// reverse proxy) важнее последнего хопа X-Forwarded-For.
func requestIP(r *http.Request) string {
	if realIP := strings.TrimSpace(r.Header.Get("X-Real-Ip")); realIP != "" {
		return realIP
	}
	if forwardedFor := r.Header.Get("X-Forwarded-For"); forwardedFor != "" {
		hops := strings.Split(forwardedFor, ",")
		return strings.TrimSpace(hops[len(hops)-1])
	}
	return "unknown"
}
