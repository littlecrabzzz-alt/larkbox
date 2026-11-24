package sdk_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/larksuite/oapi-sdk-go/v3/core/httpserverext"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkcardkit "github.com/larksuite/oapi-sdk-go/v3/service/cardkit/v1"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/littlecrabzzz-alt/larkbox/internal/event"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
)

const sdkStreamingCard = `{"schema":"2.0","config":{"streaming_mode":true},"body":{"elements":[{"tag":"markdown","element_id":"md","content":"initial"}]}}`

func sdkCardEntity(t *testing.T, client *lark.Client, kind, data string) string {
	t.Helper()
	resp, err := client.Cardkit.V1.Card.Create(t.Context(), larkcardkit.NewCreateCardReqBuilder().
		Body(larkcardkit.NewCreateCardReqBodyBuilder().Type(kind).Data(data).Build()).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, resp.CodeError, 0)
	if resp.Data == nil || value(resp.Data.CardId) == "" || resp.StatusCode != http.StatusOK {
		t.Fatalf("invalid create response: %s", resp.RawBody)
	}
	return value(resp.Data.CardId)
}

func sdkEntityReference(cardID string) string {
	return fmt.Sprintf(`{"type":"card","data":{"card_id":%q}}`, cardID)
}

func sdkCardMessage(t *testing.T, client *lark.Client, chatID, content string) *larkim.CreateMessageRespData {
	t.Helper()
	resp, err := client.Im.Message.Create(t.Context(), larkim.NewCreateMessageReqBuilder().ReceiveIdType("chat_id").
		Body(larkim.NewCreateMessageReqBodyBuilder().ReceiveId(chatID).MsgType("interactive").Content(content).Build()).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, resp.CodeError, 0)
	if resp.Data == nil || value(resp.Data.MessageId) == "" || resp.Data.Body == nil || value(resp.Data.Body.Content) == "" {
		t.Fatalf("invalid message: %s", resp.RawBody)
	}
	return resp.Data
}

func sdkGetCardMessage(t *testing.T, client *lark.Client, messageID string, updated bool) *larkim.Message {
	t.Helper()
	resp, err := client.Im.Message.Get(t.Context(), larkim.NewGetMessageReqBuilder().MessageId(messageID).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, resp.CodeError, 0)
	if resp.Data == nil || len(resp.Data.Items) != 1 || resp.Data.Items[0].Body == nil ||
		value(resp.Data.Items[0].Updated) != updated || value(resp.Data.Items[0].UpdateTime) == "" {
		t.Fatalf("unexpected message: %s", resp.RawBody)
	}
	return resp.Data.Items[0]
}

