package telemetry

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func newTestMetrics(t *testing.T) (*Metrics, *prometheus.Registry) {
	t.Helper()
	registry := prometheus.NewRegistry()
	return NewMetrics(registry), registry
}

// gather возвращает значения метрики по набору меток (подмножество).
func gather(t *testing.T, g prometheus.Gatherer, name string, labels map[string]string) (value float64, found bool) {
	t.Helper()
	families, err := g.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			if !hasLabels(metric, labels) {
				continue
			}
			switch {
			case metric.Counter != nil:
				return metric.GetCounter().GetValue(), true
			case metric.Gauge != nil:
				return metric.GetGauge().GetValue(), true
			case metric.Histogram != nil:
				return float64(metric.GetHistogram().GetSampleCount()), true
			}
		}
	}
	return 0, false
}

func hasLabels(metric *dto.Metric, want map[string]string) bool {
	have := map[string]string{}
	for _, label := range metric.GetLabel() {
		have[label.GetName()] = label.GetValue()
	}
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// routedServer собирает ту же схему, что main.go: публичный mux с
// catch-all "/", за которым защищённый mux; CaptureRoute -- на обоих.
func routedServer(m *Metrics) http.Handler {
	protected := http.NewServeMux()
	protected.HandleFunc("GET /api/v1/families/{familyId}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"` + r.PathValue("familyId") + `"}`))
	})
	protected.HandleFunc("POST /api/v1/boom", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	public := http.NewServeMux()
	public.HandleFunc("POST /api/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	})
	public.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {})
	// Между публичным и защищённым mux'ом стоит middleware, делающий
	// r.WithContext (в проде это auth.Middleware): он создаёт копию запроса.
	public.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		CaptureRoute(protected).ServeHTTP(w, r.WithContext(r.Context()))
	}))

	return m.HTTPMiddleware(CaptureRoute(public))
}

func TestHTTPMetricsUseRouteTemplatesNotRawPaths(t *testing.T) {
	m, registry := newTestMetrics(t)
	server := routedServer(m)

	for _, id := range []string{"a", "b", "c"} {
		server.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/families/"+id, nil))
	}

	got, ok := gather(t, registry, "http_requests_total", map[string]string{
		"method": "GET", "path": "/api/v1/families/{familyId}", "status": "200",
	})
	if !ok || got != 3 {
		t.Errorf("requests for the family route = (%v, %v), want 3 under one template", got, ok)
	}

	families, _ := registry.Gather()
	for _, family := range families {
		if family.GetName() != "http_requests_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "path" && strings.Contains(label.GetValue(), "/families/a") {
					t.Errorf("a raw URL leaked into the path label: %q", label.GetValue())
				}
			}
		}
	}
}

func TestHTTPMetricsCoverPublicRoutes(t *testing.T) {
	m, registry := newTestMetrics(t)
	server := routedServer(m)

	server.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil))

	got, ok := gather(t, registry, "http_requests_total", map[string]string{
		"method": "POST", "path": "/api/v1/auth/login", "status": "401",
	})
	if !ok || got != 1 {
		t.Errorf("a public route is not counted: (%v, %v)", got, ok)
	}
}

func TestHTTPMetricsRecordStatusAndSize(t *testing.T) {
	m, registry := newTestMetrics(t)
	server := routedServer(m)

	server.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/boom", nil))
	if got, ok := gather(t, registry, "http_requests_total", map[string]string{"path": "/api/v1/boom", "status": "418"}); !ok || got != 1 {
		t.Errorf("status 418 not recorded: (%v, %v)", got, ok)
	}

	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/families/xyz", nil))
	if got, ok := gather(t, registry, "http_response_size_bytes", map[string]string{"path": "/api/v1/families/{familyId}"}); !ok || got != 1 {
		t.Errorf("response size histogram has %v observations (found=%v), want 1", got, ok)
	}
	if got, ok := gather(t, registry, "http_request_duration_seconds", map[string]string{"path": "/api/v1/families/{familyId}"}); !ok || got != 1 {
		t.Errorf("duration histogram has %v observations (found=%v), want 1", got, ok)
	}
}

