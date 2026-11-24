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

func TestOfficialSDKCardKitIMReadModesAndOwnership(t *testing.T) {
	cfg := sdkTestConfig()
	other := cfg.Apps[0]
	other.AppID, other.BotOpenID = protocol.NewID("cli_"), "ou_reader_app"
	cfg.Apps = append(cfg.Apps, other)
	cfg.Chats[0].MemberIDs = append(cfg.Chats[0].MemberIDs, other.BotOpenID)
	srv, web := sdkEventServer(t, cfg, event.Options{})
	owner := sdkEventREST(web, cfg.Apps[0])
	foreign := sdkEventREST(web, other)
	chatID := cfg.Chats[0].ChatID
	doc := strings.Replace(sdkStreamingCard, `"body":`, `"header":{"title":{"tag":"plain_text","content":"Status"}},"body":`, 1)
	id := sdkCardEntity(t, owner, "card_json", doc)
	ref := sdkEntityReference(id)
	messageID := value(sdkCardMessage(t, owner, chatID, ref).MessageId)
	stream, err := owner.Cardkit.V1.CardElement.Content(t.Context(), larkcardkit.NewContentCardElementReqBuilder().CardId(id).ElementId("md").
		Body(larkcardkit.NewContentCardElementReqBodyBuilder().Sequence(1).Content("owner live content").Build()).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, stream.CodeError, 0)

	for _, tc := range []struct {
		name         string
		client       *lark.Client
		mode, expect string
		code         int
	}{
		{"owner default", owner, "", `{"elements":[[{"tag":"text","text":"owner live content"}]],"title":"Status"}`, 0},
		{"owner original", owner, "user_card_content", `"schema":"2.0"`, 0},
		{"foreign default", foreign, "", ref, 0},
		{"foreign original", foreign, "user_card_content", ref, 0},
		{"unknown mode", owner, "bogus", "", 99992402},
	} {
		t.Run(tc.name, func(t *testing.T) {
			get, err := tc.client.Im.Message.Get(t.Context(), larkim.NewGetMessageReqBuilder().MessageId(messageID).CardMsgContentType(tc.mode).Build())
			requireSDKNetwork(t, err)
			requireSDKCode(t, get.CodeError, tc.code)
			list, err := tc.client.Im.Message.List(t.Context(), larkim.NewListMessageReqBuilder().ContainerIdType("chat").
				ContainerId(chatID).CardMsgContentType(tc.mode).Build())
			requireSDKNetwork(t, err)
			requireSDKCode(t, list.CodeError, tc.code)
			if tc.code != 0 {
				return
			}
			for _, items := range [][]*larkim.Message{get.Data.Items, list.Data.Items} {
				if len(items) != 1 || value(items[0].MessageId) != messageID || items[0].Body == nil {
					t.Fatalf("unexpected message result: %+v", items)
				}
				content := value(items[0].Body.Content)
				if tc.client == owner && tc.mode == "user_card_content" {
					if !strings.Contains(content, tc.expect) || !strings.Contains(content, `"content":"owner live content"`) {
						t.Fatalf("wrong original card content: %s", content)
					}
				} else if content != tc.expect {
					t.Fatalf("content = %s, want %s", content, tc.expect)
				}
			}
		})
	}

	patch, err := owner.Im.Message.Patch(t.Context(), larkim.NewPatchMessageReqBuilder().MessageId(messageID).
		Body(larkim.NewPatchMessageReqBodyBuilder().Content(sdkStreamingCard).Build()).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, patch.CodeError, 230001)
	message, _ := srv.Store().Messages().Get(messageID)
	card, _ := srv.Store().Card(id)
	if message.CardID != id || message.Content != ref || message.Updated || card.Sequence != 1 || card.Elements["md"] != "owner live content" {
		t.Fatalf("forbidden IM patch altered state: %+v, %+v", message, card)
	}
}

