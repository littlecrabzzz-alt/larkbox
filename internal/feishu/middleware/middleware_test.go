package middleware

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

func TestBearer(t *testing.T) {
	now := time.Unix(1700000000, 0)
	state := store.New(func() time.Time { return now }, 0)
	state.Apps().Put(store.App{AppID: "cli_test"})
	state.Apps().Put(store.App{AppID: "cli_removed"})
	state.Users().Put(store.User{OpenID: "ou_test"})
	state.Users().Put(store.User{OpenID: "ou_removed"})
	for _, token := range []store.Token{
		{Value: "t-valid", Kind: store.TenantToken, AppID: "cli_test", ExpiresAt: now.Add(time.Second)},
		{Value: "a-valid", Kind: store.AppToken, AppID: "cli_test", ExpiresAt: now.Add(time.Second)},
		{Value: "u-valid", Kind: store.UserToken, AppID: "cli_test", UserOpenID: "ou_test", ExpiresAt: now.Add(time.Second)},
		{Value: "r-valid", Kind: store.RefreshToken, AppID: "cli_test", ExpiresAt: now.Add(time.Second)},
		{Value: "t-expired", Kind: store.TenantToken, AppID: "cli_test", ExpiresAt: now},
		{Value: "t-revoked", Kind: store.TenantToken, AppID: "cli_test", Revoked: true, ExpiresAt: now.Add(time.Second)},
		{Value: "a-expired", Kind: store.AppToken, AppID: "cli_test", ExpiresAt: now},
		{Value: "u-expired", Kind: store.UserToken, AppID: "cli_test", UserOpenID: "ou_test", ExpiresAt: now},
		{Value: "u-revoked", Kind: store.UserToken, AppID: "cli_test", UserOpenID: "ou_test", Revoked: true, ExpiresAt: now.Add(time.Second)},
		{Value: "t-app", Kind: store.AppToken, AppID: "cli_test", ExpiresAt: now.Add(time.Second)},
		{Value: "t-app-expired", Kind: store.AppToken, AppID: "cli_test", ExpiresAt: now},
		{Value: "t-app-owner-deleted", Kind: store.AppToken, AppID: "cli_removed", ExpiresAt: now.Add(time.Second)},
		{Value: "t-owner-deleted", Kind: store.TenantToken, AppID: "cli_removed", ExpiresAt: now.Add(time.Second)},
		{Value: "u-owner-deleted", Kind: store.UserToken, AppID: "cli_test", UserOpenID: "ou_removed", ExpiresAt: now.Add(time.Second)},
	} {
		if err := state.PutToken(token); err != nil {
			t.Fatal(err)
		}
	}
	state.Apps().Delete("cli_removed")
	state.Users().Delete("ou_removed")
	const (
		missing = "Missing access token for authorization. Please make a request with token attached."
		format  = "Invalid token: must start with t-/u-"
		invalid = "Invalid access token for authorization. Please make a request with token attached."
	)
	for _, tc := range []struct {
		name      string
		required  RequiredToken
		headers   []string
		status    int
		code      int
		message   string
		wantToken store.TokenKind
		loose     bool
	}{
		{"missing", Tenant, nil, 400, 99991661, missing, "", false},
		{"wrong scheme", Tenant, []string{"Basic t-valid"}, 400, 99991661, missing, "", false},
		{"duplicate header", Tenant, []string{"Bearer t-valid", "Bearer t-valid"}, 400, 99991661, missing, "", false},
		{"empty value", Tenant, []string{"Bearer"}, 400, 99991671, format, "", false},
		{"bare prefix", Tenant, []string{"Bearer t-"}, 400, 99991671, format, "", false},
		{"extra component", Tenant, []string{"Bearer t-valid extra"}, 400, 99991671, format, "", false},
		{"non-token character", Tenant, []string{"Bearer t-valid!"}, 400, 99991671, format, "", false},
		{"unrecognized prefix", Tenant, []string{"Bearer unrecognized"}, 400, 99991671, format, "", false},
		{"refresh in authorization", TenantOrUser, []string{"Bearer r-valid"}, 400, 99991671, format, "", false},
		{"unknown tenant", Tenant, []string{"Bearer t-unknown"}, 400, 99991665, "invalid tenant code", "", false},
		{"t-prefixed app on tenant route", Tenant, []string{"Bearer t-app"}, 400, 99991664, "invalid app token", "", false},
		{"tenant missing owner", Tenant, []string{"Bearer t-owner-deleted"}, 400, 99991665, "invalid tenant code", "", false},
		{"user missing owner", User, []string{"Bearer u-owner-deleted"}, 400, 99991668, invalid, "", false},
		{"unknown user", TenantOrUser, []string{"Bearer u-unknown"}, 400, 99991668, invalid, "", false},
		{"unknown app", App, []string{"Bearer a-unknown"}, 400, 99991664, "invalid app token", "", false},
		{"expired tenant", Tenant, []string{"Bearer t-expired"}, 400, 99991663, invalid, "", false},
		{"revoked tenant", Tenant, []string{"Bearer t-revoked"}, 400, 99991663, invalid, "", false},
		{"expired app", App, []string{"Bearer a-expired"}, 400, 99991663, invalid, "", false},
		{"expired user", User, []string{"Bearer u-expired"}, 400, 99991668, "token invalid", "", false},
		{"revoked user", User, []string{"Bearer u-revoked"}, 400, 99991668, "token invalid", "", false},
		{"tenant on a user route", User, []string{"Bearer t-valid"}, 400, 99991663, invalid, "", false},
		{"user on a tenant route", Tenant, []string{"Bearer u-valid"}, 400, 99991668, "user access token not support", "", false},
		{"app on tenant or user route", TenantOrUser, []string{"Bearer a-valid"}, 400, 99991664, "invalid app token", "", false},
		{"tenant", Tenant, []string{"bearer  t-valid"}, 200, 0, "ok", store.TenantToken, false},
		{"a-prefixed app", App, []string{"Bearer a-valid"}, 200, 0, "ok", store.AppToken, false},
		{"t-prefixed app", App, []string{"Bearer t-app"}, 200, 0, "ok", store.AppToken, false},
		{"t-prefixed app expired", App, []string{"Bearer t-app-expired"}, 400, 99991663, invalid, "", false},
		{"t-prefixed app missing owner", App, []string{"Bearer t-app-owner-deleted"}, 400, 99991664, "invalid app token", "", false},
		{"user", User, []string{"Bearer u-valid"}, 200, 0, "ok", store.UserToken, false},
		{"tenant or user uses tenant", TenantOrUser, []string{"Bearer t-valid"}, 200, 0, "ok", store.TenantToken, false},
		{"tenant or user uses user", TenantOrUser, []string{"Bearer u-valid"}, 200, 0, "ok", store.UserToken, false},
		{"loose missing", Tenant, nil, 200, 0, "ok", "", true},
		{"loose valid", Tenant, []string{"Bearer t-valid"}, 200, 0, "ok", store.TenantToken, true},
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
			if int(envelope.Code) != tc.code || envelope.Msg != tc.message || called != (tc.code == 0) || w.Code != tc.status {
				t.Fatalf("HTTP %d: %s, handler called=%t", w.Code, w.Body, called)
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
	if trace.RequestHeaders.Get("Content-Type") != "application/json" || len(trace.RequestHeaders) != 1 || len(trace.ResponseHeaders) != 2 ||
		trace.RequestID == "" || trace.RequestID != trace.ResponseHeaders.Get("X-Request-Id") || trace.RequestID != w.Header().Get("X-Request-Id") {
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
			truncated := len(body) > auditBodyLimit
			if w.Body.String() != body || trace.Status != http.StatusOK || trace.Code != nil ||
				len(trace.RequestBody) != 0 || len(trace.ResponseBody) != 0 ||
				trace.RequestBodyTruncated != truncated || trace.ResponseBodyTruncated != truncated {
				t.Fatal("HTTP echo or captured body was incorrect")
			}
		})
	}
}

