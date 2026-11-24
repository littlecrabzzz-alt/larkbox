package sdk_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/larksuite/oapi-sdk-go/v3/core/httpserverext"
	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/event"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/server"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

type sdkEventInbox struct {
	received chan *larkim.P2MessageReceiveV1
	recalled chan *larkim.P2MessageRecalledV1
	added    chan *larkim.P2ChatMemberBotAddedV1
	deleted  chan *larkim.P2ChatMemberBotDeletedV1
}

func sdkEventDispatcher(app config.App) (*dispatcher.EventDispatcher, sdkEventInbox) {
	inbox := sdkEventInbox{
		make(chan *larkim.P2MessageReceiveV1, 8), make(chan *larkim.P2MessageRecalledV1, 8),
		make(chan *larkim.P2ChatMemberBotAddedV1, 8), make(chan *larkim.P2ChatMemberBotDeletedV1, 8),
	}
	d := dispatcher.NewEventDispatcher(app.VerificationToken, app.EncryptKey).
		OnP2MessageReceiveV1(func(_ context.Context, incoming *larkim.P2MessageReceiveV1) error {
			inbox.received <- incoming
			return nil
		}).
		OnP2MessageRecalledV1(func(_ context.Context, incoming *larkim.P2MessageRecalledV1) error {
			inbox.recalled <- incoming
			return nil
		}).
		OnP2ChatMemberBotAddedV1(func(_ context.Context, incoming *larkim.P2ChatMemberBotAddedV1) error {
			inbox.added <- incoming
			return nil
		}).
		OnP2ChatMemberBotDeletedV1(func(_ context.Context, incoming *larkim.P2ChatMemberBotDeletedV1) error {
			inbox.deleted <- incoming
			return nil
		})
	return d, inbox
}

func sdkEventConfig() config.Config {
	cfg := sdkTestConfig()
	cfg.Apps[0].TenantKey = "tenant-owner"
	receiver := config.DefaultApp()
	receiver.AppID = protocol.NewID("cli_")
	receiver.BotOpenID = "ou_event_bot"
	receiver.TenantKey = "tenant-events"
	cfg.Apps = append(cfg.Apps, receiver)
	// Give the receiving bot its own direct chat with Alice.
	cfg.Chats[1].MemberIDs = []string{cfg.Users[0].OpenID, receiver.BotOpenID}
	return cfg
}

func sdkEventServer(t *testing.T, cfg config.Config, opts event.Options) (*server.Server, *httptest.Server) {
	t.Helper()
	srv := server.New(cfg, server.WithEventOptions(opts))
	web := httptest.NewServer(srv)
	t.Cleanup(web.Close)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return srv, web
}

func sdkEventREST(web *httptest.Server, app config.App) *lark.Client {
	return lark.NewClient(app.AppID, app.AppSecret, lark.WithOpenBaseUrl(web.URL), lark.WithHttpClient(web.Client()))
}

func receiveSDKEvent[T any](t *testing.T, events <-chan T) T {
	t.Helper()
	select {
	case received := <-events:
		return received
	case <-time.After(4 * time.Second):
		t.Fatal("SDK event handler did not receive the delivery")
		var zero T
		return zero
	}
}

func assertSDKEventHeader(t *testing.T, base *larkevent.EventV2Base, app config.App, kind string) string {
	t.Helper()
	if base == nil || base.Schema != "2.0" || base.Header == nil {
		t.Fatalf("invalid Schema 2.0 event header: %+v", base)
	}
	h := base.Header
	stamp, err := strconv.ParseInt(h.CreateTime, 10, 64)
	if err != nil || stamp < 1_000_000_000_000 || h.EventID == "" || h.EventType != kind ||
		h.AppID != app.AppID || h.TenantKey != app.TenantKey || h.Token != app.VerificationToken {
		t.Fatalf("invalid event header: %+v", h)
	}
	return h.EventID
}

func sdkBotJoin(t *testing.T, client *lark.Client, chatID, appID string) {
	t.Helper()
	joined, err := client.Im.ChatMembers.Create(t.Context(), larkim.NewCreateChatMembersReqBuilder().
		ChatId(chatID).MemberIdType("app_id").
		Body(larkim.NewCreateChatMembersReqBodyBuilder().IdList([]string{appID}).Build()).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, joined.CodeError, 0)
}

