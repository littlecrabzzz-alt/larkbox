package sandbox_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	assertContains(t, trace, "url_verification", "[redacted]")
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
