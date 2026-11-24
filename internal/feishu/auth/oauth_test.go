package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

func v1Tokens(t *testing.T, f fixture, path, bearer, body string) oidcResponse {
	t.Helper()
	wire := readResponse(t, f.post(path, bearer, body), http.StatusOK, 0)
	var data oidcResponse
	if err := json.Unmarshal(wire.Data, &data); err != nil {
		t.Fatal(err)
	}
	if wire.Msg != "success" {
		t.Fatalf("unexpected OIDC message: %q", wire.Msg)
	}
	checkUserTokens(t, f, data.tokenResponse, data.RefreshExpiresIn)
	return data
}

func v2Tokens(t *testing.T, f fixture, w *httptest.ResponseRecorder) oauthResponse {
	t.Helper()
	wire := readResponse(t, w, http.StatusOK, 0)
	var data oauthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if wire.Msg != "" || wire.Error != "" || len(wire.Data) != 0 {
		t.Fatal("OAuth v2 response is not a top-level token result")
	}
	checkUserTokens(t, f, data.tokenResponse, data.RefreshExpiresIn)
	return data
}

func checkUserTokens(t *testing.T, f fixture, tokens tokenResponse, refreshExpires int64) {
	t.Helper()
	if tokens.TokenType != "Bearer" || tokens.ExpiresIn != f.cfg.TokenTTLs.User || refreshExpires != f.cfg.TokenTTLs.Refresh {
		t.Fatal("token type or TTL does not match OAuth configuration")
	}
	assertStored(t, f, tokens.AccessToken, "u-", store.UserToken, f.cfg.TokenTTLs.User)
	assertStored(t, f, tokens.RefreshToken, "ur-", store.RefreshToken, f.cfg.TokenTTLs.Refresh)
}

func TestOIDCExchangeAndRefresh(t *testing.T) {
	f := newFixture()
	appToken := f.internal(t, appPath, f.app).App
	otherToken := f.internal(t, appPath, f.foreign).App
	for _, tc := range []struct {
		path, body string
		code       int
	}{
		{oidcPath, `{}`, 20001},
		{oidcPath, `{"grant_type":"authorization_code"}`, 20001},
		{oidcPath, `{"grant_type":"refresh_token"}`, 20036},
		{oidcPath, `{"grant_type":"authorization_code","code":"bogus"}`, 20003},
		{refreshPath, `{}`, 20001},
		{refreshPath, `{"grant_type":"refresh_token"}`, 20001},
		{refreshPath, `{"grant_type":"authorization_code"}`, 20036},
		{refreshPath, `{"grant_type":"refresh_token","refresh_token":"bogus"}`, 20026},
	} {
		readResponse(t, f.post(tc.path, appToken, tc.body), http.StatusOK, tc.code)
	}

	const scope = "auth:user.id:read offline_access"
	code := f.authorize(t, authorizePath, url.Values{"scope": {scope}}).Query().Get("code")
	exchangeBody := jsonFields(t, map[string]string{"grant_type": "authorization_code", "code": code})
	readResponse(t, f.post(oidcPath, otherToken, exchangeBody), http.StatusOK, 20024)
	initial := v1Tokens(t, f, oidcPath, appToken, exchangeBody)
	readResponse(t, f.post(oidcPath, appToken, exchangeBody), http.StatusOK, 20003)
	refreshBody := jsonFields(t, map[string]string{"grant_type": "refresh_token", "refresh_token": initial.RefreshToken})
	readResponse(t, f.post(refreshPath, otherToken, refreshBody), http.StatusOK, 20024)
	rotated := v1Tokens(t, f, refreshPath, appToken, refreshBody)
	readResponse(t, f.post(refreshPath, appToken, refreshBody), http.StatusOK, 20038)
	if initial.AccessToken == rotated.AccessToken || initial.RefreshToken == rotated.RefreshToken ||
		initial.Scope != scope || rotated.Scope != scope {
		t.Fatal("OIDC refresh lost scope or reused credentials")
	}
	assertStored(t, f, initial.AccessToken, "u-", store.UserToken, f.cfg.TokenTTLs.User)

	expiring := f.authorize(t, authorizePath, nil).Query().Get("code")
	*f.now = f.now.Add(5 * time.Second)
	body := jsonFields(t, map[string]string{"grant_type": "authorization_code", "code": expiring})
	readResponse(t, f.post(oidcPath, appToken, body), http.StatusOK, 20004)
}