type observedBody struct {
	data   string
	err    error
	reads  int
	closed bool
}

func (b *observedBody) Read(data []byte) (int, error) {
	b.reads++
	n := copy(data, b.data)
	b.data = b.data[n:]
	return n, b.err
}

func (b *observedBody) Close() error {
	b.closed = true
	return b.err
}

func TestAuditWithoutConsumingRequest(t *testing.T) {
	for _, tc := range []struct {
		name string
		wrap func(*store.Store, http.Handler) http.Handler
		code int
	}{
		{"bearer", func(state *store.Store, next http.Handler) http.Handler {
			return Bearer(state, Tenant, true)(next)
		}, 99991661},
		{"fault", func(_ *store.Store, next http.Handler) http.Handler {
			return Fault(faultFunc(func(w http.ResponseWriter, _ *http.Request) bool {
				_ = protocol.WriteError(w, protocol.NewError(protocol.RateLimited, nil))
				return true
			}), next)
		}, 99991400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := store.New(nil, 0)
			w := httptest.NewRecorder()
			body := &observedBody{data: "untouched"}
			r := httptest.NewRequest(http.MethodPost, "/open-apis/example", body)
			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Error("rejected request reached the handler")
			})
			Audit(state, tc.wrap(state, next)).ServeHTTP(w, r)
			trace := state.Traces()[0]
			if body.reads != 0 || body.closed || len(trace.RequestBody) != 0 || trace.RequestBodyTruncated ||
				trace.Code == nil || *trace.Code != tc.code {
				t.Fatalf("request body was touched or response was not captured: %+v", trace)
			}
		})
	}
}

