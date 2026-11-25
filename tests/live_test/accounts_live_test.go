//go:build live

package livetest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
)

func (h *harness) accountsBase() string {
	if h.live {
		return "https://accounts.feishu.cn"
	}
	return h.base
}

func TestUnknownTokenRevocationContract(t *testing.T) {
	h := newHarness(t)
	// Random nonexistent credentials exercise the wire contract without
	// invalidating a credential held by the test tenant.
	for hint, value := range map[string]string{
		"access_token": protocol.NewID(protocol.UserToken), "refresh_token": protocol.NewID(protocol.RefreshToken),
	} {
		t.Run(hint, func(t *testing.T) {
			form := url.Values{"client_id": {h.appID}, "client_secret": {h.appSecret}, "token": {value}, "token_type_hint": {hint}}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.accountsBase()+"/oauth/v1/revoke", strings.NewReader(form.Encode()))
			requireSDK(t, "build revocation request", err)
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			resp, err := h.http.Do(req)
			requireSDK(t, "send revocation request", err)
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
			requireSDK(t, "read revocation response", err)
			if resp.StatusCode != http.StatusOK || len(body) != 0 {
				t.Fatalf("revocation returned HTTP %d with a %d-byte body, expected an empty HTTP 200", resp.StatusCode, len(body))
			}
		})
	}
}

func TestTenantClientCredentialsContract(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct {
		name, id, secret, scope, kind string
		basic                         bool
		status, code                  int
	}{
		{name: "basic", basic: true, status: 200},
		{name: "form", id: h.appID, secret: h.appSecret, status: 200},
		{name: "basic with body id", id: h.appID, basic: true, status: 200},
		{name: "both", id: h.appID, secret: h.appSecret, basic: true, status: 400, code: 20070, kind: "invalid_request"},
		{name: "missing", status: 400, code: 20063, kind: "invalid_request"},
		{name: "invalid secret", id: h.appID, secret: "invalid-fixture-secret", status: 400, code: 20002, kind: "invalid_client"},
		{name: "duplicate scope", id: h.appID, secret: h.appSecret, scope: "im:message:send_as_bot im:message:send_as_bot", status: 400, code: 20067, kind: "invalid_scope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			form := url.Values{"grant_type": {"client_credentials"}}
			for key, value := range map[string]string{"client_id": tc.id, "client_secret": tc.secret, "scope": tc.scope} {
				if value != "" {
					form.Set(key, value)
				}
			}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.accountsBase()+"/oauth/v3/token", strings.NewReader(form.Encode()))
			requireSDK(t, "build client credentials request", err)
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if tc.basic {
				req.SetBasicAuth(h.appID, h.appSecret)
			}
			resp, err := h.http.Do(req)
			requireSDK(t, "send client credentials request", err)
			defer func() { _ = resp.Body.Close() }()
			var body struct {
				Code        *int   `json:"code"`
				Error       string `json:"error"`
				Description string `json:"error_description"`
				AccessToken string `json:"access_token"`
				TokenType   string `json:"token_type"`
				ExpiresIn   int64  `json:"expires_in"`
			}
			requireSDK(t, "read client credentials response", json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body))
			if resp.StatusCode != tc.status || body.Code == nil || *body.Code != tc.code || body.Error != tc.kind {
				t.Fatalf("unexpected client credentials response for %s, HTTP %d", tc.name, resp.StatusCode)
			}
			if tc.status == 200 && (body.AccessToken == "" || body.TokenType != "Bearer" || body.ExpiresIn <= 0) {
				t.Fatal("expected a Bearer tenant token and positive TTL")
			}
			if tc.status != 200 && (body.Description == "" || body.AccessToken != "") {
				t.Fatal("expected an OAuth error without an access token")
			}
		})
	}
}