func oauthFields(app config.App, grant, token string) map[string]string {
	fields := map[string]string{"client_id": app.AppID, "client_secret": app.AppSecret, "grant_type": grant}
	if grant == authorizationCode {
		fields["code"], fields["redirect_uri"] = token, redirectURI
	} else {
		fields["refresh_token"] = token
	}
	return fields
}

func (f fixture) v2(t *testing.T, fields map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return f.post(oauthPath, "", jsonFields(t, fields))
}

func (f fixture) v2Form(fields map[string]string) *httptest.ResponseRecorder {
	form := make(url.Values)
	for name, value := range fields {
		form.Set(name, value)
	}
	return f.call(http.MethodPost, oauthPath, "", "application/x-www-form-urlencoded; charset=utf-8", form.Encode())
}

func expectOAuthError(t *testing.T, w *httptest.ResponseRecorder, code int, kind string) wireResponse {
	t.Helper()
	wire := readResponse(t, w, http.StatusBadRequest, code)
	if wire.Error != kind || wire.ErrorDescription == "" || wire.Msg != "" || len(wire.Data) != 0 ||
		w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("invalid OAuth error shape: %s", w.Body)
	}
	return wire
}

func TestOAuthV2ErrorsAndRotation(t *testing.T) {
	f := newFixture()
	for _, tc := range []struct {
		name, grant, token, key, value, kind string
		code                                 int
	}{
		{"unknown code and app", authorizationCode, "unknown", "client_id", "unknown", "invalid_grant", 20003},
		{"unknown refresh and app", refreshGrant, "unknown", "client_id", "unknown", "invalid_grant", 20026},
		{"wrong secret", authorizationCode, "value", "client_secret", "wrong", "invalid_client", 20002},
		{"unsupported grant", "client_credentials", "", "", "", "unsupported_grant_type", 20036},
		{"unknown code", authorizationCode, "unknown", "", "", "invalid_grant", 20003},
		{"unknown refresh", refreshGrant, "unknown", "", "", "invalid_grant", 20026},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fields := oauthFields(f.app, tc.grant, tc.token)
			if tc.key != "" {
				fields[tc.key] = tc.value
			}
			expectOAuthError(t, f.v2(t, fields), tc.code, tc.kind)
			f.assertAudit(t, oauthPath, http.StatusBadRequest, tc.code)
		})
	}
	for _, tc := range []struct{ path, mediaType, body string }{
		{oauthPath, "application/json", `{"grant_type":`},
		{oauthPath, "text/plain", `{}`},
		{oauthPath, "application/x-www-form-urlencoded", "grant_type=authorization_code&grant_type=refresh_token"},
		{oauthPath, "application/x-www-form-urlencoded", "code=%"},
	} {
		wire := expectOAuthError(t, f.call(http.MethodPost, tc.path, "", tc.mediaType, tc.body), 20001, "invalid_request")
		if wire.ErrorDescription != "The request is malformed. Please check your request." {
			t.Fatalf("unexpected parse error: %q", wire.ErrorDescription)
		}
		f.assertAudit(t, oauthPath, http.StatusBadRequest, 20001)
	}
	query := url.Values{"client_id": {f.app.AppID}, "client_secret": {f.app.AppSecret}}
	expectOAuthError(t, f.call(http.MethodPost, oauthPath+"?"+query.Encode(), "", "application/x-www-form-urlencoded",
		"grant_type=refresh_token&refresh_token=test"), 20063, "invalid_request")

	for _, field := range []string{"grant_type", "client_id", "client_secret", "code", "refresh_token"} {
		grant := authorizationCode
		if field == "refresh_token" {
			grant = refreshGrant
		}
		fields := oauthFields(f.app, grant, "value")
		delete(fields, field)
		wire := expectOAuthError(t, f.v2(t, fields), 20063, "invalid_request")
		if want := "Missing required parameter: " + field + "."; wire.ErrorDescription != want {
			t.Fatalf("error description = %q, want %q", wire.ErrorDescription, want)
		}
		f.assertAudit(t, oauthPath, http.StatusBadRequest, 20063)
	}

	const scope = "offline_access scope1 scope2"
	code := f.authorize(t, authorizePath, url.Values{"scope": {scope}}).Query().Get("code")
	missingApp := f.app
	missingApp.AppID = "unknown"
	expectOAuthError(t, f.v2(t, oauthFields(missingApp, authorizationCode, code)), 20048, "invalid_client")
	expectOAuthError(t, f.v2(t, oauthFields(f.foreign, authorizationCode, code)), 20024, "invalid_grant")
	fields := oauthFields(f.app, authorizationCode, code)
	fields["redirect_uri"] = "https://example.test/other"
	expectOAuthError(t, f.v2(t, fields), 20071, "invalid_grant")
	delete(fields, "redirect_uri")
	expectOAuthError(t, f.v2(t, fields), 20071, "invalid_grant")
	fields["redirect_uri"] = redirectURI

	fields["scope"] = "scope1 scope1"
	expectOAuthError(t, f.v2(t, fields), 20067, "invalid_scope")
	fields["scope"] = "ungranted"
	expectOAuthError(t, f.v2(t, fields), 20068, "invalid_scope")
	fields["scope"] = "offline_access scope2"
	first := v2Tokens(t, f, f.v2(t, fields))
	expectOAuthError(t, f.v2(t, fields), 20065, "invalid_grant")
	if first.Scope != "offline_access scope2" {
		t.Fatalf("scope = %q, want restricted scope", first.Scope)
	}

	expectOAuthError(t, f.v2(t, oauthFields(missingApp, refreshGrant, first.RefreshToken)), 20048, "invalid_client")
	expectOAuthError(t, f.v2(t, oauthFields(missingApp, authorizationCode, first.RefreshToken)), 20003, "invalid_grant")
	expectOAuthError(t, f.v2(t, oauthFields(missingApp, refreshGrant, code)), 20026, "invalid_grant")
	expectOAuthError(t, f.v2(t, oauthFields(f.foreign, refreshGrant, first.RefreshToken)), 20024, "invalid_grant")
	refresh := oauthFields(f.app, refreshGrant, first.RefreshToken)
	rotated := v2Tokens(t, f, f.v2Form(refresh))
	expectOAuthError(t, f.v2(t, refresh), 20073, "invalid_grant")
	if rotated.Scope != scope || rotated.AccessToken == first.AccessToken || rotated.RefreshToken == first.RefreshToken {
		t.Fatal("OAuth v2 refresh lost the original grant or reused credentials")
	}

	expiring := f.authorize(t, indexPath, nil).Query().Get("code")
	*f.now = f.now.Add(5 * time.Second)
	expectOAuthError(t, f.v2(t, oauthFields(f.app, authorizationCode, expiring)), 20004, "invalid_grant")
	*f.now = f.now.Add(12 * time.Second)
	expectOAuthError(t, f.v2(t, oauthFields(f.app, refreshGrant, rotated.RefreshToken)), 20037, "invalid_grant")
}