func TestOfficialSDKCardActionRejectsStaleRESTOverwrite(t *testing.T) {
	cfg := sdkTestConfig()
	received, release := make(chan struct{}), make(chan struct{}, 1)
	handler := dispatcher.NewEventDispatcher(cfg.Apps[0].VerificationToken, cfg.Apps[0].EncryptKey).
		OnP2CardActionTrigger(func(ctx context.Context, _ *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
			close(received)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &callback.CardActionTriggerResponse{Card: &callback.Card{Type: "raw", Data: json.RawMessage(sdkStreamingCard)}}, nil
		})
	hook := httptest.NewServer(http.HandlerFunc(httpserverext.NewEventHandlerFunc(handler)))
	t.Cleanup(hook.Close)
	cfg.Apps[0].WebhookURL = hook.URL
	srv, web := sdkEventServer(t, cfg, event.Options{})
	client := sdkEventREST(web, cfg.Apps[0])
	id := sdkCardEntity(t, client, "card_json", strings.Replace(sdkStreamingCard, `"streaming_mode":true`, `"streaming_mode":false`, 1))
	messageID := value(sdkCardMessage(t, client, cfg.Chats[0].ChatID, sdkEntityReference(id)).MessageId)
	type outcome struct {
		result event.CardActionResult
		err    error
	}
	done := make(chan outcome, 1)
	defer close(release)
	go func() {
		result, err := srv.TriggerCardAction(t.Context(), event.CardActionTriggerInput{MessageID: messageID, OperatorOpenID: cfg.Users[0].OpenID})
		done <- outcome{result, err}
	}()
	select {
	case <-received:
	case result := <-done:
		t.Fatalf("callback completed before receiving the request: %v", result.err)
	case <-time.After(2 * time.Second):
		t.Fatal("callback did not reach SDK webhook")
	}
	patch, err := client.Cardkit.V1.CardElement.Patch(t.Context(), larkcardkit.NewPatchCardElementReqBuilder().CardId(id).ElementId("md").
		Body(larkcardkit.NewPatchCardElementReqBodyBuilder().Sequence(12).PartialElement(`{"content":"REST update wins"}`).Build()).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, patch.CodeError, 0)
	// Release the callback only after the REST write has committed.
	release <- struct{}{}
	select {
	case attempt := <-done:
		if attempt.err == nil || !strings.Contains(attempt.err.Error(), "card entity changed") || attempt.result.Updated {
			t.Fatalf("stale callback update was not rejected: %+v", attempt)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("callback did not complete")
	}
	card, _ := srv.Store().Card(id)
	got := sdkGetCardMessage(t, client, messageID, false)
	if card.Sequence != 12 || card.Elements["md"] != "REST update wins" || !strings.Contains(value(got.Body.Content), "REST update wins") {
		t.Fatalf("REST update was lost: card=%+v message=%s", card, value(got.Body.Content))
	}
}

func TestOfficialSDKCardActionTransitionConstraints(t *testing.T) {
	cfg := sdkTestConfig()
	responses := make(chan *callback.CardActionTriggerResponse, 1)
	handler := dispatcher.NewEventDispatcher(cfg.Apps[0].VerificationToken, cfg.Apps[0].EncryptKey).
		OnP2CardActionTrigger(func(ctx context.Context, _ *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
			select {
			case response := <-responses:
				return response, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		})
	hook := httptest.NewServer(http.HandlerFunc(httpserverext.NewEventHandlerFunc(handler)))
	t.Cleanup(hook.Close)
	cfg.Apps[0].WebhookURL = hook.URL
	srv, web := sdkEventServer(t, cfg, event.Options{})
	client := sdkEventREST(web, cfg.Apps[0])
	id := sdkCardEntity(t, client, "card_json", sdkStreamingCard)
	linkedMessageID := value(sdkCardMessage(t, client, cfg.Chats[0].ChatID, sdkEntityReference(id)).MessageId)
	legacy := `{"elements":[{"tag":"markdown","content":"legacy"}]}`
	cardV2 := strings.Replace(sdkStreamingCard, `"streaming_mode":true`, `"streaming_mode":false`, 1)
	for _, tc := range []struct {
		name                  string
		disable               bool
		inline, data, failure string
	}{
		{"streaming entity replacement", false, "", sdkStreamingCard, "streaming mode"},
		{"toast only on streaming entity", false, "", "", ""},
		{"legacy raw entity replacement", true, "", legacy, "cannot be replaced"},
		{"inline 2.0 to 1.0", false, cardV2, legacy, "2.0 cannot be replaced with card JSON 1.0"},
		{"inline 1.0 to 2.0", false, legacy, cardV2, "1.0 cannot be replaced with card JSON 2.0"},
		{"inline streaming replacement", false, sdkStreamingCard, cardV2, "streaming mode"},
		{"toast only on inline streaming card", false, sdkStreamingCard, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			messageID := linkedMessageID
			if tc.inline != "" {
				messageID = value(sdkCardMessage(t, client, cfg.Chats[0].ChatID, tc.inline).MessageId)
			}
			if tc.disable {
				settings, err := client.Cardkit.V1.Card.Settings(t.Context(), larkcardkit.NewSettingsCardReqBuilder().CardId(id).
					Body(larkcardkit.NewSettingsCardReqBodyBuilder().Sequence(1).Settings(`{"config":{"streaming_mode":false}}`).Build()).Build())
				requireSDKNetwork(t, err)
				requireSDKCode(t, settings.CodeError, 0)
			}
			before, _ := srv.Store().Card(id)
			reply := &callback.CardActionTriggerResponse{Toast: &callback.Toast{Type: "info", Content: "Clicked"}}
			if tc.data != "" {
				reply.Card = &callback.Card{Type: "raw", Data: json.RawMessage(tc.data)}
			}
			responses <- reply
			result, err := srv.TriggerCardAction(t.Context(), event.CardActionTriggerInput{MessageID: messageID, OperatorOpenID: cfg.Users[0].OpenID})
			if tc.failure == "" {
				requireSDKNetwork(t, err)
			} else if err == nil || !strings.Contains(err.Error(), tc.failure) {
				t.Fatalf("got %v, want %q", err, tc.failure)
			}
			if tc.inline != "" {
				detail, err := client.Im.Message.Get(t.Context(), larkim.NewGetMessageReqBuilder().MessageId(messageID).
					CardMsgContentType("user_card_content").Build())
				requireSDKNetwork(t, err)
				requireSDKCode(t, detail.CodeError, 0)
				if len(detail.Data.Items) != 1 || value(detail.Data.Items[0].Updated) ||
					value(detail.Data.Items[0].Body.Content) != tc.inline {
					t.Fatalf("inline callback changed the original message: %s", detail.RawBody)
				}
			}
			after, _ := srv.Store().Card(id)
			if result.Updated || result.Toast == nil || result.Toast.Content != "Clicked" ||
				after.Revision != before.Revision || after.Sequence != before.Sequence || string(after.JSON) != string(before.JSON) {
				t.Fatalf("invalid SDK transition: %+v, %+v", result, after)
			}
		})
	}
}

func TestOfficialSDKCardRenderingSelectorsAndLinks(t *testing.T) {
	cfg := sdkTestConfig()
	_, web := sdkEventServer(t, cfg, event.Options{})
	client := sdkEventREST(web, cfg.Apps[0])
	chatID := cfg.Chats[0].ChatID
	const original = `{"schema":"2.0","header":{"title":{"tag":"plain_text","content":"Resources"}},"body":{"elements":[
		{"tag":"select_static","placeholder":{"tag":"plain_text","content":"Choose a service"},"options":[
			{"text":{"tag":"plain_text","content":"Cards"},"value":"cardkit"},
			{"text":{"tag":"plain_text","content":"Messages"},"value":"im"}]},
		{"tag":"markdown","content":"Read [Cards](https://example.test/wiki/Card_(Lark)) and [Messages](https://example.test/im)."},
		{"tag":"div","text":{"tag":"lark_md","content":"[More](https://example.test/more)"}},
		{"tag":"plain_text","content":"[Literal](https://example.test/literal)"}
	]}}`
	const rendered = `{"elements":[[{"options":["Cards","Messages"],"placeholder":"Choose a service","tag":"select_static"}],[{"tag":"text","text":"Read "},{"href":"https://example.test/wiki/Card_(Lark)","tag":"a","text":"Cards"},{"tag":"text","text":" and "},{"href":"https://example.test/im","tag":"a","text":"Messages"},{"tag":"text","text":"."}],[{"href":"https://example.test/more","tag":"a","text":"More"}],[{"tag":"text","text":"[Literal](https://example.test/literal)"}]],"title":"Resources"}`
	messageID := value(sdkCardMessage(t, client, chatID, original).MessageId)
	for _, tc := range []struct{ mode, expect string }{{"", rendered}, {"user_card_content", original}} {
		detail, err := client.Im.Message.Get(t.Context(), larkim.NewGetMessageReqBuilder().MessageId(messageID).
			CardMsgContentType(tc.mode).Build())
		requireSDKNetwork(t, err)
		requireSDKCode(t, detail.CodeError, 0)
		history, err := client.Im.Message.List(t.Context(), larkim.NewListMessageReqBuilder().ContainerIdType("chat").
			ContainerId(chatID).CardMsgContentType(tc.mode).Build())
		requireSDKNetwork(t, err)
		requireSDKCode(t, history.CodeError, 0)
		for _, items := range [][]*larkim.Message{detail.Data.Items, history.Data.Items} {
			if len(items) != 1 || value(items[0].MessageId) != messageID || items[0].Body == nil {
				t.Fatalf("unexpected card read for %q: %+v", tc.mode, items)
			}
			if actual := value(items[0].Body.Content); actual != tc.expect {
				t.Fatalf("card mode %q content = %s, want %s", tc.mode, actual, tc.expect)
			}
		}
	}
}