func TestUnmatchedRequestsShareOneLabel(t *testing.T) {
	m, registry := newTestMetrics(t)
	// Без catch-all: любой незнакомый путь -- 404 от mux'а.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /known", func(w http.ResponseWriter, r *http.Request) {})
	server := m.HTTPMiddleware(CaptureRoute(mux))

	for _, path := range []string{"/wp-login.php", "/.env", "/a/b/c/d"} {
		server.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}

	got, ok := gather(t, registry, "http_requests_total", map[string]string{"path": "unmatched", "status": "404"})
	if !ok || got != 3 {
		t.Errorf("scanner traffic = (%v, %v), want 3 requests under the single 'unmatched' label", got, ok)
	}
}

func TestMetricsEndpointIsNotCounted(t *testing.T) {
	m, registry := newTestMetrics(t)
	server := routedServer(m)

	server.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if _, ok := gather(t, registry, "http_requests_total", map[string]string{"path": "/metrics"}); ok {
		t.Error("the Prometheus scrape counts itself")
	}
}

func TestInFlightGaugeRisesDuringARequestAndFallsAfter(t *testing.T) {
	m, registry := newTestMetrics(t)
	started := make(chan struct{})
	release := make(chan struct{})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
	})
	server := m.HTTPMiddleware(CaptureRoute(mux))

	done := make(chan struct{})
	go func() {
		server.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/slow", nil))
		close(done)
	}()

	<-started
	if got, _ := gather(t, registry, "http_requests_in_flight", nil); got != 1 {
		t.Errorf("in flight during the request = %v, want 1", got)
	}
	close(release)
	<-done
	if got, _ := gather(t, registry, "http_requests_in_flight", nil); got != 0 {
		t.Errorf("in flight after the request = %v, want 0", got)
	}
}

func TestRouteLabel(t *testing.T) {
	cases := map[string]string{
		"":                                "unmatched",
		"GET /api/v1/families/{familyId}": "/api/v1/families/{familyId}",
		"POST /api/v1/auth/login":         "/api/v1/auth/login",
		"/":                               "/",
		"GET /healthz":                    "/healthz",
	}
	for pattern, want := range cases {
		if got := routeLabel(pattern); got != want {
			t.Errorf("routeLabel(%q) = %q, want %q", pattern, got, want)
		}
	}
}

func TestBucketLabelDropsTheVariablePart(t *testing.T) {
	cases := map[string]string{
		"auth:ip:203.0.113.7":               "auth:ip",
		"auth:ip:2001:db8::1":               "auth:ip",
		"auth:login-email:anna@example.com": "auth:login-email",
		"invite-resend:3f2a0c52-aaaa-bbbb":  "invite-resend",
		"2fa:token:abcdef":                  "2fa:token",
		"plain":                             "plain",
	}
	for key, want := range cases {
		if got := bucketLabel(key); got != want {
			t.Errorf("bucketLabel(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestOperationOf(t *testing.T) {
	cases := map[string]string{
		"SELECT public.check_rate_limit($1, $2, $3)":     "function",
		"select public.register_profile_account($1)":     "function",
		"  SELECT id FROM public.families WHERE id = $1": "select",
		"\n\t\tSELECT * FROM public.profiles":            "select",
		"INSERT INTO public.x VALUES ($1)":               "insert",
		"update public.x set a = 1":                      "update",
		"DELETE FROM public.x":                           "delete",
		"WITH t AS (SELECT 1) SELECT * FROM t":           "select",
		"BEGIN":                                          "transaction",
		"COMMIT":                                         "transaction",
		"-- comment\nSELECT public.fn()":                 "function",
		"":                                               "other",
		"VACUUM":                                         "other",
	}
	for sql, want := range cases {
		if got := operationOf(sql); got != want {
			t.Errorf("operationOf(%q) = %q, want %q", sql, got, want)
		}
	}
}

func TestQueryTracerCountsByOperationAndResult(t *testing.T) {
	tracer := QueryTracer{}
	run := func(sql string, err error) {
		ctx := tracer.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: sql})
		tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: err})
	}

	before := func(op, result string) float64 {
		v, _ := gather(t, prometheus.DefaultGatherer, "db_queries_total", map[string]string{"operation": op, "result": result})
		return v
	}
	okBefore, errBefore, canceledBefore := before("function", "ok"), before("function", "error"), before("function", "canceled")

	run("SELECT public.fn()", nil)
	run("SELECT public.fn()", errors.New("boom"))
	run("SELECT public.fn()", context.Canceled)

	if got := before("function", "ok") - okBefore; got != 1 {
		t.Errorf("ok queries = %v, want 1", got)
	}
	if got := before("function", "error") - errBefore; got != 1 {
		t.Errorf("failed queries = %v, want 1", got)
	}
	if got := before("function", "canceled") - canceledBefore; got != 1 {
		t.Errorf("canceled queries = %v, want 1 (a client that went away is not a database failure)", got)
	}
}