func TestPKCE(t *testing.T) {
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	hash := sha256.Sum256([]byte(verifier))
	for _, tc := range []struct{ method, challenge string }{
		{"S256", base64.RawURLEncoding.EncodeToString(hash[:])},
		{"plain", verifier},
		{"", verifier},
	} {
		t.Run("method="+tc.method, func(t *testing.T) {
			f := newFixture()
			appToken := f.internal(t, appPath, f.app).App
			code := f.authorize(t, authorizePath, url.Values{
				"code_challenge": {tc.challenge}, "code_challenge_method": {tc.method}, "scope": {"offline_access"},
			}).Query().Get("code")

			body := jsonFields(t, map[string]string{"grant_type": "authorization_code", "code": code})
			readResponse(t, f.post(oidcPath, appToken, body), http.StatusOK, 20003)
			fields := oauthFields(f.app, authorizationCode, code)
			expectOAuthError(t, f.v2(t, fields), 20049, "invalid_grant")
			fields["code_verifier"] = strings.Repeat("x", 43)
			expectOAuthError(t, f.v2(t, fields), 20049, "invalid_grant")
			fields["code_verifier"] = verifier
			v2Tokens(t, f, f.v2(t, fields))
		})
	}
}

func TestConcurrentGrantExchange(t *testing.T) {
	f := newFixture()
	appToken := f.internal(t, appPath, f.app).App
	code := f.authorize(t, authorizePath, url.Values{"scope": {"offline_access"}}).Query().Get("code")
	v1 := jsonFields(t, map[string]string{"grant_type": authorizationCode, "code": code})
	v2 := jsonFields(t, oauthFields(f.app, authorizationCode, code))
	winner := raceExchange(t, func(i int) *httptest.ResponseRecorder {
		if i%2 == 0 {
			return f.post(oidcPath, appToken, v1)
		}
		return f.post(oauthPath, "", v2)
	}, 20003, 20065)
	var result tokenResponse
	body := winner.Body.Bytes()
	if wire := readResponse(t, winner, http.StatusOK, 0); len(wire.Data) != 0 {
		body = wire.Data
	}
	if err := json.Unmarshal(body, &result); err != nil || result.RefreshToken == "" {
		t.Fatalf("exchange race winner has no refresh token: %v", err)
	}

	v1 = jsonFields(t, map[string]string{"grant_type": refreshGrant, "refresh_token": result.RefreshToken})
	v2 = jsonFields(t, oauthFields(f.app, refreshGrant, result.RefreshToken))
	raceExchange(t, func(i int) *httptest.ResponseRecorder {
		if i%2 == 0 {
			return f.post(refreshPath, appToken, v1)
		}
		return f.post(oauthPath, "", v2)
	}, 20038, 20073)
}

