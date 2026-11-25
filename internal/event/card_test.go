package event

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
	"github.com/littlecrabzzz-alt/larkbox/internal/wsproto"
)

const replyCredential = "test-private-callback-response"

const replyDiagnostics = `{"note":{"token":"hidden-duplicate"},"note":"display data","items":[{"preview_token":"nested-private"}]}`

const (
	initialCard = `{"schema":"2.0","body":{"elements":[{"tag":"markdown","element_id":"md","content":"initial"}]}}`
	clickedCard = `{"schema":"2.0","body":{"elements":[{"tag":"markdown","element_id":"md","content":"clicked"}]}}`
)

func actionMessage(t *testing.T, d *Dispatcher, cfg config.Config) (store.Message, CardActionTriggerInput) {
	t.Helper()
	app := cfg.Apps[0]
	card := store.Card{ID: protocol.NewID(protocol.CardID), AppID: app.AppID, Type: "card_json", JSON: []byte(initialCard), Sequence: 7}
	requireNoError(t, d.state.CreateCard(card))
	message, _, err := d.state.CreateIMMessage(store.IMActor{AppID: app.AppID, OpenID: app.BotOpenID}, store.NewMessage{
		ChatID: cfg.Chats[1].ChatID, MsgType: "interactive", CardID: card.ID, Content: `{"type":"card","data":{"card_id":"` + card.ID + `"}}`,
	})
	requireNoError(t, err)
	return message, CardActionTriggerInput{MessageID: message.ID, OperatorOpenID: cfg.Users[0].OpenID,
		Action: callback.CallBackAction{Tag: "button", Name: "submit", Value: map[string]any{"click": true}, Option: "opt", Timezone: "UTC", FormValue: map[string]any{"field": "answer"}, InputValue: "input", Options: []string{"opt"}, Checked: true},
	}
}

