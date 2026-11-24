//go:build live

package livetest

import (
	"encoding/json"
	"net/http"
	"testing"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkauth "github.com/larksuite/oapi-sdk-go/v3/service/auth/v3"

	"github.com/littlecrabzzz-alt/larkbox/tests/fixturetest"
)

const tenantPath = "/open-apis/auth/v3/tenant_access_token/internal"

func TestAuthContract(t *testing.T) {
	h := newHarness(t)
	t.Run("tenant token", func(t *testing.T) {
		resp, err := h.sdk.Auth.TenantAccessToken.Internal(t.Context(), larkauth.NewInternalTenantAccessTokenReqBuilder().
			Body(larkauth.NewInternalTenantAccessTokenReqBodyBuilder().AppId(h.appID).AppSecret(h.appSecret).Build()).Build())
		requireSDK(t, "get tenant token", err)
		golden(t, "auth/tenant_access_token_ok", resp.ApiResp)
	})
	t.Run("app token", func(t *testing.T) {
		resp, err := h.sdk.Auth.AppAccessToken.Internal(t.Context(), larkauth.NewInternalAppAccessTokenReqBuilder().
			Body(larkauth.NewInternalAppAccessTokenReqBodyBuilder().AppId(h.appID).AppSecret(h.appSecret).Build()).Build())
		requireSDK(t, "get app token", err)
		golden(t, "auth/app_access_token_ok", resp.ApiResp)
	})
	wrong, err := json.Marshal(map[string]string{"app_id": h.appID, "app_secret": "invalid-fixture-secret"})
	requireSDK(t, "encode invalid credentials", err)
	for _, tc := range []struct {
		name, fixture string
		body          []byte
	}{
		{"bad secret", "auth/tenant_access_token_bad_secret", wrong},
		{"missing parameter", "auth/tenant_access_token_missing_param", []byte("{}")},
		{"malformed JSON", "auth/tenant_access_token_malformed", []byte("{")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, raw := fixturetest.Request(t, h.http, h.base, http.MethodPost, tenantPath, "", tc.body)
			fixturetest.Golden(t, tc.fixture, status, raw, false)
		})
	}
	for _, tc := range []struct {
		name, token, shape string
	}{
		{"missing bearer", "", `{"code":99991661,"msg":""}`},
		{"invalid bearer", "t-invalid-fixture-token", `{"code":99991665,"msg":""}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, raw := fixturetest.Request(t, h.http, h.base, http.MethodGet, botInfoPath, tc.token, nil)
			fixturetest.Envelope(t, status, raw, http.StatusBadRequest, tc.shape)
		})
	}
}

func (h *harness) tenantToken(t *testing.T) string {
	t.Helper()
	token, err := h.sdk.GetTenantAccessTokenBySelfBuiltApp(t.Context(),
		&larkcore.SelfBuiltTenantAccessTokenReq{AppID: h.appID, AppSecret: h.appSecret})
	requireSDK(t, "authenticate as tenant app", err)
	if token.Code != 0 || token.TenantAccessToken == "" {
		t.Fatalf("authenticate as tenant app: business code %d or missing token", token.Code)
	}
	return token.TenantAccessToken
}