func raceExchange(t *testing.T, call func(int) *httptest.ResponseRecorder, v1Failure, v2Failure int) *httptest.ResponseRecorder {
	t.Helper()
	const workers = 24
	start := make(chan struct{})
	var group sync.WaitGroup
	responses := make([]*httptest.ResponseRecorder, workers)
	for i := range responses {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			responses[i] = call(i)
		}()
	}
	close(start)
	group.Wait()

	var winner *httptest.ResponseRecorder
	for i, w := range responses {
		var response wireResponse
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.Code == nil {
			t.Fatalf("invalid response from worker %d: %s", i, w.Body)
		}
		switch {
		case *response.Code == 0:
			if winner != nil {
				t.Fatal("multiple requests exchanged the same OAuth grant")
			}
			winner = w
		case i%2 == 0:
			readResponse(t, w, http.StatusOK, v1Failure)
		default:
			expectOAuthError(t, w, v2Failure, "invalid_grant")
		}
	}
	if winner == nil {
		t.Fatal("no worker exchanged the OAuth grant")
	}
	return winner
}

func TestUnchallengedCodeRejectsSuppliedVerifier(t *testing.T) {
	for _, format := range []string{"json", "form"} {
		t.Run(format, func(t *testing.T) {
			f := newFixture()
			code := f.authorize(t, authorizePath, url.Values{"scope": {"offline_access"}}).Query().Get("code")
			fields := oauthFields(f.app, authorizationCode, code)
			exchange := func() *httptest.ResponseRecorder {
				if format == "form" {
					return f.v2Form(fields)
				}
				return f.v2(t, fields)
			}
			for _, verifier := range []string{strings.Repeat("v", 43), ""} {
				fields["code_verifier"] = verifier
				expectOAuthError(t, exchange(), 20049, "invalid_grant")
				grant, err := f.state.CheckToken(code)
				if err != nil || grant.Consumed || grant.Revoked {
					t.Fatal("rejected PKCE request spent the grant")
				}
			}
			delete(fields, "code_verifier")
			v2Tokens(t, f, exchange())
		})
	}
}