func sdkRecallEvent(t *testing.T, client *lark.Client, chatID string) string {
	t.Helper()
	message := sdkSend(t, client, "chat_id", chatID, "text", `{"text":"recall this"}`)
	id := value(message.MessageId)
	sdkRecallMessage(t, client, id)
	return id
}

func sdkRecallMessage(t *testing.T, client *lark.Client, id string) {
	t.Helper()
	resp, err := client.Im.Message.Delete(t.Context(), larkim.NewDeleteMessageReqBuilder().MessageId(id).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, resp.CodeError, 0)
}

func assertSDKBotJoin(t *testing.T, inbox sdkEventInbox, cfg config.Config) {
	t.Helper()
	joined := receiveSDKEvent(t, inbox.added)
	assertSDKEventHeader(t, joined.EventV2Base, cfg.Apps[1], "im.chat.member.bot.added_v1")
	if joined.Event == nil || value(joined.Event.ChatId) != cfg.Chats[0].ChatID || joined.Event.OperatorId == nil ||
		value(joined.Event.OperatorId.OpenId) != cfg.Apps[0].BotOpenID ||
		value(joined.Event.OperatorTenantKey) != cfg.Apps[0].TenantKey || value(joined.Event.Name) != cfg.Chats[0].Name {
		t.Fatalf("invalid bot join: %+v", joined.Event)
	}
}

func assertSDKRecall(t *testing.T, inbox sdkEventInbox, app config.App, chatID, id, recallType string) {
	t.Helper()
	recalled := receiveSDKEvent(t, inbox.recalled)
	assertSDKEventHeader(t, recalled.EventV2Base, app, "im.message.recalled_v1")
	if recalled.Event == nil || value(recalled.Event.MessageId) != id || value(recalled.Event.ChatId) != chatID ||
		value(recalled.Event.RecallType) != recallType || value(recalled.Event.RecallTime) != recalled.EventV2Base.Header.CreateTime {
		t.Fatalf("invalid recall: %+v", recalled.Event)
	}
}

func sdkReceiveMessage(cfg config.Config, chat config.Chat) store.Message {
	alice := cfg.Users[0]
	return store.Message{
		ID: protocol.NewID(protocol.MessageID), AppID: cfg.Apps[1].AppID, ChatID: chat.ChatID,
		SenderOpenID: alice.OpenID, SenderIsUser: true, MsgType: "text",
		Content: `{"text":"hello"}`, CreatedAt: time.Now(),
	}
}

func assertSDKMessageReceive(t *testing.T, inbox sdkEventInbox, cfg config.Config, want store.Message, kind, eventID string) {
	t.Helper()
	received := receiveSDKEvent(t, inbox.received)
	if id := assertSDKEventHeader(t, received.EventV2Base, cfg.Apps[1], "im.message.receive_v1"); id != eventID {
		t.Fatalf("event ID = %q, want %q", id, eventID)
	}
	if received.Event == nil || received.Event.Sender == nil || received.Event.Sender.SenderId == nil ||
		value(received.Event.Sender.SenderId.OpenId) != cfg.Users[0].OpenID ||
		value(received.Event.Sender.SenderId.UserId) != cfg.Users[0].UserID ||
		value(received.Event.Sender.SenderType) != "user" {
		t.Fatalf("invalid sender: %+v", received.Event)
	}
	m := received.Event.Message
	if m == nil || value(m.MessageId) != want.ID || value(m.ChatId) != want.ChatID || value(m.ChatType) != kind ||
		value(m.MessageType) != want.MsgType || value(m.Content) != want.Content ||
		value(m.ThreadId) != want.ThreadID || value(m.RootId) != want.RootID || value(m.ParentId) != want.ParentID {
		t.Fatalf("invalid SDK message: %+v", m)
	}
	if len(m.Mentions) != len(want.Mentions) {
		t.Fatalf("SDK got %d mentions, want %d", len(m.Mentions), len(want.Mentions))
	}
	for i, mention := range m.Mentions {
		if value(mention.Key) != want.Mentions[i].Key || mention.Id == nil ||
			value(mention.Id.OpenId) != cfg.Apps[1].BotOpenID || value(mention.MentionedType) != "bot" {
			t.Fatalf("invalid SDK bot mention: %+v", mention)
		}
	}
}

