package middleware

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

func TestBearer(t *testing.T) {
	now := time.Unix(1700000000, 0)
	state := store.New(func() time.Time { return now }, 0)
	for _, token := range []store.Token{
		{Value: "t-valid", Kind: store.TenantToken, AppID: "cli_test", ExpiresAt: now.Add(time.Second)},
		{Value: "a-valid", Kind: store.AppToken, AppID: "cli_test", ExpiresAt: now.Add(time.Second)},
		{Value: "u-valid", Kind: store.UserToken, AppID: "cli_test", UserOpenID: "ou_test", ExpiresAt: now.Add(time.Second)},
		{Value: "r-valid", Kind: store.RefreshToken, ExpiresAt: now.Add(time.Second)},
		{Value: "t-expired", Kind: store.TenantToken, ExpiresAt: now},
		{Value: "a-expired", Kind: store.AppToken, ExpiresAt: now},
		{Value: "u-expired", Kind: store.UserToken, ExpiresAt: now},
		{Value: "u-revoked", Kind: store.UserToken, Revoked: true, ExpiresAt: now.Add(time.Second)},
	} {
		state.Tokens().Put(token)
	}
	for _, tc := range []struct {
		name      string
		required  RequiredToken
		headers   []string
		wantCode  protocol.Code
		wantToken store.TokenKind
		loose     bool
	}{
		{"missing", Tenant, nil, protocol.InvalidToken, "", false},
		{"wrong scheme", Tenant, []string{"Basic t-valid"}, protocol.InvalidToken, "", false},
		{"empty value", Tenant, []string{"Bearer"}, protocol.InvalidToken, "", false},
		{"extra component", Tenant, []string{"Bearer t-valid extra"}, protocol.InvalidToken, "", false},
		{"non-token character", Tenant, []string{"Bearer t-valid!"}, protocol.InvalidToken, "", false},
		{"duplicate header", Tenant, []string{"Bearer t-valid", "Bearer t-valid"}, protocol.InvalidToken, "", false},
		{"unknown", Tenant, []string{"Bearer unrecognized"}, protocol.InvalidToken, "", false},
		{"unknown tenant", Tenant, []string{"Bearer t-unknown"}, protocol.ExpiredTenantToken, "", false},
		{"unknown user", TenantOrUser, []string{"Bearer u-unknown"}, protocol.InvalidUserToken, "", false},
		{"expired tenant", Tenant, []string{"Bearer t-expired"}, protocol.ExpiredTenantToken, "", false},
		{"expired app", App, []string{"Bearer a-expired"}, protocol.InvalidAppToken, "", false},
		{"expired user", User, []string{"Bearer u-expired"}, protocol.InvalidUserToken, "", false},
		{"revoked user", User, []string{"Bearer u-revoked"}, protocol.InvalidUserToken, "", false},
		{"tenant on a user route", User, []string{"Bearer t-valid"}, protocol.ExpiredTenantToken, "", false},
		{"user on a tenant route", Tenant, []string{"Bearer u-valid"}, protocol.InvalidUserToken, "", false},
		{"app on tenant or user route", TenantOrUser, []string{"Bearer a-valid"}, protocol.InvalidAppToken, "", false},
		{"refresh in authorization", TenantOrUser, []string{"Bearer r-valid"}, protocol.InvalidToken, "", false},
		{"tenant", Tenant, []string{"bearer  t-valid"}, protocol.Success, store.TenantToken, false},
		{"app", App, []string{"Bearer a-valid"}, protocol.Success, store.AppToken, false},
		{"user", User, []string{"Bearer u-valid"}, protocol.Success, store.UserToken, false},
		{"tenant or user uses tenant", TenantOrUser, []string{"Bearer t-valid"}, protocol.Success, store.TenantToken, false},
		{"tenant or user uses user", TenantOrUser, []string{"Bearer u-valid"}, protocol.Success, store.UserToken, false},
		{"loose missing", Tenant, nil, protocol.Success, "", true},
		{"loose valid", Tenant, []string{"Bearer t-valid"}, protocol.Success, store.TenantToken, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/open-apis/example", nil)
			for _, header := range tc.headers {
				r.Header.Add("Authorization", header)
			}
			w := httptest.NewRecorder()
			called := false
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				auth, ok := AuthFromContext(r.Context())
				if ok != (tc.wantToken != "") || auth.Kind != tc.wantToken || ok && auth.AppID != "cli_test" ||
					tc.wantToken == store.UserToken && auth.UserOpenID != "ou_test" {
					t.Errorf("unexpected auth identity: %+v, present=%t", auth, ok)
				}
				if err := protocol.WriteSuccess(w, nil); err != nil {
					t.Error(err)
				}
			})
			Bearer(state, tc.required, !tc.loose)(next).ServeHTTP(w, r)
			var envelope protocol.Envelope
			if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Code != tc.wantCode || called != (tc.wantCode == protocol.Success) || w.Code != protocol.NewError(tc.wantCode, nil).Status {
				t.Fatalf("HTTP %d: %s, handler called=%t", w.Code, w.Body, called)
			}
			if tc.name == "user on a tenant route" && envelope.Msg != "user access token not support" {
				t.Errorf("unexpected mismatch message: %s", envelope.Msg)
			}
		})
	}
}

