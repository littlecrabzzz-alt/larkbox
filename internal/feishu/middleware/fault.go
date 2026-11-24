package middleware

import (
	"bufio"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"

	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/sandbox/fault"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

// FaultHook decides whether to intercept an Open API request. BeforeRequest
// returns true if it has written a response and the downstream handler can stop.
type FaultHook interface {
	BeforeRequest(http.ResponseWriter, *http.Request) bool
}

// Fault activates the supplied fault hook. A nil hook leaves next as-is.
func Fault(hook FaultHook, next http.Handler) http.Handler {
	if hook == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hook.BeforeRequest(w, r) {
			next.ServeHTTP(w, r)
		}
	})
}

// SandboxFaults applies runtime fault rules to the Open API route group.
func SandboxFaults(engine *fault.Engine, state *store.Store, routes *http.ServeMux, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		input := fault.Match{Channel: "inbound", Method: r.Method, PathPrefix: r.URL.EscapedPath()}
		headers := r.Header.Values("Authorization")
		handler, _ := routes.Handler(r)
		var auth Auth
		if bearer, ok := handler.(*bearerHandler); ok {
			auth, _ = authenticate(bearer.state, bearer.required, headers)
		}
		input.AppID = auth.AppID
		if value, ok := bearerToken(headers); input.AppID == "" && ok {
			token, _ := state.CheckToken(value)
			input.AppID = token.AppID
			SetTraceApp(r, input.AppID)
		}
		if input.AppID == "" && engine.NeedsApp(input) {
			input.AppID = declaredApp(r)
			if _, ok := state.Apps().Get(input.AppID); !ok {
				input.AppID = ""
			}
		}
		effect, err := store.Commit(r.Context(), state, func() (fault.Effect, error) {
			return engine.Match(input, auth.Kind != ""), nil
		})
		if WriteResetConflict(w, err) {
			return
		}
		if effect.Kind != "" && input.AppID != "" {
			SetTraceApp(r, input.AppID)
		}
		switch effect.Kind {
		case fault.Latency:
			if effect.Wait(r.Context()) != nil {
				_ = protocol.WriteError(w, protocol.APIError{Status: http.StatusServiceUnavailable, Code: 99991401, Message: "request canceled"})
				return
			}
		case fault.TokenExpiry:
			_ = protocol.WriteError(w, expiryError(auth.Kind))
			return
		case fault.RateLimit:
			w.Header().Set("Retry-After", strconv.Itoa(effect.RetryAfter))
			_ = protocol.WriteError(w, protocol.NewError(protocol.RateLimited, nil))
			return
		case fault.HTTPError:
			_ = protocol.WriteError(w, protocol.APIError{Status: effect.StatusCode, Code: effect.ErrorCode, Message: effect.ErrorMsg})
			return
		}
		next.ServeHTTP(w, r)
	})
}

type bufferedRequestBody struct {
	io.Reader
	io.Closer
}

// Credential endpoints name their app in the request instead of using Bearer
// auth. Peek only when an app-scoped rule could match, preserving the reader.
func declaredApp(r *http.Request) string {
	var field string
	switch {
	case r.Method == http.MethodGet && (r.URL.Path == "/open-apis/authen/v1/authorize" || r.URL.Path == "/open-apis/authen/v1/index"):
		query := r.URL.Query()
		appID, clientID := query.Get("app_id"), query.Get("client_id")
		if appID != "" {
			return appID
		}
		return clientID
	case r.Method == http.MethodPost && (r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" || r.URL.Path == "/open-apis/auth/v3/app_access_token/internal"):
		field = "app_id"
	case r.Method == http.MethodPost && r.URL.Path == "/open-apis/authen/v2/oauth/token":
		field = "client_id"
	default:
		return ""
	}
	const maxCredentialsBody = 16 * 1024
	reader := bufio.NewReaderSize(r.Body, maxCredentialsBody+1)
	r.Body = bufferedRequestBody{Reader: reader, Closer: r.Body}
	body, err := reader.Peek(maxCredentialsBody + 1)
	if err != io.EOF {
		return ""
	}
	if mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); field == "client_id" && mediaType == "application/x-www-form-urlencoded" {
		form, err := url.ParseQuery(string(body))
		if err != nil || len(form[field]) != 1 {
			return ""
		}
		return form.Get(field)
	}
	var fields map[string]string
	if json.Unmarshal(body, &fields) != nil {
		return ""
	}
	return fields[field]
}
