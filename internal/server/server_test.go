package server

import (
	"context"
	"encoding/json"
	"errors"
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
	srv.Store().Tokens().Put(store.Token{Value: "t-test", Kind: store.TenantToken, AppID: appID, ExpiresAt: srv.Store().Now().Add(time.Hour)})
	for _, tc := range []struct {
		name, method, path, auth string
		status                   int
		traceCount               int
		code                     string
	}{
		{"Open API", http.MethodGet, "/open-apis/demo/" + appID, "Bearer t-test", http.StatusOK, 1, `"code":0`},
		{"missing token", http.MethodGet, "/open-apis/demo/" + appID, "", http.StatusBadRequest, 2, `"code":99991661`},
		{"sandbox", http.MethodPost, "/_sandbox/reset", "", http.StatusNoContent, 0, ""},
		{"token reset", http.MethodGet, "/open-apis/demo/" + appID, "Bearer t-test", http.StatusBadRequest, 1, `"code":99991663`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, nil)
			if tc.auth != "" {
				r.Header.Set("Authorization", tc.auth)
			}
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, r)
			if w.Code != tc.status || len(srv.Store().Traces()) != tc.traceCount || !strings.Contains(w.Body.String(), tc.code) {
				t.Fatalf("HTTP %d: %s, trace count=%d", w.Code, w.Body, len(srv.Store().Traces()))
			}
		})
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
