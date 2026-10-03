//go:build integration

package notifications_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"notrecinema/api/internal/auth"
	"notrecinema/api/internal/notifications"
	"notrecinema/api/internal/platform/apperror"
	"notrecinema/api/internal/postgres"
)

const testSecret = "integration-unsubscribe-secret"

func connectOrSkip(t *testing.T) *postgres.Pool {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL не задан, пропускаем интеграционный тест")
	}
	db, err := postgres.Connect(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

func newUser(t *testing.T, db *postgres.Pool) auth.Session {
	t.Helper()
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand: %v", err)
	}
	session, err := auth.NewService(db, "notre_cinema_session", "development").
		Register(context.Background(), fmt.Sprintf("prefs-%s@example.com", hex.EncodeToString(buf)), "Prefs User", "correct-horse-battery-1!")
	if err != nil {
		t.Fatalf("Register() error: %v", err)
	}
	return session
}

func byCategory(prefs []notifications.Preference) map[string]notifications.Preference {
	m := map[string]notifications.Preference{}
	for _, p := range prefs {
		m[p.Category] = p
	}
	return m
}

// Значения по умолчанию в Go и в SQL (notification_default) обязаны совпадать:
// интерфейс показывает Go-значения, а рассылку решает SQL.
func TestGoDefaultsMatchTheSQLDefaults(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()

	for _, c := range notifications.Categories {
		var sqlPush, sqlEmail bool
		if err := db.QueryRow(ctx, `
			SELECT public.notification_default($1, 'push'), public.notification_default($1, 'email')
		`, c.Key).Scan(&sqlPush, &sqlEmail); err != nil {
			t.Fatalf("notification_default(%s): %v", c.Key, err)
		}
		if !sqlPush {
			t.Errorf("%s: SQL default for push is off, Go assumes on", c.Key)
		}
		if sqlEmail != c.EmailDefault {
			t.Errorf("%s: SQL email default = %v, Go = %v", c.Key, sqlEmail, c.EmailDefault)
		}
	}
}

func TestPreferencesDefaultsThenOverrides(t *testing.T) {
	db := connectOrSkip(t)
	svc := notifications.NewService(db)
	ctx := context.Background()
	user := newUser(t, db)

	prefs, err := svc.GetPreferences(ctx, user.User.ID)
	if err != nil {
		t.Fatalf("GetPreferences() error: %v", err)
	}
	if len(prefs) != len(notifications.Categories) {
		t.Fatalf("preferences = %d, want one per category", len(prefs))
	}
	got := byCategory(prefs)
	if !got["series_added"].Push || !got["series_added"].Email || !got["series_added"].EmailSupported {
		t.Errorf("series_added default = %+v", got["series_added"])
	}
	if !got["poll"].Push || got["poll"].Email || got["poll"].EmailSupported {
		t.Errorf("poll default = %+v", got["poll"])
	}

	err = svc.SetPreferences(ctx, user.User.ID, []notifications.PreferenceInput{
		{Category: "series_added", Push: false, Email: false},
		{Category: "poll", Push: false, Email: false},
	})
	if err != nil {
		t.Fatalf("SetPreferences() error: %v", err)
	}

	got = byCategory(mustGet(t, svc, user.User.ID))
	if got["series_added"].Push || got["series_added"].Email {
		t.Errorf("series_added after override = %+v", got["series_added"])
	}
	if got["poll"].Push {
		t.Errorf("poll after override = %+v", got["poll"])
	}
	// Остальные категории не тронуты.
	if !got["season_update"].Push || !got["season_update"].Email {
		t.Errorf("season_update changed unexpectedly: %+v", got["season_update"])
	}

	// SQL видит ровно то, что видит интерфейс.
	var pushOn, emailOn, seasonEmailOn bool
	if err := db.QueryRow(ctx, `
		SELECT public.notification_enabled($1, 'series_added', 'push'),
		       public.notification_enabled($1, 'series_added', 'email'),
		       public.notification_enabled($1, 'season_update', 'email')
	`, user.User.ID).Scan(&pushOn, &emailOn, &seasonEmailOn); err != nil {
		t.Fatalf("notification_enabled: %v", err)
	}
	if pushOn || emailOn || !seasonEmailOn {
		t.Errorf("notification_enabled = (push=%v, email=%v, season email=%v)", pushOn, emailOn, seasonEmailOn)
	}

	// Повторное сохранение обновляет, а не падает на дубле.
	if err := svc.SetPreferences(ctx, user.User.ID, []notifications.PreferenceInput{
		{Category: "series_added", Push: true, Email: true},
	}); err != nil {
		t.Fatalf("second SetPreferences() error: %v", err)
	}
	if p := byCategory(mustGet(t, svc, user.User.ID))["series_added"]; !p.Push || !p.Email {
		t.Errorf("series_added after re-save = %+v", p)
	}
}

