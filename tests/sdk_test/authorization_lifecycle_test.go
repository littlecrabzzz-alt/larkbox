package sdk_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/event"
)

const revokeEvent = "larkbox.auth.authorization_revoked_v1"

type grantList struct {
	AppID string `json:"app_id"`
	Items []struct {
		OpenID string    `json:"open_id"`
		Scope  string    `json:"scope"`
		At     time.Time `json:"authorized_at"`
	} `json:"items"`
}

func TestAuthorizationLifecycle(t *testing.T) {
	type notification struct {
		Header struct {
			AppID string `json:"app_id"`
			Type  string `json:"event_type"`
		} `json:"header"`
		Event map[string]string `json:"event"`
	}
	inbox := make(chan notification, 4)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var incoming notification
		if err := json.NewDecoder(r.Body).Decode(&incoming); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		inbox <- incoming
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(webhook.Close)
	cfg := sdkEventConfig()
	first, second := &cfg.Apps[0], &cfg.Apps[1]
	first.RedirectURIs, second.RedirectURIs = []string{callbackURL}, []string{callbackURL}
	first.WebhookURL, first.EncryptKey, first.EventTypes = webhook.URL, "", []string{revokeEvent}
	alice := cfg.Users[0].OpenID
	srv, web := sdkEventServer(t, cfg, event.Options{})

	list := func(appID string) grantList {
		return sandboxDecode[grantList](t, sandboxRequest(t, web, http.MethodGet, "apps/"+appID+"/grants", nil, http.StatusOK))
	}
	consent := func(app *config.App, decision string) url.Values {
		query := url.Values{"client_id": {app.AppID}, "redirect_uri": {callbackURL}, "state": {"s + / & ="},
			"open_id": {alice}, "scope": {grantedScope}, "decision": {decision}}
		header, _ := accountRequest(t, web, "/open-apis/authen/v1/authorize?"+query.Encode(), nil, nil, http.StatusFound)
		location, err := url.Parse(header.Get("Location"))
		requireSDKNetwork(t, err)
		callback := location.Query()
		if location.Host != "example.test" || location.Path != "/callback" || callback.Get("keep") != "yes" || callback.Get("state") != query.Get("state") {
			t.Fatalf("incorrect callback %s", location)
		}
		return callback
	}
	issue := func(app *config.App, kind, field, credential string, status int) oauthWire {
		form := url.Values{"client_id": {app.AppID}, "client_secret": {app.AppSecret}, "grant_type": {kind}, field: {credential}}
		if kind == "authorization_code" {
			form.Set("redirect_uri", callbackURL)
		}
		_, raw := accountRequest(t, web, "/oauth/v3/token", form, nil, status)
		return sandboxDecode[oauthWire](t, raw)
	}
	assertBearer := func(app *config.App, token string, valid bool) {
		t.Helper()
		info, err := sdkEventREST(web, *app).Authen.UserInfo.Get(t.Context(), larkcore.WithUserAccessToken(token))
		requireSDKNetwork(t, err)
		if info.Success() != valid || (valid && (info.Data == nil || value(info.Data.OpenId) != alice)) {
			t.Fatalf("expected bearer valid=%t, received HTTP %d code %d", valid, info.StatusCode, info.Code)
		}
	}
	revoke := func(app *config.App, token, hint string) {
		form := url.Values{"client_id": {app.AppID}, "client_secret": {app.AppSecret}, "token": {token}, "token_type_hint": {hint}}
		_, raw := accountRequest(t, web, "/oauth/v1/revoke", form, nil, http.StatusOK)
		if len(raw) != 0 {
			t.Fatal("expected empty revoke response")
		}
	}

	if denied := consent(first, "deny"); denied.Get("error") != "access_denied" || denied.Has("code") || len(list(first.AppID).Items) != 0 {
		t.Fatal("denial should redirect without a code or recorded consent")
	}
	initial := issue(first, "authorization_code", "code", consent(first, "approve").Get("code"), http.StatusOK)
	rotated := issue(first, "refresh_token", "refresh_token", value(initial.RefreshToken), http.StatusOK)
	otherApp := issue(second, "authorization_code", "code", consent(second, "approve").Get("code"), http.StatusOK)
	unusedCode := consent(first, "approve").Get("code")
	entries := list(first.AppID)
	if entries.AppID != first.AppID || len(entries.Items) != 1 || entries.Items[0].OpenID != alice ||
		entries.Items[0].Scope != "auth:user.id:read offline_access" || entries.Items[0].At.IsZero() {
		t.Fatalf("incorrect authorization list: %+v", entries)
	}

	revoke(second, value(rotated.RefreshToken), "refresh_token")
	assertBearer(first, value(initial.AccessToken), true)
	assertBearer(first, value(rotated.AccessToken), true)
	revoke(first, value(rotated.RefreshToken), "refresh_token")
	received := receiveSDKEvent(t, inbox)
	if received.Header.AppID != first.AppID || received.Header.Type != revokeEvent || len(received.Event) != 2 ||
		received.Event["open_id"] != alice || received.Event["scope"] != entries.Items[0].Scope || len(list(first.AppID).Items) != 0 {
		t.Fatalf("incorrect authorization-removal event or state: %+v", received)
	}
	assertBearer(first, value(initial.AccessToken), false)
	assertBearer(first, value(rotated.AccessToken), false)
	assertBearer(second, value(otherApp.AccessToken), true)
	if len(list(second.AppID).Items) != 1 {
		t.Fatal("another app's grant was removed")
	}
	issue(first, "refresh_token", "refresh_token", value(rotated.RefreshToken), http.StatusBadRequest)
	issue(first, "authorization_code", "code", unusedCode, http.StatusBadRequest)

	newGrant := issue(first, "authorization_code", "code", consent(first, "approve").Get("code"), http.StatusOK)
	revoke(first, value(rotated.AccessToken), "access_token")
	assertBearer(first, value(newGrant.AccessToken), true)
	revoke(first, value(newGrant.AccessToken), "access_token")
	assertBearer(first, value(newGrant.AccessToken), false)
	receiveSDKEvent(t, inbox)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	requireSDKNetwork(t, srv.Shutdown(ctx))
	if len(inbox) != 0 || len(list(first.AppID).Items) != 0 {
		t.Fatal("idempotent revocation generated duplicate events or retained a grant")
	}
}

