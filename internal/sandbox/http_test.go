package sandbox_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/event"
	"github.com/littlecrabzzz-alt/larkbox/internal/server"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

func sandboxServer(t *testing.T, cfg config.Config) *server.Server {
	t.Helper()
	srv := server.New(cfg, server.WithEventOptions(event.Options{WebhookMaxAttempts: 1}))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return srv
}

func call(t *testing.T, srv *server.Server, method, path, body string, status int) []byte {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != status || !json.Valid(w.Body.Bytes()) {
		t.Fatalf("%s %s got %d %s, want %d and JSON", method, path, w.Code, w.Body, status)
	}
	return w.Body.Bytes()
}

func assertContains(t *testing.T, raw []byte, terms ...string) {
	t.Helper()
	for _, term := range terms {
		if !bytes.Contains(raw, []byte(term)) {
			t.Fatalf("missing %q in %s", term, raw)
		}
	}
}

func TestControlValidationAndRouting(t *testing.T) {
	srv := sandboxServer(t, config.Default())
	for _, test := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", "nonexistent", "", 404},
		{"PUT", "state", "", 405},
		{"POST", "seed", `{"unknown":1}`, 400},
		{"POST", "reset", `{"seed":{"extra":true}}`, 400},
		{"POST", "reset", `{}{}`, 400},
		{"POST", "reset", `null`, 400},
		{"POST", "seed", strings.Repeat(" ", 1<<20+1), 413},
		{"POST", "seed", `{"apps":[{"app_id":"example","app_secret":"fixture","bot_open_id":"ou_example","redirect_uris":["ftp://localhost"]}]}`, 400},
		{"POST", "seed", `{"chats":[{"chat_id":"oc_invalid","chat_mode":"group","member_ids":["missing"]}]}`, 400},
		{"POST", "seed", `{"chats":[{"chat_id":"oc_invalid","chat_mode":"group","owner_id":"ou_larkbox_alice","member_ids":["ou_larkbox_bob"]}]}`, 400},
		{"POST", "seed", `{"users":[{"open_id":"ou_duplicate","user_id":"alice"}]}`, 409},
		{"POST", "seed", `{"card_templates":[{"template_id":"bad","data":"{\"schema\":\"2.0\",\"body\":false}"}]}`, 400},
		{"GET", "chats/missing/messages", "", 404},
		{"GET", "cards/missing", "", 404},
		{"POST", "messages/send", `{"chat_id":"oc_larkbox_group","app_id":"cli_larkbox","sender_user_id":"alice","text":"hi"}`, 400},
		{"POST", "messages/send", `{"chat_id":"oc_larkbox_group","sender_user_id":"nobody","text":"hi"}`, 404},
		{"POST", "messages/send", `{"chat_id":"oc_larkbox_group","sender_user_id":"alice","msg_type":"image","content":"{}"}`, 400},
		{"POST", "messages/send", `{"chat_id":"oc_larkbox_group","sender_user_id":"alice","text":"hi","mentions":["ou_unknown"]}`, 400},
		{"POST", "messages/send", `{"chat_id":"oc_larkbox_group","sender_user_id":"alice","text":"hi","reply_in_thread":true}`, 400},
		{"POST", "cards/action", `{"message_id":"missing","operator_open_id":"ou_larkbox_alice","action":{"tag":"button","unknown":1}}`, 400},
		{"POST", "cards/action", `{"message_id":"missing","operator_open_id":"ou_larkbox_alice","action":{"tag":"button"}}`, 404},
		{"POST", "events/webhook/verify", `{"app_id":"missing"}`, 404},
		{"POST", "events/webhook/verify", `{"app_id":"cli_larkbox","webhook_url":"http://user:secret@localhost"}`, 400},
		{"POST", "tokens/expire", `{"app_id":"cli_larkbox","tokens":[]}`, 400},
		{"POST", "tokens/expire", `{"app_id":"cli_larkbox","token":""}`, 400},
		{"POST", "tokens/expire", `{"app_id":"missing"}`, 404},
		{"GET", "traces?direction=other", "", 400},
		{"GET", "traces?channel=other", "", 400},
		{"GET", "traces?limit=257", "", 400},
		{"GET", "traces?limit=1&limit=2", "", 400},
		{"GET", "traces?unknown=1", "", 400},
		{"POST", "faults", `{"kind":"no_such_fault"}`, 400},
		{"POST", "faults", `{"kind":"latency","delay_ms":60001}`, 400},
		{"POST", "faults", `{"kind":"rate_limit","remaining_hits":0}`, 400},
		{"POST", "faults", `{"kind":"http_error","status_code":200}`, 400},
		{"POST", "faults", `{"kind":"token_expiry","probability":1.01}`, 400},
		{"POST", "faults", `{"kind":"duplicate_event","channel":"inbound"}`, 400},
		{"DELETE", "faults/missing", "", 404},
	} {
		t.Run(test.method+" "+test.path+fmt.Sprint(test.status), func(t *testing.T) {
			call(t, srv, test.method, "/_sandbox/"+test.path, test.body, test.status)
		})
	}
	if len(srv.Store().Traces()) != 0 {
		t.Fatal("control-plane operations polluted the Open API audit ring")
	}
}

