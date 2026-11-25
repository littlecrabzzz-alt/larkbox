package sdk_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkauth "github.com/larksuite/oapi-sdk-go/v3/service/auth/v3"
	larkauthen "github.com/larksuite/oapi-sdk-go/v3/service/authen/v1"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/server"
)

const (
	callbackURL  = "https://example.test/callback?keep=yes"
	grantedScope = "offline_access auth:user.id:read"
)

func TestOfficialSDKAuth(t *testing.T) {
	cfg := sdkTestConfig()
	cfg.Apps[0].RedirectURIs = []string{callbackURL}
	cfg.Users[0].EmployeeNo = "EMP-001"
	app := cfg.Apps[0]
	srv := server.New(cfg)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	transport := ts.Client()
	transport.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client := lark.NewClient(app.AppID, app.AppSecret, lark.WithOpenBaseUrl(ts.URL), lark.WithHttpClient(transport))

	t.Run("internal tenant and app tokens", func(t *testing.T) {
		tenantReq := larkauth.NewInternalTenantAccessTokenReqBuilder().
			Body(larkauth.NewInternalTenantAccessTokenReqBodyBuilder().AppId(app.AppID).AppSecret(app.AppSecret).Build()).Build()
		tenant, err := client.Auth.TenantAccessToken.Internal(t.Context(), tenantReq)
		if err != nil {
			t.Fatal(err)
		}
		if !tenant.Success() || tenant.Msg != "ok" {
			t.Fatalf("tenant token result: %+v", tenant.CodeError)
		}
		assertInternalWire(t, tenant.ApiResp, false)

		appReq := larkauth.NewInternalAppAccessTokenReqBuilder().
			Body(larkauth.NewInternalAppAccessTokenReqBodyBuilder().AppId(app.AppID).AppSecret(app.AppSecret).Build()).Build()
		appToken, err := client.Auth.AppAccessToken.Internal(t.Context(), appReq)
		if err != nil {
			t.Fatal(err)
		}
		if !appToken.Success() || appToken.Msg != "ok" {
			t.Fatalf("app token result: %+v", appToken.CodeError)
		}
		assertInternalWire(t, appToken.ApiResp, true)
	})

	t.Run("OIDC exchange user info and refresh", func(t *testing.T) {
		code := authorize(t, client, "/open-apis/authen/v1/index", app.AppID, "app_id")
		exchange := larkauthen.NewCreateOidcAccessTokenReqBuilder().
			Body(larkauthen.NewCreateOidcAccessTokenReqBodyBuilder().GrantType("authorization_code").Code(code).Build()).Build()
		// The SDK obtains its own app token from the sandbox for this call.
		token, err := client.Authen.OidcAccessToken.Create(t.Context(), exchange)
		if err != nil {
			t.Fatal(err)
		}
		if !token.Success() || token.Msg != "success" || token.Data == nil {
			t.Fatalf("OIDC exchange result: %+v", token.CodeError)
		}
		assertTokens(t, token.Data.AccessToken, token.Data.RefreshToken, token.Data.TokenType, token.Data.Scope, token.Data.ExpiresIn, token.Data.RefreshExpiresIn)
		assertProfile(t, client, *token.Data.AccessToken, cfg.Users[0])

		refresh := larkauthen.NewCreateOidcRefreshAccessTokenReqBuilder().
			Body(larkauthen.NewCreateOidcRefreshAccessTokenReqBodyBuilder().GrantType("refresh_token").RefreshToken(*token.Data.RefreshToken).Build()).Build()
		updated, err := client.Authen.OidcRefreshAccessToken.Create(t.Context(), refresh)
		if err != nil {
			t.Fatal(err)
		}
		if !updated.Success() || updated.Data == nil {
			t.Fatalf("OIDC refresh result: %+v", updated.CodeError)
		}
		assertTokens(t, updated.Data.AccessToken, updated.Data.RefreshToken, updated.Data.TokenType, updated.Data.Scope, updated.Data.ExpiresIn, updated.Data.RefreshExpiresIn)
		if *updated.Data.AccessToken == *token.Data.AccessToken || *updated.Data.RefreshToken == *token.Data.RefreshToken {
			t.Fatal("OIDC refresh did not rotate both credentials")
		}
		assertProfile(t, client, *updated.Data.AccessToken, cfg.Users[0])
	})

	t.Run("OAuth v2 exchange and refresh", func(t *testing.T) {
		code := authorize(t, client, "/open-apis/authen/v1/authorize", app.AppID, "client_id")
		// The SDK has no generated v2 resource. Its generic Post still uses
		// the official transport, serialization, and configured base URL.
		token := oauthV2(t, client, map[string]string{
			"grant_type": "authorization_code", "code": code, "redirect_uri": callbackURL,
			"client_id": app.AppID, "client_secret": app.AppSecret,
		})
		assertProfile(t, client, *token.AccessToken, cfg.Users[0])
		rotated := oauthV2(t, client, map[string]string{
			"grant_type": "refresh_token", "refresh_token": *token.RefreshToken,
			"client_id": app.AppID, "client_secret": app.AppSecret,
		})
		if *rotated.AccessToken == *token.AccessToken || *rotated.RefreshToken == *token.RefreshToken {
			t.Fatal("OAuth v2 refresh did not rotate both credentials")
		}
		assertProfile(t, client, *rotated.AccessToken, cfg.Users[0])
	})

	t.Run("invalid credentials and wrong bearer kind", func(t *testing.T) {
		badReq := larkauth.NewInternalTenantAccessTokenReqBuilder().
			Body(larkauth.NewInternalTenantAccessTokenReqBodyBuilder().AppId(app.AppID).AppSecret("wrong").Build()).Build()
		bad, err := client.Auth.TenantAccessToken.Internal(t.Context(), badReq)
		if err != nil {
			t.Fatal(err)
		}
		if bad.Success() || bad.Code != 10014 || bad.Msg != "app secret invalid" {
			t.Fatalf("credential error: %+v", bad.CodeError)
		}
		tenant, err := client.GetTenantAccessTokenBySelfBuiltApp(t.Context(),
			&larkcore.SelfBuiltTenantAccessTokenReq{AppID: app.AppID, AppSecret: app.AppSecret})
		if err != nil {
			t.Fatal(err)
		}
		appToken, err := client.GetAppAccessTokenBySelfBuiltApp(t.Context(),
			&larkcore.SelfBuiltAppAccessTokenReq{AppID: app.AppID, AppSecret: app.AppSecret})
		if err != nil {
			t.Fatal(err)
		}
		for token, want := range map[string]int{tenant.TenantAccessToken: 99991663, appToken.AppAccessToken: 99991664} {
			// Supply the wrong kind through the SDK's user-token option so
			// the request reaches the sandbox's bearer validation.
			info, err := client.Authen.UserInfo.Get(t.Context(), larkcore.WithUserAccessToken(token))
			if err != nil {
				t.Fatal(err)
			}
			if info.Success() || info.Code != want || info.StatusCode != http.StatusBadRequest {
				t.Errorf("wrong bearer kind: HTTP %d, %+v, want %d", info.StatusCode, info.CodeError, want)
			}
		}
	})
}

