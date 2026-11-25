package auth

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/middleware"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

const (
	tenantPath    = "/open-apis/auth/v3/tenant_access_token/internal"
	appPath       = "/open-apis/auth/v3/app_access_token/internal"
	authorizePath = "/open-apis/authen/v1/authorize"
	indexPath     = "/open-apis/authen/v1/index"
	oidcPath      = "/open-apis/authen/v1/oidc/access_token"
	refreshPath   = "/open-apis/authen/v1/oidc/refresh_access_token"
	oauthPath     = "/open-apis/authen/v2/oauth/token"
	userPath      = "/open-apis/authen/v1/user_info"
	redirectURI   = "http://127.0.0.1:8081/callback?keep=one&keep=two"
)

type fixture struct {
	mux          http.Handler
	state        *store.Store
	cfg          config.Config
	now          *time.Time
	app, foreign config.App
}

func newFixture() fixture {
	cfg := config.Default()
	cfg.StrictMode = false                                  // These OAuth endpoints always require the correct bearer kind.
	cfg.Users[0], cfg.Users[1] = cfg.Users[1], cfg.Users[0] // Seed order differs from open ID order.
	cfg.Users[0].EnName = "Robert"
	cfg.Users[0].Avatar = "https://example.test/avatar"
	cfg.Users[0].AvatarThumb = "https://example.test/avatar/72"
	cfg.Users[0].AvatarMiddle = "https://example.test/avatar/240"
	cfg.Users[0].AvatarBig = "https://example.test/avatar/640"
	cfg.Users[0].EnterpriseEmail = "bob@enterprise.example"
	cfg.Users[0].Mobile = "+1234567890"
	cfg.Users[0].TenantKey = "tenant-local"
	cfg.Users[0].EmployeeNo = "EMP-002"
	cfg.Apps[0].RedirectURIs = []string{redirectURI}
	foreign := cfg.Apps[0]
	foreign.AppID += "_foreign"
	foreign.RedirectURIs = []string{"https://foreign.example.test/callback"}
	cfg.Apps = append(cfg.Apps, foreign)
	cfg.TokenTTLs = config.TokenTTLs{Tenant: 9, App: 11, User: 13, Refresh: 17, Code: 5}
	now := time.Unix(1000, 0)
	state := store.New(func() time.Time { return now }, 0)
	state.Seed(cfg)
	mux := http.NewServeMux()
	RegisterRoutes(mux, cfg, state)
	return fixture{middleware.Audit(state, mux), state, cfg, &now, cfg.Apps[0], foreign}
}

