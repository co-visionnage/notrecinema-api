//go:build integration

package auth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"

	"notrecinema/api/internal/auth"
	"notrecinema/api/internal/postgres"
)

func newService(db *postgres.Pool) *auth.Service {
	return auth.NewService(db, "notre_cinema_session", "development")
}

func currentSessionID(t *testing.T, db *postgres.Pool, token string) string {
	t.Helper()
	var id string
	if err := db.QueryRow(context.Background(), `
		SELECT session_id FROM public.get_session_user($1)
	`, sha256Hex(token)).Scan(&id); err != nil {
		t.Fatalf("get_session_user: %v", err)
	}
	return id
}

// enableTwoFactor включает 2FA для нового пользователя и возвращает
// секрет, резервные коды и сессию, созданную при регистрации.
func enableTwoFactor(t *testing.T, svc *auth.Service) (email string, secret string, codes []string, session auth.Session) {
	t.Helper()
	ctx := context.Background()
	email, session = registerUser(t, svc, "2fa")

	setup, err := svc.StartTwoFactorSetup(ctx, session.User.ID, email)
	if err != nil {
		t.Fatalf("StartTwoFactorSetup() error: %v", err)
	}
	code, err := totp.GenerateCode(setup.Secret, time.Now())
	if err != nil {
		t.Fatalf("GenerateCode() error: %v", err)
	}
	codes, err = svc.ConfirmTwoFactor(ctx, session.User.ID, code)
	if err != nil {
		t.Fatalf("ConfirmTwoFactor() error: %v", err)
	}
	return email, setup.Secret, codes, session
}

func loginChallenge(t *testing.T, svc *auth.Service, email string) string {
	t.Helper()
	result, err := svc.Login(context.Background(), email, testPassword)
	if err != nil {
		t.Fatalf("Login() error: %v", err)
	}
	if !result.RequiresTwoFactor {
		t.Fatal("Login() did not ask for the second factor")
	}
	return result.ChallengeToken
}

func TestConfirmTwoFactorIssuesTenBackupCodes(t *testing.T) {
	db := connectOrSkip(t)
	svc := newService(db)
	_, _, codes, session := enableTwoFactor(t, svc)

	if len(codes) != 10 {
		t.Fatalf("backup codes = %d, want 10", len(codes))
	}
	_, remaining, err := svc.TwoFactorStatus(context.Background(), session.User.ID)
	if err != nil {
		t.Fatalf("TwoFactorStatus() error: %v", err)
	}
	if remaining != 10 {
		t.Errorf("remaining = %d, want 10", remaining)
	}

	// В БД не должно быть кодов в открытом виде: только хэши, которые
	// нельзя сопоставить с кодом без scrypt.
	rows, err := db.Query(context.Background(), `
		SELECT code_hash FROM public.get_two_factor_backup_code_hashes($1)
	`, session.User.ID)
	if err != nil {
		t.Fatalf("hashes: %v", err)
	}
	defer rows.Close()
	stored := 0
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			t.Fatalf("scan: %v", err)
		}
		stored++
		for _, code := range codes {
			plain := strings.ReplaceAll(code, "-", "")
			if strings.Contains(hash, plain) {
				t.Errorf("a stored hash contains a code in the clear")
			}
		}
	}
	if stored != 10 {
		t.Errorf("stored hashes = %d, want 10", stored)
	}
}

func TestConfirmTwoFactorTwiceIsRejected(t *testing.T) {
	db := connectOrSkip(t)
	svc := newService(db)
	_, secret, _, session := enableTwoFactor(t, svc)

	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("GenerateCode() error: %v", err)
	}
	if _, err := svc.ConfirmTwoFactor(context.Background(), session.User.ID, code); err == nil {
		t.Error("ConfirmTwoFactor() succeeded for an account that already has 2FA (would silently reissue codes)")
	}
}

