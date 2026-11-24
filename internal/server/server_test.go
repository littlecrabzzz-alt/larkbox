package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/middleware"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

func TestHealthz(t *testing.T) {
	for _, tc := range []struct {
		name, method string
		strict       bool
		status       int
	}{
		{"strict", http.MethodGet, true, http.StatusOK},
		{"permissive", http.MethodGet, false, http.StatusOK},
		{"wrong method", http.MethodPost, true, http.StatusMethodNotAllowed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.StrictMode = tc.strict
			srv := New(cfg)
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, httptest.NewRequest(tc.method, "/healthz", nil))
			if w.Code != tc.status {
				t.Fatalf("HTTP %d: %s, want %d", w.Code, w.Body, tc.status)
			}
			if tc.status != http.StatusOK {
				return
			}
			var health struct {
				Status  string `json:"status"`
				Version string `json:"version"`
				Strict  *bool  `json:"strict_mode"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &health); err != nil {
				t.Fatal(err)
			}
			if health.Status != "ok" || health.Version != Version || health.Strict == nil || *health.Strict != tc.strict ||
				w.Header().Get("Content-Type") != "application/json; charset=utf-8" {
				t.Fatalf("unexpected health response: %s (%v)", w.Body, w.Header())
			}
		})
	}
}

func TestRouteRegistration(t *testing.T) {
	appID := config.DefaultApp().AppID
	openAPI := func(mux *http.ServeMux, cfg config.Config, state *store.Store) {
		mux.Handle("GET /open-apis/demo/{app_id}", middleware.Bearer(state, middleware.Tenant, cfg.StrictMode)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := state.Apps().Get(r.PathValue("app_id")); !ok {
				t.Error("route did not get the seeded state or app path parameter")
			}
			if err := protocol.WriteSuccess(w, nil); err != nil {
				t.Error(err)
			}
		})))
	}
	sandbox := func(mux *http.ServeMux, _ config.Config, state *store.Store) {
		mux.HandleFunc("POST /_sandbox/reset", func(w http.ResponseWriter, _ *http.Request) {
			state.Reset()
			w.WriteHeader(http.StatusNoContent)
		})
	}
	srv := New(config.Default(), WithFeishuRoutes(openAPI), WithSandboxRoutes(sandbox))
	if err := srv.Store().PutToken(store.Token{Value: "t-test", Kind: store.TenantToken, AppID: appID, ExpiresAt: srv.Store().Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, method, path, auth string
		status                   int
		traceCount               int
		code                     string
	}{
		{"Open API", http.MethodGet, "/open-apis/demo/" + appID, "Bearer t-test", http.StatusOK, 1, `"code":0`},
		{"missing token", http.MethodGet, "/open-apis/demo/" + appID, "", http.StatusBadRequest, 2, `"code":99991661`},
		{"sandbox", http.MethodPost, "/_sandbox/reset", "", http.StatusNoContent, 0, ""},
		{"token reset", http.MethodGet, "/open-apis/demo/" + appID, "Bearer t-test", http.StatusBadRequest, 1, `"code":99991665`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, nil)
			if tc.auth != "" {
				r.Header.Set("Authorization", tc.auth)
			}
			r.Header.Set("X-App-ID", "untrusted")
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, r)
			traces := srv.Store().Traces()
			if w.Code != tc.status || len(traces) != tc.traceCount || !strings.Contains(w.Body.String(), tc.code) {
				t.Fatalf("HTTP %d: %s, trace count=%d", w.Code, w.Body, len(traces))
			}
			if len(traces) > 0 {
				wantAppID := ""
				if tc.status == http.StatusOK {
					wantAppID = appID
				}
				if got := traces[len(traces)-1].AppID; got != wantAppID {
					t.Fatalf("trace AppID = %q, want %q", got, wantAppID)
				}
			}
		})
	}
}

func TestOpenAPI404And405(t *testing.T) {
	srv := New(config.Default(), WithFeishuRoutes(func(mux *http.ServeMux, _ config.Config, _ *store.Store) {
		mux.HandleFunc("GET /open-apis/known/{id}", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "handler-generated 404", http.StatusNotFound)
		})
	}))
	for _, tc := range []struct {
		name, method, path string
		status             int
		allow, body        string
		traceCode          int
	}{
		{"unmatched GET", http.MethodGet, "/open-apis/unknown", 404, "", `{"code":99991201,"msg":"resource not find"}`, 99991201},
		{"unmatched POST", http.MethodPost, "/open-apis/unknown", 404, "", `{"code":99991201,"msg":"resource not find"}`, 99991201},
		{"wrong method", http.MethodPost, "/open-apis/known/example", 405, "GET, HEAD", "Method Not Allowed\n", 0},
		{"matched 404", http.MethodGet, "/open-apis/known/example", 404, "", "handler-generated 404\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
			if w.Code != tc.status || w.Header().Get("Allow") != tc.allow || w.Body.String() != tc.body {
				t.Fatalf("HTTP %d, headers=%v: %s", w.Code, w.Header(), w.Body)
			}
			traces := srv.Store().Traces()
			trace := traces[len(traces)-1]
			if trace.Status != tc.status || (tc.traceCode == 0 && trace.Code != nil) ||
				(tc.traceCode != 0 && (trace.Code == nil || *trace.Code != tc.traceCode)) {
				t.Fatalf("unexpected trace: %+v", trace)
			}
			if tc.traceCode != 0 && w.Header().Get("Content-Type") != "application/json; charset=utf-8" {
				t.Errorf("unexpected translated 404 media type: %v", w.Header())
			}
		})
	}
}

func TestLargeEnvelopeAuditOverHTTP(t *testing.T) {
	payload := strings.Repeat("x", 70*1024)
	srv := New(config.Default(), WithFeishuRoutes(func(mux *http.ServeMux, _ config.Config, _ *store.Store) {
		mux.HandleFunc("GET /open-apis/large", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Request-Id", "committed")
			if err := protocol.WriteSuccess(w, payload); err != nil {
				t.Error(err)
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			} else {
				t.Error("audit writer does not support http.Flusher")
			}
			if _, ok := w.(interface{ Unwrap() http.ResponseWriter }); !ok {
				t.Error("audit writer does not support Unwrap")
			}
			w.Header().Set("X-Request-Id", "too late")
		})
	}))
	ts := httptest.NewServer(srv)
	defer ts.Close()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/open-apis/large", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	var envelope struct {
		Code int    `json:"code"`
		Data string `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || envelope.Code != 0 || envelope.Data != payload ||
		resp.Header.Get("X-Request-Id") != "committed" {
		t.Fatal("unexpected wire response")
	}
	trace := srv.Store().Traces()[0]
	if trace.Status != http.StatusOK || trace.Code == nil || *trace.Code != 0 || !trace.ResponseBodyTruncated ||
		trace.ResponseHeaders.Get("X-Request-Id") != "committed" {
		t.Fatalf("lost response metadata: %+v", trace)
	}
}

func TestRejectedExpectContinueDoesNotReadBody(t *testing.T) {
	srv := New(config.Default(), WithFeishuRoutes(func(mux *http.ServeMux, cfg config.Config, state *store.Store) {
		mux.Handle("POST /open-apis/protected", middleware.Bearer(state, middleware.Tenant, cfg.StrictMode)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Error("unauthenticated request reached the handler")
		})))
	}))
	ts := httptest.NewServer(srv)
	defer ts.Close()
	dialer := net.Dialer{Timeout: 3 * time.Second}
	conn, err := dialer.DialContext(t.Context(), "tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, "POST /open-apis/protected HTTP/1.1\r\nHost: localhost\r\nExpect: 100-continue\r\nContent-Length: 50\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("first wire response = %s, want final 400", resp.Status)
	}
}