func TestControlSeedSnapshotAndReset(t *testing.T) {
	cfg := config.Default()
	srv := sandboxServer(t, cfg)
	seed := `{"apps":[{"app_id":"cli_extra","app_secret":"secret-fixture","bot_open_id":"ou_extra","bot_name":"Extra","encrypt_key":"encryption-fixture","verification_token":"verification-fixture","redirect_uris":["https://localhost/cb"]}],"chats":[{"chat_id":"oc_shared","chat_mode":"group","member_ids":["ou_larkbox_alice","ou_extra"],"owner_id":"ou_larkbox_alice"}],"card_templates":[{"template_id":"one","data":"{\"schema\":\"2.0\",\"body\":{\"elements\":[{\"tag\":\"markdown\",\"content\":\"seed\"}]}}"}]}`
	call(t, srv, "POST", "/_sandbox/seed", seed, 200)
	raw := call(t, srv, "GET", "/_sandbox/state", "", 200)
	assertContains(t, raw, `"strict_mode":true`, `"version":"`, `"app_id":"cli_extra"`, `"has_encrypt_key":true`, `"has_verification_token":true`,
		`"redirect_uris":["https://localhost/cb"]`, `"active_ws_sessions":0`, `"member_ids":["ou_larkbox_alice","ou_extra"]`)
	for _, secret := range []string{"secret-fixture", "encryption-fixture", "verification-fixture", `"app_secret"`, `"encrypt_key"`, `"verification_token"`} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Errorf("state includes private app data %q", secret)
		}
	}
	if srv.Store().CardTemplate("one", "") == "" {
		t.Fatal("template was not registered")
	}
	assertContains(t, call(t, srv, "GET", "/_sandbox/chats", "", 200), "oc_shared")
	call(t, srv, "POST", "/_sandbox/seed", `{"users":[{"open_id":"ou_larkbox_alice","user_id":"alice","name":"Changed"}]}`, 200)
	assertContains(t, call(t, srv, "GET", "/_sandbox/state", "", 200), `"name":"Changed"`)

	msg := call(t, srv, "POST", "/_sandbox/messages/send", `{"app_id":"cli_extra","sender_user_id":"alice","mentions":["ou_extra"],"text":"DM"}`, 201)
	var direct struct {
		Message struct {
			ID   string `json:"message_id"`
			Chat string `json:"chat_id"`
		}
	}
	if err := json.Unmarshal(msg, &direct); err != nil || direct.Message.ID == "" || direct.Message.Chat == "" {
		t.Fatalf("invalid direct message: %s", msg)
	}
	assertContains(t, msg, `@_user_1 DM`, `"sender_name":"Changed"`, `"app_id":"cli_extra"`)
	reply := call(t, srv, "POST", "/_sandbox/messages/send", fmt.Sprintf(
		`{"app_id":"cli_extra","sender_user_id":"alice","text":"thread","parent_id":%q,"reply_in_thread":true}`, direct.Message.ID), 201)
	assertContains(t, reply, `"root_id":"`+direct.Message.ID+`"`, `"thread_id":"`)
	assertContains(t, call(t, srv, "GET", "/_sandbox/chats/"+direct.Message.Chat+"/messages", "", 200), "thread", `"reactions":[]`)
	call(t, srv, "POST", "/_sandbox/reset", `{"seed":{"users":[{"open_id":"ou_custom","user_id":"custom"}]}}`, 200)
	assertContains(t, call(t, srv, "GET", "/_sandbox/state", "", 200), `"open_id":"ou_custom"`, `"apps":[]`, `"chats":[]`)
	if _, ok := srv.Store().OAuthUser("", ""); !ok || srv.Store().CardTemplate("one", "") != "" {
		t.Fatal("custom seed did not replace runtime state")
	}
	call(t, srv, "POST", "/_sandbox/reset", "", 200)
	raw = call(t, srv, "GET", "/_sandbox/state", "", 200)
	assertContains(t, raw, `"app_id":"cli_larkbox"`, `"name":"Alice"`)
	if bytes.Contains(raw, []byte("ou_custom")) || bytes.Contains(raw, []byte("cli_extra")) {
		t.Fatal("custom or runtime seed became the default baseline")
	}
}