func TestBackupCodeSignsInExactlyOnce(t *testing.T) {
	db := connectOrSkip(t)
	svc := newService(db)
	ctx := context.Background()
	email, _, codes, session := enableTwoFactor(t, svc)

	// Любой регистр и без дефиса -- так человек перепечатывает код с бумаги.
	typed := strings.ToUpper(strings.ReplaceAll(codes[3], "-", ""))
	signedIn, err := svc.VerifyTwoFactor(ctx, loginChallenge(t, svc, email), typed)
	if err != nil {
		t.Fatalf("VerifyTwoFactor() with a backup code error: %v", err)
	}
	if signedIn.User.ID != session.User.ID {
		t.Errorf("signed in as %q, want %q", signedIn.User.ID, session.User.ID)
	}

	_, remaining, _ := svc.TwoFactorStatus(ctx, session.User.ID)
	if remaining != 9 {
		t.Errorf("remaining = %d, want 9", remaining)
	}
	if got := countEvents(t, db, session.User.ID, "security.backup_code_used"); got != 1 {
		t.Errorf("backup_code_used events = %d, want 1", got)
	}

	if _, err := svc.VerifyTwoFactor(ctx, loginChallenge(t, svc, email), codes[3]); err == nil {
		t.Error("the same backup code worked twice")
	}
	// Остальные коды при этом живы.
	if _, err := svc.VerifyTwoFactor(ctx, loginChallenge(t, svc, email), codes[4]); err != nil {
		t.Errorf("another unused backup code was rejected: %v", err)
	}
}

func TestBackupCodeEventReportsRemainingCount(t *testing.T) {
	db := connectOrSkip(t)
	svc := newService(db)
	email, _, codes, session := enableTwoFactor(t, svc)

	if _, err := svc.VerifyTwoFactor(context.Background(), loginChallenge(t, svc, email), codes[0]); err != nil {
		t.Fatalf("VerifyTwoFactor() error: %v", err)
	}

	var remaining int
	if err := db.QueryRow(context.Background(), `
		SELECT (payload->>'remaining')::int FROM public.outbox_events
		WHERE aggregate_id = $1 AND event_type = 'security.backup_code_used'
	`, session.User.ID).Scan(&remaining); err != nil {
		t.Fatalf("event: %v", err)
	}
	if remaining != 9 {
		t.Errorf("event remaining = %d, want 9", remaining)
	}
}

func TestWrongBackupCodeIsRejectedAndSpendsNothing(t *testing.T) {
	db := connectOrSkip(t)
	svc := newService(db)
	ctx := context.Background()
	email, _, _, session := enableTwoFactor(t, svc)

	for _, wrong := range []string{"abcde-fghjk", "zzzzz-zzzzz", "not a code", "000000"} {
		if _, err := svc.VerifyTwoFactor(ctx, loginChallenge(t, svc, email), wrong); err == nil {
			t.Errorf("VerifyTwoFactor() accepted %q", wrong)
		}
	}

	_, remaining, _ := svc.TwoFactorStatus(ctx, session.User.ID)
	if remaining != 10 {
		t.Errorf("remaining = %d after failed attempts, want 10", remaining)
	}
}

func TestRegenerateBackupCodesInvalidatesTheOldSet(t *testing.T) {
	db := connectOrSkip(t)
	svc := newService(db)
	ctx := context.Background()
	email, secret, oldCodes, session := enableTwoFactor(t, svc)

	if _, err := svc.RegenerateBackupCodes(ctx, session.User.ID, "000000"); err == nil {
		t.Error("RegenerateBackupCodes() accepted a wrong code")
	}

	code, _ := totp.GenerateCode(secret, time.Now())
	newCodes, err := svc.RegenerateBackupCodes(ctx, session.User.ID, code)
	if err != nil {
		t.Fatalf("RegenerateBackupCodes() error: %v", err)
	}
	if len(newCodes) != 10 || newCodes[0] == oldCodes[0] {
		t.Fatalf("regenerated set looks wrong: %d codes", len(newCodes))
	}
	if got := countEvents(t, db, session.User.ID, "security.backup_codes_regenerated"); got != 1 {
		t.Errorf("regenerated events = %d, want 1", got)
	}

	if _, err := svc.VerifyTwoFactor(ctx, loginChallenge(t, svc, email), oldCodes[0]); err == nil {
		t.Error("an old backup code still works after regeneration")
	}
	if _, err := svc.VerifyTwoFactor(ctx, loginChallenge(t, svc, email), newCodes[0]); err != nil {
		t.Errorf("a new backup code does not work: %v", err)
	}
}

