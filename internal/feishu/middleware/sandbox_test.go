package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/sandbox/fault"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

func TestSandboxFaultResponsesAndAudit(t *testing.T) {
	cfg := config.Default()
	state := store.New(nil, 0)
	state.Seed(cfg)
	engine := fault.New(nil)
	for _, tc := range []struct {
		name     string
		spec     fault.Spec
		token    string
		kind     store.TokenKind
		required RequiredToken
		status   int
		code     int
		body     string
	}{
		{"rate limit", fault.Spec{Kind: fault.RateLimit}, "t-tenant", store.TenantToken, Tenant, 429, 99991400, `{"code":99991400,"msg":"request trigger frequency limit"}`},
		{"server error", fault.Spec{Kind: fault.HTTPError}, "t-tenant", store.TenantToken, Tenant, 500, 99991401, `{"code":99991401,"msg":"internal server error"}`},
		{"tenant expiry", fault.Spec{Kind: fault.TokenExpiry}, "t-tenant", store.TenantToken, Tenant, 400, 99991663, ""},
		{"app expiry", fault.Spec{Kind: fault.TokenExpiry}, "a-app", store.AppToken, App, 400, 99991663, ""},
		{"user expiry", fault.Spec{Kind: fault.TokenExpiry}, "u-user", store.UserToken, User, 400, 99991668, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state.ClearTraces()
			appID := cfg.Apps[0].AppID
			err := state.PutToken(store.Token{Value: tc.token, Kind: tc.kind, AppID: appID, UserOpenID: cfg.Users[0].OpenID, ExpiresAt: time.Now().Add(time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
			one := 1
			tc.spec.RemainingHits, tc.spec.AppID = &one, appID
			if _, err := engine.Add(tc.spec); err != nil {
				t.Fatal(err)
			}
			h := Audit(state, SandboxFaults(engine, state, Bearer(state, tc.required, true)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))))
			call := func(token string) *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				r := httptest.NewRequest(http.MethodGet, "/open-apis/test", nil)
				r.Header.Set("Authorization", "Bearer "+token)
				h.ServeHTTP(w, r)
				return w
			}
			if tc.spec.Kind == fault.TokenExpiry {
				_ = call(tc.token + "-invalid")
				if len(engine.List()) != 1 {
					t.Fatal("invalid token spent the expiry rule")
				}
			}
			w := call(tc.token)
			if w.Code != tc.status || (tc.body != "" && w.Body.String() != tc.body) {
				t.Fatalf("HTTP %d body %s", w.Code, w.Body)
			}
			if tc.spec.Kind == fault.RateLimit && w.Header().Get("Retry-After") != "1" {
				t.Fatal("missing default Retry-After")
			}
			traces := state.Traces()
			last := traces[len(traces)-1]
			if last.Code == nil || *last.Code != tc.code || last.AppID != appID || last.Status != tc.status ||
				strings.Contains(string(last.RequestBody), tc.token) || len(last.RequestHeaders.Values("Authorization")) != 0 {
				t.Fatalf("unexpected audit: %+v", last)
			}
			if w := call(tc.token); w.Code != http.StatusNoContent {
				t.Fatalf("expired rule still applied: %d %s", w.Code, w.Body)
			}
		})
	}
}
