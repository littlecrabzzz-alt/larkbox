// Package bot serves the Feishu bot profile and application menu APIs.
package bot

import (
	"net/http"

	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/middleware"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

// RegisterRoutes registers the authenticated bot profile and menu endpoints.
func RegisterRoutes(mux *http.ServeMux, state *store.Store) {
	auth := middleware.Bearer(state, middleware.Tenant, true)
	mux.Handle("GET /open-apis/bot/v3/info", auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, _ := middleware.AuthFromContext(r.Context())
		app, ok := state.Apps().Get(identity.AppID)
		if !ok || app.BotOpenID == "" {
			_ = protocol.WriteError(w, protocol.NewError(protocol.AppUnauthorized, nil))
			return
		}
		_ = protocol.WriteResult(w, http.StatusOK, protocol.Success, struct {
			Code protocol.Code `json:"code"`
			Msg  string        `json:"msg"`
			Bot  profile       `json:"bot"`
		}{Msg: "ok", Bot: profile{2, app.BotName, app.BotAvatarURL, []string{}, app.BotOpenID}})
	})))
	mux.Handle("PATCH /open-apis/application/v7/applications/{app_id}/ability", auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		patchMenu(w, r, state)
	})))
}

type profile struct {
	ActivateStatus int      `json:"activate_status"`
	AppName        string   `json:"app_name"`
	AvatarURL      string   `json:"avatar_url"`
	IPWhiteList    []string `json:"ip_white_list"`
	OpenID         string   `json:"open_id"`
}