func TestRegenerateBackupCodesRequiresTwoFactor(t *testing.T) {
	db := connectOrSkip(t)
	svc := newService(db)
	_, session := registerUser(t, svc, "no2fa")

	if _, err := svc.RegenerateBackupCodes(context.Background(), session.User.ID, "123456"); err == nil {
		t.Error("RegenerateBackupCodes() succeeded without 2FA enabled")
	}
}

func TestDisableTwoFactorWithBackupCodeWipesTheRest(t *testing.T) {
	db := connectOrSkip(t)
	svc := newService(db)
	ctx := context.Background()
	email, _, codes, session := enableTwoFactor(t, svc)

	// Человек потерял телефон, вошёл по резервному коду и отключает 2FA
	// тем же способом.
	signedIn, err := svc.VerifyTwoFactor(ctx, loginChallenge(t, svc, email), codes[0])
	if err != nil {
		t.Fatalf("VerifyTwoFactor() error: %v", err)
	}
	if err := svc.DisableTwoFactor(ctx, signedIn.User.ID, codes[1]); err != nil {
		t.Fatalf("DisableTwoFactor() with a backup code error: %v", err)
	}

	enabled, remaining, err := svc.TwoFactorStatus(ctx, session.User.ID)
	if err != nil {
		t.Fatalf("TwoFactorStatus() error: %v", err)
	}
	if enabled || remaining != 0 {
		t.Errorf("status = (enabled=%v, remaining=%d), want (false, 0)", enabled, remaining)
	}
	if result, err := svc.Login(ctx, email, testPassword); err != nil || result.RequiresTwoFactor {
		t.Errorf("login after disabling 2FA = (%+v, %v), want a plain session", result, err)
	}
}

func TestSessionRecordsWhereItWasCreated(t *testing.T) {
	db := connectOrSkip(t)
	svc := newService(db)
	ctx := context.Background()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
	req.Header.Set("X-Real-Ip", "203.0.113.7")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Test)")

	email := randomEmail(t, "meta")
	session, err := svc.Register(auth.WithRequest(ctx, req), email, "Test User", testPassword)
	if err != nil {
		t.Fatalf("Register() error: %v", err)
	}
	// Второй вход без метаданных (например, из теста): сессия всё равно
	// должна создаться.
	if _, err := svc.Login(ctx, email, testPassword); err != nil {
		t.Fatalf("Login() error: %v", err)
	}

	sessions, err := svc.ListSessions(ctx, session.User.ID, currentSessionID(t, db, session.Token))
	if err != nil {
		t.Fatalf("ListSessions() error: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("sessions = %d, want 2", len(sessions))
	}

	var current, other *auth.SessionInfo
	for i := range sessions {
		if sessions[i].Current {
			current = &sessions[i]
		} else {
			other = &sessions[i]
		}
	}
	if current == nil || other == nil {
		t.Fatalf("expected one current and one other session, got %+v", sessions)
	}
	if current.IP != "203.0.113.7" || current.UserAgent != "Mozilla/5.0 (Test)" {
		t.Errorf("current session metadata = (%q, %q)", current.IP, current.UserAgent)
	}
	if other.IP != "" || other.UserAgent != "" {
		t.Errorf("a session created without request info has metadata: %+v", other)
	}
}

