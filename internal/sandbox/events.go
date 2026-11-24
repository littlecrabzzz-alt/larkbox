package sandbox

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/event"
	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/im"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

type sendInput struct {
	ChatID        string   `json:"chat_id"`
	AppID         string   `json:"app_id"`
	SenderOpenID  string   `json:"sender_open_id"`
	SenderUserID  string   `json:"sender_user_id"`
	MsgType       string   `json:"msg_type"`
	Content       *string  `json:"content"`
	Text          *string  `json:"text"`
	Mentions      []string `json:"mentions"`
	AtBot         bool     `json:"at_bot"`
	MentionBot    bool     `json:"mention_bot"`
	ParentID      string   `json:"parent_id"`
	ReplyInThread bool     `json:"reply_in_thread"`
}

func (c *control) send(w http.ResponseWriter, r *http.Request) {
	var input sendInput
	if !read(w, r, &input, false) {
		return
	}
	if (input.ChatID == "") == (input.AppID == "") ||
		(input.SenderOpenID == "") == (input.SenderUserID == "") || (input.Content == nil) == (input.Text == nil) ||
		(input.ReplyInThread && input.ParentID == "") {
		fail(w, http.StatusBadRequest, "provide one chat_id or app_id, one sender selector, and one content or text; reply_in_thread needs parent_id")
		return
	}
	if input.MsgType == "" {
		input.MsgType = "text"
	}
	if input.Text != nil && input.MsgType != "text" {
		fail(w, http.StatusBadRequest, "text requires msg_type text")
		return
	}
	c.mu.RLock()
	ctx := c.events.Scope(r.Context())
	message, recipients, err := c.saveMessage(input)
	var view data
	if err == nil {
		view = im.InspectMessageData(c.state, message)
	}
	c.mu.RUnlock()
	if err != nil {
		stateError(w, err)
		return
	}

	deliveries := make([]data, len(recipients))
	// Independent bots can handle the same message concurrently.
	done := make(chan struct{}, len(recipients))
	for i, appID := range recipients {
		go func() {
			result, err := c.events.DispatchMessageReceive(ctx, appID, message)
			outcomes := make([]data, 0, len(result.Deliveries))
			for _, delivery := range result.Deliveries {
				outcomes = append(outcomes, deliveryData(delivery))
			}
			deliveries[i] = data{"app_id": appID, "event_id": result.EventID, "deliveries": outcomes, "error": errorText(err)}
			done <- struct{}{}
		}()
	}
	for range recipients {
		<-done
	}
	reply(w, http.StatusCreated, data{"message": view, "deliveries": deliveries})
}

func (c *control) saveMessage(input sendInput) (store.Message, []string, error) {
	user, ok := c.state.OAuthUser(input.SenderOpenID, input.SenderUserID)
	if !ok {
		return store.Message{}, nil, store.ErrNotFound
	}
	if user.Inactive {
		return store.Message{}, nil, errors.New("sender is inactive")
	}
	actor := store.IMActor{AppID: input.AppID, OpenID: user.OpenID, IsUser: true, Sandbox: true}
	chat, apps, peer, err := c.messageTarget(input, actor)
	if err != nil {
		return store.Message{}, nil, err
	}
	var bots []string
	if input.AtBot || input.MentionBot || slices.Contains(input.Mentions, "@bot") {
		for _, app := range apps {
			bots = append(bots, app.BotOpenID)
		}
		if len(bots) == 0 {
			return store.Message{}, nil, errors.New("chat has no bot to mention")
		}
	}
	mentions := make([]string, 0, len(input.Mentions))
	for _, id := range input.Mentions {
		if id == "@bot" {
			continue
		}
		if !slices.Contains(chat.MemberIDs, id) || strings.ContainsAny(id, "\"<>") {
			return store.Message{}, nil, errors.New("mentions must identify existing chat members by open_id")
		}
		if !slices.Contains(mentions, id) && !slices.Contains(bots, id) {
			mentions = append(mentions, id)
		}
	}
	var content string
	if input.Text != nil {
		encoded, _ := json.Marshal(map[string]string{"text": *input.Text})
		content = string(encoded)
	} else {
		content = *input.Content
	}
	req, err := im.PrepareSandboxMessage(c.state, input.MsgType, content, mentions, bots)
	if err != nil {
		return store.Message{}, nil, err
	}
	req.ChatID, req.Peer, req.ParentID, req.ReplyInThread = chat.ChatID, peer, input.ParentID, input.ReplyInThread
	return c.state.CreateSandboxMessage(actor, req)
}