func TestControlFaultsTraceAndTokenExpiry(t *testing.T) {
	cfg := config.Default()
	srv := sandboxServer(t, cfg)
	appID := cfg.Apps[0].AppID
	for _, token := range []string{"t-first", "t-second"} {
		if err := srv.Store().PutToken(store.Token{Value: token, AppID: appID, Kind: store.TenantToken, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	raw := call(t, srv, "POST", "/_sandbox/faults", `{"kind":"http_error","status_code":503,"error_code":321,"error_msg":"fixture"}`, 201)
	var rule struct {
		ID string `json:"fault_id"`
	}
	if err := json.Unmarshal(raw, &rule); err != nil || rule.ID == "" {
		t.Fatalf("bad fault: %s", raw)
	}
	call(t, srv, "GET", "/healthz", "", 200)
	assertContains(t, call(t, srv, "GET", "/_sandbox/faults", "", 200), rule.ID)
	call(t, srv, "GET", "/open-apis/bot/v3/info", "", 503)
	trace := call(t, srv, "GET", "/_sandbox/traces?direction=inbound&method=get&path_prefix=/open-apis/bot&limit=1", "", 200)
	assertContains(t, trace, `"code":321`, `"status_code":503`, `"msg":"fixture"`)
	call(t, srv, "DELETE", "/_sandbox/faults/"+rule.ID, "", 200)
	call(t, srv, "DELETE", "/_sandbox/faults/"+rule.ID, "", 404)
	call(t, srv, "POST", "/_sandbox/faults", `{"kind":"rate_limit"}`, 201)
	call(t, srv, "DELETE", "/_sandbox/faults", "", 200)
	call(t, srv, "DELETE", "/_sandbox/traces", "", 200)
	if got := call(t, srv, "GET", "/_sandbox/traces", "", 200); string(got) != `{"traces":[]}` {
		t.Fatalf("trace ring not empty: %s", got)
	}
	call(t, srv, "POST", "/_sandbox/tokens/expire", `{"app_id":"cli_larkbox","tokens":["t-first","t-absent"]}`, 404)
	if _, err := srv.Store().CheckToken("t-first"); err != nil {
		t.Fatal("invalid request partially expired credentials")
	}
	assertContains(t, call(t, srv, "POST", "/_sandbox/tokens/expire", `{"app_id":"cli_larkbox","tokens":["t-first"]}`, 200), `"expired":1`)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/open-apis/bot/v3/info", nil)
	r.Header.Set("Authorization", "Bearer t-first")
	srv.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatalf("token still valid: %d", w.Code)
	}
	assertContains(t, w.Body.Bytes(), "99991663")
	if _, err := srv.Store().CheckToken("t-second"); err != nil {
		t.Fatal("selected expiry changed an unrelated token")
	}
	assertContains(t, call(t, srv, "POST", "/_sandbox/tokens/expire", `{"app_id":"cli_larkbox"}`, 200), `"expired":1`)
}

func TestControlWebhookVerification(t *testing.T) {
	cfg := config.Default()
	cfg.Apps[0].EncryptKey = ""
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Challenge string `json:"challenge"`
			Type      string `json:"type"`
			Token     string `json:"token"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.Type != "url_verification" || request.Token != cfg.Apps[0].VerificationToken {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.URL.Path == "/mismatch" {
			request.Challenge = "unexpected"
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"challenge": request.Challenge})
	}))
	defer webhook.Close()
	cfg.Apps[0].WebhookURL = webhook.URL
	srv := sandboxServer(t, cfg)
	raw := call(t, srv, "POST", "/_sandbox/events/webhook/verify", `{"app_id":"cli_larkbox"}`, 200)
	assertContains(t, raw, `"verified":true`, `"status_code":200`, `"latency_ms":`)
	raw = call(t, srv, "POST", "/_sandbox/events/webhook/verify", fmt.Sprintf(
		`{"app_id":"cli_larkbox","webhook_url":%q}`, webhook.URL+"/mismatch"), 200)
	assertContains(t, raw, `"verified":false`, `"status_code":200`, "challenge mismatch")
	trace := call(t, srv, "GET", "/_sandbox/traces?channel=webhook&direction=outbound&event_type=url_verification", "", 200)
	assertContains(t, trace, "url_verification", "[REDACTED]")
	if bytes.Contains(trace, []byte(cfg.Apps[0].VerificationToken)) {
		t.Fatal("trace exposed the verification token")
	}
}

func TestControlResetInterruptsInboundDelay(t *testing.T) {
	srv := sandboxServer(t, config.Default())
	call(t, srv, "POST", "/_sandbox/faults", `{"kind":"latency","delay_ms":60000,"remaining_hits":1,"channel":"inbound"}`, 201)
	request, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		r := httptest.NewRequestWithContext(request, http.MethodGet, "/open-apis/bot/v3/info", nil)
		srv.ServeHTTP(w, r)
		done <- w.Code
	}()
	deadline := time.After(2 * time.Second)
	for string(call(t, srv, "GET", "/_sandbox/faults", "", 200)) != `{"faults":[]}` {
		select {
		case <-deadline:
			t.Fatal("request did not begin its delay")
		case <-time.After(time.Millisecond):
		}
	}
	call(t, srv, "POST", "/_sandbox/reset", "", 200)
	select {
	case status := <-done:
		if status != http.StatusServiceUnavailable {
			t.Fatalf("reset of delayed request returned %d", status)
		}
	case <-time.After(time.Second):
		t.Fatal("reset left the request waiting")
	}
	if len(srv.Store().Traces()) != 0 {
		t.Fatal("old audit exchange repopulated the trace ring")
	}
}

func TestAppScopedFaultOnCredentialEndpoint(t *testing.T) {
	srv := sandboxServer(t, config.Default())
	path := "/open-apis/auth/v3/tenant_access_token/internal"
	app := config.DefaultApp()
	call(t, srv, "POST", "/_sandbox/faults", fmt.Sprintf(`{"kind":"rate_limit","app_id":%q,"path_prefix":%q,"remaining_hits":1}`, app.AppID, path), 201)
	call(t, srv, "POST", path, `{"app_id":"other","app_secret":"local-test"}`, 200)
	credentials := fmt.Sprintf(`{"app_id":%q,"app_secret":%q}`, app.AppID, app.AppSecret)
	call(t, srv, "POST", path, credentials, 429)
	traces := call(t, srv, "GET", "/_sandbox/traces?app_id="+app.AppID+"&limit=1", "", 200)
	assertContains(t, traces, `"code":99991400`, "[REDACTED]")
	if bytes.Contains(traces, []byte(app.AppSecret)) {
		t.Fatal("app secret appeared in the fault trace")
	}
	raw := call(t, srv, "POST", path, credentials, 200)
	assertContains(t, raw, `"code":0`, `"tenant_access_token"`)
}

type pausedBody struct {
	*strings.Reader
	entered chan struct{}
	release chan struct{}
}

func (b *pausedBody) Read(p []byte) (int, error) {
	if b.entered != nil {
		close(b.entered)
		b.entered = nil
		<-b.release
	}
	return b.Reader.Read(p)
}

func TestResetFencesOpenAPIWritesAdmittedBeforeReset(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
		auth             bool
	}{
		{"credential issuance", "/open-apis/auth/v3/tenant_access_token/internal", `{"app_id":"cli_larkbox","app_secret":"larkbox-local-secret"}`, false},
		{"chat creation after Bearer", "/open-apis/im/v1/chats", `{"name":"stale chat"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			srv := sandboxServer(t, cfg)
			if tc.auth {
				err := srv.Store().PutToken(store.Token{Value: "t-pending", Kind: store.TenantToken, AppID: cfg.Apps[0].AppID, ExpiresAt: time.Now().Add(time.Hour)})
				if err != nil {
					t.Fatal(err)
				}
			}
			entered, release := make(chan struct{}), make(chan struct{})
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, tc.path, nil)
			r.Body = io.NopCloser(&pausedBody{strings.NewReader(tc.body), entered, release})
			if tc.auth {
				r.Header.Set("Authorization", "Bearer t-pending")
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				w := httptest.NewRecorder()
				srv.ServeHTTP(w, r)
				done <- w
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				close(release)
				t.Fatal("Open API request did not reach its body read")
			}
			call(t, srv, "POST", "/_sandbox/reset", "", 200)
			close(release)
			select {
			case w := <-done:
				if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "sandbox reset") {
					t.Errorf("stale Open API write returned %d %s", w.Code, w.Body)
				}
			case <-time.After(time.Second):
				t.Fatal("stale request did not finish")
			}
			assertContains(t, call(t, srv, "POST", "/_sandbox/tokens/expire", `{"app_id":"cli_larkbox"}`, 200), `"expired":0`)
			if len(srv.Store().Traces()) != 0 || len(srv.Store().Chats().List()) != len(cfg.Chats) {
				t.Fatal("stale Open API request repopulated the state or trace ring")
			}
		})
	}
}

