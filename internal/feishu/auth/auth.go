// Package auth serves Feishu internal credentials and user OAuth endpoints.
package auth

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/middleware"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

type handler struct {
	state *store.Store
	ttls  config.TokenTTLs
}

// RegisterRoutes installs the Auth v3 and Authen v1/v2 Open APIs.
func RegisterRoutes(mux *http.ServeMux, cfg config.Config, state *store.Store) {
	h := handler{state, cfg.TokenTTLs}
	requireApp := middleware.Bearer(state, middleware.App, true)
	requireUser := middleware.Bearer(state, middleware.User, true)

	mux.HandleFunc("POST /open-apis/auth/v3/tenant_access_token/internal", h.internalToken(false))
	mux.HandleFunc("POST /open-apis/auth/v3/app_access_token/internal", h.internalToken(true))
	mux.HandleFunc("GET /open-apis/authen/v1/authorize", h.authorize)
	mux.HandleFunc("GET /open-apis/authen/v1/index", h.authorize)
	mux.Handle("POST /open-apis/authen/v1/oidc/access_token", requireApp(h.oidcToken(false)))
	mux.Handle("POST /open-apis/authen/v1/oidc/refresh_access_token", requireApp(h.oidcToken(true)))
	mux.HandleFunc("POST /open-apis/authen/v2/oauth/token", h.oauthToken)
	mux.Handle("GET /open-apis/authen/v1/user_info", requireUser(http.HandlerFunc(h.userInfo)))
}

type internalResponse struct {
	Code   protocol.Code `json:"code"`
	Msg    string        `json:"msg"`
	App    string        `json:"app_access_token,omitempty"`
	Tenant string        `json:"tenant_access_token"`
	Expire int64         `json:"expire"`
}

func (h handler) internalToken(includeApp bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fields, ok := readJSON(w, r, "app_id", "app_secret")
		appID, secret := fields["app_id"], fields["app_secret"]
		if !ok || appID == "" || secret == "" {
			writeAPIError(w, protocol.InvalidParameter)
			return
		}
		app, found := h.state.Apps().Get(appID)
		if !found {
			writeAPIError(w, protocol.AppUnauthorized)
			return
		}
		if !matchesSecret(app, secret) {
			writeAPIError(w, protocol.WrongAppSecret)
			return
		}

		middleware.SetTraceApp(r, appID)
		now := h.state.Now()
		tenant := store.Token{
			Value: protocol.NewID(protocol.TenantToken), Kind: store.TenantToken, AppID: appID,
			ExpiresAt: now.Add(seconds(h.ttls.Tenant)),
		}
		tokens := []store.Token{tenant}
		response := internalResponse{Code: protocol.Success, Msg: "ok", Tenant: tenant.Value, Expire: h.ttls.Tenant}
		if includeApp {
			appToken := store.Token{
				Value: protocol.NewID(protocol.AppToken), Kind: store.AppToken, AppID: appID,
				ExpiresAt: now.Add(seconds(h.ttls.App)),
			}
			tokens = append(tokens, appToken)
			response.App, response.Expire = appToken.Value, h.ttls.App
		}
		if err := h.state.PutTokens(tokens...); err != nil {
			writeAPIError(w, protocol.AppUnauthorized)
			return
		}
		_ = protocol.WriteResult(w, http.StatusOK, response.Code, response)
	}
}

func matchesSecret(app store.App, secret string) bool {
	return secret != "" && subtle.ConstantTimeCompare([]byte(app.AppSecret), []byte(secret)) == 1
}

func seconds(value int64) time.Duration { return time.Duration(value) * time.Second }

const maxRequestBody = 16 * 1024

// readJSON accepts exactly one object with unique, recognized string fields.
func readJSON(w http.ResponseWriter, r *http.Request, allowed ...string) (map[string]string, bool) {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if start, err := decoder.Token(); err != nil || start != json.Delim('{') {
		return nil, false
	}
	fields := make(map[string]string, len(allowed))
	for decoder.More() {
		key, err := decoder.Token()
		name, ok := key.(string)
		if err != nil || !ok || !slices.Contains(allowed, name) {
			return nil, false
		}
		if _, duplicate := fields[name]; duplicate {
			return nil, false
		}
		var value *string
		if err := decoder.Decode(&value); err != nil || value == nil {
			return nil, false
		}
		fields[name] = *value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, false
	}
	_, err := decoder.Token()
	return fields, errors.Is(err, io.EOF)
}

func writeAPIError(w http.ResponseWriter, code protocol.Code) {
	_ = protocol.WriteError(w, protocol.NewError(code, nil))
}
