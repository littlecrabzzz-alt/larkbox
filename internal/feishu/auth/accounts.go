package auth

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/event"
	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/middleware"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

const deviceGrant = "urn:ietf:params:oauth:grant-type:device_code"

// RegisterAccountsRoutes adds the Accounts API used by the official CLI.
func RegisterAccountsRoutes(mux *http.ServeMux, cfg config.Config, state *store.Store, events *event.Dispatcher) {
	h := handler{state: state, ttls: cfg.TokenTTLs}
	mux.HandleFunc("POST /oauth/v1/revoke", h.revoke(events))
	mux.HandleFunc("POST /oauth/v1/app/registration", h.registration)
	mux.HandleFunc("POST /oauth/v1/device_authorization", h.deviceAuthorization)
	mux.HandleFunc("POST /oauth/v3/token", h.accountsToken)
	mux.HandleFunc("GET /page/cli", h.devicePage)
	mux.HandleFunc("POST /page/cli", h.deviceDecisionPage)
}

func (h handler) revoke(events *event.Dispatcher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		fields, ok := readOAuthFields(w, r, "client_id", "client_secret", "token", "token_type_hint")
		if !ok || fields["token"] == "" {
			writeAccountsError(w, http.StatusBadRequest, "invalid_request", "A token is required.")
			return
		}
		app, ok := h.checkAccountsClient(w, r, fields["client_id"], fields["client_secret"])
		if !ok {
			return
		}
		var removed bool
		removedGrant, err := store.Commit(r.Context(), h.state, func() (store.Authorization, error) {
			grant, changed := h.state.RevokeAuthorization(fields["token"], app.AppID)
			removed = changed
			return grant, nil
		})
		if middleware.WriteResetConflict(w, err) {
			return
		}
		if removed && events != nil {
			events.Enqueue(r.Context(), app.AppID, "larkbox.auth.authorization_revoked_v1", h.state.Now(), map[string]string{
				"open_id": removedGrant.OpenID,
				"scope":   removedGrant.Scope,
			})
		}
		w.WriteHeader(http.StatusOK)
	}
}

func (h handler) checkAccountsClient(w http.ResponseWriter, r *http.Request, id, secret string) (store.App, bool) {
	app, ok := h.state.Apps().Get(id)
	if !ok || !matchesSecret(app, secret) {
		writeAccountsError(w, http.StatusUnauthorized, "invalid_client", "Invalid client credentials.")
		return store.App{}, false
	}
	middleware.SetTraceApp(r, id)
	return app, true
}

func (h handler) registration(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	fields, ok := readOAuthFields(w, r, "action", "archetype", "auth_method", "request_user_info", "device_code")
	if !ok {
		writeAccountsError(w, http.StatusBadRequest, "invalid_request", "Malformed registration request.")
		return
	}
	switch fields["action"] {
	case "begin":
		if fields["archetype"] != "PersonalAgent" || fields["auth_method"] != "client_secret" {
			writeAccountsError(w, http.StatusBadRequest, "invalid_request", "Expected PersonalAgent and client_secret.")
			return
		}
		h.beginDevice(w, r, store.DeviceRegistration, "", "")
	case "poll":
		var openID string
		app, err := store.Commit(r.Context(), h.state, func() (store.App, error) {
			registered, user, err := h.state.FinishDeviceRegistration(fields["device_code"])
			openID = user
			return registered, err
		})
		if err != nil {
			writeDeviceError(w, err)
			return
		}
		middleware.SetTraceApp(r, app.AppID)
		_ = protocol.WriteJSON(w, http.StatusOK, map[string]any{
			"client_id": app.AppID, "client_secret": app.AppSecret,
			"user_info": map[string]string{"open_id": openID, "tenant_brand": "feishu"},
		})
	default:
		writeAccountsError(w, http.StatusBadRequest, "invalid_request", "Expected action begin or poll.")
	}
}

func (h handler) deviceAuthorization(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	fields, ok := readOAuthFields(w, r, "client_id", "scope")
	if !ok || fields["client_id"] == "" {
		writeAccountsError(w, http.StatusBadRequest, "invalid_request", "A client_id is required.")
		return
	}
	id, secret, basic := r.BasicAuth()
	if !basic || id != fields["client_id"] {
		writeAccountsError(w, http.StatusUnauthorized, "invalid_client", "Invalid client credentials.")
		return
	}
	if _, ok := h.checkAccountsClient(w, r, id, secret); !ok {
		return
	}
	h.beginDevice(w, r, store.DeviceAuthorization, id, fields["scope"])
}

