package auth

import (
	"errors"
	"net/http"

	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

type oauthCode protocol.Code

const (
	oauthInvalidRequest   oauthCode = 20001
	oauthInvalidClient    oauthCode = 20002
	oauthUserMissing      oauthCode = 20008
	oauthWrongApp         oauthCode = 20024
	oauthInvalidRefresh   oauthCode = 20026
	oauthUnknownApp       oauthCode = 20028
	oauthRedirect         oauthCode = 20029
	oauthGrantType        oauthCode = 20036
	oauthExpiredRefresh   oauthCode = 20037
	oauthRefreshNotFound  oauthCode = 20038
	oauthAppNotFound      oauthCode = 20048
	oauthPKCE             oauthCode = 20049
	oauthServerError      oauthCode = 20050
	oauthMalformed        oauthCode = 20063
	oauthUsedCode         oauthCode = 20065
	oauthDuplicateScope   oauthCode = 20067
	oauthInvalidScope     oauthCode = 20068
	oauthRedirectMismatch oauthCode = 20071
	oauthUsedRefresh      oauthCode = 20073
)

type errorInfo struct {
	kind, description string
}

var oauthErrors = map[oauthCode]errorInfo{
	oauthInvalidRequest:                  {"invalid_request", "The request is missing a required parameter."},
	oauthInvalidClient:                   {"invalid_client", "The client secret is invalid."},
	oauthCode(protocol.InvalidOAuthCode): {"invalid_grant", "The authorization code is invalid."},
	oauthCode(protocol.ExpiredOAuthCode): {"invalid_grant", "The authorization code has expired."},
	oauthUserMissing:                     {"invalid_grant", "The user does not exist."},
	oauthWrongApp:                        {"invalid_grant", "The credential does not belong to this app."},
	oauthInvalidRefresh:                  {"invalid_grant", "The refresh token is invalid."},
	oauthUnknownApp:                      {"invalid_client", "Invalid app id"},
	oauthRedirect:                        {"invalid_request", "Invalid redirect uri"},
	oauthGrantType:                       {"unsupported_grant_type", "The specified grant_type is not supported."},
	oauthExpiredRefresh:                  {"invalid_grant", "The refresh token has expired."},
	oauthRefreshNotFound:                 {"invalid_grant", "The refresh token was not found or has already been used."},
	oauthAppNotFound:                     {"invalid_client", "The specified app does not exist."},
	oauthPKCE:                            {"invalid_grant", "PKCE code challenge failed."},
	oauthServerError:                     {"server_error", "The server could not complete the request."},
	oauthMalformed:                       {"invalid_request", "The request is malformed."},
	oauthUsedCode:                        {"invalid_grant", "The authorization code has already been used."},
	oauthDuplicateScope:                  {"invalid_scope", "The requested scopes contain duplicates."},
	oauthInvalidScope:                    {"invalid_scope", "The requested scopes exceed the authorized scopes."},
	oauthRedirectMismatch:                {"invalid_grant", "The redirect URI differs from the authorization request."},
	oauthUsedRefresh:                     {"invalid_grant", "The refresh token has already been used."},
}

func (c oauthCode) Error() string { return oauthErrors[c].description }

func writeLegacyError(w http.ResponseWriter, code oauthCode) {
	status := http.StatusOK
	if code == oauthServerError {
		status = http.StatusInternalServerError
	}
	_ = protocol.WriteError(w, protocol.APIError{Code: protocol.Code(code), Message: code.Error(), Status: status})
}

func writeOAuthError(w http.ResponseWriter, code oauthCode) {
	status := http.StatusBadRequest
	if code == oauthServerError {
		status = http.StatusInternalServerError
	}
	info := oauthErrors[code]
	body := struct {
		Code             protocol.Code `json:"code"`
		Error            string        `json:"error"`
		ErrorDescription string        `json:"error_description"`
	}{protocol.Code(code), info.kind, info.description}
	_ = protocol.WriteResult(w, status, body.Code, body)
}

func exchangeCode(err error, refresh, v2 bool) oauthCode {
	var code oauthCode
	switch {
	case errors.As(err, &code):
		switch {
		case !v2 && code == oauthPKCE:
			return oauthCode(protocol.InvalidOAuthCode)
		case !v2 && code == oauthRedirectMismatch:
			return oauthRedirect
		default:
			return code
		}
	case errors.Is(err, store.ErrTokenOwner):
		return oauthUserMissing
	case errors.Is(err, store.ErrTokenBinding):
		return oauthWrongApp
	case errors.Is(err, store.ErrTokenExpired):
		if refresh {
			return oauthExpiredRefresh
		}
		return oauthCode(protocol.ExpiredOAuthCode)
	case errors.Is(err, store.ErrTokenRevoked):
		if v2 {
			if refresh {
				return oauthUsedRefresh
			}
			return oauthUsedCode
		}
		if refresh {
			return oauthRefreshNotFound
		}
		return oauthCode(protocol.InvalidOAuthCode)
	case errors.Is(err, store.ErrNotFound):
		if refresh {
			return oauthInvalidRefresh
		}
		return oauthCode(protocol.InvalidOAuthCode)
	default:
		return oauthServerError
	}
}