func assertInternalWire(t *testing.T, resp *larkcore.ApiResp, includeApp bool) {
	t.Helper()
	// The generated SDK response structs model the credentials under Data.
	// Feishu sends them at the top level, which the SDK token manager uses.
	var wire struct {
		Code   *int            `json:"code"`
		Msg    string          `json:"msg"`
		Tenant string          `json:"tenant_access_token"`
		App    string          `json:"app_access_token"`
		Expire int             `json:"expire"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(resp.RawBody, &wire); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || wire.Code == nil || *wire.Code != 0 || wire.Msg != "ok" ||
		!strings.HasPrefix(wire.Tenant, "t-") || wire.Expire != 7200 || len(wire.Data) != 0 ||
		(includeApp && !strings.HasPrefix(wire.App, "a-")) || (!includeApp && wire.App != "") {
		t.Fatal("unexpected top-level internal token response")
	}
}

func authorize(t *testing.T, client *lark.Client, path, appID, appParam string) string {
	t.Helper()
	resp, err := client.Do(t.Context(), &larkcore.ApiReq{
		HttpMethod: http.MethodGet, ApiPath: path,
		SupportedAccessTokenTypes: []larkcore.AccessTokenType{larkcore.AccessTokenTypeNone},
		QueryParams: larkcore.QueryParams{
			appParam: {appID}, "redirect_uri": {callbackURL}, "state": {"sdk state"},
			"scope": {grantedScope}, "response_type": {"code"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize status = %d, want 302", resp.StatusCode)
	}
	location, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	query := location.Query()
	if location.Host != "example.test" || location.Path != "/callback" ||
		query.Get("keep") != "yes" || query.Get("state") != "sdk state" || query.Get("code") == "" {
		t.Fatal("authorize did not return the expected redirect and query")
	}
	return query.Get("code")
}

type oauthWire struct {
	Code             *int            `json:"code"`
	AccessToken      *string         `json:"access_token"`
	RefreshToken     *string         `json:"refresh_token"`
	TokenType        *string         `json:"token_type"`
	Scope            *string         `json:"scope"`
	ExpiresIn        *int            `json:"expires_in"`
	RefreshExpiresIn *int            `json:"refresh_token_expires_in"`
	Data             json.RawMessage `json:"data"`
}

func oauthV2(t *testing.T, client *lark.Client, fields map[string]string) oauthWire {
	t.Helper()
	resp, err := client.Post(t.Context(), "/open-apis/authen/v2/oauth/token", fields, larkcore.AccessTokenTypeNone)
	if err != nil {
		t.Fatal(err)
	}
	var token oauthWire
	if err := json.Unmarshal(resp.RawBody, &token); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || token.Code == nil || *token.Code != 0 || len(token.Data) != 0 {
		t.Fatalf("unexpected OAuth v2 wire response, status=%d", resp.StatusCode)
	}
	assertTokens(t, token.AccessToken, token.RefreshToken, token.TokenType, token.Scope, token.ExpiresIn, token.RefreshExpiresIn)
	return token
}

func assertTokens(t *testing.T, access, refresh, tokenType, scope *string, expire, refreshExpire *int) {
	t.Helper()
	if access == nil || !strings.HasPrefix(*access, "u-") ||
		refresh == nil || !strings.HasPrefix(*refresh, "ur-") ||
		tokenType == nil || *tokenType != "Bearer" || scope == nil || *scope != grantedScope ||
		expire == nil || *expire != 7200 || refreshExpire == nil || *refreshExpire != 30*24*3600 {
		t.Fatal("unexpected token wire values")
	}
}

func assertProfile(t *testing.T, client *lark.Client, token string, user config.User) {
	t.Helper()
	info, err := client.Authen.UserInfo.Get(t.Context(), larkcore.WithUserAccessToken(token))
	if err != nil {
		t.Fatal(err)
	}
	if !info.Success() || info.Msg != "success" || info.Data == nil ||
		info.Data.OpenId == nil || *info.Data.OpenId != user.OpenID ||
		info.Data.Name == nil || *info.Data.Name != user.Name ||
		info.Data.Email == nil || *info.Data.Email != user.Email ||
		info.Data.UnionId == nil || *info.Data.UnionId != user.UnionID ||
		info.Data.EmployeeNo == nil || *info.Data.EmployeeNo != user.EmployeeNo {
		t.Fatalf("unexpected user info result: %+v", info.CodeError)
	}
}