func TestCardActionWebhook(t *testing.T) {
	for _, tc := range []struct {
		name, kind, data   string
		encrypted, invalid bool
	}{
		{"plaintext raw", "raw", clickedCard, false, false},
		{"encrypted template", "template", `{"template_id":"updated","template_variable":{"count":2}}`, true, false},
		{"invalid raw", "raw", `{"schema":"2.0","body":{"elements":[{"tag":"markdown","content":false}]}}`, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			app := &cfg.Apps[0]
			app.TenantKey = "tenant-app"
			cfg.Users[0].TenantKey = "tenant-operator"
			if !tc.encrypted {
				app.EncryptKey = ""
			}
			var attempts atomic.Int32
			received := make(chan []byte, 1)
			web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if app.EncryptKey != "" {
					expected := protocol.WebhookSignature(r.Header.Get("X-Lark-Request-Timestamp"), r.Header.Get("X-Lark-Request-Nonce"), app.EncryptKey, raw)
					if r.Header.Get("X-Lark-Signature") != expected {
						t.Error("signature mismatch")
					}
					raw, err = protocol.DecryptWebhook(app.EncryptKey, raw)
					if err != nil {
						t.Error(err)
					}
				}
				select {
				case received <- raw:
				default:
				}
				var wire callback.CardActionTriggerEvent
				if err := json.Unmarshal(raw, &wire); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				echo, _ := json.Marshal(wire.Event.Token + " / " + wire.Event.Context.PreviewToken + " / " + wire.EventV2Base.Header.Token)
				_, _ = io.WriteString(w, `{"token":"`+replyCredential+`","echo":`+string(echo)+`,"diagnostics":`+replyDiagnostics+`,"toast":{"type":"success","content":"Saved","i18n":{"en_us":"Saved"}},"card":{"type":"`+tc.kind+`","data":`+tc.data+`}}`)
			}))
			t.Cleanup(web.Close)
			app.WebhookURL = web.URL + "/card-click"
			stamp := time.Unix(1_700_000_000, 123456000)
			d := testDispatcher(t, cfg, Options{RetryBackoff: noBackoff}, func() time.Time { return stamp })
			m, input := actionMessage(t, d, cfg)
			input.DeliveryType, input.URL, input.PreviewToken = "url_preview", "https://example.test", "unique-card-preview-credential"
			before, _ := d.state.Messages().Get(m.ID)
			result, err := d.TriggerCardAction(t.Context(), input)
			trace := cardTraces(d.state, result.EventID)
			current, _ := d.state.Messages().Get(m.ID)
			card, _ := d.state.Card(m.CardID)
			if attempts.Load() != 1 || len(trace) != 1 || trace[0].Status != 200 || trace[0].Channel != "webhook" ||
				trace[0].Attempt != 1 || trace[0].Path != "/card-click" || !strings.Contains(string(trace[0].ResponseBody), "Saved") {
				t.Fatalf("invalid delivery trace: %+v", trace)
			}
			payload := <-received
			var incoming callback.CardActionTriggerEvent
			requireNoError(t, json.Unmarshal(payload, &incoming))
			if incoming.EventV2Base == nil || incoming.Schema != "2.0" || incoming.EventV2Base.Header == nil || incoming.EventV2Base.Header.EventType != cardActionTrigger ||
				incoming.EventV2Base.Header.EventID != result.EventID || incoming.EventV2Base.Header.EventID == "" ||
				incoming.EventV2Base.Header.AppID != app.AppID || incoming.EventV2Base.Header.Token != app.VerificationToken ||
				incoming.EventV2Base.Header.TenantKey != app.TenantKey || incoming.EventV2Base.Header.CreateTime != strconv.FormatInt(stamp.UnixMicro(), 10) || incoming.Event == nil {
				t.Fatalf("invalid event: %s", payload)
			}
			e := incoming.Event
			if e.Host != "im_message" || e.Token == "" || e.Token == incoming.EventV2Base.Header.Token || e.Operator == nil || e.Operator.OpenID != input.OperatorOpenID ||
				e.Operator.TenantKey == nil || *e.Operator.TenantKey != cfg.Users[0].TenantKey || e.Operator.UserID == nil || *e.Operator.UserID != cfg.Users[0].UserID ||
				!strings.Contains(string(payload), cfg.Users[0].UnionID) || e.Action == nil || e.Action.Value["click"] != true || !e.Action.Checked ||
				e.Action.Tag != "button" || e.Action.Option != "opt" || e.Action.Timezone != "UTC" || e.Action.Name != "submit" ||
				e.Action.FormValue["field"] != "answer" || e.Action.InputValue != "input" || len(e.Action.Options) != 1 ||
				e.Context == nil || e.Context.OpenMessageID != m.ID || e.Context.OpenChatID != m.ChatID || e.DeliveryType != "url_preview" ||
				e.Context.PreviewToken != input.PreviewToken || e.Context.URL != "https://example.test" {
				t.Fatalf("invalid callback payload: %s", payload)
			}
			assertCardAuditTokens(t, trace[0].RequestBody, trace[0].ResponseBody, result.Delivery.Data,
				app.VerificationToken, e.Token, e.Context.PreviewToken)
			if !strings.Contains(string(trace[0].ResponseBody), `"echo":"[REDACTED] / [REDACTED] / [REDACTED]"`) ||
				!strings.Contains(string(result.Delivery.Data), e.Context.PreviewToken) {
				t.Fatal("reflected webhook credentials were not isolated from the real response")
			}
			if result.Toast == nil || result.Toast.I18nContent["en_us"] != "Saved" || result.Card == nil || result.Card.Type != tc.kind || card.Sequence != 7 {
				t.Fatalf("invalid structured response or sequence: %+v, %+v", result, card)
			}
			if tc.invalid {
				if err == nil || result.Updated || current.Content != before.Content || current.Updated || !current.UpdatedAt.Equal(before.UpdatedAt) ||
					string(card.JSON) != initialCard || !strings.Contains(trace[0].Error, "invalid card action response card") {
					t.Fatalf("invalid update was not isolated: %+v, %v, %+v", result, err, current)
				}
				return
			}
			requireNoError(t, err)
			if !result.Updated || !current.Updated || current.UpdatedAt.Before(before.UpdatedAt) || current.CardID != m.CardID ||
				(tc.kind == "raw" && card.Elements["md"] != "clicked") ||
				(tc.kind == "template" && (!strings.Contains(string(card.JSON), `"template_id":"updated"`) || card.Type != "template")) || trace[0].Error != "" {
				t.Fatalf("update was not persisted: %+v, %+v, %+v", result, current, card)
			}
		})
	}
}