func TestOAuthV2OfflineAccess(t *testing.T) {
	f := newFixture()
	for _, tc := range []struct{ granted, requested, effective string }{
		{"profile", "", "profile"},
		{"profile offline_access_extra", "", "profile offline_access_extra"},
		{"profile\toffline_access", "", "profile\toffline_access"},
		{"profile offline_access", "profile", "profile"},
	} {
		code := f.authorize(t, authorizePath, url.Values{"scope": {tc.granted}}).Query().Get("code")
		fields := oauthFields(f.app, authorizationCode, code)
		fields["scope"] = tc.requested
		assertAccessOnly(t, f, f.v2(t, fields), tc.effective)
	}

	code := f.authorize(t, authorizePath, url.Values{"scope": {"profile offline_access"}}).Query().Get("code")
	first := v2Tokens(t, f, f.v2(t, oauthFields(f.app, authorizationCode, code)))
	refresh := oauthFields(f.app, refreshGrant, first.RefreshToken)
	rotated := v2Tokens(t, f, f.v2(t, refresh))
	if rotated.RefreshToken == first.RefreshToken {
		t.Fatal("refresh with offline_access did not rotate the refresh token")
	}
	refresh = oauthFields(f.app, refreshGrant, rotated.RefreshToken)
	refresh["scope"] = "profile"
	assertAccessOnly(t, f, f.v2(t, refresh), "profile")
	expectOAuthError(t, f.v2(t, refresh), 20073, "invalid_grant")

	// A v1 refresh token without offline_access can yield a v2 access token.
	appToken := f.internal(t, appPath, f.app).App
	code = f.authorize(t, authorizePath, url.Values{"scope": {"profile"}}).Query().Get("code")
	body := jsonFields(t, map[string]string{"grant_type": authorizationCode, "code": code})
	legacy := v1Tokens(t, f, oidcPath, appToken, body)
	assertAccessOnly(t, f, f.v2(t, oauthFields(f.app, refreshGrant, legacy.RefreshToken)), "profile")
	if _, err := f.state.CheckToken(""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("access-only exchange stored an empty refresh credential: %v", err)
	}
}

func assertAccessOnly(t *testing.T, f fixture, w *httptest.ResponseRecorder, scope string) {
	t.Helper()
	readResponse(t, w, http.StatusOK, 0)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	if fields["refresh_token"] != nil || fields["refresh_token_expires_in"] != nil {
		t.Fatal("OAuth v2 response included refresh fields without offline_access")
	}
	var result oauthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Scope != scope || result.TokenType != "Bearer" || result.ExpiresIn != f.cfg.TokenTTLs.User {
		t.Fatal("unexpected access-only token result")
	}
	assertStored(t, f, result.AccessToken, "u-", store.UserToken, f.cfg.TokenTTLs.User)
}

func TestOAuthV2RevokedAndConsumedRefreshTokens(t *testing.T) {
	f := newFixture()
	code := f.authorize(t, authorizePath, url.Values{"scope": {"offline_access"}}).Query().Get("code")
	initial := v2Tokens(t, f, f.v2(t, oauthFields(f.app, authorizationCode, code)))
	first := oauthFields(f.app, refreshGrant, initial.RefreshToken)
	rotated := v2Tokens(t, f, f.v2(t, first))
	if !f.state.RevokeToken(rotated.RefreshToken) {
		t.Fatal("missing refresh token to revoke")
	}
	expectOAuthError(t, f.v2(t, first), 20073, "invalid_grant")
	expectOAuthError(t, f.v2(t, oauthFields(f.app, refreshGrant, rotated.RefreshToken)), 20064, "invalid_grant")
	consumed, err := f.state.CheckToken(initial.RefreshToken)
	if !errors.Is(err, store.ErrTokenConsumed) || !consumed.Consumed || consumed.Revoked {
		t.Fatal("successful exchange did not preserve a distinct consumed state")
	}
	revoked, err := f.state.CheckToken(rotated.RefreshToken)
	if !errors.Is(err, store.ErrTokenRevoked) || !revoked.Revoked || revoked.Consumed {
		t.Fatal("explicit revocation did not preserve a distinct revoked state")
	}
}
