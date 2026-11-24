package sdk_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/larksuite/oapi-sdk-go/v3/core/httpserverext"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkcardkit "github.com/larksuite/oapi-sdk-go/v3/service/cardkit/v1"

	"github.com/littlecrabzzz-alt/larkbox/internal/event"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/tests/fixturetest"
)

var sensitiveFixtureValue = regexp.MustCompile(`(?i)(\bbearer[ \t]+[a-z0-9_-]{12,}|\b(?:t|a|u|ur)-[a-z0-9_-]{16,}|\b(?:cli|ou|on|oc|om|omt|omr|evt|img_v3|file_v3)_[a-z0-9_-]{20,}|\b(?:gh[pousr]_[a-z0-9]{20,}|github_pat_[a-z0-9_]{20,}|AKIA[A-Z0-9]{16})\b|-----BEGIN [A-Z ]*PRIVATE KEY-----|\beyJ[a-z0-9_-]+\.[a-z0-9_-]+\.[a-z0-9_-]+|[a-z0-9_.+%-]+@[a-z0-9.-]+\.[a-z]{2,}|\b(?:\+?86[ -]?)?1[3-9][0-9]{9}\b)`)
var sensitiveFixtureKey = regexp.MustCompile(`(?i)(?:secret|token|tenant[_-]?key|encrypt[_-]?key|password|authorization|cookie|api[_-]?key|private[_-]?key|email|mobile|phone|credential)`)

