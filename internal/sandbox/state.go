package sandbox

import (
	"cmp"
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/cardkit"
	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/im"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

type seedInput struct {
	Apps          []config.App          `json:"apps"`
	Users         []config.User         `json:"users"`
	Chats         []config.Chat         `json:"chats"`
	CardTemplates []config.CardTemplate `json:"card_templates"`
}

func (s seedInput) config() config.Config {
	return config.Config{Apps: s.Apps, Users: s.Users, Chats: s.Chats, CardTemplates: s.CardTemplates}
}

func (s seedInput) validateTemplates(w http.ResponseWriter) bool {
	for _, template := range s.CardTemplates {
		if err := cardkit.ValidateTemplate(template.Data); err != nil {
			fail(w, http.StatusBadRequest, "card_templates entries must contain valid Card JSON 2.0")
			return false
		}
	}
	return true
}

func (c *control) snapshot(w http.ResponseWriter, _ *http.Request) {
	c.mu.RLock()
	sessions := c.events.Sessions()
	counts := make(map[string]int)
	for _, session := range sessions {
		counts[session.AppID]++
	}
	apps := c.state.Apps().List()
	slices.SortFunc(apps, func(a, b store.App) int { return cmp.Compare(a.AppID, b.AppID) })
	appData := make([]data, 0, len(apps))
	for _, app := range apps {
		appData = append(appData, data{
			"app_id": app.AppID, "bot_open_id": app.BotOpenID, "bot_name": app.BotName,
			"webhook_url": app.WebhookURL, "has_encrypt_key": app.EncryptKey != "",
			"has_verification_token": app.VerificationToken != "", "redirect_uris": append([]string{}, app.RedirectURIs...),
			"active_ws_sessions": counts[app.AppID],
		})
	}
	users := c.state.Users().List()
	slices.SortFunc(users, func(a, b store.User) int { return cmp.Compare(a.OpenID, b.OpenID) })
	snapshot := data{
		"strict_mode": c.cfg.StrictMode, "version": c.version, "apps": appData, "users": users,
		"chats": c.chatData(), "cards": c.cardData(false), "faults": c.faults.List(), "ws_sessions": sessions,
	}
	c.mu.RUnlock()
	reply(w, http.StatusOK, snapshot)
}

func (c *control) seed(w http.ResponseWriter, r *http.Request) {
	var input seedInput
	if !read(w, r, &input, false) || !input.validateTemplates(w) {
		return
	}
	c.mu.Lock()
	err := c.state.UpsertSeed(input.config())
	c.mu.Unlock()
	if err != nil {
		stateError(w, err)
		return
	}
	reply(w, http.StatusOK, data{"apps": len(input.Apps), "users": len(input.Users), "chats": len(input.Chats), "card_templates": len(input.CardTemplates)})
}

func (c *control) reset(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Seed *seedInput `json:"seed"`
	}
	if !read(w, r, &input, true) {
		return
	}
	reset := c.state.Reset
	if input.Seed != nil {
		if !input.Seed.validateTemplates(w) {
			return
		}
		cfg := input.Seed.config()
		if err := store.ValidateSeed(cfg); err != nil {
			stateError(w, err)
			return
		}
		reset = func() { c.state.ResetTo(cfg) }
	}
	c.mu.Lock()
	c.events.Reset(func() {
		c.faults.Reset()
		reset()
	})
	c.mu.Unlock()
	reply(w, http.StatusOK, data{"reset": true})
}

func (c *control) chatData() []data {
	chats := c.state.Chats().List()
	slices.SortFunc(chats, func(a, b store.Chat) int { return cmp.Compare(a.ChatID, b.ChatID) })
	result := make([]data, 0, len(chats))
	for _, chat := range chats {
		result = append(result, data{
			"chat_id": chat.ChatID, "chat_mode": chat.ChatMode, "name": chat.Name, "owner_id": chat.OwnerID,
			"member_ids": append([]string{}, chat.MemberIDs...), "settings": chat.ChatSettings,
		})
	}
	return result
}

func (c *control) chats(w http.ResponseWriter, _ *http.Request) {
	reply(w, http.StatusOK, data{"chats": c.chatData()})
}