func mustGet(t *testing.T, svc *notifications.Service, userID string) []notifications.Preference {
	t.Helper()
	prefs, err := svc.GetPreferences(context.Background(), userID)
	if err != nil {
		t.Fatalf("GetPreferences() error: %v", err)
	}
	return prefs
}

func TestSetPreferencesValidation(t *testing.T) {
	db := connectOrSkip(t)
	svc := notifications.NewService(db)
	user := newUser(t, db)

	cases := map[string][]notifications.PreferenceInput{
		"empty":              {},
		"unknown category":   {{Category: "everything", Push: true}},
		"email unsupported":  {{Category: "poll", Push: true, Email: true}},
		"duplicate category": {{Category: "poll", Push: true}, {Category: "poll", Push: false}},
	}
	for name, inputs := range cases {
		err := svc.SetPreferences(context.Background(), user.User.ID, inputs)
		appErr, ok := err.(*apperror.Error)
		if !ok || appErr.Kind != apperror.KindInvalidInput {
			t.Errorf("%s: err = %v, want invalid input", name, err)
		}
	}
	// Ни одна отклонённая попытка ничего не сохранила.
	for _, p := range mustGet(t, svc, user.User.ID) {
		if p.Category == "poll" && !p.Push {
			t.Error("a rejected request changed the stored preferences")
		}
	}
}

func TestPreferencesAreIsolatedBetweenUsers(t *testing.T) {
	db := connectOrSkip(t)
	svc := notifications.NewService(db)
	ctx := context.Background()
	alice := newUser(t, db)
	bob := newUser(t, db)

	if err := svc.SetPreferences(ctx, alice.User.ID, []notifications.PreferenceInput{{Category: "poll", Push: false}}); err != nil {
		t.Fatalf("SetPreferences() error: %v", err)
	}
	if p := byCategory(mustGet(t, svc, bob.User.ID))["poll"]; !p.Push {
		t.Error("one user's preferences leaked to another")
	}
}

func TestUnsubscribeEmailTurnsOffOnlyEmailOfThatCategory(t *testing.T) {
	db := connectOrSkip(t)
	svc := notifications.NewService(db).WithUnsubscribeSecret(testSecret)
	ctx := context.Background()
	user := newUser(t, db)

	token := notifications.SignUnsubscribeToken(testSecret, user.User.ID, "series_added")
	category, err := svc.UnsubscribeEmail(ctx, token)
	if err != nil || category != "series_added" {
		t.Fatalf("UnsubscribeEmail() = (%q, %v)", category, err)
	}

	got := byCategory(mustGet(t, svc, user.User.ID))
	if got["series_added"].Email {
		t.Error("email for the category is still on")
	}
	if !got["series_added"].Push {
		t.Error("unsubscribing from email must not touch push")
	}
	if !got["season_update"].Email {
		t.Error("another category's email was switched off")
	}

	// Повторная отписка безвредна.
	if _, err := svc.UnsubscribeEmail(ctx, token); err != nil {
		t.Errorf("repeating the unsubscribe: %v", err)
	}
}