type statDB struct {
	calls atomic.Int32
	fail  bool
	stats PlatformStats
}

type statRow struct {
	db *statDB
}

func (r statRow) Scan(dest ...any) error {
	r.db.calls.Add(1)
	if r.db.fail {
		return errors.New("db down")
	}
	values := []int64{
		r.db.stats.Users, r.db.stats.UsersVerified, r.db.stats.UsersWithTwoFactor, r.db.stats.Families,
		r.db.stats.Series, r.db.stats.ActiveSessions, r.db.stats.PushSubscriptions, r.db.stats.PendingInvitations,
	}
	for i, v := range values {
		*(dest[i].(*int64)) = v
	}
	return nil
}

func (d *statDB) QueryRow(context.Context, string, ...any) pgx.Row { return statRow{db: d} }

func TestPlatformStatsAreExportedAndCached(t *testing.T) {
	db := &statDB{stats: PlatformStats{Users: 10, UsersVerified: 8, UsersWithTwoFactor: 3, Families: 4, Series: 120, ActiveSessions: 7, PushSubscriptions: 5, PendingInvitations: 2}}
	collector := NewPlatformStatsCollector(db, nil)
	registry := prometheus.NewRegistry()
	registry.MustRegister(collector)

	for i := 0; i < 3; i++ {
		if _, err := registry.Gather(); err != nil {
			t.Fatalf("gather: %v", err)
		}
	}
	if got := db.calls.Load(); got != 1 {
		t.Errorf("the database was queried %d times for 3 scrapes, want 1 (30s cache)", got)
	}

	checks := map[string]struct {
		labels map[string]string
		want   float64
	}{
		"platform_users":               {map[string]string{"state": "all"}, 10},
		"platform_families":            {nil, 4},
		"platform_series":              {nil, 120},
		"platform_active_sessions":     {nil, 7},
		"platform_push_subscriptions":  {nil, 5},
		"platform_pending_invitations": {nil, 2},
	}
	for name, c := range checks {
		if got, ok := gather(t, registry, name, c.labels); !ok || got != c.want {
			t.Errorf("%s = (%v, %v), want %v", name, got, ok, c.want)
		}
	}
	if got, _ := gather(t, registry, "platform_users", map[string]string{"state": "verified"}); got != 8 {
		t.Errorf("verified users = %v, want 8", got)
	}
	if got, _ := gather(t, registry, "platform_users", map[string]string{"state": "two_factor"}); got != 3 {
		t.Errorf("users with 2FA = %v, want 3", got)
	}
}