func TestInvalidChatSettingsRejectEntireSeedAndReset(t *testing.T) {
	srv := sandboxServer(t, config.Default())
	call(t, srv, "POST", "/_sandbox/faults", `{"kind":"rate_limit"}`, 201)
	before := call(t, srv, "GET", "/_sandbox/state", "", 200)
	batch := `{"users":[{"open_id":"ou_valid","user_id":"valid","name":"Uncommitted"}],"chats":[{"chat_id":"oc_bad","chat_mode":"group","group_message_type":"unsupported","owner_id":"ou_valid","member_ids":["ou_valid"]}]}`
	for path, body := range map[string]string{"seed": batch, "reset": `{"seed":` + batch + `}`} {
		assertContains(t, call(t, srv, "POST", "/_sandbox/"+path, body, 400), "group_message_type")
		after := call(t, srv, "GET", "/_sandbox/state", "", 200)
		if !bytes.Equal(before, after) {
			t.Fatalf("%s mutated state after rejecting a mixed seed batch", path)
		}
	}
}

func TestControlDirectTokenExpiryMatchesFaultCodes(t *testing.T) {
	cfg := config.Default()
	srv := sandboxServer(t, cfg)
	for _, tc := range []struct {
		kind                      store.TokenKind
		value, method, path, code string
	}{
		{store.TenantToken, "t-tenant", "GET", "/open-apis/bot/v3/info", "99991663"},
		{store.AppToken, "a-app", "POST", "/open-apis/authen/v1/oidc/access_token", "99991663"},
		{store.UserToken, "u-user", "GET", "/open-apis/authen/v1/user_info", "99991668"},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			err := srv.Store().PutToken(store.Token{
				Value: tc.value, Kind: tc.kind, AppID: cfg.Apps[0].AppID,
				UserOpenID: cfg.Users[0].OpenID, ExpiresAt: time.Now().Add(time.Hour),
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, body := range []string{`{"app_id":"cli_larkbox","token":null}`, `{"app_id":"cli_larkbox","tokens":null}`} {
				call(t, srv, "POST", "/_sandbox/tokens/expire", body, 400)
				if _, err := srv.Store().CheckToken(tc.value); err != nil {
					t.Fatalf("null token selector changed credentials: %v", err)
				}
			}
			assertContains(t, call(t, srv, "POST", "/_sandbox/tokens/expire",
				fmt.Sprintf(`{"app_id":"cli_larkbox","token":%q}`, tc.value), 200), `"expired":1`)
			w := httptest.NewRecorder()
			r := httptest.NewRequest(tc.method, tc.path, nil)
			r.Header.Set("Authorization", "Bearer "+tc.value)
			srv.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expired %s returned %d: %s", tc.kind, w.Code, w.Body)
			}
			assertContains(t, w.Body.Bytes(), `"code":`+tc.code)
		})
	}
}