type deviceChallenge struct {
	DeviceCode   string `json:"device_code"`
	UserCode     string `json:"user_code"`
	Verification string `json:"verification_uri_complete"`
	Expires      int64  `json:"expires_in"`
	Interval     int64  `json:"interval"`
}

func TestDeviceRegistrationAndConsent(t *testing.T) {
	cfg := sdkEventConfig()
	start := time.Now().UTC()
	cfg.ClockStart = &start
	_, web := sdkEventServer(t, cfg, event.Options{})
	alice := cfg.Users[0].OpenID
	const registration, deviceStart, exchange = "/oauth/v1/app/registration", "/oauth/v1/device_authorization", "/oauth/v3/token"
	call := func(path string, form url.Values, basic *config.App, status int) []byte {
		_, raw := accountRequest(t, web, path, form, basic, status)
		return raw
	}
	begin := func(app *config.App) deviceChallenge {
		path := deviceStart
		var form url.Values
		if app == nil {
			path = registration
			form = url.Values{"action": {"begin"}, "archetype": {"PersonalAgent"}, "auth_method": {"client_secret"}}
		} else {
			form = url.Values{"client_id": {app.AppID}, "scope": {grantedScope}}
		}
		flow := sandboxDecode[deviceChallenge](t, call(path, form, app, http.StatusOK))
		if flow.DeviceCode == "" || flow.UserCode == "" || flow.DeviceCode == flow.UserCode || flow.Interval != 1 ||
			flow.Expires != cfg.TokenTTLs.Code || flow.Verification != web.URL+"/page/cli?user_code="+flow.UserCode {
			t.Fatal("invalid device flow challenge")
		}
		return flow
	}
	poll := func(flow deviceChallenge, app *config.App, status int) []byte {
		if app == nil {
			return call(registration, url.Values{"action": {"poll"}, "device_code": {flow.DeviceCode}}, nil, status)
		}
		return call(exchange, url.Values{
			"client_id": {app.AppID}, "client_secret": {app.AppSecret}, "device_code": {flow.DeviceCode},
			"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"},
		}, nil, status)
	}
	pollError := func(flow deviceChallenge, app *config.App, kind string) {
		t.Helper()
		if got := sandboxDecode[map[string]string](t, poll(flow, app, http.StatusBadRequest))["error"]; got != kind {
			t.Fatalf("poll error %q, want %q", got, kind)
		}
	}
	decide := func(flow deviceChallenge, decision string, status int) {
		sandboxRequest(t, web, http.MethodPost, "auth/devices/decision", map[string]string{
			"user_code": flow.UserCode, "open_id": alice, "decision": decision,
		}, status)
	}
	advance := func(by string) {
		sandboxRequest(t, web, http.MethodPost, "clock/advance", map[string]string{"by": by}, http.StatusOK)
	}

	appFlow := begin(nil)
	pollError(appFlow, nil, "authorization_pending")
	pollError(appFlow, nil, "slow_down")
	page := call("/page/cli?user_code="+appFlow.UserCode, nil, nil, http.StatusOK)
	if !strings.Contains(string(page), "Register a sandbox app") || !strings.Contains(string(page), alice) {
		t.Fatal("registration confirmation page lacks the simulated user")
	}
	decide(appFlow, "approve", http.StatusOK)
	decide(appFlow, "deny", http.StatusConflict)
	advance("6s")
	created := sandboxDecode[map[string]json.RawMessage](t, poll(appFlow, nil, http.StatusOK))
	app := config.App{
		AppID:     sandboxDecode[string](t, created["client_id"]),
		AppSecret: sandboxDecode[string](t, created["client_secret"]),
	}
	if app.AppID == "" || app.AppSecret == "" || sandboxDecode[map[string]string](t, created["user_info"])["open_id"] != alice {
		t.Fatal("registration did not provision credentials for the selected user")
	}
	pollError(appFlow, nil, "invalid_grant")
	grants := func() grantList {
		return sandboxDecode[grantList](t, sandboxRequest(t, web, http.MethodGet, "apps/"+app.AppID+"/grants", nil, http.StatusOK))
	}
	if len(grants().Items) != 0 {
		t.Fatal("application registration recorded a user grant")
	}
	login := begin(&app)
	pollError(login, nil, "invalid_grant")
	pollError(login, &cfg.Apps[1], "invalid_grant")
	decide(login, "approve", http.StatusOK)
	tokens := sandboxDecode[oauthWire](t, poll(login, &app, http.StatusOK))
	if value(tokens.AccessToken) == "" || value(tokens.RefreshToken) == "" || len(grants().Items) != 1 {
		t.Fatal("device login did not issue credentials and record authorization")
	}
	pollError(login, &app, "invalid_grant")
	for _, client := range []*config.App{nil, &app} {
		denied := begin(client)
		decide(denied, "deny", http.StatusOK)
		pollError(denied, client, "access_denied")
		expired := begin(client)
		advance("5m")
		pollError(expired, client, "expired_token")
		decide(expired, "approve", http.StatusNotFound)
	}
	trace := string(sandboxRequest(t, web, http.MethodGet, "traces", nil, http.StatusOK))
	for _, secret := range []string{appFlow.UserCode, appFlow.DeviceCode, app.AppSecret, login.DeviceCode, login.UserCode, value(tokens.AccessToken), value(tokens.RefreshToken)} {
		if strings.Contains(trace, secret) {
			t.Fatal("trace retained a device code or credential")
		}
	}
	sandboxRequest(t, web, http.MethodPost, "reset", nil, http.StatusOK)
	sandboxRequest(t, web, http.MethodGet, "apps/"+app.AppID+"/grants", nil, http.StatusNotFound)
}

func accountRequest(t *testing.T, web *httptest.Server, path string, fields url.Values, basic *config.App, status int) (http.Header, []byte) {
	t.Helper()
	method := http.MethodGet
	var body io.Reader
	if fields != nil {
		method, body = http.MethodPost, strings.NewReader(fields.Encode())
	}
	req, err := http.NewRequestWithContext(t.Context(), method, web.URL+path, body)
	requireSDKNetwork(t, err)
	if fields != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if basic != nil {
		req.SetBasicAuth(basic.AppID, basic.AppSecret)
	}
	client := *web.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	requireSDKNetwork(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	requireSDKNetwork(t, err)
	if resp.StatusCode != status {
		t.Fatalf("%s %s = %d, want %d: %s", method, req.URL.Path, resp.StatusCode, status, raw)
	}
	return resp.Header, raw
}