func TestRevokeSessionsStaysWithinTheOwner(t *testing.T) {
	db := connectOrSkip(t)
	svc := newService(db)
	ctx := context.Background()

	email, mine := registerUser(t, svc, "revoke-mine")
	_, theirs := registerUser(t, svc, "revoke-theirs")
	second, err := svc.Login(ctx, email, testPassword)
	if err != nil {
		t.Fatalf("Login() error: %v", err)
	}
	mineID := currentSessionID(t, db, mine.Token)
	secondID := currentSessionID(t, db, second.Session.Token)
	theirsID := currentSessionID(t, db, theirs.Token)

	// Чужую сессию закрыть нельзя -- для RLS она не существует.
	deleted, err := svc.RevokeSession(ctx, mine.User.ID, theirsID)
	if err != nil || deleted {
		t.Errorf("RevokeSession(foreign) = (%v, %v), want (false, nil)", deleted, err)
	}
	if sessionCount(t, db, theirs.User.ID) != 1 {
		t.Error("someone else's session was revoked")
	}

	deleted, err = svc.RevokeSession(ctx, mine.User.ID, secondID)
	if err != nil || !deleted {
		t.Errorf("RevokeSession(own) = (%v, %v), want (true, nil)", deleted, err)
	}

	third, _ := svc.Login(ctx, email, testPassword)
	_ = third
	revoked, err := svc.RevokeOtherSessions(ctx, mine.User.ID, mineID)
	if err != nil {
		t.Fatalf("RevokeOtherSessions() error: %v", err)
	}
	if revoked != 1 {
		t.Errorf("revoked = %d, want 1", revoked)
	}
	if sessionCount(t, db, mine.User.ID) != 1 {
		t.Error("the current session did not survive 'sign out everywhere else'")
	}
	if sessionCount(t, db, theirs.User.ID) != 1 {
		t.Error("another user's session was touched")
	}
}

func TestChangePassword(t *testing.T) {
	db := connectOrSkip(t)
	svc := newService(db)
	ctx := context.Background()
	email, current := registerUser(t, svc, "change-pw")
	other, err := svc.Login(ctx, email, testPassword)
	if err != nil {
		t.Fatalf("Login() error: %v", err)
	}
	currentID := currentSessionID(t, db, current.Token)

	if _, err := svc.ChangePassword(ctx, current.User.ID, currentID, "wrong-password-1!", "brand-new-Passw0rd!"); err == nil {
		t.Error("ChangePassword() accepted a wrong current password")
	}
	if _, err := svc.ChangePassword(ctx, current.User.ID, currentID, testPassword, testPassword); err == nil {
		t.Error("ChangePassword() accepted an unchanged password")
	}
	if got := countEvents(t, db, current.User.ID, "security.password_changed"); got != 0 {
		t.Fatalf("password_changed events after rejected attempts = %d, want 0", got)
	}

	revoked, err := svc.ChangePassword(ctx, current.User.ID, currentID, testPassword, "brand-new-Passw0rd!")
	if err != nil {
		t.Fatalf("ChangePassword() error: %v", err)
	}
	if revoked != 1 {
		t.Errorf("revoked sessions = %d, want 1 (the other device)", revoked)
	}
	if sessionCount(t, db, current.User.ID) != 1 {
		t.Error("only the current session should survive a password change")
	}
	_ = other

	if got := countEvents(t, db, current.User.ID, "security.password_changed"); got != 1 {
		t.Errorf("password_changed events = %d, want 1", got)
	}
	if _, err := svc.Login(ctx, email, testPassword); err == nil {
		t.Error("the old password still works")
	}
	if _, err := svc.Login(ctx, email, "brand-new-Passw0rd!"); err != nil {
		t.Errorf("the new password does not work: %v", err)
	}
}