func TestShutdownDrainsRequest(t *testing.T) {
	active, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	srv := New(config.Default(), WithSandboxRoutes(func(mux *http.ServeMux, _ config.Config, _ *store.Store) {
		mux.HandleFunc("GET /_sandbox/active", func(w http.ResponseWriter, _ *http.Request) {
			close(active)
			<-release
			w.WriteHeader(http.StatusNoContent)
		})
	}))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serving := make(chan error, 1)
	go func() { serving <- srv.Serve(listener) }()
	url := "http://" + listener.Addr().String() + "/_sandbox/active"
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	completed := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			err = resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				t.Errorf("active response status = %d", resp.StatusCode)
			}
		}
		completed <- err
	}()
	select {
	case <-active:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	stopped := make(chan error, 1)
	go func() { stopped <- srv.Shutdown(ctx) }()
	select {
	case err := <-serving:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("Serve error = %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case err := <-stopped:
		t.Fatalf("shutdown returned while a request was active: %v", err)
	default:
	}
	unblock()
	if err := <-completed; err != nil {
		t.Fatal(err)
	}
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
}

func TestAuthenticationAuditAndFaultHook(t *testing.T) {
	cfg := config.Default()
	app := cfg.Apps[0]
	const tenantPath = "/open-apis/auth/v3/tenant_access_token/internal"
	const oauthPath = "/open-apis/authen/v2/oauth/token"
	srv := New(cfg, WithFaultHook(authFaultHook{}))
	for _, tc := range []struct {
		path         string
		body         map[string]string
		fault        bool
		status, code int
		tracedApp    string
	}{
		{tenantPath, map[string]string{"app_id": app.AppID, "app_secret": app.AppSecret}, false, 200, 0, app.AppID},
		{oauthPath, map[string]string{
			"grant_type": "authorization_code", "client_id": app.AppID, "client_secret": app.AppSecret,
			"code": "authorization-secret", "code_verifier": "verifier-secret",
		}, false, 400, 20003, app.AppID},
		{tenantPath, nil, true, 429, 99991400, ""},
		{oauthPath, nil, true, 429, 99991400, ""},
	} {
		body, err := json.Marshal(tc.body)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(string(body)))
		r.Header.Set("Content-Type", "application/json")
		if tc.fault {
			r.Header.Set("X-Test-Fault", "true")
		}
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		traces := srv.Store().QueryTraces(store.TraceFilter{Limit: 1})
		if len(traces) != 1 {
			t.Fatalf("expected audit trace for %s", tc.path)
		}
		trace := traces[0]
		if w.Code != tc.status || trace.Path != tc.path || trace.Status != tc.status ||
			trace.Code == nil || *trace.Code != tc.code || trace.AppID != tc.tracedApp {
			t.Fatalf("unexpected auth route response or trace metadata for %s: HTTP %d, status=%d, code=%v, app=%q",
				tc.path, w.Code, trace.Status, trace.Code, trace.AppID)
		}
		captured := string(trace.RequestBody) + string(trace.ResponseBody)
		for _, secret := range []string{app.AppSecret, "authorization-secret", "verifier-secret"} {
			if strings.Contains(captured, secret) {
				t.Fatal("auth audit contains a credential")
			}
		}
		if tc.code == 0 && !strings.Contains(string(trace.ResponseBody), `"tenant_access_token":"[REDACTED]"`) {
			t.Fatal("audit did not capture and redact the top-level tenant credential")
		}
	}
}