func TestOfficialSDKWebSocketEvents(t *testing.T) {
	cfg := sdkEventConfig()
	cfg.Chats[0].AppID = cfg.Apps[0].AppID
	ownerHandler, ownerInbox := sdkEventDispatcher(cfg.Apps[0])
	ownerWebhook := httptest.NewServer(http.HandlerFunc(httpserverext.NewEventHandlerFunc(ownerHandler)))
	t.Cleanup(ownerWebhook.Close)
	cfg.Apps[0].WebhookURL = ownerWebhook.URL
	app := cfg.Apps[1]
	srv, web := sdkEventServer(t, cfg, event.Options{WebSocketMaxAttempts: 1, WebhookMaxAttempts: 1})
	handler, inbox := sdkEventDispatcher(app)
	handler.OnP2CardActionTrigger(func(_ context.Context, incoming *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
		if incoming.Event == nil || incoming.Event.Host != "im_message" {
			return nil, errors.New("unexpected card host")
		}
		return &callback.CardActionTriggerResponse{Toast: &callback.Toast{Type: "success", Content: "confirmed"}}, nil
	})
	ready := make(chan struct{})
	client := larkws.NewClient(app.AppID, app.AppSecret, larkws.WithDomain(web.URL), larkws.WithHttpClient(web.Client()),
		larkws.WithEventHandler(handler), larkws.WithAutoReconnect(false), larkws.WithOnReady(func() { close(ready) }))
	stopped := make(chan struct{})
	var startErr error
	go func() {
		defer close(stopped)
		startErr = client.Start(t.Context())
	}()
	t.Cleanup(func() {
		client.Close()
		select {
		case <-stopped:
		case <-time.After(4 * time.Second):
			t.Error("SDK client did not stop")
		}
	})
	select {
	case <-ready:
	case <-stopped:
		t.Fatalf("SDK connection failed: %v", startErr)
	case <-time.After(4 * time.Second):
		t.Fatal("SDK connection did not become ready")
	}

	owner, target := sdkEventREST(web, cfg.Apps[0]), sdkEventREST(web, app)
	chatID := cfg.Chats[0].ChatID
	sdkBotJoin(t, owner, chatID, app.AppID)
	assertSDKBotJoin(t, inbox, cfg)

	p2p := sdkReceiveMessage(cfg, cfg.Chats[1])
	group := sdkReceiveMessage(cfg, cfg.Chats[0])
	group.Content = `{"text":"@_user_1 hello"}`
	group.Mentions = []store.Mention{{Key: "@_user_1", OpenID: app.BotOpenID, Name: app.BotName}}
	reply := sdkReceiveMessage(cfg, cfg.Chats[0])
	reply.RootID, reply.ParentID, reply.ThreadID = group.ID, group.ID, protocol.NewID(protocol.ThreadID)
	for _, tc := range []struct {
		name, chatType string
		message        store.Message
	}{{"direct", "p2p", p2p}, {"mention", "group", group}, {"reply within thread", "group", reply}} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := srv.Events().DispatchMessageReceive(t.Context(), app.AppID, tc.message)
			requireSDKNetwork(t, err)
			if len(result.Deliveries) != 1 || result.Deliveries[0].Channel != "websocket" || result.Deliveries[0].Status != 200 {
				t.Fatalf("missing websocket acknowledgement: %+v", result)
			}
			assertSDKMessageReceive(t, inbox, cfg, tc.message, tc.chatType, result.EventID)
		})
	}

	id := sdkRecallEvent(t, target, chatID)
	assertSDKRecall(t, inbox, app, chatID, id, "message_owner")
	assertSDKRecall(t, ownerInbox, cfg.Apps[0], chatID, id, "message_owner")
	// The second bot receives a recall of a message authored by the first.
	id = sdkRecallEvent(t, owner, chatID)
	assertSDKRecall(t, inbox, app, chatID, id, "message_owner")
	assertSDKRecall(t, ownerInbox, cfg.Apps[0], chatID, id, "message_owner")
	priorMessage := sdkSend(t, target, "chat_id", chatID, "text", `{"text":"sent before leaving"}`)
	removeBody, err := larkim.NewDeleteChatMembersPathReqBodyBuilder().IdList([]string{app.AppID}).Build()
	requireSDKNetwork(t, err)
	removed, err := target.Im.ChatMembers.Delete(t.Context(), larkim.NewDeleteChatMembersReqBuilder().
		ChatId(chatID).MemberIdType("app_id").Body(removeBody).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, removed.CodeError, 0)
	deleted := receiveSDKEvent(t, inbox.deleted)
	assertSDKEventHeader(t, deleted.EventV2Base, app, "im.chat.member.bot.deleted_v1")
	if deleted.Event == nil || value(deleted.Event.ChatId) != chatID {
		t.Fatalf("invalid bot removal: %+v", deleted.Event)
	}
	// The chat manager can recall the departed bot's message without
	// notifying the departed bot, even while its subscription is still open.
	sdkRecallMessage(t, owner, value(priorMessage.MessageId))
	assertSDKRecall(t, ownerInbox, cfg.Apps[0], chatID, value(priorMessage.MessageId), "group_manager")

	card, err := srv.Events().DispatchCardAction(t.Context(), app.AppID, callback.CardActionTriggerRequest{Host: "im_message"})
	requireSDKNetwork(t, err)
	var response callback.CardActionTriggerResponse
	if len(card.Deliveries) != 1 || card.Deliveries[0].Status != 200 {
		t.Fatalf("missing callback acknowledgement: %+v", card)
	}
	requireSDKNetwork(t, json.Unmarshal(card.Deliveries[0].Data, &response))
	if response.Toast == nil || response.Toast.Content != "confirmed" {
		t.Fatalf("invalid card response: %+v", response)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	requireSDKNetwork(t, srv.Shutdown(ctx))
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("server did not disconnect the SDK")
	}
	select {
	case recalled := <-inbox.recalled:
		t.Fatalf("removed bot received recall for its old chat: %+v", recalled.Event)
	default:
	}
}