func TestOfficialSDKCardKit(t *testing.T) {
	cfg := sdkTestConfig()
	other := cfg.Apps[0]
	other.AppID, other.BotOpenID = protocol.NewID("cli_"), "ou_other_bot"
	cfg.Apps = append(cfg.Apps, other)
	cfg.Chats[0].MemberIDs = append(cfg.Chats[0].MemberIDs, other.BotOpenID)
	srv, web := sdkEventServer(t, cfg, event.Options{})
	client := sdkEventREST(web, cfg.Apps[0])
	foreign := sdkEventREST(web, other)
	api := client.Cardkit.V1
	ctx := t.Context()
	id := sdkCardEntity(t, client, "card_json", sdkStreamingCard)
	msg := sdkCardMessage(t, client, cfg.Chats[0].ChatID, sdkEntityReference(id))
	messageID := value(msg.MessageId)
	if persisted, ok := srv.Store().Messages().Get(messageID); !ok || persisted.CardID != id {
		t.Fatalf("message missing card entity linkage: %+v", persisted)
	}
	for _, test := range []struct {
		client *lark.Client
		card   string
		code   int
	}{{foreign, id, 300311}, {client, "123", 230001}} {
		resp, err := test.client.Im.Message.Create(ctx, larkim.NewCreateMessageReqBuilder().ReceiveIdType("chat_id").
			Body(larkim.NewCreateMessageReqBodyBuilder().ReceiveId(cfg.Chats[0].ChatID).MsgType("interactive").
				Content(sdkEntityReference(test.card)).Build()).Build())
		requireSDKNetwork(t, err)
		requireSDKCode(t, resp.CodeError, test.code)
	}
	replyID := sdkCardEntity(t, client, "template", `{"template_id":"reply_template","template_variable":{"count":2}}`)
	reply, err := client.Im.Message.Reply(ctx, larkim.NewReplyMessageReqBuilder().MessageId(messageID).
		Body(larkim.NewReplyMessageReqBodyBuilder().MsgType("interactive").Content(sdkEntityReference(replyID)).Build()).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, reply.CodeError, 0)
	if reply.Data == nil || reply.Data.Body == nil || !strings.Contains(value(reply.Data.Body.Content), "reply_template") {
		t.Fatalf("invalid reply: %s", reply.RawBody)
	}
	if persisted, ok := srv.Store().Messages().Get(value(reply.Data.MessageId)); !ok || persisted.CardID != replyID {
		t.Fatalf("reply missing card entity linkage: %+v", persisted)
	}

	stream := func(seq int, content string, code, status int) {
		t.Helper()
		resp, err := api.CardElement.Content(ctx, larkcardkit.NewContentCardElementReqBuilder().CardId(id).ElementId("md").
			Body(larkcardkit.NewContentCardElementReqBodyBuilder().Sequence(seq).Content(content).Build()).Build())
		requireSDKNetwork(t, err)
		requireSDKCode(t, resp.CodeError, code)
		if resp.StatusCode != status {
			t.Fatalf("HTTP status = %d, want %d: %s", resp.StatusCode, status, resp.RawBody)
		}
	}
	for seq := 1; seq <= 3; seq++ {
		stream(seq, fmt.Sprintf("stream %d", seq), 0, http.StatusOK)
	}
	stream(2, "should not appear", 300317, http.StatusBadRequest)
	got := sdkGetCardMessage(t, client, messageID, false)
	if !strings.Contains(value(got.Body.Content), "stream 3") {
		t.Fatalf("stream update missing from GET: %s", value(got.Body.Content))
	}

	added, err := api.CardElement.Create(ctx, larkcardkit.NewCreateCardElementReqBuilder().CardId(id).
		Body(larkcardkit.NewCreateCardElementReqBodyBuilder().Sequence(4).Type("insert_after").TargetElementId("md").
			Elements(`[{"tag":"hr","element_id":"next"}]`).Build()).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, added.CodeError, 0)
	changed, err := api.CardElement.Update(ctx, larkcardkit.NewUpdateCardElementReqBuilder().CardId(id).ElementId("next").
		Body(larkcardkit.NewUpdateCardElementReqBodyBuilder().Sequence(5).Element(`{"tag":"plain_text","content":"changed","element_id":"next"}`).Build()).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, changed.CodeError, 0)
	patched, err := api.CardElement.Patch(ctx, larkcardkit.NewPatchCardElementReqBuilder().CardId(id).ElementId("next").
		Body(larkcardkit.NewPatchCardElementReqBodyBuilder().Sequence(6).PartialElement(`{"content":"patched"}`).Build()).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, patched.CodeError, 0)
	got = sdkGetCardMessage(t, client, messageID, false)
	if !strings.Contains(value(got.Body.Content), "patched") {
		t.Fatalf("patch missing from GET: %s", value(got.Body.Content))
	}
	deleted, err := api.CardElement.Delete(ctx, larkcardkit.NewDeleteCardElementReqBuilder().CardId(id).ElementId("next").
		Body(larkcardkit.NewDeleteCardElementReqBodyBuilder().Sequence(7).Build()).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, deleted.CodeError, 0)
	got = sdkGetCardMessage(t, client, messageID, false)
	if strings.Contains(value(got.Body.Content), "patched") {
		t.Fatalf("deleted element still in GET: %s", value(got.Body.Content))
	}
	settings, err := api.Card.Settings(ctx, larkcardkit.NewSettingsCardReqBuilder().CardId(id).
		Body(larkcardkit.NewSettingsCardReqBodyBuilder().Sequence(8).Settings(`{"config":{"streaming_mode":false,"summary":{"content":"complete"}}}`).Build()).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, settings.CodeError, 0)
	stream(9, "blocked", 300309, http.StatusBadRequest)

	update, err := api.Card.Update(ctx, larkcardkit.NewUpdateCardReqBuilder().CardId(id).
		Body(larkcardkit.NewUpdateCardReqBodyBuilder().Sequence(9).Card(larkcardkit.NewCardBuilder().Type("card_json").Data(sdkStreamingCard).Build()).Build()).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, update.CodeError, 0)
	batched, err := api.Card.BatchUpdate(ctx, larkcardkit.NewBatchUpdateCardReqBuilder().CardId(id).
		Body(larkcardkit.NewBatchUpdateCardReqBodyBuilder().Sequence(10).
			Actions(`[{"action":"partial_update_setting","params":{"settings":{"config":{"streaming_mode":false}}}},{"action":"partial_update_element","params":{"element_id":"md","partial_element":{"content":"batch persisted"}}}]`).Build()).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, batched.CodeError, 0)
	got = sdkGetCardMessage(t, client, messageID, false)
	if !strings.Contains(value(got.Body.Content), "batch persisted") {
		t.Fatalf("batch missing from GET: %s", value(got.Body.Content))
	}
}