func TestSandboxRejectsPreassignedMentionKeys(t *testing.T) {
	wire := make(chan string, 4)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		wire <- string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer webhook.Close()
	cfg := config.Default()
	cfg.Apps[0].WebhookURL, cfg.Apps[0].EncryptKey = webhook.URL, ""
	srv := sandboxServer(t, cfg)
	if err := srv.Store().CreateCard(store.Card{
		ID: "1", AppID: cfg.Apps[0].AppID, Type: "card_json",
		JSON: json.RawMessage(`{"schema":"2.0","body":{"elements":[{"tag":"markdown","content":"@_user_9"}]}}`),
	}); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"chat_id":"oc_larkbox_group","sender_user_id":"alice","text":"@_user_9 hello","at_bot":true}`,
		`{"chat_id":"oc_larkbox_group","sender_user_id":"alice","content":"{\"text\":\"\\u0040_user_9\"}"}`,
		`{"chat_id":"oc_larkbox_group","sender_user_id":"alice","msg_type":"post","content":"{\"content\":[[{\"tag\":\"at\",\"user_id\":\"@_user_9\"}]]}"}`,
		fmt.Sprintf(`{"chat_id":"oc_larkbox_group","sender_user_id":"alice","msg_type":"interactive","content":%q}`, `{"type":"card","data":{"card_id":"1"}}`),
	} {
		assertContains(t, call(t, srv, "POST", "/_sandbox/messages/send", body, 400), "preassigned")
	}
	if len(wire) != 0 || len(srv.Store().Messages().List()) != 0 {
		t.Fatal("invalid mention was stored or dispatched")
	}
	call(t, srv, "POST", "/_sandbox/messages/send", `{"chat_id":"oc_larkbox_group","sender_user_id":"alice","text":"@bot hello","at_bot":true}`, 201)
	sent := <-wire
	message := srv.Store().Messages().List()[0]
	if message.Content != `{"text":"@_user_1 hello"}` || len(message.Mentions) != 1 || message.Mentions[0].Key != "@_user_1" ||
		!strings.Contains(sent, `"key":"@_user_1"`) || !strings.Contains(sent, `@_user_1 hello`) {
		t.Fatalf("normalized mentions missing from store or wire: %+v / %s", message, sent)
	}
}

func TestControlWebhookVerificationCapsEmbeddedTimeout(t *testing.T) {
	cfg := config.Default()
	cfg.Apps[0].EncryptKey = ""
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(4 * time.Second):
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer hook.Close()
	cfg.Apps[0].WebhookURL = hook.URL
	srv := server.New(cfg, server.WithEventOptions(event.Options{VerificationTimeout: 10 * time.Second}))
	defer func() { _ = srv.Shutdown(context.Background()) }()
	start := time.Now()
	result := call(t, srv, "POST", "/_sandbox/events/webhook/verify", `{"app_id":"cli_larkbox"}`, 200)
	if time.Since(start) > 3*time.Second {
		t.Fatalf("verification inherited the longer timeout: %s", result)
	}
	assertContains(t, result, `"verified":false`, `"status_code":0`, "timed out")
}

func TestControlCardActionRedactsDiagnosticCardContent(t *testing.T) {
	cfg := config.Default()
	cfg.Apps[0].EncryptKey = ""
	reflected := make(chan string, 1)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var incoming struct {
			Header struct{ Token string }
			Event  struct{ Token string }
		}
		if err := json.NewDecoder(r.Body).Decode(&incoming); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		reflected <- incoming.Event.Token
		echo := incoming.Event.Token + " / " + incoming.Header.Token
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"toast":{"type":%q,"content":%q,"i18n":{"en_us":%q,%q:%q,%q:%q}},"card":{"type":"raw","data":{"schema":"2.0","body":{"elements":[{"tag":"markdown","element_id":"visible","content":%q,"preview_token":"private-card-value","data":{%q:"dynamic label"}}]}}}}`,
			"info-"+incoming.Event.Token, "Saved "+echo, "Localized "+echo,
			"locale-"+incoming.Event.Token, "Alternate "+echo, "locale-"+incoming.Header.Token, "Translated "+echo,
			"Updated card "+echo, "field-"+incoming.Event.Token)
	}))
	defer hook.Close()
	cfg.Apps[0].WebhookURL = hook.URL
	srv := sandboxServer(t, cfg)
	srv.Store().Messages().Put(store.Message{
		ID: "om_click", AppID: cfg.Apps[0].AppID, ChatID: cfg.Chats[0].ChatID, MsgType: "interactive",
		Content:      `{"schema":"2.0","body":{"elements":[{"tag":"markdown","content":"Initial card"}]}}`,
		SenderOpenID: cfg.Apps[0].BotOpenID, CreatedAt: time.Now(),
	})
	raw := call(t, srv, "POST", "/_sandbox/cards/action", `{"message_id":"om_click","operator_open_id":"ou_larkbox_alice","action":{"tag":"button"}}`, 200)
	token := <-reflected
	if token == "" {
		t.Fatal("webhook did not receive the action token")
	}
	assertContains(t, raw, `"card_applied":true`, "Updated card [REDACTED] / [REDACTED]", "Saved [REDACTED] / [REDACTED]",
		"Localized [REDACTED] / [REDACTED]", `"preview_token":"[REDACTED]"`)
	traces := call(t, srv, "GET", "/_sandbox/traces?channel=webhook&event_type=card.action.trigger", "", 200)
	for _, projection := range [][]byte{raw, traces} {
		assertContains(t, projection, "Saved [REDACTED] / [REDACTED]", "Updated card [REDACTED] / [REDACTED]",
			`"type":"info-[REDACTED]"`, `"locale-[REDACTED]":`, `"locale-[REDACTED]#2":`,
			"Alternate [REDACTED] / [REDACTED]", "Translated [REDACTED] / [REDACTED]", `"field-[REDACTED]":"dynamic label"`)
	}
	history := call(t, srv, "GET", "/_sandbox/chats/"+cfg.Chats[0].ChatID+"/messages", "", 200)
	assertContains(t, history, "Updated card [REDACTED] / [REDACTED]")
	for _, projection := range [][]byte{raw, traces, history} {
		for _, secret := range []string{token, cfg.Apps[0].VerificationToken, "private-card-value"} {
			if bytes.Contains(projection, []byte(secret)) {
				t.Fatal("webhook callback diagnostics exposed a credential")
			}
		}
	}
	message, ok := srv.Store().Messages().Get("om_click")
	if !ok || !strings.Contains(message.Content, token) {
		t.Fatal("diagnostic redaction changed the stored callback result")
	}
}