func (h handler) beginDevice(w http.ResponseWriter, r *http.Request, kind store.DeviceKind, appID, scope string) {
	flow, err := store.Commit(r.Context(), h.state, func() (store.DeviceFlow, error) {
		return h.state.BeginDeviceFlow(kind, appID, scope, seconds(h.ttls.Code))
	})
	if err != nil {
		writeDeviceError(w, err)
		return
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	uri := url.URL{Scheme: scheme, Host: r.Host, Path: "/page/cli"}
	base := uri.String()
	uri.RawQuery = url.Values{"user_code": {flow.UserCode}}.Encode()
	response := map[string]any{
		"device_code": flow.DeviceCode, "user_code": flow.UserCode,
		"verification_uri": base, "verification_uri_complete": uri.String(),
		"expires_in": h.ttls.Code, "interval": int(flow.Interval / time.Second),
	}
	if kind == store.DeviceRegistration {
		response["expire_in"] = h.ttls.Code
	}
	_ = protocol.WriteJSON(w, http.StatusOK, response)
}

func (h handler) accountsToken(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	fields, ok := readOAuthFields(w, r, append(slices.Clone(userTokenFields), "device_code")...)
	if !ok {
		writeOAuthError(w, oauthInvalidRequest)
		return
	}
	switch fields["grant_type"] {
	case "client_credentials":
		h.tenantCredentialGrant(w, r, fields)
	case deviceGrant:
		h.deviceToken(w, r, fields)
	default:
		h.exchangeUserToken(w, r, fields)
	}
}

func (h handler) tenantCredentialGrant(w http.ResponseWriter, r *http.Request, fields map[string]string) {
	id, secret := fields["client_id"], fields["client_secret"]
	if basicID, basicSecret, basic := r.BasicAuth(); basic {
		if secret != "" {
			writeOAuthError(w, oauthMultipleAuth)
			return
		}
		id, secret = basicID, basicSecret
	}
	if id == "" {
		writeMissingOAuthField(w, "client_id")
		return
	}
	if secret == "" {
		writeMissingOAuthField(w, "client_secret")
		return
	}
	middleware.SetTraceApp(r, id)
	app, ok := h.state.Apps().Get(id)
	if !ok {
		writeOAuthError(w, oauthAppNotFound)
		return
	}
	if !matchesSecret(app, secret) {
		writeOAuthError(w, oauthInvalidClient)
		return
	}
	scope, err := tokenScope(fields["scope"], fields["scope"])
	if err != nil {
		writeOAuthError(w, oauthDuplicateScope)
		return
	}
	credential := store.Token{
		Value: protocol.NewID(protocol.TenantToken), Kind: store.TenantToken, AppID: app.AppID,
		ExpiresAt: h.state.Now().Add(seconds(h.ttls.Tenant)), Scope: scope,
	}
	if err := h.state.Mutate(r.Context(), func() error { return h.state.PutToken(credential) }); err != nil {
		if !middleware.WriteResetConflict(w, err) {
			writeOAuthError(w, oauthAppNotFound)
		}
		return
	}
	_ = protocol.WriteResult(w, http.StatusOK, protocol.Success, struct {
		Code        protocol.Code `json:"code"`
		AccessToken string        `json:"access_token"`
		ExpiresIn   int64         `json:"expires_in"`
		TokenType   string        `json:"token_type"`
		Scope       string        `json:"scope,omitempty"`
	}{
		Code: protocol.Success, AccessToken: credential.Value, ExpiresIn: h.ttls.Tenant,
		TokenType: "Bearer", Scope: credential.Scope,
	})
}

func (h handler) deviceToken(w http.ResponseWriter, r *http.Request, fields map[string]string) {
	app, ok := h.checkAccountsClient(w, r, fields["client_id"], fields["client_secret"])
	if !ok {
		return
	}
	tokens, err := store.Commit(r.Context(), h.state, func() ([2]store.Token, error) {
		return h.state.FinishDeviceAuthorization(fields["device_code"], app.AppID, func(grant store.Token, now time.Time) [2]store.Token {
			return h.issueUserTokens(grant, grant.Scope, now, false)
		})
	})
	if err != nil {
		writeDeviceError(w, err)
		return
	}
	h.writeOAuthTokens(w, tokens)
}

func writeDeviceError(w http.ResponseWriter, err error) {
	if middleware.WriteResetConflict(w, err) {
		return
	}
	switch {
	case errors.Is(err, store.ErrDevicePending), errors.Is(err, store.ErrDeviceDenied),
		errors.Is(err, store.ErrDeviceExpired), errors.Is(err, store.ErrDeviceSlowDown):
		writeAccountsError(w, http.StatusBadRequest, err.Error(), "")
	case errors.Is(err, store.ErrAlreadyExists):
		writeAccountsError(w, http.StatusInternalServerError, "server_error", "")
	default:
		writeAccountsError(w, http.StatusBadRequest, "invalid_grant", "The device code is invalid.")
	}
}

func writeAccountsError(w http.ResponseWriter, status int, kind, description string) {
	_ = protocol.WriteJSON(w, status, struct {
		Kind        string `json:"error"`
		Description string `json:"error_description,omitempty"`
	}{kind, description})
}