func TestAuditPreservesReadAndCloseErrors(t *testing.T) {
	const payload = "bytes delivered along with an error"
	readErr := errors.New("fixture read error")
	body := &observedBody{data: payload, err: readErr}
	r := httptest.NewRequest(http.MethodPost, "/open-apis/example", body)
	state := store.New(nil, 0)
	Audit(state, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if string(got) != payload || err != readErr || body.reads != 1 {
			t.Errorf("downstream read = %q, %v after %d reads", got, err, body.reads)
		}
		if err := r.Body.Close(); err != readErr || !body.closed {
			t.Errorf("downstream close = %v, wrapped close called=%t", err, body.closed)
		}
	})).ServeHTTP(httptest.NewRecorder(), r)
	trace := state.Traces()[0]
	if len(trace.RequestBody) != 0 || !trace.RequestBodyTruncated {
		t.Fatalf("partial request body was not omitted: %+v", trace)
	}
}

type withoutFlush struct{ http.ResponseWriter }

func TestAuditFlushCommitsHeaders(t *testing.T) {
	for _, supported := range []bool{true, false} {
		state := store.New(nil, 0)
		recorder := httptest.NewRecorder()
		var writer http.ResponseWriter = recorder
		if !supported {
			writer = withoutFlush{recorder}
		}
		Audit(state, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if _, ok := w.(http.Flusher); ok != supported {
				t.Errorf("direct http.Flusher support = %t, want %t", ok, supported)
			}
			if wrapped, ok := w.(interface{ Unwrap() http.ResponseWriter }); !ok || wrapped.Unwrap() != writer {
				t.Error("audit writer did not unwrap to the original writer")
			}
			w.Header().Set("X-Request-Id", "before flush")
			err := http.NewResponseController(w).Flush()
			if supported && err != nil || !supported && !errors.Is(err, http.ErrNotSupported) {
				t.Errorf("flush support = %t, result = %v", supported, err)
			}
			w.Header().Set("X-Request-Id", "after flush")
			if _, err := io.WriteString(w, "body"); err != nil {
				t.Error(err)
			}
		})).ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/open-apis/example", nil))
		want := "before flush"
		if !supported {
			want = "after flush"
		}
		if got := state.Traces()[0].ResponseHeaders.Get("X-Request-Id"); got != want || recorder.Flushed != supported {
			t.Errorf("flush support = %t, snapshot = %q, recorder flushed = %t", supported, got, recorder.Flushed)
		}
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

func TestAuditHeaderLimitsPreserveDownloads(t *testing.T) {
	state := store.New(nil, 0)
	body := "\x00binary\xff"
	long := strings.Repeat("é", store.TraceHeaderValueLimit)
	handler := Audit(state, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != long {
			t.Fatal("request header was modified")
		}
		w.Header().Set("Content-Disposition", "attachment; filename=fixture.bin")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Request-Id", long)
		_, _ = io.WriteString(w, body)
	}))
	req := httptest.NewRequest("GET", "/images/example", nil)
	for range 30 {
		req.Header.Add("Accept", long)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Body.String() != body || w.Header().Get("X-Request-Id") != long {
		t.Fatalf("audit altered the binary response or headers: %v", w.Header())
	}
	trace := state.Traces()[0]
	if len(trace.RequestHeaders.Values("Accept")) >= 30 || trace.RequestHeaders.Get("Accept") != long[:store.TraceHeaderValueLimit] ||
		trace.ResponseHeaders.Get("X-Request-Id") != long[:store.TraceHeaderValueLimit] || trace.Code != nil || trace.ResponseBody != nil {
		t.Fatal("trace lost metadata or recorded unbounded headers or attachment bytes")
	}
	for _, headers := range []http.Header{trace.RequestHeaders, trace.ResponseHeaders} {
		var bytes int
		for name, values := range headers {
			for _, value := range values {
				if len(value) > store.TraceHeaderValueLimit || !utf8.ValidString(value) {
					t.Fatal("captured header value exceeds its bounds or splits a UTF-8 rune")
				}
				bytes += len(name) + len(value)
			}
		}
		if bytes > store.TraceHeaderLimit {
			t.Fatalf("captured %d header bytes", bytes)
		}
	}
}
