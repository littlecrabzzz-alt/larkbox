package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/middleware"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

const (
	authorizationCode = "authorization_code"
	refreshGrant      = "refresh_token"
)

var pkceValue = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)
var s256Challenge = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

func (h handler) authorize(w http.ResponseWriter, r *http.Request) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeAPIError(w, protocol.InvalidParameter)
		return
	}
	appID := query.Get("app_id")
	clientID := query.Get("client_id")
	if appID == "" {
		appID = clientID
	}
	if clientID != "" && clientID != appID {
		writeAPIError(w, protocol.InvalidParameter)
		return
	}
	if _, ok := h.state.Apps().Get(appID); !ok {
		writeLegacyError(w, oauthUnknownApp)
		return
	}
	middleware.SetTraceApp(r, appID)

	redirectURI := query.Get("redirect_uri")
	redirect, err := url.Parse(redirectURI)
	if err != nil || (redirect.Scheme != "http" && redirect.Scheme != "https") ||
		redirect.Hostname() == "" || redirect.User != nil {
		writeLegacyError(w, oauthRedirect)
		return
	}
	callback, err := url.ParseQuery(redirect.RawQuery)
	if err != nil {
		writeLegacyError(w, oauthRedirect)
		return
	}
	if responseType := query.Get("response_type"); responseType != "" && responseType != "code" {
		writeLegacyError(w, oauthInvalidRequest)
		return
	}
	challenge, method := query.Get("code_challenge"), query.Get("code_challenge_method")
	if challenge != "" && method == "" {
		method = "plain"
	}
	switch {
	case challenge == "" && method == "", method == "plain" && pkceValue.MatchString(challenge),
		method == "S256" && s256Challenge.MatchString(challenge):
	default:
		writeAPIError(w, protocol.InvalidParameter)
		return
	}
	user, ok := h.state.OAuthUser(query.Get("open_id"), query.Get("user_id"))
	if !ok {
		writeLegacyError(w, oauthUserMissing)
		return
	}

	grant := store.Token{
		Value: protocol.NewID(protocol.OAuthCode), Kind: store.OAuthCode, AppID: appID, UserOpenID: user.OpenID,
		ExpiresAt: h.state.Now().Add(seconds(h.ttls.Code)), Scope: query.Get("scope"),
		RedirectURI: redirectURI, CodeChallenge: challenge, CodeChallengeMethod: method,
	}
	if err := h.state.PutToken(grant); err != nil {
		writeLegacyError(w, oauthUserMissing)
		return
	}

	callback.Set("code", grant.Value)
	if query.Has("state") {
		callback.Set("state", query.Get("state"))
	}
	redirect.RawQuery = callback.Encode()
	w.Header().Set("Location", redirect.String())
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusFound)
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
}

type oidcResponse struct {
	tokenResponse
	RefreshExpiresIn int64 `json:"refresh_expires_in"`
}

type oauthResponse struct {
	Code protocol.Code `json:"code"`
	tokenResponse
	RefreshExpiresIn int64 `json:"refresh_token_expires_in"`
}

func (h handler) oidcToken(refresh bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity, _ := middleware.AuthFromContext(r.Context())
		wantGrant, tokenField := authorizationCode, "code"
		allowed := []string{"grant_type", "code", "code_verifier", "redirect_uri"}
		if refresh {
			wantGrant, tokenField = refreshGrant, "refresh_token"
			allowed = []string{"grant_type", "refresh_token"}
		}
		fields, ok := readJSON(w, r, allowed...)
		switch {
		case !ok || fields["grant_type"] == "":
			writeLegacyError(w, oauthInvalidRequest)
			return
		case fields["grant_type"] != wantGrant:
			writeLegacyError(w, oauthGrantType)
			return
		case fields[tokenField] == "":
			writeLegacyError(w, oauthInvalidRequest)
			return
		}
		tokens, err := h.exchange(identity.AppID, fields, refresh, false)
		if err != nil {
			writeLegacyError(w, exchangeCode(err, refresh, false))
			return
		}
		noStore(w)
		data := oidcResponse{h.tokenData(tokens), h.ttls.Refresh}
		_ = protocol.WriteJSON(w, http.StatusOK, protocol.Envelope{Code: protocol.Success, Msg: "success", Data: data})
	}
}

func readOAuthFields(w http.ResponseWriter, r *http.Request) (map[string]string, bool) {
	allowed := []string{"grant_type", "client_id", "client_secret", "code", "refresh_token", "redirect_uri", "code_verifier", "scope"}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch {
	case mediaType == "application/json" || r.Header.Get("Content-Type") == "":
		return readJSON(w, r, allowed...)
	case err != nil || mediaType != "application/x-www-form-urlencoded":
		return nil, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		return nil, false
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, false
	}
	fields := make(map[string]string, len(form))
	for key, values := range form {
		if !slices.Contains(allowed, key) || len(values) != 1 {
			return nil, false
		}
		fields[key] = values[0]
	}
	return fields, true
}