func TestControlCardDetailPreservesStandardIcon(t *testing.T) {
	cfg := config.Default()
	srv := sandboxServer(t, cfg)
	credential := cfg.Apps[0].VerificationToken
	card := store.Card{
		ID: "1", AppID: cfg.Apps[0].AppID, Type: "card_json",
		JSON: json.RawMessage(fmt.Sprintf(`{"schema":"2.0","body":{"elements":[
			{"tag":"markdown","content":"Info","icon":{"tag":"standard_icon","token":"info_outlined"}},
			{"tag":"markdown","content":"Reflected","icon":{"tag":"standard_icon","token":%q}},
			{"tag":"markdown","content":"Private","value":{"token":"private-value"}}
		]}}`, credential)),
	}
	if err := srv.Store().CreateCard(card); err != nil {
		t.Fatal(err)
	}
	raw := call(t, srv, "GET", "/_sandbox/cards/1", "", 200)
	assertContains(t, raw, `"icon":{"tag":"standard_icon","token":"info_outlined"}`, `"icon":{"tag":"standard_icon","token":"[REDACTED]"}`, `"value":{"token":"[REDACTED]"}`)
	if bytes.Contains(raw, []byte(credential)) || bytes.Contains(raw, []byte("private-value")) {
		t.Fatal("card detail exposed a credential")
	}
}