func TestGoldenFixtureHygiene(t *testing.T) {
	err := filepath.WalkDir(fixturetest.Root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		t.Run(strings.TrimPrefix(path, fixturetest.Root+"/"), func(t *testing.T) {
			if !entry.Type().IsRegular() {
				t.Fatal("expected a regular file")
			}
			raw, err := os.ReadFile(path) // #nosec G304 -- path is a file walked inside the fixture directory.
			if err != nil {
				t.Fatal(err)
			}
			if sensitiveFixtureValue.Match(raw) {
				t.Fatal("file contains a credential, live ID, or PII pattern")
			}
			if path == filepath.Join(fixturetest.Root, "README.md") {
				return
			}
			var doc struct {
				Status int            `json:"status"`
				Body   map[string]any `json:"body"`
			}
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			if filepath.Ext(path) != ".json" || !json.Valid(raw) || decoder.Decode(&doc) != nil ||
				doc.Status < 100 || doc.Status > 599 || doc.Body == nil {
				t.Fatal("expected a JSON fixture with an HTTP status and an object body")
			}
			checkFixtureFields(t, doc.Body)
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func checkFixtureFields(t *testing.T, value any) {
	t.Helper()
	switch node := value.(type) {
	case map[string]any:
		for key, child := range node {
			if sensitiveFixtureKey.MatchString(key) {
				switch child {
				case "", "fixture:redacted", "fixture:tenant", "fixture:app_token", "fixture:nonce":
				default:
					t.Errorf("%q contains an unredacted sensitive field", key)
				}
			}
			checkFixtureFields(t, child)
		}
	case []any:
		for _, child := range node {
			checkFixtureFields(t, child)
		}
	case string:
		if sensitiveFixtureValue.MatchString(node) {
			t.Error("decoded string contains a credential, live ID, or PII pattern")
		}
		var embedded map[string]any
		if strings.HasPrefix(strings.TrimSpace(node), "{") && json.Unmarshal([]byte(node), &embedded) == nil {
			checkFixtureFields(t, embedded)
		}
	}
}

func fixtureID(t *testing.T, body []byte, path ...string) string {
	t.Helper()
	var root any
	requireSDKNetwork(t, json.Unmarshal(body, &root))
	for _, key := range path {
		object, ok := root.(map[string]any)
		if !ok {
			t.Fatalf("invalid field path %v", path)
		}
		root = object[key]
	}
	id, ok := root.(string)
	if !ok || id == "" {
		t.Fatalf("missing nonempty string at %v", path)
	}
	return id
}

func TestGoldenRESTContracts(t *testing.T) {
	cfg := sdkTestConfig()
	cfg.Apps[0].BotName = "Fixture Bot"
	_, web := sdkEventServer(t, cfg, event.Options{})
	app, chatID := cfg.Apps[0], cfg.Chats[0].ChatID
	credentials, err := json.Marshal(map[string]string{"app_id": app.AppID, "app_secret": app.AppSecret})
	requireSDKNetwork(t, err)
	oauth, err := json.Marshal(map[string]string{
		"client_id": app.AppID, "client_secret": app.AppSecret, "grant_type": "authorization_code", "code": "fixture-invalid",
	})
	requireSDKNetwork(t, err)
	const auth = "/open-apis/auth/v3/"
	const tenantPath = auth + "tenant_access_token/internal"
	const oauthPath = "/open-apis/authen/v2/oauth/token"
	const messages = "/open-apis/im/v1/messages"
	const cards = "/open-apis/cardkit/v1/cards"
	call := func(name, method, path, token, input string) []byte {
		t.Helper()
		status, body := fixturetest.Request(t, web.Client(), web.URL, method, path, token, []byte(input))
		assertGoldenWire(t, name, status, body)
		return body
	}
	post := func(name, path, input string) []byte {
		t.Helper()
		return call(name, http.MethodPost, path, "", input)
	}
	token := fixtureID(t, post("auth/tenant_access_token_ok", tenantPath, string(credentials)), "tenant_access_token")
	post("auth/tenant_access_token_bad_secret", tenantPath, strings.Replace(string(credentials), app.AppSecret, "fixture-invalid", 1))
	post("auth/tenant_access_token_missing_param", tenantPath, "{}")
	post("auth/tenant_access_token_malformed", tenantPath, "{")
	post("auth/app_access_token_ok", auth+"app_access_token/internal", string(credentials))
	post("auth/oauth_v2_missing_param", oauthPath, "{}")
	post("auth/oauth_v2_invalid_code", oauthPath, string(oauth))
	call("bot/bot_info_ok", http.MethodGet, "/open-apis/bot/v3/info", token, "")
	sendPath := messages + "?receive_id_type=chat_id"
	send := `{"receive_id":"` + chatID + `","msg_type":"text","content":`
	sent := call("im/message_send_text_ok", http.MethodPost, sendPath, token, send+`"{\"text\":\"fixture message\"}"}`)
	call("im/message_send_invalid_content_99992402", http.MethodPost, sendPath, token, send+`{"text":"invalid"}}`)
	message := messages + "/" + fixtureID(t, sent, "data", "message_id")
	call("im/message_reply_thread_ok", http.MethodPost, message+"/reply", token,
		`{"msg_type":"text","content":"{\"text\":\"fixture reply\"}","reply_in_thread":true}`)
	call("im/message_get_ok", http.MethodGet, message, token, "")
	call("im/chat_get_not_found_232006", http.MethodGet, "/open-apis/im/v1/chats/oc_missing_fixture", token, "")
	call("im/reaction_create_ok", http.MethodPost, message+"/reactions", token, `{"reaction_type":{"emoji_type":"THUMBSUP"}}`)
	card, err := sdkEventREST(web, app).Cardkit.V1.Card.Create(t.Context(), larkcardkit.NewCreateCardReqBuilder().
		Body(larkcardkit.NewCreateCardReqBodyBuilder().Type("card_json").Data(sdkStreamingCard).Build()).Build())
	requireSDKNetwork(t, err)
	assertGoldenWire(t, "cardkit/card_create_ok", card.StatusCode, card.RawBody)
	contentPath := cards + "/" + fixtureID(t, card.RawBody, "data", "card_id") + "/elements/md/content"
	status, body := fixturetest.Request(t, web.Client(), web.URL, http.MethodPut, contentPath, token, []byte(`{"sequence":1,"content":"first"}`))
	fixturetest.Envelope(t, status, body, http.StatusOK, `{"code":0,"msg":"","data":{}}`)
	call("cardkit/card_element_content_stale_seq_300317", http.MethodPut, contentPath, token, `{"sequence":1,"content":"duplicate"}`)
}

type fixtureWebhook struct {
	raw, plain []byte
	status     int
}

func TestGoldenWebhookContracts(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		mode := "plain"
		if encrypted {
			mode = "encrypted"
		}
		t.Run(mode, func(t *testing.T) {
			cfg := sdkEventConfig()
			cfg.Apps[0].TenantKey, cfg.Apps[1].TenantKey = "fixture:redacted", "fixture:redacted"
			cfg.Chats[0].Name = "Fixture Chat"
			app := &cfg.Apps[1]
			app.VerificationToken = "fixture:redacted"
			if !encrypted {
				app.EncryptKey = ""
			}
			dispatcher, _ := sdkEventDispatcher(*app)
			dispatcher.OnP2CardActionTrigger(func(context.Context, *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
				return &callback.CardActionTriggerResponse{Toast: &callback.Toast{Type: "success", Content: "Fixture confirmed"}}, nil
			})
			handle := httpserverext.NewEventHandlerFunc(dispatcher)
			inbox := make(chan fixtureWebhook, 8)
			receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, ok := sdkReadWebhook(t, w, r, app.EncryptKey)
				if !ok {
					return
				}
				plain := raw
				if encrypted {
					var err error
					plain, err = protocol.DecryptWebhook(app.EncryptKey, raw)
					if err != nil {
						t.Error("fixture webhook decryption failed")
						w.WriteHeader(http.StatusBadRequest)
						return
					}
				}
				r.Body = io.NopCloser(bytes.NewReader(raw))
				result := httptest.NewRecorder()
				handle(result, r)
				w.Header().Set("Content-Type", result.Header().Get("Content-Type"))
				w.WriteHeader(result.Code)
				if _, err := w.Write(result.Body.Bytes()); err != nil {
					t.Error("fixture webhook response write failed")
				}
				inbox <- fixtureWebhook{raw, plain, result.Code}
			}))
			t.Cleanup(receiver.Close)
			app.WebhookURL = receiver.URL
			srv, web := sdkEventServer(t, cfg, event.Options{WebhookMaxAttempts: 1})
			receive := func(name string) fixtureWebhook {
				t.Helper()
				delivery := receiveSDKEvent(t, inbox)
				assertGoldenWire(t, "events/"+name, delivery.status, delivery.plain)
				return delivery
			}

			requireSDKNetwork(t, srv.Events().VerifyWebhook(t.Context(), app.AppID))
			verification := receive("url_verification_plain")
			if encrypted {
				assertGoldenWire(t, "events/url_verification_encrypted", verification.status, verification.raw)
				return
			}

			chatID := cfg.Chats[0].ChatID
			sdkBotJoin(t, sdkEventREST(web, cfg.Apps[0]), chatID, app.AppID)
			receive("im_chat_member_bot_added_v1")
			_, err := srv.Events().DispatchMessageReceive(t.Context(), app.AppID, sdkReceiveMessage(cfg, cfg.Chats[1]))
			requireSDKNetwork(t, err)
			receive("im_message_receive_v1")
			client := sdkEventREST(web, *app)
			sdkRecallEvent(t, client, chatID)
			receive("im_message_recalled_v1")
			message := sdkCardMessage(t, client, chatID, `{"elements":[{"tag":"markdown","content":"fixture"}]}`)
			result, err := srv.TriggerCardAction(t.Context(), event.CardActionTriggerInput{
				MessageID: value(message.MessageId), OperatorOpenID: cfg.Users[0].OpenID,
				Action: callback.CallBackAction{Tag: "button", Value: map[string]any{"action": "confirm"}},
			})
			requireSDKNetwork(t, err)
			delivery := receive("card_action_trigger_event")
			assertGoldenWire(t, "events/card_action_trigger_response", delivery.status, result.Delivery.Data)
		})
	}
}

func assertGoldenWire(t *testing.T, name string, status int, body []byte) {
	t.Helper()
	fixturetest.Golden(t, name, status, body, true)
}