func TestChangePasswordForOAuthOnlyAccountPointsToReset(t *testing.T) {
	db := connectOrSkip(t)
	svc := newService(db)

	session, err := svc.CreateSessionForEmail(context.Background(), randomEmail(t, "oauth-pw"), "GitHub User")
	if err != nil {
		t.Fatalf("CreateSessionForEmail() error: %v", err)
	}
	_, err = svc.ChangePassword(context.Background(), session.User.ID, currentSessionID(t, db, session.Token), "", "brand-new-Passw0rd!")
	if err == nil {
		t.Fatal("an account without a password was allowed to 'change' it from a session")
	}
	if !strings.Contains(err.Error(), "нет пароля") && !strings.Contains(err.Error(), "Забыли") {
		t.Logf("error text: %v", err)
	}
}

// HTTP-уровень: контракт JSON, который читает фронтенд.
func TestSecurityRoutesOverHTTP(t *testing.T) {
	db := connectOrSkip(t)
	svc := newService(db)
	mux := http.NewServeMux()
	auth.RegisterProtectedRoutes(mux, svc)
	auth.RegisterSessionRoutes(mux, svc)
	handler := svc.Middleware(mux)

	email, secret, codes, session := enableTwoFactor(t, svc)
	_ = email
	_ = codes

	do := func(method, path string, body any) *httptest.ResponseRecorder {
		var reader *bytes.Reader
		if body != nil {
			raw, _ := json.Marshal(body)
			reader = bytes.NewReader(raw)
		} else {
			reader = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(method, path, reader)
		req.AddCookie(&http.Cookie{Name: "notre_cinema_session", Value: session.Token})
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	rec := do(http.MethodGet, "/api/v1/auth/2fa/status", nil)
	var status struct {
		Enabled              bool `json:"enabled"`
		BackupCodesRemaining int  `json:"backupCodesRemaining"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil || !status.Enabled || status.BackupCodesRemaining != 10 {
		t.Errorf("2fa status = %s", rec.Body.String())
	}

	code, _ := totp.GenerateCode(secret, time.Now())
	rec = do(http.MethodPost, "/api/v1/auth/2fa/backup-codes/regenerate", map[string]string{"code": code})
	var regenerated struct {
		BackupCodes []string `json:"backupCodes"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &regenerated) != nil || len(regenerated.BackupCodes) != 10 {
		t.Errorf("regenerate = %d %s", rec.Code, rec.Body.String())
	}

	rec = do(http.MethodGet, "/api/v1/auth/sessions", nil)
	var listed struct {
		Sessions []struct {
			ID      string `json:"id"`
			Current bool   `json:"current"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil || len(listed.Sessions) != 1 || !listed.Sessions[0].Current {
		t.Errorf("sessions = %s", rec.Body.String())
	}

	rec = do(http.MethodPost, "/api/v1/auth/password/change", map[string]string{
		"currentPassword": testPassword, "newPassword": "short", "confirmPassword": "short",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("weak new password = %d, want 400", rec.Code)
	}
	rec = do(http.MethodPost, "/api/v1/auth/password/change", map[string]string{
		"currentPassword": testPassword, "newPassword": "brand-new-Passw0rd!", "confirmPassword": "different-Passw0rd!",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("mismatching confirmation = %d, want 400", rec.Code)
	}
	rec = do(http.MethodPost, "/api/v1/auth/password/change", map[string]string{
		"currentPassword": testPassword, "newPassword": "brand-new-Passw0rd!", "confirmPassword": "brand-new-Passw0rd!",
	})
	if rec.Code != http.StatusOK {
		t.Errorf("password change = %d %s", rec.Code, rec.Body.String())
	}

	// Закрытие текущей сессии = выход: cookie сбрасывается.
	rec = do(http.MethodDelete, "/api/v1/auth/sessions/"+listed.Sessions[0].ID, nil)
	if rec.Code != http.StatusOK {
		t.Errorf("revoking the current session = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(strings.Join(rec.Header().Values("Set-Cookie"), ";"), "notre_cinema_session=;") {
		t.Errorf("session cookie was not cleared: %v", rec.Header().Values("Set-Cookie"))
	}
	if rec = do(http.MethodGet, "/api/v1/auth/sessions", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("a revoked session still works: %d", rec.Code)
	}
}
