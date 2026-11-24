package event

import (
	"context"
	"fmt"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

const (
	messageReceive = "im.message.receive_v1"
	messageRecall  = "im.message.recalled_v1"
	botAdded       = "im.chat.member.bot.added_v1"
	botDeleted     = "im.chat.member.bot.deleted_v1"
)

type userID struct {
	OpenID  string `json:"open_id"`
	UserID  string `json:"user_id,omitempty"`
	UnionID string `json:"union_id,omitempty"`
}

func (d *Dispatcher) userIdentity(openID string) userID {
	user, _ := d.state.Users().Get(openID)
	return userID{OpenID: openID, UserID: user.UserID, UnionID: user.UnionID}
}

type receivedMessage struct {
	ID         string           `json:"message_id"`
	RootID     string           `json:"root_id,omitempty"`
	ParentID   string           `json:"parent_id,omitempty"`
	ThreadID   string           `json:"thread_id,omitempty"`
	CreateTime string           `json:"create_time"`
	UpdateTime string           `json:"update_time"`
	ChatID     string           `json:"chat_id"`
	ChatType   string           `json:"chat_type"`
	Type       string           `json:"message_type"`
	Content    string           `json:"content"`
	Mentions   []receiveMention `json:"mentions,omitempty"`
}

type receiveMention struct {
	Key       string `json:"key"`
	ID        userID `json:"id"`
	Type      string `json:"mentioned_type"`
	Name      string `json:"name"`
	TenantKey string `json:"tenant_key"`
}

type sender struct {
	ID        userID `json:"sender_id"`
	Type      string `json:"sender_type"`
	TenantKey string `json:"tenant_key"`
}

// DispatchMessageReceive delivers a stored message, including its normalized
// content (for example, "@_user_1") and matching mention metadata.
func (d *Dispatcher) DispatchMessageReceive(ctx context.Context, appID string, m store.Message) (Result, error) {
	chat, ok := d.state.Chats().Get(m.ChatID)
	if !ok {
		return Result{}, fmt.Errorf("unknown event chat %q", m.ChatID)
	}
	app, _ := d.state.Apps().Get(appID)
	from := sender{ID: d.userIdentity(m.SenderOpenID), Type: "user", TenantKey: d.userTenant(m.SenderOpenID, app)}
	if !m.SenderIsUser && m.AppID != "" {
		bot, _ := d.state.Apps().Get(m.AppID)
		from.Type, from.TenantKey = "bot", tenantKey(bot)
		if from.ID.OpenID == "" {
			from.ID.OpenID = bot.BotOpenID
		}
	}
	updated := m.UpdatedAt
	if updated.IsZero() {
		updated = m.CreatedAt
	}
	message := receivedMessage{
		ID: m.ID, ChatID: m.ChatID, ChatType: chat.ChatMode, RootID: m.RootID, ParentID: m.ParentID, ThreadID: m.ThreadID,
		CreateTime: millis(m.CreatedAt), UpdateTime: millis(updated), Type: m.MsgType, Content: m.Content,
	}
	if len(m.Mentions) > 0 {
		bots := make(map[string]store.App)
		for _, bot := range d.state.Apps().List() {
			bots[bot.BotOpenID] = bot
		}
		for _, mention := range m.Mentions {
			mapped := receiveMention{
				Key: mention.Key, ID: d.userIdentity(mention.OpenID), Name: mention.Name,
				Type: "user", TenantKey: d.userTenant(mention.OpenID, app),
			}
			if mention.UserID != "" {
				mapped.ID.UserID = mention.UserID
			}
			if bot, found := bots[mention.OpenID]; found && mention.OpenID != "" {
				mapped.Type, mapped.TenantKey = "bot", tenantKey(bot)
			}
			message.Mentions = append(message.Mentions, mapped)
		}
	}
	return d.Dispatch(ctx, appID, messageReceive, struct {
		Sender  sender          `json:"sender"`
		Message receivedMessage `json:"message"`
	}{from, message})
}

func (d *Dispatcher) userTenant(openID string, app store.App) string {
	if user, ok := d.state.Users().Get(openID); ok && user.TenantKey != "" {
		return user.TenantKey
	}
	return tenantKey(app)
}

// EnqueueMessageRecalled sends the notification for a committed recall.
func (d *Dispatcher) EnqueueMessageRecalled(appID, chatID, messageID, recallType string, at time.Time) {
	d.enqueue(appID, messageRecall, at, struct {
		ChatID     string `json:"chat_id"`
		MessageID  string `json:"message_id"`
		RecallTime string `json:"recall_time"`
		RecallType string `json:"recall_type"`
	}{chatID, messageID, millis(at), recallType})
}

// EnqueueBotMembership sends the notification for a committed bot membership change.
func (d *Dispatcher) EnqueueBotMembership(appID, chatID, operatorID, operatorAppID string, at time.Time, added bool) {
	kind := botDeleted
	if added {
		kind = botAdded
	}
	chat, _ := d.state.Chats().Get(chatID)
	operatorApp, _ := d.state.Apps().Get(operatorAppID)
	d.enqueue(appID, kind, at, struct {
		ChatID            string            `json:"chat_id"`
		OperatorID        userID            `json:"operator_id"`
		OperatorTenantKey string            `json:"operator_tenant_key"`
		External          bool              `json:"external"`
		Name              string            `json:"name"`
		I18nNames         map[string]string `json:"i18n_names,omitempty"`
	}{chatID, d.userIdentity(operatorID), d.userTenant(operatorID, operatorApp), chat.External, chat.Name, chat.I18nNames})
}

// DispatchCardAction delivers a card callback and returns receiver responses
// for the caller to interpret.
func (d *Dispatcher) DispatchCardAction(ctx context.Context, appID string, action any) (Result, error) {
	return d.Dispatch(ctx, appID, "card.action.trigger", action)
}