func TestPlatformStatsFailureKeepsOldValuesAndCountsTheError(t *testing.T) {
	db := &statDB{stats: PlatformStats{Users: 5}}
	collector := NewPlatformStatsCollector(db, nil)
	registry := prometheus.NewRegistry()
	registry.MustRegister(collector)
	_, _ = registry.Gather()

	// Протухший кэш + упавшая БД: отдаём прежнее значение, а не пустоту.
	collector.mu.Lock()
	collector.fetchedAt = time.Now().Add(-time.Hour)
	collector.mu.Unlock()
	db.fail = true

	if got, ok := gather(t, registry, "platform_users", map[string]string{"state": "all"}); !ok || got != 5 {
		t.Errorf("users after a failed refresh = (%v, %v), want the previous 5", got, ok)
	}
	// Каждый скрейп при упавшей БД -- ещё одна неудачная попытка, поэтому
	// проверяем «хотя бы одна», а не точное число.
	if got, _ := gather(t, registry, "platform_stats_errors_total", nil); got < 1 {
		t.Errorf("errors = %v, want at least 1", got)
	}
}

func TestPlatformStatsBeforeTheFirstSuccessExportNothing(t *testing.T) {
	db := &statDB{fail: true}
	registry := prometheus.NewRegistry()
	registry.MustRegister(NewPlatformStatsCollector(db, nil))

	if _, ok := gather(t, registry, "platform_users", nil); ok {
		t.Error("zeros were exported before any successful read: a dashboard would show an empty platform")
	}
}

func TestRecordJobTracksResultDurationAndLastSuccess(t *testing.T) {
	before := func(result string) float64 {
		v, _ := gather(t, prometheus.DefaultGatherer, "background_job_runs_total", map[string]string{"job": "unit-test-job", "result": result})
		return v
	}
	okBefore, failBefore := before("success"), before("failure")

	RecordJob("unit-test-job", time.Now(), nil)
	RecordJob("unit-test-job", time.Now(), errors.New("boom"))

	if before("success")-okBefore != 1 || before("failure")-failBefore != 1 {
		t.Error("job runs were not counted per result")
	}
	last, ok := gather(t, prometheus.DefaultGatherer, "background_job_last_success_timestamp_seconds", map[string]string{"job": "unit-test-job"})
	if !ok || time.Since(time.Unix(int64(last), 0)) > time.Minute {
		t.Errorf("last success timestamp = (%v, %v), want about now", last, ok)
	}
}

func TestInstrumentedClientClassifiesOutcomesByHost(t *testing.T) {
	statuses := map[string]int{"/ok": 200, "/missing": 404, "/down": 503}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(statuses[r.URL.Path])
	}))
	t.Cleanup(server.Close)

	host := strings.TrimPrefix(server.URL, "http://")
	host = host[:strings.LastIndex(host, ":")]
	count := func(outcome string) float64 {
		v, _ := gather(t, prometheus.DefaultGatherer, "outbound_http_requests_total", map[string]string{"host": host, "outcome": outcome})
		return v
	}
	ok0, client0, server0 := count("success"), count("client_error"), count("server_error")

	client := InstrumentedClient()
	for path := range statuses {
		resp, err := client.Get(server.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
	}

	if count("success")-ok0 != 1 || count("client_error")-client0 != 1 || count("server_error")-server0 != 1 {
		t.Error("outcomes were not classified into success / client_error / server_error")
	}

	// Сетевая ошибка -- отдельный исход.
	server.Close()
	errors0 := count("error")
	if _, err := client.Get(server.URL + "/ok"); err == nil {
		t.Fatal("expected a connection error")
	}
	if count("error")-errors0 != 1 {
		t.Error("a transport error was not counted")
	}
}

func TestRecordAuthAndRateLimitDoNotPanicAndCount(t *testing.T) {
	before := func(event, result string) float64 {
		v, _ := gather(t, prometheus.DefaultGatherer, "auth_events_total", map[string]string{"event": event, "result": result})
		return v
	}
	b := before("unit-test", "success")
	RecordAuth("unit-test", "success")
	if before("unit-test", "success")-b != 1 {
		t.Error("auth event not counted")
	}
}
