package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/event"
	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/middleware"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

func TestSharedControlAuthorization(t *testing.T) {
	cfg := config.Default()
	cfg.SandboxToken, cfg.Pprof = "shared-operator-fixture", true
	srv := New(cfg, WithSandboxRoutes(func(mux *http.ServeMux, _ config.Config, _ *store.Store) {
		mux.HandleFunc("GET /_sandbox/extension", func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "" {
				t.Error("operator credential reached an extension")
			}
			w.WriteHeader(http.StatusNoContent)
		})
	}))
	for _, path := range []string{"/", "/_sandbox/state", "/_sandbox/metrics", "/_sandbox/debug/pprof/heap", "/_sandbox/extension"} {
		w := deploymentRequest(srv, http.MethodGet, path, "", "")
		if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") == "" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("unprotected %s: %d %v", path, w.Code, w.Header())
		}
	}
	token := "Bearer " + cfg.SandboxToken
	for _, tc := range []struct {
		path, authorization string
		status              int
	}{
		{"/healthz", "", http.StatusOK},
		{"/_sandbox/state", "Bearer t-unrelated-credential", http.StatusUnauthorized},
		{"/_sandbox/metrics", token, http.StatusOK},
		{"/_sandbox/extension", token, http.StatusNoContent},
		{"/_sandbox/debug/pprof/heap", token, http.StatusOK},
		{"/_sandbox/debug/pprof/profile?seconds=31", token, http.StatusBadRequest},
		{"/debug/pprof/heap", token, http.StatusNotFound},
		{"/_sandbox/debug/pprof/cmdline", token, http.StatusNotFound},
	} {
		if w := deploymentRequest(srv, http.MethodGet, tc.path, tc.authorization, ""); w.Code != tc.status {
			t.Fatalf("%s received %d, want %d: %s", tc.path, w.Code, tc.status, w.Body)
		}
	}
	browser := httptest.NewRequest(http.MethodGet, "/_sandbox/state", nil)
	browser.SetBasicAuth("sandbox", cfg.SandboxToken)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, browser)
	if w.Code != http.StatusOK {
		t.Fatalf("browser credentials rejected: %s", w.Body)
	}
	browser = httptest.NewRequest(http.MethodPost, "/_sandbox/reset", strings.NewReader("{}"))
	browser.SetBasicAuth("sandbox", cfg.SandboxToken)
	browser.Header.Set("Origin", "https://unauthorized.example.test")
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, browser)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin browser reset returned %d", w.Code)
	}
}