func TestOfficialSDKRejectsBadWebSocketCredentials(t *testing.T) {
	cfg := sdkEventConfig()
	_, web := sdkEventServer(t, cfg, event.Options{})
	client := larkws.NewClient(cfg.Apps[1].AppID, "invalid", larkws.WithDomain(web.URL),
		larkws.WithHttpClient(web.Client()), larkws.WithAutoReconnect(false))
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	var denied *larkws.ClientError
	if err := client.Start(ctx); !errors.As(err, &denied) || denied.Code != 1000040345 {
		t.Fatalf("unexpected SDK authentication failure: %v", err)
	}
}

func TestOfficialSDKHTTPWebhookEvents(t *testing.T) {
	for _, mode := range []string{"plaintext", "encrypted"} {
		t.Run(mode, func(t *testing.T) {
			cfg := sdkEventConfig()
			app := &cfg.Apps[1]
			if mode == "plaintext" {
				app.EncryptKey = ""
			}
			d, inbox := sdkEventDispatcher(*app)
			handler := httpserverext.NewEventHandlerFunc(d)
			receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, ok := sdkReadWebhook(t, w, r, app.EncryptKey)
				if !ok {
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
				handler(w, r)
			}))
			t.Cleanup(receiver.Close)
			app.WebhookURL = receiver.URL + "/events"
			srv, web := sdkEventServer(t, cfg, event.Options{})
			requireSDKNetwork(t, srv.Events().VerifyWebhook(t.Context(), app.AppID))

			chatID := cfg.Chats[0].ChatID
			sdkBotJoin(t, sdkEventREST(web, cfg.Apps[0]), chatID, app.AppID)
			assertSDKBotJoin(t, inbox, cfg)
			message := sdkReceiveMessage(cfg, cfg.Chats[1])
			result, err := srv.Events().DispatchMessageReceive(t.Context(), app.AppID, message)
			requireSDKNetwork(t, err)
			assertSDKMessageReceive(t, inbox, cfg, message, "p2p", result.EventID)
			id := sdkRecallEvent(t, sdkEventREST(web, *app), chatID)
			assertSDKRecall(t, inbox, *app, chatID, id, "message_owner")

			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
			defer cancel()
			requireSDKNetwork(t, srv.Shutdown(ctx))
			count := 0
			for _, trace := range srv.Store().Traces() {
				if trace.Direction == store.Outbound {
					if trace.Channel != "webhook" || trace.Status != 200 || trace.Attempt != 1 || trace.AppID != app.AppID {
						t.Fatalf("unexpected outbound trace: %+v", trace)
					}
					count++
				}
			}
			if count != 4 {
				t.Fatalf("got %d webhook traces, want 4", count)
			}
		})
	}
}