func TestUnsubscribeEmailRejectsBadTokensAndMissingSecret(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()
	user := newUser(t, db)

	svc := notifications.NewService(db).WithUnsubscribeSecret(testSecret)
	for _, bad := range []string{"", "junk", notifications.SignUnsubscribeToken("other-secret", user.User.ID, "series_added")} {
		if _, err := svc.UnsubscribeEmail(ctx, bad); err == nil {
			t.Errorf("UnsubscribeEmail(%q) accepted a bad token", bad)
		}
	}
	if p := byCategory(mustGet(t, svc, user.User.ID))["series_added"]; !p.Email {
		t.Error("a rejected token changed the preferences")
	}

	noSecret := notifications.NewService(db)
	if _, err := noSecret.UnsubscribeEmail(ctx, notifications.SignUnsubscribeToken("", user.User.ID, "series_added")); err == nil {
		t.Error("without a configured secret an empty-key signature was accepted")
	}
}

func TestPreferenceRoutesOverHTTP(t *testing.T) {
	db := connectOrSkip(t)
	authSvc := auth.NewService(db, "notre_cinema_session", "development")
	svc := notifications.NewService(db).WithUnsubscribeSecret(testSecret)
	user := newUser(t, db)

	protected := http.NewServeMux()
	public := http.NewServeMux()
	notifications.RegisterPreferenceRoutes(protected, public, svc)
	protectedHandler := authSvc.Middleware(protected)

	call := func(h http.Handler, method, path, body string, withSession bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("X-Real-Ip", "test-"+hex.EncodeToString([]byte(path))[:8]+fmt.Sprint(len(body)))
		if withSession {
			req.AddCookie(&http.Cookie{Name: "notre_cinema_session", Value: user.Token})
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := call(protectedHandler, http.MethodGet, "/api/v1/notification-preferences", "", false); rec.Code != http.StatusUnauthorized {
		t.Errorf("preferences without a session = %d, want 401", rec.Code)
	}

	rec := call(protectedHandler, http.MethodGet, "/api/v1/notification-preferences", "", true)
	var listed struct {
		Preferences []notifications.Preference `json:"preferences"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil || len(listed.Preferences) != len(notifications.Categories) {
		t.Errorf("GET preferences = %d %s", rec.Code, rec.Body.String())
	}

	rec = call(protectedHandler, http.MethodPut, "/api/v1/notification-preferences",
		`{"preferences":[{"category":"poll","push":false,"email":false}]}`, true)
	if rec.Code != http.StatusOK {
		t.Errorf("PUT preferences = %d %s", rec.Code, rec.Body.String())
	}
	rec = call(protectedHandler, http.MethodPut, "/api/v1/notification-preferences",
		`{"preferences":[{"category":"poll","push":true,"email":true}]}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("email for a category without letters = %d, want 400", rec.Code)
	}

	token := notifications.SignUnsubscribeToken(testSecret, user.User.ID, "season_update")
	// Почтовый клиент (RFC 8058): POST по ссылке из заголовка, токен в query.
	rec = call(public, http.MethodPost, "/api/v1/unsubscribe?token="+token, "List-Unsubscribe=One-Click", false)
	if rec.Code != http.StatusOK {
		t.Errorf("one-click unsubscribe = %d %s", rec.Code, rec.Body.String())
	}
	// Страница на фронтенде: тот же POST, токен в теле.
	rec = call(public, http.MethodPost, "/api/v1/unsubscribe", `{"token":"`+token+`"}`, false)
	if rec.Code != http.StatusOK {
		t.Errorf("unsubscribe with a JSON body = %d %s", rec.Code, rec.Body.String())
	}
	rec = call(public, http.MethodPost, "/api/v1/unsubscribe?token=forged", "", false)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unsubscribe with a forged token = %d, want 400", rec.Code)
	}
	// Отписка меняет данные, поэтому по GET её быть не должно.
	rec = call(public, http.MethodGet, "/api/v1/unsubscribe?token="+token, "", false)
	if rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
		t.Errorf("GET unsubscribe = %d, want 405/404", rec.Code)
	}

	if p := byCategory(mustGet(t, svc, user.User.ID))["season_update"]; p.Email {
		t.Error("the one-click unsubscribe did not switch the email off")
	}
}
