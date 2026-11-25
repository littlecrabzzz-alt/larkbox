package auth

import (
	"errors"
	"html/template"
	"net/http"
	"slices"

	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/middleware"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

var deviceTemplate = template.Must(template.New("verify-device").Parse(`<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Larkbox device confirmation</title></head>
<body><main><h1>{{.Title}}</h1>
{{if .Message}}<p role="status">{{.Message}}</p>{{end}}
{{if .Approve}}
{{if .AppID}}<p>{{.AppName}} <code>{{.AppID}}</code></p>{{end}}
<p>Check that the code shown by your CLI is <strong>{{.UserCode}}</strong>.</p>
{{if .Scope}}<p>Requested permissions</p><p><code>{{.Scope}}</code></p>{{end}}
<form method="post" action="/page/cli">
<input type="hidden" name="user_code" value="{{.UserCode}}">
<label>Simulated user <select name="open_id">{{range .Users}}<option value="{{.OpenID}}">{{.Name}}</option>{{end}}</select></label>
<button name="decision" value="approve" {{if not .Users}}disabled{{end}}>Approve</button>
<button name="decision" value="deny">Deny</button>
</form>
{{else if .EnterCode}}
<form method="get" action="/page/cli">
<label>Device user code <input name="user_code" required autofocus></label>
<button>Continue</button>
</form>
{{end}}
</main></body></html>`))

type devicePageData struct {
	Title, Scope, UserCode, Message string
	AppID, AppName                  string
	Approve, EnterCode              bool
	Users                           []store.User
}

func writeDevicePage(w http.ResponseWriter, status int, data devicePageData) {
	noStore(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	_ = deviceTemplate.Execute(w, data)
}

func (h handler) devicePage(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("user_code")
	if code == "" {
		writeDevicePage(w, http.StatusOK, devicePageData{Title: "Larkbox device confirmation", EnterCode: true})
		return
	}
	flow, ok := h.state.DeviceByUserCode(code)
	if !ok {
		writeDevicePage(w, http.StatusNotFound, devicePageData{
			Title: "Device code unavailable", Message: "Start a new request from the CLI.", EnterCode: true,
		})
		return
	}
	data := devicePageData{Title: "Authorize a sandbox app", UserCode: flow.UserCode, Scope: flow.Scope}
	if flow.Kind == store.DeviceRegistration {
		data.Title = "Register a sandbox app"
	} else if app, ok := h.state.Apps().Get(flow.AppID); ok {
		data.AppID, data.AppName = app.AppID, app.BotName
	}
	switch {
	case flow.Denied:
		data.Message = "Request denied."
	case flow.Consumed || flow.OpenID != "":
		data.Message = "Request approved. Continue in the CLI."
	default:
		data.Approve = true
		data.Users = slices.DeleteFunc(h.state.Users().List(), func(user store.User) bool { return user.Inactive })
	}
	writeDevicePage(w, http.StatusOK, data)
}

func (h handler) deviceDecisionPage(w http.ResponseWriter, r *http.Request) {
	fields, ok := readOAuthFields(w, r, "user_code", "open_id", "decision")
	decision := fields["decision"]
	if !ok || fields["user_code"] == "" || (decision != "approve" && decision != "deny") {
		writeDevicePage(w, http.StatusBadRequest, devicePageData{
			Title: "Device confirmation unavailable", Message: "A code and decision are required.", EnterCode: true,
		})
		return
	}
	err := h.state.Mutate(r.Context(), func() error {
		return h.state.DecideDeviceFlow(fields["user_code"], fields["open_id"], decision == "approve")
	})
	switch {
	case middleware.WriteResetConflict(w, err):
		return
	case errors.Is(err, store.ErrAlreadyExists):
		writeDevicePage(w, http.StatusConflict, devicePageData{Title: "This request has already been decided"})
	case errors.Is(err, store.ErrDeviceExpired):
		writeDevicePage(w, http.StatusNotFound, devicePageData{Title: "Device code unavailable", EnterCode: true})
	case err != nil:
		writeDevicePage(w, http.StatusBadRequest, devicePageData{Title: "Choose an available simulated user", EnterCode: true})
	case decision == "deny":
		writeDevicePage(w, http.StatusOK, devicePageData{Title: "Request denied"})
	default:
		writeDevicePage(w, http.StatusOK, devicePageData{Title: "Request approved", Message: "Continue in the CLI."})
	}
}
