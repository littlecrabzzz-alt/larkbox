package sandbox

import (
	"net/http"
	"slices"
	"strconv"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/event"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

const (
	chatEntered = "im.chat.access_event.bot_p2p_chat_entered_v1"
	menuClicked = "application.bot.menu_v6"
)

func (c *control) openBotChat(w http.ResponseWriter, r *http.Request) {
	var input struct {
		AppID  string `json:"app_id"`
		OpenID string `json:"operator_open_id"`
	}
	if !read(w, r, &input, false) {
		return
	}
	if input.AppID == "" || input.OpenID == "" {
		fail(w, http.StatusBadRequest, "app_id and operator_open_id are required")
		return
	}
	c.mu.RLock()
	ctx := c.events.Scope(r.Context())
	chat, user, created, latest, err := c.state.OpenBotConversation(input.AppID, input.OpenID)
	c.mu.RUnlock()
	if err != nil {
		stateError(w, err)
		return
	}
	var payload any
	kind := chatEntered
	if created {
		kind = event.FirstBotConversation
		payload = event.FirstBotConversationPayload(chat.ChatID, user, user)
	} else {
		fields := data{"chat_id": chat.ChatID, "operator_id": eventUser(user)}
		if latest.ID != "" {
			fields["last_message_id"] = latest.ID
			fields["last_message_create_time"] = strconv.FormatInt(latest.CreatedAt.UnixMilli(), 10)
		}
		payload = fields
	}
	result, err := c.events.DispatchSubscribed(ctx, input.AppID, kind, payload)
	receipt := eventReceipt(result, err)
	receipt["chat_id"], receipt["created"], receipt["event_type"] = chat.ChatID, created, kind
	reply(w, http.StatusOK, receipt)
}

func eventUser(user store.User) data {
	return data{"open_id": user.OpenID, "user_id": user.UserID, "union_id": user.UnionID}
}

func (c *control) getBotMenu(w http.ResponseWriter, r *http.Request) {
	app, ok := c.state.Apps().Get(r.PathValue("app_id"))
	if !ok || app.BotOpenID == "" {
		stateError(w, store.ErrNotFound)
		return
	}
	menu := app.BotMenu
	if menu.Items == nil {
		menu.Items = []config.BotMenuItem{}
	}
	reply(w, http.StatusOK, menu)
}

func (c *control) clickBotMenu(w http.ResponseWriter, r *http.Request) {
	var input struct {
		MenuID string `json:"menu_id"`
		OpenID string `json:"operator_open_id"`
	}
	if !read(w, r, &input, false) {
		return
	}
	if input.MenuID == "" || input.OpenID == "" {
		fail(w, http.StatusBadRequest, "menu_id and operator_open_id are required")
		return
	}
	c.mu.RLock()
	ctx := c.events.Scope(r.Context())
	app, found := c.state.Apps().Get(r.PathValue("app_id"))
	user, userExists := c.state.Users().Get(input.OpenID)
	clickedAt := c.state.Now().UnixMilli()
	c.mu.RUnlock()
	if !found || !userExists || app.BotOpenID == "" {
		stateError(w, store.ErrNotFound)
		return
	}
	if user.Inactive || !app.BotMenu.Enabled {
		fail(w, http.StatusConflict, "menu is disabled or the operator is inactive")
		return
	}
	index := slices.IndexFunc(app.BotMenu.Items, func(item config.BotMenuItem) bool { return item.ID == input.MenuID })
	if index < 0 {
		stateError(w, store.ErrNotFound)
		return
	}
	item := app.BotMenu.Items[index]
	if item.Kind != config.MenuEvent {
		fail(w, http.StatusUnprocessableEntity, "this menu item has no callback event")
		return
	}
	result, err := c.events.DispatchSubscribed(ctx, app.AppID, menuClicked, data{
		"operator":  data{"operator_id": eventUser(user), "operator_name": user.Name},
		"event_key": item.EventKey, "timestamp": clickedAt,
	})
	reply(w, http.StatusOK, eventReceipt(result, err))
}