func TestOfficialSDKCardCallbacks(t *testing.T) {
	for _, channel := range []string{"websocket", "webhook"} {
		t.Run(channel, func(t *testing.T) {
			cfg := sdkTestConfig()
			cfg.Apps[0].TenantKey = "tenant-card-callback"
			app := cfg.Apps[0]
			events := make(chan *callback.CardActionTriggerEvent, 1)
			d := dispatcher.NewEventDispatcher(app.VerificationToken, app.EncryptKey).
				OnP2CardActionTrigger(func(_ context.Context, incoming *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
					events <- incoming
					card := &callback.Card{Type: "raw", Data: json.RawMessage(`{"schema":"2.0","body":{"elements":[{"tag":"markdown","element_id":"md","content":"clicked via SDK"}]}}`)}
					if channel == "webhook" {
						card = &callback.Card{Type: "template", Data: callback.TemplateCard{TemplateID: "callback_template", TemplateVariable: map[string]any{"clicked": true}}}
					}
					return &callback.CardActionTriggerResponse{Toast: &callback.Toast{Type: "success", Content: "Confirmed"}, Card: card}, nil
				})
			if channel == "webhook" {
				handler := httpserverext.NewEventHandlerFunc(d)
				webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if app.EncryptKey == "" || r.Header.Get("X-Lark-Signature") == "" {
						t.Error("missing encrypted webhook signature")
					}
					handler(w, r)
				}))
				t.Cleanup(webhook.Close)
				cfg.Apps[0].WebhookURL = webhook.URL
			}
			srv, web := sdkEventServer(t, cfg, event.Options{})
			if channel == "websocket" {
				startSDKCardSocket(t, web, app.AppID, app.AppSecret, d)
			}
			client := sdkEventREST(web, app)
			id := sdkCardEntity(t, client, "card_json", strings.Replace(sdkStreamingCard, `"streaming_mode":true`, `"streaming_mode":false`, 1))
			msg := sdkCardMessage(t, client, cfg.Chats[0].ChatID, sdkEntityReference(id))
			messageID := value(msg.MessageId)
			result, err := srv.TriggerCardAction(t.Context(), event.CardActionTriggerInput{
				MessageID: messageID, OperatorOpenID: cfg.Users[0].OpenID,
				Action: callback.CallBackAction{Tag: "button", Value: map[string]any{"action": "confirm"}},
			})
			requireSDKNetwork(t, err)
			if result.Delivery.Channel != channel || result.Delivery.Status != http.StatusOK || !result.Updated ||
				result.Toast == nil || result.Toast.Content != "Confirmed" || result.Card == nil {
				t.Fatalf("invalid callback result: %+v", result)
			}
			incoming := receiveSDKEvent(t, events)
			assertSDKEventHeader(t, incoming.EventV2Base, app, "card.action.trigger")
			if incoming.Event == nil || incoming.Event.Operator == nil || incoming.Event.Operator.OpenID != cfg.Users[0].OpenID ||
				incoming.Event.Action == nil || incoming.Event.Action.Value["action"] != "confirm" || incoming.Event.Context == nil ||
				incoming.Event.Context.OpenMessageID != messageID || incoming.Event.Context.OpenChatID != cfg.Chats[0].ChatID {
				t.Fatalf("incorrect SDK callback payload: %+v", incoming.Event)
			}
			want := "clicked via SDK"
			if channel == "webhook" {
				want = "callback_template"
			}
			got := sdkGetCardMessage(t, client, messageID, true)
			card, ok := srv.Store().Card(id)
			if !ok || !strings.Contains(string(card.JSON), want) || !strings.Contains(value(got.Body.Content), want) {
				t.Fatalf("callback update missing: %s, %s", card.JSON, value(got.Body.Content))
			}
		})
	}
}

func startSDKCardSocket(t *testing.T, web *httptest.Server, appID, secret string, d *dispatcher.EventDispatcher) {
	t.Helper()
	ready, stopped := make(chan struct{}), make(chan struct{})
	client := larkws.NewClient(appID, secret, larkws.WithDomain(web.URL), larkws.WithHttpClient(web.Client()), larkws.WithEventHandler(d),
		larkws.WithAutoReconnect(false), larkws.WithOnReady(func() { close(ready) }))
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
			t.Error("SDK card socket did not stop")
		}
	})
	select {
	case <-ready:
	case <-stopped:
		t.Fatalf("SDK card socket failed: %v", startErr)
	case <-time.After(4 * time.Second):
		t.Fatal("SDK card socket did not become ready")
	}
}