func (c *control) messages(w http.ResponseWriter, r *http.Request) {
	c.mu.RLock()
	messages, err := c.state.InspectChatMessages(r.PathValue("chat_id"))
	if err != nil {
		c.mu.RUnlock()
		stateError(w, err)
		return
	}
	result := make([]data, 0, len(messages))
	for _, message := range messages {
		result = append(result, im.InspectMessageData(c.state, message))
	}
	c.mu.RUnlock()
	reply(w, http.StatusOK, data{"messages": result})
}

type cardView struct {
	ID                 string             `json:"card_id"`
	AppID              string             `json:"app_id"`
	Type               string             `json:"type"`
	StreamingMode      bool               `json:"streaming_mode"`
	Sequence           int64              `json:"last_sequence"`
	Revision           uint64             `json:"revision"`
	CreatedAt          time.Time          `json:"created_at"`
	ExpiresAt          time.Time          `json:"expires_at"`
	StreamingExpiresAt *time.Time         `json:"streaming_expires_at"`
	JSON               json.RawMessage    `json:"card_json,omitempty"`
	Elements           *map[string]string `json:"elements,omitempty"`
}

func viewCard(card store.Card, detail bool) cardView {
	view := cardView{
		ID: card.ID, AppID: card.AppID, Type: card.Type, StreamingMode: card.StreamingMode, Sequence: card.Sequence,
		Revision: card.Revision, CreatedAt: card.CreatedAt, ExpiresAt: card.ExpiresAt(), StreamingExpiresAt: card.StreamingExpiresAt(),
	}
	if detail {
		view.JSON = store.RedactTraceJSON(card.JSON)
		elements := make(map[string]string, len(card.Elements))
		for id, content := range card.Elements {
			elements[id] = store.RedactDiagnosticText(content)
		}
		view.Elements = &elements
	}
	return view
}

func (c *control) cardData(detail bool) []cardView {
	cards := c.state.Cards()
	slices.SortFunc(cards, func(a, b store.Card) int { return cmp.Compare(a.ID, b.ID) })
	result := make([]cardView, 0, len(cards))
	for _, card := range cards {
		result = append(result, viewCard(card, detail))
	}
	return result
}

func (c *control) cards(w http.ResponseWriter, _ *http.Request) {
	reply(w, http.StatusOK, data{"cards": c.cardData(true)})
}

func (c *control) card(w http.ResponseWriter, r *http.Request) {
	card, ok := c.state.Card(r.PathValue("card_id"))
	if !ok {
		stateError(w, store.ErrNotFound)
		return
	}
	reply(w, http.StatusOK, viewCard(card, true))
}

func (c *control) sessions(w http.ResponseWriter, _ *http.Request) {
	reply(w, http.StatusOK, data{"ws_sessions": c.events.Sessions()})
}

func (c *control) expireTokens(w http.ResponseWriter, r *http.Request) {
	var input struct {
		AppID  string          `json:"app_id"`
		Token  json.RawMessage `json:"token"`
		Tokens json.RawMessage `json:"tokens"`
	}
	if !read(w, r, &input, false) {
		return
	}
	if input.AppID == "" || (input.Tokens != nil && input.Token != nil) {
		fail(w, http.StatusBadRequest, "app_id is required; provide either token or a nonempty tokens list")
		return
	}
	var values []string
	switch {
	case input.Token != nil:
		var token string
		if json.Unmarshal(input.Token, &token) != nil || token == "" {
			fail(w, http.StatusBadRequest, "token must be a nonempty string")
			return
		}
		values = []string{token}
	case input.Tokens != nil:
		if json.Unmarshal(input.Tokens, &values) != nil || len(values) == 0 {
			fail(w, http.StatusBadRequest, "tokens must be a nonempty list of strings")
			return
		}
	}
	for _, value := range values {
		if value == "" {
			fail(w, http.StatusBadRequest, "token values must be nonempty")
			return
		}
	}
	count, err := c.state.ExpireTokens(input.AppID, values)
	if err != nil {
		stateError(w, err)
		return
	}
	reply(w, http.StatusOK, data{"expired": count})
}
