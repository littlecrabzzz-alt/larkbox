//go:build live

package livetest

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkauth "github.com/larksuite/oapi-sdk-go/v3/service/auth/v3"

	"github.com/littlecrabzzz-alt/larkbox/tests/fixturetest"
)

const tenantPath = "/open-apis/auth/v3/tenant_access_token/internal"

const (
	tenantEnvelope = `{"code":0,"msg":"","expire":0,"tenant_access_token":"fixture:tenant"}`
	appEnvelope    = `{"code":0,"msg":"","expire":0,"tenant_access_token":"fixture:tenant","app_access_token":"fixture:live_app_token"}`
)

func TestAuthContract(t *testing.T) {
	h := newHarness(t)
	t.Run("tenant token", func(t *testing.T) {
		resp, err := h.sdk.Auth.TenantAccessToken.Internal(t.Context(), larkauth.NewInternalTenantAccessTokenReqBuilder().
			Body(larkauth.NewInternalTenantAccessTokenReqBodyBuilder().AppId(h.appID).AppSecret(h.appSecret).Build()).Build())
		requireSDK(t, "get tenant token", err)
		h.checkResponse(t, "auth/tenant_access_token_ok", resp.ApiResp, http.StatusOK, tenantEnvelope)
	})
	t.Run("app token", func(t *testing.T) {
		resp, err := h.sdk.Auth.AppAccessToken.Internal(t.Context(), larkauth.NewInternalAppAccessTokenReqBuilder().
			Body(larkauth.NewInternalAppAccessTokenReqBodyBuilder().AppId(h.appID).AppSecret(h.appSecret).Build()).Build())
		requireSDK(t, "get app token", err)
		h.checkResponse(t, "auth/app_access_token_ok", resp.ApiResp, http.StatusOK, appEnvelope)
	})
	wrong, err := json.Marshal(map[string]string{"app_id": h.appID, "app_secret": "invalid-fixture-secret"})
	requireSDK(t, "encode invalid credentials", err)
	for _, tc := range []struct {
		name, fixture string
		body          []byte
		status, code  int
	}{
		{"bad secret", "auth/tenant_access_token_bad_secret", wrong, http.StatusOK, 10014},
		{"missing parameter", "auth/tenant_access_token_missing_param", []byte("{}"), http.StatusOK, 10003},
		{"malformed JSON", "auth/tenant_access_token_malformed", []byte("{"), http.StatusBadRequest, 9499},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, raw := fixturetest.Request(t, h.http, h.base, http.MethodPost, tenantPath, "", tc.body)
			h.checkWire(t, tc.fixture, status, raw, tc.status, liveErrorShape(tc.code))
		})
	}
	for _, tc := range []struct {
		name, token string
		code        int
		alternate   []int
	}{
		{"missing bearer", "", 99991661, nil},
		{"invalid bearer", "t-invalid-fixture-token", 99991665, []int{99991663}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, raw := fixturetest.Request(t, h.http, h.base, http.MethodGet, botInfoPath, tc.token, nil)
			if h.live {
				fixturetest.Envelope(t, status, raw, http.StatusBadRequest, liveErrorShape(tc.code), tc.alternate...)
			} else {
				shape := `{"code":` + strconv.Itoa(tc.code) + `,"msg":""}`
				fixturetest.Envelope(t, status, raw, http.StatusBadRequest, shape)
			}
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