func (f fixture) call(method, path, bearer, mediaType, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	if mediaType != "" {
		r.Header.Set("Content-Type", mediaType)
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	return w
}

func (f fixture) post(path, bearer, body string) *httptest.ResponseRecorder {
	return f.call(http.MethodPost, path, bearer, "application/json", body)
}

func jsonFields(t *testing.T, fields map[string]string) string {
	t.Helper()
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

type wireResponse struct {
	Code             *int            `json:"code"`
	Msg              string          `json:"msg"`
	Error            string          `json:"error"`
	ErrorDescription string          `json:"error_description"`
	Tenant           string          `json:"tenant_access_token"`
	App              string          `json:"app_access_token"`
	Expire           int64           `json:"expire"`
	Data             json.RawMessage `json:"data"`
}

func readResponse(t *testing.T, w *httptest.ResponseRecorder, wantStatus, wantCode int) wireResponse {
	t.Helper()
	var response wireResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("invalid JSON response, HTTP %d: %s", w.Code, w.Body)
	}
	if w.Code != wantStatus || response.Code == nil || *response.Code != wantCode {
		t.Fatalf("HTTP %d: %s, want HTTP %d with code %d", w.Code, w.Body, wantStatus, wantCode)
	}
	return response
}

func (f fixture) internal(t *testing.T, path string, app config.App) wireResponse {
	t.Helper()
	body := jsonFields(t, map[string]string{"app_id": app.AppID, "app_secret": app.AppSecret})
	return readResponse(t, f.post(path, "", body), http.StatusOK, 0)
}

func TestInternalTokens(t *testing.T) {
	f := newFixture()
	for _, path := range []string{tenantPath, appPath} {
		t.Run(path, func(t *testing.T) {
			success := f.internal(t, path, f.app)
			wantExpire := f.cfg.TokenTTLs.Tenant
			if path == appPath {
				wantExpire = f.cfg.TokenTTLs.App
				assertStored(t, f, success.App, "a-", store.AppToken, wantExpire)
			} else if success.App != "" {
				t.Fatal("tenant response unexpectedly included an app token")
			}
			if success.Msg != "ok" || success.Expire != wantExpire || len(success.Data) != 0 {
				t.Fatal("invalid top-level internal token shape")
			}
			assertStored(t, f, success.Tenant, "t-", store.TenantToken, f.cfg.TokenTTLs.Tenant)

			for _, tc := range []struct {
				name, body, msg string
				status, code    int
			}{
				{"invalid syntax", `{"app_id":`, "bad request or invalid param", 400, 9499},
				{"invalid type", `{"app_id":1,"app_secret":"value"}`, "invalid param", 200, 10003},
				{"missing app", `{"app_secret":"value"}`, "invalid param", 200, 10003},
				{"missing secret", `{"app_id":"cli_larkbox"}`, "invalid param", 200, 10003},
				{"null secret", `{"app_id":"cli_larkbox","app_secret":null}`, "invalid param", 200, 10003},
				{"extra field", `{"app_id":"cli_larkbox","app_secret":"value","extra":1}`, "invalid param", 200, 10003},
				{"duplicate key", `{"app_id":"first","app_id":"cli_larkbox","app_secret":"value"}`, "invalid param", 200, 10003},
				{"trailing value", `{"app_id":"cli_larkbox","app_secret":"value"} {}`, "bad request or invalid param", 400, 9499},
				{"unknown app", `{"app_id":"unknown","app_secret":"value"}`, "app id not exists", 200, 10014},
				{"wrong secret", `{"app_id":"cli_larkbox","app_secret":"value"}`, "app secret invalid", 200, 10014},
			} {
				t.Run(tc.name, func(t *testing.T) {
					w := f.post(path, "", tc.body)
					readResponse(t, w, tc.status, tc.code)
					want := fmt.Sprintf(`{"code":%d,"msg":%q}`, tc.code, tc.msg)
					if tc.code == 10003 {
						want = fmt.Sprintf(`{"code":%d,"msg":%q,"data":{}}`, tc.code, tc.msg)
					}
					if w.Body.String() != want {
						t.Fatalf("response = %s, want %s", w.Body, want)
					}
					f.assertAudit(t, path, tc.status, tc.code)
				})
			}
		})
	}
}

func (f fixture) assertAudit(t *testing.T, path string, status, code int) {
	t.Helper()
	traces := f.state.QueryTraces(store.TraceFilter{PathPrefix: path, Limit: 1})
	if len(traces) != 1 || traces[0].Status != status || traces[0].Code == nil || *traces[0].Code != code {
		t.Fatalf("audit did not capture HTTP %d with code %d on %s", status, code, path)
	}
}

func assertStored(t *testing.T, f fixture, value, prefix string, kind store.TokenKind, ttl int64) {
	t.Helper()
	token, err := f.state.CheckToken(value)
	if err != nil || !strings.HasPrefix(value, prefix) || token.Kind != kind || token.AppID != f.app.AppID ||
		!token.ExpiresAt.Equal(f.now.Add(time.Duration(ttl)*time.Second)) {
		t.Fatalf("credential %s has incorrect type, owner or expiry: %v", kind, err)
	}
}

func (f fixture) authorize(t *testing.T, path string, options url.Values) *url.URL {
	t.Helper()
	if options == nil {
		options = make(url.Values)
	}
	options.Set("app_id", f.app.AppID)
	if !options.Has("redirect_uri") {
		options.Set("redirect_uri", redirectURI)
	}
	w := f.call(http.MethodGet, path+"?"+options.Encode(), "", "", "")
	if w.Code != http.StatusFound || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("authorize response HTTP %d: %s", w.Code, w.Body)
	}
	callback, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if callback.Query().Get("code") == "" {
		t.Fatal("redirect has no code")
	}
	return callback
}

func TestAuthorize(t *testing.T) {
	f := newFixture()
	for _, tc := range []struct {
		path, selector, value, expectedOpenID string
	}{
		{authorizePath, "", "", f.cfg.Users[0].OpenID},
		{indexPath, "open_id", f.cfg.Users[1].OpenID, f.cfg.Users[1].OpenID},
		{authorizePath, "user_id", f.cfg.Users[1].UserID, f.cfg.Users[1].OpenID},
	} {
		options := url.Values{"state": {"state & ?=+"}, "scope": {"scope1 scope2"}}
		if tc.selector != "" {
			options.Set(tc.selector, tc.value)
		}
		callback := f.authorize(t, tc.path, options)
		query := callback.Query()
		if callback.Scheme != "http" || callback.Path != "/callback" || callback.Host != "127.0.0.1:8081" ||
			query.Get("state") != "state & ?=+" || !reflect.DeepEqual(query["keep"], []string{"one", "two"}) {
			t.Fatalf("callback fields not preserved: %v", callback)
		}
		grant, err := f.state.CheckToken(query.Get("code"))
		if err != nil || grant.Kind != store.OAuthCode || grant.AppID != f.app.AppID || grant.UserOpenID != tc.expectedOpenID ||
			grant.Scope != "scope1 scope2" || grant.RedirectURI != redirectURI || !grant.ExpiresAt.Equal(f.now.Add(5*time.Second)) {
			t.Fatal("authorization grant not bound to requested app, user, URI, scope and expiry")
		}
	}
	callback := f.authorize(t, authorizePath, nil)
	if callback.Query().Has("state") {
		t.Fatal("authorization synthesized an unrequested state")
	}
	for _, tc := range []struct {
		name, appID, redirect, parameter string
		code                             int
	}{
		{"missing app", "", redirectURI, "", 20028},
		{"unknown app", "unknown", redirectURI, "", 20028},
		{"missing redirect", f.app.AppID, "", "", 20029},
		{"relative redirect", f.app.AppID, "/callback", "", 20029},
		{"other scheme", f.app.AppID, "ftp://example.test/path", "", 20029},
		{"empty host", f.app.AppID, "https://:443/path", "", 20029},
		{"unknown user", f.app.AppID, redirectURI, "&open_id=unknown", 20008},
		{"unsupported PKCE", f.app.AppID, redirectURI, "&code_challenge=abc&code_challenge_method=MD5", 10003},
		{"method without challenge", f.app.AppID, redirectURI, "&code_challenge_method=S256", 10003},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query := url.Values{"app_id": {tc.appID}, "redirect_uri": {tc.redirect}}
			w := f.call(http.MethodGet, authorizePath+"?"+query.Encode()+tc.parameter, "", "", "")
			readResponse(t, w, http.StatusOK, tc.code)
			if w.Header().Get("Location") != "" {
				t.Fatal("invalid authorization request redirected")
			}
		})
	}
}