func (c *control) messageTarget(input sendInput, actor store.IMActor) (store.Chat, []store.App, string, error) {
	if input.AppID != "" {
		app, ok := c.state.Apps().Get(input.AppID)
		if !ok {
			return store.Chat{}, nil, "", store.ErrNotFound
		}
		chat := store.Chat{MemberIDs: []string{actor.OpenID, app.BotOpenID}, ChatMode: "p2p"}
		if input.ParentID != "" {
			parent, ok := c.state.Messages().Get(input.ParentID)
			if !ok {
				return store.Chat{}, nil, "", store.ErrNotFound
			}
			target, err := c.state.IMChat(actor, parent.ChatID)
			if err != nil {
				return store.Chat{}, nil, "", err
			}
			if target.ChatMode != "p2p" || !slices.Contains(target.MemberIDs, app.BotOpenID) {
				return store.Chat{}, nil, "", store.ErrOutsideChat
			}
			chat = target
		}
		return chat, []store.App{app}, app.BotOpenID, nil
	}

	chat, err := c.state.IMChat(actor, input.ChatID)
	if err != nil {
		return store.Chat{}, nil, "", err
	}
	apps := c.state.Apps().List()
	apps = slices.DeleteFunc(apps, func(app store.App) bool {
		return (chat.ChatMode == "p2p" && chat.AppID != "" && chat.AppID != app.AppID) ||
			app.BotOpenID == "" || !slices.Contains(chat.MemberIDs, app.BotOpenID)
	})
	slices.SortFunc(apps, func(a, b store.App) int { return cmp.Compare(a.AppID, b.AppID) })
	return chat, apps, "", nil
}

func (c *control) cardAction(w http.ResponseWriter, r *http.Request) {
	var input struct {
		MessageID      string                  `json:"message_id"`
		OperatorOpenID string                  `json:"operator_open_id"`
		Action         callback.CallBackAction `json:"action"`
		Host           string                  `json:"host"`
		DeliveryType   string                  `json:"delivery_type"`
	}
	if !read(w, r, &input, false) {
		return
	}
	if input.MessageID == "" || input.OperatorOpenID == "" || strings.TrimSpace(input.Action.Tag) == "" {
		fail(w, http.StatusBadRequest, "message_id, operator_open_id, and action.tag are required")
		return
	}
	result, err := c.events.TriggerCardAction(r.Context(), event.CardActionTriggerInput{
		MessageID: input.MessageID, OperatorOpenID: input.OperatorOpenID, Action: input.Action,
		Host: input.Host, DeliveryType: input.DeliveryType,
	})
	if err != nil && result.EventID == "" {
		stateError(w, err)
		return
	}
	var message data
	if saved, ok := c.state.InspectMessage(input.MessageID); ok {
		message = im.InspectMessageData(c.state, saved)
	}
	delivery := deliveryData(result.Delivery)
	delivery["event_id"] = result.EventID
	var toast *callback.Toast
	if result.Toast != nil {
		redacted := *result.Toast
		redacted.Content = store.RedactDiagnosticText(redacted.Content)
		if len(redacted.I18nContent) > 0 {
			redacted.I18nContent = make(map[string]string, len(result.Toast.I18nContent))
			for lang, text := range result.Toast.I18nContent {
				redacted.I18nContent[lang] = store.RedactDiagnosticText(text)
			}
		}
		toast = &redacted
	}
	reply(w, http.StatusOK, data{
		"toast": toast, "card_applied": result.Updated, "card_error": errorText(err),
		"message": message, "delivery": delivery,
	})
}

func (c *control) verifyWebhook(w http.ResponseWriter, r *http.Request) {
	var input struct {
		AppID      string  `json:"app_id"`
		WebhookURL *string `json:"webhook_url"`
	}
	if !read(w, r, &input, false) {
		return
	}
	if input.AppID == "" {
		fail(w, http.StatusBadRequest, "app_id is required")
		return
	}
	app, ok := c.state.Apps().Get(input.AppID)
	if !ok {
		stateError(w, store.ErrNotFound)
		return
	}
	var override string
	if input.WebhookURL != nil {
		override = *input.WebhookURL
	}
	target := app.WebhookURL
	if input.WebhookURL != nil {
		target = override
	}
	if _, err := config.ParseRedirectURI(target); err != nil {
		fail(w, http.StatusBadRequest, "webhook_url must be an absolute HTTP(S) URL with a valid host")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	start := time.Now()
	result := c.events.VerifyWebhookAt(ctx, input.AppID, override)
	reply(w, http.StatusOK, data{
		"verified": result.Err == nil, "status_code": result.Status, "error": errorText(result.Err),
		"latency_ms": time.Since(start).Milliseconds(),
	})
}

func deliveryData(delivery event.Delivery) data {
	return data{"channel": delivery.Channel, "status_code": delivery.Status, "error": errorText(delivery.Err)}
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "delivery timed out"
	}
	return err.Error()
}
