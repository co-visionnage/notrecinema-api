package auth

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"notrecinema/api/internal/platform/apperror"
	"notrecinema/api/internal/telemetry"
)

// SessionInfo -- одна активная сессия пользователя для списка устройств.
type SessionInfo struct {
	ID         string
	Current    bool
	IP         string
	UserAgent  string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
}

// ListSessions возвращает действующие сессии пользователя, самые свежие
// первыми. Идёт под пользовательским контекстом: политика
// app_sessions_owner_only и так отдаёт только его строки, а токены в
// список не попадают вовсе.
func (s *Service) ListSessions(ctx context.Context, userID, currentSessionID string) ([]SessionInfo, error) {
	var sessions []SessionInfo

	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, ip, user_agent, created_at, COALESCE(last_seen_at, created_at), expires_at
			FROM public.app_sessions
			WHERE user_id = $1 AND expires_at > NOW()
			ORDER BY COALESCE(last_seen_at, created_at) DESC
		`, userID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var (
				info      SessionInfo
				ip, agent *string
			)
			if err := rows.Scan(&info.ID, &ip, &agent, &info.CreatedAt, &info.LastSeenAt, &info.ExpiresAt); err != nil {
				return err
			}
			if ip != nil {
				info.IP = *ip
			}
			if agent != nil {
				info.UserAgent = *agent
			}
			info.Current = info.ID == currentSessionID
			sessions = append(sessions, info)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, apperror.Internal("failed to list sessions", err)
	}
	return sessions, nil
}

// RevokeSession закрывает одну сессию пользователя. false -- такой сессии у
// него нет (чужой id под RLS выглядит так же, как несуществующий).
func (s *Service) RevokeSession(ctx context.Context, userID, sessionID string) (bool, error) {
	var deleted bool
	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM public.app_sessions WHERE id = $1 AND user_id = $2`, sessionID, userID)
		deleted = tag.RowsAffected() > 0
		return err
	})
	if err != nil {
		return false, apperror.Internal("failed to revoke session", err)
	}
	if deleted {
		telemetry.RecordAuth("session_revoke", "one")
	}
	return deleted, nil
}

// RevokeOtherSessions закрывает все сессии пользователя, кроме текущей, и
// возвращает, сколько их было.
func (s *Service) RevokeOtherSessions(ctx context.Context, userID, keepSessionID string) (int, error) {
	var revoked int
	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM public.app_sessions WHERE user_id = $1 AND id <> $2`, userID, keepSessionID)
		revoked = int(tag.RowsAffected())
		return err
	})
	if err != nil {
		return 0, apperror.Internal("failed to revoke sessions", err)
	}
	if revoked > 0 {
		telemetry.RecordAuth("session_revoke", "others")
	}
	return revoked, nil
}

// ChangePassword меняет пароль из аккаунта: нужен текущий пароль. У
// OAuth-аккаунта без пароля менять нечего -- ему остаётся сброс по
// email, который доказывает владение почтой (украденная сессия не должна
// позволять задать пароль и закрепиться в аккаунте). Остальные сессии
// закрываются, на почту уходит уведомление.
func (s *Service) ChangePassword(ctx context.Context, userID, currentSessionID, currentPassword, newPassword string) (int, error) {
	var passwordHash *string
	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT password_hash FROM public.profiles WHERE id = $1`, userID).Scan(&passwordHash)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, apperror.NotFound("profile not found")
		}
		return 0, apperror.Internal("failed to load profile", err)
	}

	if passwordHash == nil {
		telemetry.RecordAuth("password_change", "no_password")
		return 0, apperror.Conflict("у аккаунта нет пароля (вход через GitHub). Задайте его через «Забыли пароль?»")
	}
	if !VerifyPassword(currentPassword, *passwordHash) {
		telemetry.RecordAuth("password_change", "wrong_password")
		return 0, apperror.Invalid("неверный текущий пароль")
	}
	if currentPassword == newPassword {
		return 0, apperror.Invalid("новый пароль должен отличаться от текущего")
	}

	newHash, err := HashPassword(newPassword)
	if err != nil {
		return 0, apperror.Internal("failed to hash password", err)
	}

	var revoked int
	err = s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT public.change_my_password($1, $2)`, newHash, currentSessionID).Scan(&revoked)
	})
	if err != nil {
		return 0, apperror.Internal("failed to change password", err)
	}
	telemetry.RecordAuth("password_change", "success")
	return revoked, nil
}