func (h handler) oauthToken(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	fields, ok := readOAuthFields(w, r)
	if !ok {
		writeOAuthError(w, oauthMalformed)
		return
	}
	appID, secret := fields["client_id"], fields["client_secret"]
	grantType := fields["grant_type"]
	switch {
	case appID == "" || secret == "" || grantType == "":
		writeOAuthError(w, oauthInvalidRequest)
		return
	case grantType != authorizationCode && grantType != refreshGrant:
		writeOAuthError(w, oauthGrantType)
		return
	case grantType == authorizationCode && fields["code"] == "", grantType == refreshGrant && fields["refresh_token"] == "":
		writeOAuthError(w, oauthMalformed)
		return
	}
	app, found := h.state.Apps().Get(appID)
	if !found {
		writeOAuthError(w, oauthAppNotFound)
		return
	}
	if !matchesSecret(app, secret) {
		writeOAuthError(w, oauthInvalidClient)
		return
	}

	middleware.SetTraceApp(r, appID)
	refresh := grantType == refreshGrant
	tokens, err := h.exchange(appID, fields, refresh, true)
	if err != nil {
		writeOAuthError(w, exchangeCode(err, refresh, true))
		return
	}
	body := oauthResponse{protocol.Success, h.tokenData(tokens), h.ttls.Refresh}
	_ = protocol.WriteResult(w, http.StatusOK, body.Code, body)
}

func (h handler) exchange(appID string, fields map[string]string, refresh, v2 bool) ([2]store.Token, error) {
	value, kind := fields["code"], store.OAuthCode
	if refresh {
		value, kind = fields["refresh_token"], store.RefreshToken
	}
	return h.state.ExchangeOAuthGrant(value, appID, kind, func(grant store.Token, now time.Time) ([2]store.Token, error) {
		if !refresh {
			redirectURI := fields["redirect_uri"]
			if (v2 || redirectURI != "") && redirectURI != grant.RedirectURI {
				return [2]store.Token{}, oauthRedirectMismatch
			}
			if !verifyPKCE(grant, fields["code_verifier"]) {
				return [2]store.Token{}, oauthPKCE
			}
		}

		scope, err := tokenScope(grant.Scope, fields["scope"])
		if err != nil {
			return [2]store.Token{}, err
		}
		return [2]store.Token{
			{Kind: store.UserToken, Value: protocol.NewID(protocol.UserToken), AppID: grant.AppID, UserOpenID: grant.UserOpenID,
				Scope: scope, ExpiresAt: now.Add(seconds(h.ttls.User))},
			{Kind: store.RefreshToken, Value: protocol.NewID(protocol.RefreshToken), AppID: grant.AppID, UserOpenID: grant.UserOpenID,
				Scope: grant.Scope, ExpiresAt: now.Add(seconds(h.ttls.Refresh))},
		}, nil
	})
}

func verifyPKCE(grant store.Token, verifier string) bool {
	if grant.CodeChallenge == "" {
		return true
	}
	if !pkceValue.MatchString(verifier) {
		return false
	}
	switch grant.CodeChallengeMethod {
	case "S256":
		hash := sha256.Sum256([]byte(verifier))
		verifier = base64.RawURLEncoding.EncodeToString(hash[:])
	case "plain":
	default:
		return false
	}
	return subtle.ConstantTimeCompare([]byte(grant.CodeChallenge), []byte(verifier)) == 1
}

func tokenScope(granted, requested string) (string, error) {
	if requested == "" {
		return granted, nil
	}
	allowed := strings.Fields(granted)
	seen := make(map[string]bool)
	for _, scope := range strings.Fields(requested) {
		if seen[scope] {
			return "", oauthDuplicateScope
		}
		if !slices.Contains(allowed, scope) {
			return "", oauthInvalidScope
		}
		seen[scope] = true
	}
	return requested, nil
}

func (h handler) tokenData(tokens [2]store.Token) tokenResponse {
	return tokenResponse{
		AccessToken: tokens[0].Value, RefreshToken: tokens[1].Value,
		TokenType: "Bearer", ExpiresIn: h.ttls.User, Scope: tokens[0].Scope,
	}
}

func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
}

func (h handler) userInfo(w http.ResponseWriter, r *http.Request) {
	identity, _ := middleware.AuthFromContext(r.Context())
	user, ok := h.state.Users().Get(identity.UserOpenID)
	if !ok {
		writeLegacyError(w, oauthUserMissing)
		return
	}
	data := map[string]string{
		"name": user.Name, "en_name": user.EnName, "avatar_url": user.Avatar,
		"avatar_thumb": user.AvatarThumb, "avatar_middle": user.AvatarMiddle, "avatar_big": user.AvatarBig,
		"open_id": user.OpenID, "union_id": user.UnionID, "email": user.Email,
		"enterprise_email": user.EnterpriseEmail, "user_id": user.UserID, "mobile": user.Mobile,
		"tenant_key": user.TenantKey,
	}
	_ = protocol.WriteJSON(w, http.StatusOK, protocol.Envelope{Code: protocol.Success, Msg: "success", Data: data})
}