func TestRateProfileWithManualClock(t *testing.T) {
	cfg := config.Default()
	at := time.Date(2026, 11, 29, 12, 0, 0, 0, time.UTC)
	cfg.ClockStart = &at
	cfg.RateLimits = config.RateLimits{Profile: "realistic", QPS: 2, DailyQuota: 2, MonthlyQuota: 3}
	cfg.Apps = append(cfg.Apps, config.App{AppID: "cli_rate_separate", AppSecret: "rate-separate", BotOpenID: "ou_rate_bot"})
	srv := New(cfg, WithFeishuRoutes(func(mux *http.ServeMux, cfg config.Config, state *store.Store) {
		mux.Handle("GET /open-apis/quota/invalid", middleware.Bearer(state, middleware.Tenant, cfg.StrictMode)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = protocol.WriteError(w, protocol.NewError(protocol.InvalidParameter, nil))
		})))
	}))
	issue := func() {
		t.Helper()
		for i, value := range []string{"t-budget-one", "t-budget-two"} {
			if err := srv.Store().PutToken(store.Token{Value: value, Kind: store.TenantToken, AppID: cfg.Apps[i].AppID, ExpiresAt: srv.Store().Now().Add(96 * time.Hour)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	issue()
	step := func(method, path, credential, body string, status int, expected string) *httptest.ResponseRecorder {
		t.Helper()
		w := deploymentRequest(srv, method, path, credential, body)
		if w.Code != status || !strings.Contains(w.Body.String(), expected) {
			t.Fatalf("%s %s received %d, want %d and %q: %s", method, path, w.Code, status, expected, w.Body)
		}
		return w
	}
	const bot, first = "/open-apis/bot/v3/info", "Bearer t-budget-one"
	advance := func(by string) {
		step(http.MethodPost, "/_sandbox/clock/advance", "", `{"by":"`+by+`"}`, http.StatusOK, `"manual":true`)
	}
	step(http.MethodGet, "/open-apis/quota/invalid", first, "", 200, `"code":10003`)
	step(http.MethodGet, bot, first, "", 200, `"code":0`)
	qps := step(http.MethodGet, bot, first, "", 429, `"code":99991400`)
	if qps.Header().Get("Retry-After") != "1" || qps.Header().Get("X-Ogw-Ratelimit-Reset") != "1" ||
		qps.Header().Get("X-Ogw-Ratelimit-Limit") != "2" {
		t.Fatalf("missing rate-limit reset headers: %v", qps.Header())
	}
	step(http.MethodGet, bot, "Bearer t-budget-two", "", 200, `"code":0`)
	advance("1s")
	step(http.MethodGet, bot, first, "", 200, `"code":0`)
	step(http.MethodGet, bot, first, "", 429, "daily API call quota")
	advance("24h")
	step(http.MethodGet, bot, first, "", 200, `"code":0`)
	step(http.MethodGet, bot, first, "", 429, "This month's API call quota")
	advance("24h")
	step(http.MethodGet, bot, first, "", 200, `"code":0`)
	step(http.MethodGet, "/_sandbox/metrics", "", "", 200, "larkbox_rate_limited_requests_total 3")
	step(http.MethodPost, "/_sandbox/reset", "", "", 200, `"reset":true`)
	issue()
	step(http.MethodGet, bot, first, "", 200, `"code":0`)
}

func TestDeliveryDiagnosticsCorrelateWithoutCredentials(t *testing.T) {
	var attempts atomic.Int64
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer peer.Close()

	cfg := config.Default()
	at := time.Date(2026, 11, 25, 1, 0, 0, 0, time.UTC)
	cfg.ClockStart = &at
	cfg.Apps[0].WebhookURL = peer.URL + "/credential-in-callback-path"
	cfg.Apps[0].EventTypes = []string{"im.chat.disbanded_v1"}
	app := cfg.Apps[0].AppID
	var logs strings.Builder
	srv := New(cfg, WithEventOptions(event.Options{RetryBackoff: func(int) time.Duration { return 15 * time.Second }}),
		WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
	// Use a normal IM mutation that enqueues an event for the requesting app.
	if err := srv.Store().PutToken(store.Token{Value: "u-diagnostics-fixture-secret", Kind: store.UserToken, AppID: app,
		UserOpenID: cfg.Users[0].OpenID, ExpiresAt: at.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	deleted := deploymentRequest(srv, http.MethodDelete, "/open-apis/im/v1/chats/oc_larkbox_group", "Bearer u-diagnostics-fixture-secret", "")
	id := deleted.Header().Get("X-Request-Id")
	if deleted.Code != 200 || id == "" || !strings.Contains(deleted.Body.String(), `"code":0`) {
		t.Fatalf("group mutation failed: %d %s", deleted.Code, deleted.Body)
	}
	deadline := time.Now().Add(5 * time.Second)
	var waiting event.Metrics
	for time.Now().Before(deadline) {
		waiting = srv.Events().Metrics()
		if waiting.RetryWaiters == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if waiting.Pending != 1 || waiting.RetryWaiters != 1 || waiting.Attempts != 1 || waiting.Failures != 1 {
		t.Fatalf("queued failure missing from metrics: %+v", waiting)
	}
	clock := deploymentRequest(srv, http.MethodPost, "/_sandbox/clock/advance", "", `{"by":"15s"}`)
	if clock.Code != 200 {
		t.Fatal(clock.Body.String())
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && srv.Events().Metrics().Pending != 0 {
		time.Sleep(time.Millisecond)
	}
	final := srv.Events().Metrics()
	if final.Pending != 0 || final.RetryWaiters != 0 || final.Attempts != 2 || final.Retries != 1 || final.Failures != 1 {
		t.Fatalf("retry completion missing from metrics: %+v", final)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	records := srv.Store().QueryTraces(store.TraceFilter{RequestID: id})
	if len(records) != 3 || records[1].EventID == "" || records[1].EventID != records[2].EventID {
		t.Fatalf("request correlation did not include both deliveries: %+v", records)
	}
	output := logs.String()
	for _, value := range []string{id, records[1].EventID, "open_api_request", "event_delivery"} {
		if !strings.Contains(output, value) {
			t.Errorf("structured logs omitted %q", value)
		}
	}
	for _, secret := range []string{"u-diagnostics-fixture-secret", "credential-in-callback-path", cfg.Apps[0].AppSecret, cfg.Apps[0].EncryptKey} {
		if strings.Contains(output, secret) {
			t.Error("structured logs exposed credential or callback location")
		}
	}
}

func deploymentRequest(srv *Server, method, path, authorization, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w
}