func TestCardActionWebSocketSingleConsumer(t *testing.T) {
	cfg := config.Default()
	var fallback atomic.Int32
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallback.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(webhook.Close)
	cfg.Apps[0].WebhookURL = webhook.URL
	d := testDispatcher(t, cfg, Options{AckTimeout: time.Second, RetryBackoff: noBackoff}, nil)
	web := testGateway(t, d)
	host := strings.TrimPrefix(web.URL, "http://")
	conn := connect(t, ticketURL(t, d, host, cfg.Apps[0]), http.StatusSwitchingProtocols)
	message, input := actionMessage(t, d, cfg)
	input.PreviewToken = "test-private-preview-token"
	type actionReply struct {
		result CardActionResult
		err    error
	}
	replies := make(chan actionReply, 1)
	for _, status := range []int{http.StatusCreated, http.StatusInternalServerError, http.StatusOK} {
		before, _ := d.state.Card(message.CardID)
		go func() {
			result, err := d.TriggerCardAction(t.Context(), input)
			replies <- actionReply{result, err}
		}()
		frame := readDataFrame(t, conn)
		if wsproto.HeaderValue(frame, wsproto.Type) != wsproto.Event || !strings.Contains(string(frame.Payload), cardActionTrigger) {
			t.Fatalf("invalid callback frame: %+v", frame)
		}
		body := []byte(`{"token":"` + replyCredential + `","diagnostics":` + replyDiagnostics + `,"toast":{"type":"info","content":"wire"},"card":{"type":"raw","data":` + clickedCard + `}}`)
		writeAck(t, conn, frame, wsproto.Response{StatusCode: status, Data: body})
		reply := <-replies
		trace := cardTraces(d.state, reply.result.EventID)
		if len(trace) != 1 || trace[0].Channel != "websocket" || trace[0].Status != status || trace[0].Attempt != 1 || fallback.Load() != 0 {
			t.Fatalf("callback delivered more than once or with wrong status: %+v, %d", trace, fallback.Load())
		}
		var ack wsproto.Response
		requireNoError(t, json.Unmarshal(trace[0].ResponseBody, &ack))
		var wire struct {
			Event  cardActionPayload `json:"event"`
			Header eventHeader       `json:"header"`
		}
		requireNoError(t, json.Unmarshal(frame.Payload, &wire))
		incoming := wire.Event
		if wire.Header.Token != cfg.Apps[0].VerificationToken || incoming.Context.PreviewToken != input.PreviewToken ||
			incoming.Token == "" || strings.Contains(string(frame.Payload), `"delivery_type"`) ||
			string(reply.result.Delivery.Data) != string(body) {
			t.Fatalf("unexpected WebSocket wire payload or ACK: %s, %s", frame.Payload, reply.result.Delivery.Data)
		}
		assertCardAuditTokens(t, trace[0].RequestBody, ack.Data, reply.result.Delivery.Data,
			wire.Header.Token, incoming.Token, incoming.Context.PreviewToken)
		if status == http.StatusOK {
			requireNoError(t, reply.err)
			if !reply.result.Updated || reply.result.Toast == nil || reply.result.Toast.Content != "wire" {
				t.Fatalf("invalid callback response: %+v", reply.result)
			}
		} else {
			after, _ := d.state.Card(message.CardID)
			stored, _ := d.state.Messages().Get(message.ID)
			if reply.err == nil || reply.result.Updated || trace[0].Error == "" ||
				stored.Updated || after.Revision != before.Revision || string(after.JSON) != string(before.JSON) {
				t.Fatalf("callback ACK %d changed the message or card: %+v, %+v", status, reply, after)
			}
		}
	}
}

func TestCardActionTimeoutAndTargetValidation(t *testing.T) {
	cfg := config.Default()
	var count atomic.Int32
	web := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		count.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
			t.Error("timed out callback was not canceled")
		}
	}))
	t.Cleanup(web.Close)
	cfg.Apps[0].WebhookURL = web.URL
	d := testDispatcher(t, cfg, Options{WebhookTimeout: 100 * time.Millisecond, RetryBackoff: noBackoff}, nil)
	m, input := actionMessage(t, d, cfg)
	bad := input
	bad.MessageID = "om_missing"
	if _, err := d.TriggerCardAction(t.Context(), bad); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing target: %v", err)
	}
	bad = input
	bad.OperatorOpenID = cfg.Users[1].OpenID
	if _, err := d.TriggerCardAction(t.Context(), bad); !errors.Is(err, store.ErrOutsideChat) {
		t.Fatalf("outside operator: %v", err)
	}
	var text = m
	text.ID, text.MsgType = "om_text", "text"
	d.state.Messages().Put(text)
	bad = input
	bad.MessageID = text.ID
	if _, err := d.TriggerCardAction(t.Context(), bad); err == nil {
		t.Fatal("accepted text target")
	}
	m.Deleted = true
	d.state.Messages().Put(m)
	if _, err := d.TriggerCardAction(t.Context(), input); !errors.Is(err, store.ErrMessageDeleted) {
		t.Fatalf("recalled target: %v", err)
	}
	m.Deleted = false
	d.state.Messages().Put(m)

	result, err := d.TriggerCardAction(t.Context(), input)
	trace := cardTraces(d.state, result.EventID)
	if !errors.Is(err, context.DeadlineExceeded) || count.Load() != 1 || result.Updated || len(trace) != 1 || trace[0].Attempt != 1 ||
		!strings.Contains(trace[0].Error, "deadline exceeded") {
		t.Fatalf("invalid timeout behavior: %+v, %v, %+v, count=%d", result, err, trace, count.Load())
	}
}

