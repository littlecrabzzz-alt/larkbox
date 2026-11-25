package bot

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/middleware"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

func patchMenu(w http.ResponseWriter, r *http.Request, state *store.Store) {
	identity, _ := middleware.AuthFromContext(r.Context())
	appID := r.PathValue("app_id")
	if identity.AppID != appID {
		_ = protocol.WriteError(w, protocol.APIError{Code: 210016, Status: http.StatusForbidden, Message: "Lack of permission to update other App."})
		return
	}
	var request struct {
		Bot *struct {
			Enable   *bool                `json:"enable"`
			Enabled  *bool                `json:"bot_menu_enable"`
			Items    []config.BotMenuItem `json:"bot_menus"`
			Strategy *int                 `json:"bot_menu_display_strategy"`
		} `json:"bot"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || !errors.Is(decoder.Decode(new(json.RawMessage)), io.EOF) ||
		request.Bot == nil || request.Bot.Enable == nil || !*request.Bot.Enable {
		_ = protocol.WriteError(w, protocol.APIError{Code: 210011, Status: http.StatusBadRequest, Message: "Expected one bot object with enable set to true and optional menu settings."})
		return
	}
	input := request.Bot
	err := state.Mutate(r.Context(), func() error {
		return state.ChangeBotMenu(appID, func(menu *config.BotMenu) error {
			if input.Enabled == nil {
				return nil
			}
			menu.Enabled = *input.Enabled
			if input.Items != nil {
				menu.Items = input.Items
			}
			if input.Strategy != nil {
				menu.Strategy = *input.Strategy
			}
			return nil
		})
	})
	switch {
	case middleware.WriteResetConflict(w, err):
	case errors.Is(err, store.ErrNotFound):
		_ = protocol.WriteError(w, protocol.APIError{Code: 210012, Status: http.StatusNotFound, Message: "The specified application or its bot does not exist."})
	case err != nil:
		_ = protocol.WriteError(w, protocol.APIError{Code: 210011, Status: http.StatusBadRequest, Message: err.Error()})
	default:
		_ = protocol.WriteJSON(w, http.StatusOK, protocol.Envelope{Code: protocol.Success, Msg: "success", Data: struct{}{}})
	}
}
