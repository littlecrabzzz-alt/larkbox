package sdk_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkauth "github.com/larksuite/oapi-sdk-go/v3/service/auth/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/littlecrabzzz-alt/larkbox/internal/event"
)

func sandboxRequest(t *testing.T, web *httptest.Server, method, path string, payload any, status int) []byte {
	t.Helper()
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, web.URL+"/_sandbox/"+path, body)
	requireSDKNetwork(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := web.Client().Do(req)
	requireSDKNetwork(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	requireSDKNetwork(t, err)
	if resp.StatusCode != status || !json.Valid(raw) {
		t.Fatalf("%s %s = %d, want %d and JSON: %s", method, path, resp.StatusCode, status, raw)
	}
	return raw
}

func sandboxDecode[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestOfficialSDKSandbox(t *testing.T) {
	const clickedCard = `{"schema":"2.0","body":{"elements":[{"tag":"markdown","element_id":"result","content":"Clicked via sandbox"}]}}`
	cfg := sdkTestConfig()
	cfg.Apps[0].TenantKey = "sdk-sandbox"
	app, chat, user := cfg.Apps[0], cfg.Chats[0], cfg.Users[0]
	_, web := sdkEventServer(t, cfg, event.Options{WebSocketMaxAttempts: 1})
	client := sdkEventREST(web, app)
	cardID := sdkCardEntity(t, client, "card_json", strings.Replace(sdkStreamingCard, `"streaming_mode":true`, `"streaming_mode":false`, 1))
	inbox := make(chan *larkim.P2MessageReceiveV1, 4)
	replies := make(chan string, 1)
	actions := make(chan *callback.CardActionTriggerEvent, 1)
	handler := dispatcher.NewEventDispatcher(app.VerificationToken, app.EncryptKey).
		OnP2MessageReceiveV1(func(ctx context.Context, incoming *larkim.P2MessageReceiveV1) error {
			inbox <- incoming
			if incoming.Event == nil || incoming.Event.Message == nil ||
				!strings.Contains(value(incoming.Event.Message.Content), "reply please") {
				return nil
			}
			result, err := client.Im.Message.Reply(ctx, larkim.NewReplyMessageReqBuilder().
				MessageId(value(incoming.Event.Message.MessageId)).Body(larkim.NewReplyMessageReqBodyBuilder().
				MsgType("interactive").Content(sdkEntityReference(cardID)).Build()).Build())
			if err != nil {
				return err
			}
			if !result.Success() || result.Data == nil {
				return fmt.Errorf("SDK reply failed: %s", result.RawBody)
			}
			replies <- value(result.Data.MessageId)
			return nil
		}).
		OnP2CardActionTrigger(func(_ context.Context, incoming *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
			actions <- incoming
			return &callback.CardActionTriggerResponse{
				Toast: &callback.Toast{Type: "success", Content: "Confirmed"},
				Card:  &callback.Card{Type: "raw", Data: json.RawMessage(clickedCard)},
			}, nil
		})
	startSDKCardSocket(t, web, app.AppID, app.AppSecret, handler)
	request := func(method, path string, body any, status int) []byte {
		return sandboxRequest(t, web, method, path, body, status)
	}
	send := func(text string, mention bool) []byte {
		return request(http.MethodPost, "messages/send", map[string]any{
			"chat_id": chat.ChatID, "sender_open_id": user.OpenID, "text": text, "at_bot": mention,
		}, http.StatusCreated)
	}
	sent := send("@bot reply please", true)
	incoming := receiveSDKEvent(t, inbox)
	messageID := value(incoming.Event.Message.MessageId)
	eventID := assertSDKEventHeader(t, incoming.EventV2Base, app, "im.message.receive_v1")
	mentions := incoming.Event.Message.Mentions
	if value(incoming.Event.Sender.SenderId.OpenId) != user.OpenID ||
		value(incoming.Event.Message.Content) != `{"text":"@_user_1 reply please"}` ||
		len(mentions) != 1 || value(mentions[0].Key) != "@_user_1" || value(mentions[0].Id.OpenId) != app.BotOpenID ||
		!bytes.Contains(sent, []byte(messageID)) || !bytes.Contains(sent, []byte(eventID)) || !bytes.Contains(sent, []byte(`"status_code":200`)) {
		t.Fatalf("incorrect SDK mention or HTTP delivery: %+v %s", incoming.Event, sent)
	}

	sessions := request(http.MethodGet, "ws/sessions", nil, http.StatusOK)
	for _, field := range []string{"session_id", "device_id", "service_id", "connected_at", app.AppID} {
		if !bytes.Contains(sessions, []byte(field)) {
			t.Fatalf("missing %q in the active session: %s", field, sessions)
		}
	}

	replyID := receiveSDKEvent(t, replies)
	click := request(http.MethodPost, "cards/action", map[string]any{
		"message_id": replyID, "operator_open_id": user.OpenID, "action": map[string]any{"tag": "button", "value": map[string]any{"confirm": true}},
	}, http.StatusOK)
	action := receiveSDKEvent(t, actions)
	callbackID := assertSDKEventHeader(t, action.EventV2Base, app, "card.action.trigger")
	if action.Event.Context.OpenMessageID != replyID || action.Event.Action.Value["confirm"] != true ||
		!bytes.Contains(click, []byte(`"card_applied":true`)) || !bytes.Contains(click, []byte("Confirmed")) ||
		!bytes.Contains(click, []byte(`"content":"Clicked via sandbox"`)) || !bytes.Contains(click, []byte(callbackID)) {
		t.Fatalf("unexpected card action: %+v %s", action.Event, click)
	}
	history := request(http.MethodGet, "chats/"+chat.ChatID+"/messages", nil, http.StatusOK)
	detail := request(http.MethodGet, "cards/"+cardID, nil, http.StatusOK)
	if !bytes.Contains(history, []byte(`"raw_card_json":{"body"`)) || !bytes.Contains(history, []byte(`"rendered_card_json":`)) ||
		!bytes.Contains(history, []byte(`"parent_id":"`+messageID+`"`)) || !bytes.Contains(detail, []byte(`"content":"Clicked via sandbox"`)) {
		t.Fatalf("updated card was not persisted: %s %s", history, detail)
	}

	traces := request(http.MethodGet, "traces", nil, http.StatusOK)
	for _, want := range []string{`"direction":"inbound"`, `"direction":"outbound"`, `"channel":"websocket"`,
		eventID, callbackID, "/open-apis/im/v1/messages/" + messageID + "/reply", "[REDACTED]"} {
		if !bytes.Contains(traces, []byte(want)) {
			t.Fatalf("missing %q from traces: %s", want, traces)
		}
	}
	for _, secret := range []string{app.AppSecret, app.VerificationToken, app.EncryptKey, action.Event.Token, "Authorization"} {
		if bytes.Contains(traces, []byte(secret)) {
			t.Fatalf("audit trace exposed a credential or private header")
		}
	}

	// With the SDK token cache disabled, the expiry error reaches the caller
	// instead of triggering an automatic refresh and retry inside the SDK.
	faultClient := lark.NewClient(app.AppID, app.AppSecret, lark.WithOpenBaseUrl(web.URL),
		lark.WithHttpClient(web.Client()), lark.WithEnableTokenCache(false))
	tokenResponse, err := faultClient.Auth.TenantAccessToken.Internal(t.Context(), larkauth.NewInternalTenantAccessTokenReqBuilder().
		Body(larkauth.NewInternalTenantAccessTokenReqBodyBuilder().AppId(app.AppID).AppSecret(app.AppSecret).Build()).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, tokenResponse.CodeError, 0)
	token := sandboxDecode[struct {
		Token string `json:"tenant_access_token"`
	}](t, tokenResponse.RawBody).Token
	if token == "" {
		t.Fatal("missing SDK tenant token")
	}
	get := func(code, status int) {
		t.Helper()
		result, err := faultClient.Im.Message.Get(t.Context(), larkim.NewGetMessageReqBuilder().MessageId(replyID).Build(), larkcore.WithTenantAccessToken(token))
		requireSDKNetwork(t, err)
		requireSDKCode(t, result.CodeError, code)
		if result.StatusCode != status {
			t.Fatalf("SDK status = %d, want %d: %s", result.StatusCode, status, result.RawBody)
		}
	}
	for _, test := range []struct {
		kind   string
		code   int
		status int
	}{{"rate_limit", 99991400, 429}, {"http_error", 99991401, 500}, {"token_expiry", 99991663, 400}} {
		request(http.MethodPost, "faults", map[string]any{
			"kind": test.kind, "remaining_hits": 1, "app_id": app.AppID, "method": "GET",
			"path_prefix": "/open-apis/im/v1/messages/" + replyID, "channel": "inbound",
		}, http.StatusCreated)
		get(test.code, test.status)
		get(0, 200)
	}

	faultTraces := request(http.MethodGet, "traces?direction=inbound&method=GET&app_id="+app.AppID+"&limit=6", nil, http.StatusOK)
	for _, code := range []string{"99991400", "99991401", "99991663"} {
		if !bytes.Contains(faultTraces, []byte(`"code":`+code)) {
			t.Fatalf("fault code %s absent from traces: %s", code, faultTraces)
		}
	}

	request(http.MethodPost, "faults", map[string]any{
		"kind": "duplicate_event", "remaining_hits": 1, "app_id": app.AppID, "event_type": "im.message.receive_v1", "channel": "websocket",
	}, http.StatusCreated)
	send("deliver twice", false)
	first, second := receiveSDKEvent(t, inbox), receiveSDKEvent(t, inbox)
	if first.EventV2Base.Header.EventID != second.EventV2Base.Header.EventID || !reflect.DeepEqual(first, second) {
		t.Fatalf("duplicate payload changed: %+v versus %+v", first, second)
	}
	send("normal", false)
	normal := receiveSDKEvent(t, inbox)
	if normal.EventV2Base.Header.EventID == first.EventV2Base.Header.EventID {
		t.Fatal("new message reused the duplicate event ID")
	}
	type traceResult struct {
		Traces []json.RawMessage
	}
	for id, count := range map[string]int{first.EventV2Base.Header.EventID: 2, normal.EventV2Base.Header.EventID: 1} {
		outbound := request(http.MethodGet, "traces?direction=outbound&channel=websocket&event_type=im.message.receive_v1&event_id="+id, nil, http.StatusOK)
		if len(sandboxDecode[traceResult](t, outbound).Traces) != count {
			t.Fatalf("unexpected delivery count: %s", outbound)
		}
	}
	request(http.MethodPost, "faults", map[string]any{"kind": "latency", "delay_ms": 1}, http.StatusCreated)
	request(http.MethodPost, "reset", nil, http.StatusOK)
	for path, want := range map[string]string{
		"traces": `{"traces":[]}`, "faults": `{"faults":[]}`, "ws/sessions": `{"ws_sessions":[]}`, "cards": `{"cards":[]}`,
		"chats/" + chat.ChatID + "/messages": `{"messages":[]}`,
	} {
		if got := request(http.MethodGet, path, nil, http.StatusOK); string(got) != want {
			t.Fatalf("reset %s: %s, want %s", path, got, want)
		}
	}
}