func TestCardActionReferenceOwnership(t *testing.T) {
	cfg := config.Default()
	var responseID atomic.Value
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"card":{"type":"card","data":{"card_id":"`+responseID.Load().(string)+`"}}}`)
	}))
	t.Cleanup(web.Close)
	cfg.Apps[0].WebhookURL = web.URL
	d := testDispatcher(t, cfg, Options{}, nil)
	m, input := actionMessage(t, d, cfg)
	card := store.Card{ID: "1", AppID: "cli_other", Type: "card_json", JSON: []byte(clickedCard)}
	requireNoError(t, d.state.CreateCard(card))
	responseID.Store(card.ID)
	result, err := d.TriggerCardAction(t.Context(), input)
	if !errors.Is(err, store.ErrCardOwner) || result.Updated {
		t.Fatalf("foreign callback card was accepted: %+v, %v", result, err)
	}
	card.ID, card.AppID = "2", cfg.Apps[0].AppID
	requireNoError(t, d.state.CreateCard(card))
	responseID.Store(card.ID)
	result, err = d.TriggerCardAction(t.Context(), input)
	requireNoError(t, err)
	current, _ := d.state.Messages().Get(m.ID)
	previous, _ := d.state.Card(m.CardID)
	if !result.Updated || current.CardID != card.ID || current.Content != `{"type":"card","data":{"card_id":"2"}}` || string(previous.JSON) != initialCard {
		t.Fatalf("invalid relink: %+v, %+v, %+v", result, current, previous)
	}
}

func cardTraces(s *store.Store, id string) []store.Trace {
	return slices.DeleteFunc(s.Traces(), func(trace store.Trace) bool { return trace.EventID != id })
}

func assertCardAuditTokens(t *testing.T, request, response, actual []byte, sensitive ...string) {
	t.Helper()
	for _, secret := range sensitive {
		if strings.Contains(string(request), secret) {
			t.Fatal("outbound audit request retained a token")
		}
	}
	if strings.Count(string(request), `"[REDACTED]"`) != len(sensitive) ||
		strings.Contains(string(response), replyCredential) || !strings.Contains(string(response), `"token":"[REDACTED]"`) ||
		!strings.Contains(string(actual), replyCredential) || !strings.Contains(string(actual), "hidden-duplicate") ||
		strings.Contains(string(response), "hidden-duplicate") || strings.Contains(string(response), "nested-private") ||
		!strings.Contains(string(response), `"preview_token":"[REDACTED]"`) || !strings.Contains(string(response), "display data") {
		t.Fatalf("request or response token was improperly redacted: request=%s response=%s", request, response)
	}
}

func TestCardActionTransitionAndHTTPStatus(t *testing.T) {
	for _, tc := range []struct {
		name, reply, failure string
		streaming            bool
		status               int
	}{
		{"streaming replacement", `{"toast":{"content":"accepted"},"card":{"type":"raw","data":` + clickedCard + `}}`, "streaming mode", true, http.StatusOK},
		{"streaming toast only", `{"toast":{"content":"accepted"}}`, "", true, http.StatusOK},
		{"legacy replacement", `{"toast":{"content":"accepted"},"card":{"type":"raw","data":{"elements":[{"tag":"markdown","content":"legacy"}]}}}`, "cannot be replaced", false, http.StatusOK},
		{"unexpected success status", `{"card":{"type":"raw","data":` + clickedCard + `}}`, "status 201", false, http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.reply)
			}))
			t.Cleanup(web.Close)
			cfg.Apps[0].WebhookURL = web.URL
			d := testDispatcher(t, cfg, Options{}, nil)
			message, input := actionMessage(t, d, cfg)
			if tc.streaming {
				card, _ := d.state.Card(message.CardID)
				card.Sequence++
				card.StreamingMode = true
				card.JSON = []byte(strings.Replace(initialCard, `"schema":"2.0",`, `"schema":"2.0","config":{"streaming_mode":true},`, 1))
				requireNoError(t, d.state.UpdateCard(card))
			}
			before, _ := d.state.Card(message.CardID)
			result, err := d.TriggerCardAction(t.Context(), input)
			switch {
			case tc.failure == "":
				requireNoError(t, err)
			case err == nil || !strings.Contains(err.Error(), tc.failure):
				t.Fatalf("got %v, want %q", err, tc.failure)
			}
			if tc.status == http.StatusOK && (result.Toast == nil || result.Toast.Content != "accepted") {
				t.Fatalf("toast was lost: %+v", result)
			}
			card, _ := d.state.Card(message.CardID)
			stored, _ := d.state.Messages().Get(message.ID)
			if result.Updated || stored.Updated || card.Sequence != before.Sequence || card.Revision != before.Revision || string(card.JSON) != string(before.JSON) {
				t.Fatalf("callback unexpectedly changed card: %+v", card)
			}
		})
	}
}