func sdkReadWebhook(t *testing.T, w http.ResponseWriter, r *http.Request, key string) ([]byte, bool) {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Error(err)
		w.WriteHeader(http.StatusBadRequest)
		return nil, false
	}
	stamp, nonce, signature := r.Header.Get("X-Lark-Request-Timestamp"), r.Header.Get("X-Lark-Request-Nonce"), r.Header.Get("X-Lark-Signature")
	if key == "" {
		if stamp != "" || nonce != "" || signature != "" {
			t.Error("plaintext event carried encryption headers")
		}
	} else {
		digest := sha256.Sum256(append([]byte(stamp+nonce+key), body...))
		if stamp == "" || nonce == "" || signature != hex.EncodeToString(digest[:]) || !bytes.Contains(body, []byte(`"encrypt"`)) {
			t.Error("encrypted webhook signature or body was invalid")
			w.WriteHeader(http.StatusBadRequest)
			return nil, false
		}
	}
	return body, true
}

func TestOfficialSDKWebhookTimeoutAndRetry(t *testing.T) {
	cfg := sdkEventConfig()
	app := &cfg.Apps[1]
	d, inbox := sdkEventDispatcher(*app)
	handler := httpserverext.NewEventHandlerFunc(d)
	var count atomic.Int32
	wireIDs := make(chan string, 4)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := sdkReadWebhook(t, w, r, app.EncryptKey)
		if !ok {
			return
		}
		plain, err := protocol.DecryptWebhook(app.EncryptKey, raw)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var envelope larkevent.EventV2Base
		if err := json.Unmarshal(plain, &envelope); err != nil || envelope.Header == nil {
			t.Errorf("invalid event: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		wireIDs <- envelope.Header.EventID
		switch count.Add(1) {
		case 1:
			http.Error(w, "retry", http.StatusInternalServerError)
		case 2:
			select {
			case <-r.Context().Done():
			case <-time.After(6 * time.Second):
				t.Error("webhook request did not time out")
			}
		default:
			r.Body = io.NopCloser(bytes.NewReader(raw))
			handler(w, r)
		}
	}))
	t.Cleanup(receiver.Close)
	app.WebhookURL = receiver.URL + "/events"
	srv, _ := sdkEventServer(t, cfg, event.Options{RetryBackoff: func(int) time.Duration { return 0 }})
	message := sdkReceiveMessage(cfg, cfg.Chats[1])
	result, err := srv.Events().DispatchMessageReceive(t.Context(), app.AppID, message)
	requireSDKNetwork(t, err)
	assertSDKMessageReceive(t, inbox, cfg, message, "p2p", result.EventID)
	if count.Load() != 3 {
		t.Fatalf("got %d attempts, want 3", count.Load())
	}
	traces := srv.Store().Traces()
	if len(traces) != 3 {
		t.Fatalf("got %d traces, want 3", len(traces))
	}
	for _, trace := range traces {
		if wireID := <-wireIDs; wireID != result.EventID || trace.EventID != wireID ||
			trace.Channel != "webhook" || trace.EventType != "im.message.receive_v1" || trace.AppID != app.AppID {
			t.Fatalf("invalid trace: %+v", trace)
		}
		switch trace.Attempt {
		case 1:
			if trace.Status != 500 || string(trace.ResponseBody) != "retry\n" || trace.Error == "" {
				t.Fatalf("invalid 500 delivery trace: %+v", trace)
			}
		case 2:
			if trace.Status != 0 || trace.Latency < 3*time.Second || !strings.Contains(trace.Error, "deadline") {
				t.Fatalf("missing default 3-second timeout: %+v", trace)
			}
		case 3:
			if trace.Status != 200 || trace.Error != "" {
				t.Fatalf("invalid successful retry: %+v", trace)
			}
		default:
			t.Fatalf("unexpected attempt: %+v", trace)
		}
	}
}