type authFaultHook struct{}

func (authFaultHook) BeforeRequest(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-Test-Fault") != "true" {
		return false
	}
	_ = protocol.WriteError(w, protocol.NewError(protocol.RateLimited, nil))
	return true
}

func TestWrappedBearerExtensionFaultPrecedence(t *testing.T) {
	const path = "/open-apis/extension"
	cfg := config.Default()
	srv := New(cfg, WithFeishuRoutes(func(mux *http.ServeMux, _ config.Config, state *store.Store) {
		wrap := func(next http.Handler) http.Handler {
			return http.TimeoutHandler(next, time.Second, "extension timed out")
		}
		mux.Handle("GET "+path, middleware.Bearer(state, middleware.Tenant, true, wrap)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := middleware.AuthFromContext(r.Context()); !ok {
				t.Error("wrapped handler did not receive the Bearer identity")
			}
			w.WriteHeader(http.StatusNoContent)
		})))
	}))
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	if err := srv.Store().PutToken(store.Token{Value: "t-extension", Kind: store.TenantToken, AppID: cfg.Apps[0].AppID, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body, bearer string, status int, want string) {
		t.Helper()
		r := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		if w.Code != status || !strings.Contains(w.Body.String(), want) {
			t.Fatalf("%s %s returned %d %s, want %d %q", method, path, w.Code, w.Body, status, want)
		}
	}
	call("POST", "/_sandbox/faults", `{"kind":"token_expiry","remaining_hits":1}`, "", 201, "fault_id")
	call("POST", "/_sandbox/faults", `{"kind":"rate_limit","remaining_hits":1}`, "", 201, "fault_id")
	call("GET", path, "", "t-extension", 400, "99991663")
	call("GET", path, "", "t-extension", 429, "99991400")
	call("GET", path, "", "t-extension", 204, "")
}