func TestAudit(t *testing.T) {
	const request = `{"app_id":"cli_test","app_secret":"original","nested":{"code":"authorization-code"}}`
	const response = `{"code":0,"msg":"ok","access_token":"original-token"}`
	state := store.New(nil, 0)
	r := httptest.NewRequest(http.MethodPost, "/open-apis/example?code=hidden", strings.NewReader(request))
	r.Header.Set("Authorization", "Bearer t-sensitive")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Cookie", "session=sensitive")
	r.Header.Set("X-Lark-Signature", "sensitive")
	w := httptest.NewRecorder()
	Audit(state, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != request {
			t.Errorf("handler body = %q, %v", body, err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "sensitive")
		w.WriteHeader(http.StatusCreated)
		if _, err := io.WriteString(w, response); err != nil {
			t.Error(err)
		}
	})).ServeHTTP(w, r)
	if w.Code != http.StatusCreated || w.Body.String() != response {
		t.Fatalf("audit changed HTTP response: %d %s", w.Code, w.Body)
	}
	traces := state.Traces()
	if len(traces) != 1 {
		t.Fatalf("trace count = %d", len(traces))
	}
	trace := traces[0]
	if trace.Direction != store.Inbound || trace.Method != http.MethodPost || trace.Path != "/open-apis/example" ||
		trace.Status != http.StatusCreated || trace.Code == nil || *trace.Code != 0 || trace.Latency <= 0 {
		t.Errorf("unexpected trace: %+v", trace)
	}
	if trace.RequestHeaders.Get("Content-Type") != "application/json" || len(trace.RequestHeaders) != 1 || len(trace.ResponseHeaders) != 1 {
		t.Errorf("headers leaked or were lost: %v, %v", trace.RequestHeaders, trace.ResponseHeaders)
	}
	if string(trace.RequestBody) != `{"app_id":"cli_test","app_secret":"[REDACTED]","nested":{"code":"[REDACTED]"}}` ||
		string(trace.ResponseBody) != `{"access_token":"[REDACTED]","code":0,"msg":"ok"}` {
		t.Errorf("unexpected audited bodies: %s, %s", trace.RequestBody, trace.ResponseBody)
	}
}

func TestAuditBodyLimit(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int
	}{
		{"under limit", auditBodyLimit - 1},
		{"at limit", auditBodyLimit},
		{"over limit", auditBodyLimit + 23},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := store.New(nil, 0)
			body := strings.Repeat("x", tc.size)
			r := httptest.NewRequest(http.MethodPost, "/open-apis/echo", strings.NewReader(body))
			w := httptest.NewRecorder()
			Audit(state, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				if n, err := io.Copy(w, r.Body); err != nil || n != int64(tc.size) {
					t.Errorf("copied %d bytes: %v", n, err)
				}
			})).ServeHTTP(w, r)
			trace := state.Traces()[0]
			want := body[:min(len(body), auditBodyLimit)]
			truncated := len(body) > auditBodyLimit
			if w.Body.String() != body || trace.Status != http.StatusOK || trace.Code != nil ||
				string(trace.RequestBody) != want || string(trace.ResponseBody) != want ||
				trace.RequestBodyTruncated != truncated || trace.ResponseBodyTruncated != truncated {
				t.Fatal("HTTP echo or captured body was incorrect")
			}
		})
	}
}

func TestAuditWithoutConsumingRequest(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, body, want string
	}{
		{"clear JSON", "application/json", `{"msg":"hello"}`, `{"msg":"hello"}`},
		{"credential and large integer", "application/json", `{"app_secret":"private","value":9007199254740993}`, `{"app_secret":"[REDACTED]","value":9007199254740993}`},
		{"truncated JSON", "application/json", `{"app_secret":"` + strings.Repeat("x", auditBodyLimit), ""},
		{"invalid JSON without content type", "", `{"app_secret":"private",`, ""},
		{"multipart", "multipart/form-data; boundary=segment", "--segment", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := store.New(nil, 0)
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/open-apis/example", strings.NewReader(tc.body))
			r.Header.Set("Content-Type", tc.contentType)
			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Error("unauthenticated request reached the handler")
			})
			Audit(state, Bearer(state, Tenant, true)(next)).ServeHTTP(w, r)
			trace := state.Traces()[0]
			if string(trace.RequestBody) != tc.want || trace.RequestBodyTruncated != (len(tc.body) > auditBodyLimit) ||
				trace.Code == nil || *trace.Code != 99991661 || w.Code != http.StatusBadRequest {
				t.Fatalf("unexpected request body and status: %q, status=%d", trace.RequestBody, w.Code)
			}
		})
	}
}

type faultFunc func(http.ResponseWriter, *http.Request) bool

func (hook faultFunc) BeforeRequest(w http.ResponseWriter, r *http.Request) bool { return hook(w, r) }

func TestAuditAndFault(t *testing.T) {
	state := store.New(nil, 0)
	rateLimit := faultFunc(func(w http.ResponseWriter, _ *http.Request) bool {
		if err := protocol.WriteError(w, protocol.NewError(protocol.RateLimited, nil)); err != nil {
			t.Error(err)
		}
		return true
	})
	for _, tc := range []struct {
		name   string
		hook   FaultHook
		status int
		code   int
	}{
		{"no fault hook", nil, http.StatusOK, 0},
		{"hook passes", faultFunc(func(http.ResponseWriter, *http.Request) bool { return false }), http.StatusOK, 0},
		{"hook intercepts", rateLimit, http.StatusTooManyRequests, 99991400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/open-apis/example", nil)
			called := false
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				if err := protocol.WriteSuccess(w, nil); err != nil {
					t.Error(err)
				}
			})
			state.Reset()
			Audit(state, Fault(tc.hook, next)).ServeHTTP(w, r)
			trace := state.Traces()[0]
			if w.Code != tc.status || called != (tc.code == 0) || trace.Status != tc.status || trace.Code == nil || *trace.Code != tc.code {
				t.Fatalf("HTTP %d, handler called=%t, trace=%+v", w.Code, called, trace)
			}
		})
	}
}
