// Package middleware provides Feishu Open API HTTP middleware.
package middleware

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

// RequiredToken describes which Bearer token kinds a route accepts.
type RequiredToken string

// Supported Bearer token requirements.
const (
	Tenant       RequiredToken = "tenant"         // Require a tenant token.
	App          RequiredToken = "app"            // Require an app token.
	User         RequiredToken = "user"           // Require a user token.
	TenantOrUser RequiredToken = "tenant_or_user" // Accept a tenant or user token.
)

// Auth identifies the app and optional user authenticated by a Bearer token.
type Auth struct {
	AppID      string
	UserOpenID string
	Kind       store.TokenKind
	Scope      string
}

type authKey struct{}
type traceAppKey struct{}

var bearerValue = regexp.MustCompile(`^[A-Za-z0-9._~+/-]+=*$`)

// AuthFromContext returns the authenticated identity, if present.
func AuthFromContext(ctx context.Context) (Auth, bool) {
	auth, ok := ctx.Value(authKey{}).(Auth)
	return auth, ok
}

// Bearer authenticates a route with the specified Feishu token kind. In
// permissive mode, unauthenticated requests proceed without an auth identity.
// Wrappers supplied here let fault selection use the policy before route entry.
func Bearer(state *store.Store, required RequiredToken, strict bool, wrappers ...func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		var handler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth, failure := authenticate(state, required, r.Header.Values("Authorization"))
			switch {
			case failure.Code == protocol.Success:
				ctx := context.WithValue(r.Context(), authKey{}, auth)
				SetTraceApp(r, auth.AppID)
				r = r.WithContext(ctx)
			case strict:
				_ = protocol.WriteError(w, failure)
				return
			}
			next.ServeHTTP(w, r)
		})
		for _, wrap := range wrappers {
			handler = wrap(handler)
		}
		return WithBearerPolicy(handler, required)
	}
}

// WithBearerPolicy declares the accepted Bearer credentials for fault selection.
// Use it on the final wrapped handler and keep Bearer authentication inside it.
func WithBearerPolicy(handler http.Handler, required RequiredToken) http.Handler {
	return bearerRoute{Handler: handler, required: required}
}

type bearerRoute struct {
	http.Handler
	required RequiredToken
}

func (route bearerRoute) bearerPolicy() RequiredToken { return route.required }

func authenticate(state *store.Store, required RequiredToken, headers []string) (Auth, protocol.APIError) {
	value, bearer := bearerToken(headers)
	if !bearer {
		return Auth{}, protocol.NewError(protocol.InvalidToken, nil)
	}
	prefixKind := bearerPrefixKind(value)
	if prefixKind == "" {
		return Auth{}, protocol.NewError(protocol.InvalidTokenFormat, nil)
	}
	token, err := state.CheckToken(value)
	// The t- prefix is shared by tenant and app tokens. Only an unknown
	// credential needs the prefix as a fallback for its error code.
	switch {
	case errors.Is(err, store.ErrNotFound):
		return Auth{}, credentialError(prefixKind, invalidCredential)
	case errors.Is(err, store.ErrTokenExpired):
		return Auth{}, expiryError(token.Kind)
	case errors.Is(err, store.ErrTokenRevoked):
		return Auth{}, credentialError(token.Kind, inactiveCredential)
	case err != nil:
		return Auth{}, credentialError(token.Kind, invalidCredential)
	case !accepts(required, token.Kind):
		return Auth{}, credentialError(token.Kind, unsupportedCredential)
	}
	return Auth{AppID: token.AppID, UserOpenID: token.UserOpenID, Kind: token.Kind, Scope: token.Scope}, protocol.APIError{}
}

func bearerToken(headers []string) (string, bool) {
	if len(headers) != 1 {
		return "", false
	}
	scheme, value, _ := strings.Cut(headers[0], " ")
	return strings.TrimLeft(value, " "), strings.EqualFold(scheme, "Bearer")
}

func bearerPrefixKind(value string) store.TokenKind {
	if len(value) <= 2 || !bearerValue.MatchString(value[2:]) {
		return ""
	}
	switch protocol.IDPrefix(value[:2]) {
	case protocol.TenantToken:
		return store.TenantToken
	case protocol.AppToken:
		return store.AppToken
	case protocol.UserToken:
		return store.UserToken
	default:
		return ""
	}
}

func accepts(required RequiredToken, kind store.TokenKind) bool {
	switch kind {
	case store.TenantToken:
		return required == Tenant || required == TenantOrUser
	case store.UserToken:
		return required == User || required == TenantOrUser
	case store.AppToken:
		return required == App
	default:
		return false
	}
}

type credentialFailure int

const (
	invalidCredential credentialFailure = iota
	inactiveCredential
	unsupportedCredential
)

func credentialError(kind store.TokenKind, reason credentialFailure) protocol.APIError {
	switch kind {
	case store.TenantToken:
		if reason == invalidCredential {
			return protocol.NewError(protocol.InvalidTenantToken, nil)
		}
		return protocol.NewError(protocol.ExpiredTenantToken, nil)
	case store.AppToken:
		return protocol.NewError(protocol.InvalidAppToken, nil)
	case store.UserToken:
		failure := protocol.NewError(protocol.InvalidUserToken, nil)
		switch reason {
		case inactiveCredential:
			failure.Message = "token invalid"
		case unsupportedCredential:
			failure.Message = "user access token not support"
		}
		return failure
	default:
		return protocol.NewError(protocol.InvalidTokenFormat, nil)
	}
}

func expiryError(kind store.TokenKind) protocol.APIError {
	if kind == store.AppToken {
		return protocol.NewError(protocol.ExpiredTenantToken, nil)
	}
	return credentialError(kind, inactiveCredential)
}

// WriteResetConflict reports that a write lost its reset generation.
func WriteResetConflict(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, store.ErrReset) {
		return false
	}
	_ = protocol.WriteError(w, protocol.APIError{Code: 99991401, Status: http.StatusConflict, Message: store.ErrReset.Error()})
	return true
}