func TestUserInfoAndBearerKind(t *testing.T) {
	f := newFixture()
	app := f.internal(t, appPath, f.app)
	code := f.authorize(t, indexPath, nil).Query().Get("code")
	body := jsonFields(t, map[string]string{"grant_type": "authorization_code", "code": code})
	for _, tc := range []struct {
		method, path, bearer, body string
		code                       int
	}{
		{http.MethodGet, userPath, "", "", 99991661},
		{http.MethodGet, userPath, app.App, "", 99991664},
		{http.MethodGet, userPath, app.Tenant, "", 99991663},
		{http.MethodPost, oidcPath, app.Tenant, body, 99991663},
		{http.MethodPost, refreshPath, app.Tenant, "{}", 99991663},
	} {
		w := f.call(tc.method, tc.path, tc.bearer, "application/json", tc.body)
		readResponse(t, w, http.StatusBadRequest, tc.code)
	}
	token := v1Tokens(t, f, oidcPath, app.App, body)
	readResponse(t, f.post(oidcPath, token.AccessToken, body), http.StatusBadRequest, 99991668)
	readResponse(t, f.post(refreshPath, token.AccessToken, "{}"), http.StatusBadRequest, 99991668)
	profile := readResponse(t, f.call(http.MethodGet, userPath, token.AccessToken, "", ""), http.StatusOK, 0)
	var fields map[string]string
	if err := json.Unmarshal(profile.Data, &fields); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"name": "Bob", "en_name": "Robert", "avatar_url": "https://example.test/avatar",
		"avatar_thumb": "https://example.test/avatar/72", "avatar_middle": "https://example.test/avatar/240",
		"avatar_big": "https://example.test/avatar/640", "open_id": "ou_larkbox_bob", "union_id": "on_larkbox_bob",
		"email": "bob@example.test", "enterprise_email": "bob@enterprise.example", "user_id": "bob",
		"mobile": "+1234567890", "tenant_key": "tenant-local", "employee_no": "EMP-002",
	}
	if profile.Msg != "success" || !reflect.DeepEqual(fields, want) {
		t.Fatalf("profile = %+v, want %+v", fields, want)
	}
	*f.now = f.now.Add(time.Duration(f.cfg.TokenTTLs.User) * time.Second)
	response := readResponse(t, f.call(http.MethodGet, userPath, token.AccessToken, "", ""), http.StatusBadRequest, 99991668)
	if response.Msg != "token invalid" {
		t.Fatalf("expired user token msg = %q", response.Msg)
	}
}

func TestAuthorizeRequiresRegisteredCallback(t *testing.T) {
	f := newFixture()
	invalid := []string{
		redirectURI + "#", redirectURI + "#fragment",
		"http://localhost:99999/callback", "http://localhost:0/callback",
		"http://localhost:abc/callback", "http://localhost:/callback",
		"http://[example.test]/callback", "http://user@localhost/callback",
	}
	// Exercise runtime validation even if a callback enters the store
	// without going through config loading.
	app := f.app
	app.RedirectURIs = append([]string{redirectURI}, invalid...)
	f.state.Apps().Put(app)
	for _, path := range []string{authorizePath, indexPath} {
		t.Run(path, func(t *testing.T) {
			f.authorize(t, path, nil)
			for _, callback := range append([]string{
				f.foreign.RedirectURIs[0],
				"http://unregistered.example.test/callback",
				"http://127.0.0.1:8081/callback?keep=one",
			}, invalid...) {
				query := url.Values{"app_id": {f.app.AppID}, "redirect_uri": {callback}}
				w := f.call(http.MethodGet, path+"?"+query.Encode(), "", "", "")
				readResponse(t, w, http.StatusOK, 20029)
				if w.Header().Get("Location") != "" {
					t.Fatalf("unauthorized callback got a redirect: %s", callback)
				}
			}
		})
	}
}
