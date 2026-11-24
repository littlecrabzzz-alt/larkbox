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
}

type authKey struct{}

var bearerValue = regexp.MustCompile(`^[A-Za-z0-9._~+/-]+=*$`)

// AuthFromContext returns the authenticated identity, if present.
func AuthFromContext(ctx context.Context) (Auth, bool) {
	auth, ok := ctx.Value(authKey{}).(Auth)
	return auth, ok
}

// Bearer authenticates a route with the specified Feishu token kind. In
// permissive mode, unauthenticated requests proceed without an auth identity.
func Bearer(state *store.Store, required RequiredToken, strict bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth, failure := authenticate(state, required, r.Header.Values("Authorization"))
			switch {
			case failure.Code == protocol.Success:
				r = r.WithContext(context.WithValue(r.Context(), authKey{}, auth))
			case strict:
				_ = protocol.WriteError(w, failure)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func authenticate(state *store.Store, required RequiredToken, headers []string) (Auth, protocol.APIError) {
	value := bearerToken(headers)
	if value == "" {
		return Auth{}, protocol.NewError(protocol.InvalidToken, nil)
	}
	token, err := state.CheckToken(value)
	if errors.Is(err, store.ErrNotFound) {
		kind := credentialKind(value)
		return Auth{}, credentialError(kind, !accepts(required, kind))
	}
	if err != nil {
		failure := credentialError(token.Kind, false)
		if token.Kind == store.UserToken {
			failure.Message = "token invalid"
		}
		return Auth{}, failure
	}
	if !accepts(required, token.Kind) {
		return Auth{}, credentialError(token.Kind, true)
	}
	return Auth{AppID: token.AppID, UserOpenID: token.UserOpenID, Kind: token.Kind}, protocol.APIError{}
}

func bearerToken(headers []string) string {
	if len(headers) != 1 {
		return ""
	}
	scheme, value, found := strings.Cut(headers[0], " ")
	value = strings.TrimLeft(value, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || !bearerValue.MatchString(value) {
		return ""
	}
	return value
}

func credentialKind(value string) store.TokenKind {
	switch {
	case strings.HasPrefix(value, string(protocol.TenantToken)):
		return store.TenantToken
	case strings.HasPrefix(value, string(protocol.AppToken)):
		return store.AppToken
	case strings.HasPrefix(value, string(protocol.UserToken)):
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

func credentialError(kind store.TokenKind, unsupported bool) protocol.APIError {
	switch kind {
	case store.TenantToken:
		return protocol.NewError(protocol.ExpiredTenantToken, nil)
	case store.AppToken:
		return protocol.NewError(protocol.InvalidAppToken, nil)
	case store.UserToken:
		failure := protocol.NewError(protocol.InvalidUserToken, nil)
		if unsupported {
			failure.Message = "user access token not support"
		}
		return failure
	default:
		return protocol.NewError(protocol.InvalidToken, nil)
	}
}
