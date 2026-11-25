package sdk_test

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"

	larkmail "github.com/larksuite/oapi-sdk-go/v3/service/mail/v1"

	"github.com/littlecrabzzz-alt/larkbox/internal/event"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

func TestMailDeliveryAndMailboxScope(t *testing.T) {
	cfg := sdkTestConfig()
	srv, web := sdkEventServer(t, cfg, event.Options{})
	const root = "/open-apis/mail/v1/user_mailboxes/"
	const scope = "mail:user_mailbox:readonly mail:user_mailbox.message:modify mail:user_mailbox.message:send mail:user_mailbox.message:readonly mail:user_mailbox.message.address:read mail:user_mailbox.message.subject:read mail:user_mailbox.message.body:read"
	expiry := time.Now().Add(time.Minute)
	alice, bob, metadataOnly := "u-mail-alice-test", "u-mail-bob-test", "u-mail-bob-metadata"
	readonly, modify := "u-mail-alice-readonly", "u-mail-alice-modify"
	app := cfg.Apps[0].AppID
	requireSDKNetwork(t, srv.Store().PutTokens(
		store.Token{Value: alice, Kind: store.UserToken, AppID: app, UserOpenID: cfg.Users[0].OpenID, Scope: scope, ExpiresAt: expiry},
		store.Token{Value: bob, Kind: store.UserToken, AppID: app, UserOpenID: cfg.Users[1].OpenID, Scope: scope, ExpiresAt: expiry},
		store.Token{Value: metadataOnly, Kind: store.UserToken, AppID: app, UserOpenID: cfg.Users[1].OpenID, Scope: "mail:user_mailbox.message:readonly", ExpiresAt: expiry},
		store.Token{Value: readonly, Kind: store.UserToken, AppID: app, UserOpenID: cfg.Users[0].OpenID, Scope: "mail:user_mailbox.message:readonly", ExpiresAt: expiry},
		store.Token{Value: modify, Kind: store.UserToken, AppID: app, UserOpenID: cfg.Users[0].OpenID, Scope: "mail:user_mailbox.message:modify", ExpiresAt: expiry},
	))
	request := func(method, endpoint, bearer, payload string, status int) string {
		t.Helper()
		return string(contentCall(t, web, method, root+endpoint, bearer, "application/json", strings.NewReader(payload), status))
	}
	const eml = "From: Alice <alice@example.test>\r\nTo: alice@example.test\r\nBcc: bob@example.test\r\nSubject: Test memo\r\nContent-Type: text/plain\r\n\r\nSimulated delivery"
	raw := base64.RawURLEncoding.EncodeToString([]byte(eml))
	create := sandboxDecode[larkmail.CreateUserMailboxDraftResp](t, []byte(request(http.MethodPost, "me/drafts", alice, `{"raw":"`+raw+`"}`, http.StatusOK)))
	if create.Data == nil || create.Data.Draft == nil || create.Data.Draft.Id == nil || *create.Data.Draft.Id == "" {
		t.Fatal("no draft ID")
	}
	saved := *create.Data.Draft.Id
	get := request(http.MethodGet, "alice@example.test/drafts/"+saved+"?format=raw", readonly, "", http.StatusOK)
	draft := sandboxDecode[larkmail.GetUserMailboxDraftResp](t, []byte(get)).Data.Draft
	if draft == nil || draft.Id == nil || *draft.Id != saved || draft.Message == nil || draft.Message.Raw == nil || *draft.Message.Raw != raw {
		t.Fatalf("draft raw output lost wire format: %s", get)
	}
	request(http.MethodGet, "alice@example.test/drafts/"+saved, modify, "", http.StatusBadRequest)
	request(http.MethodGet, "alice@example.test/drafts/"+saved, bob, "", http.StatusNotFound)
	request(http.MethodPost, "me/drafts/"+saved+"/send", alice, `{"send_time":"1800000000"}`, http.StatusBadRequest)
	if got := request(http.MethodGet, "me/messages", bob, "", http.StatusOK); !strings.Contains(got, `"items":[]`) {
		t.Fatalf("draft leaked into inbox: %s", got)
	}
	sent := request(http.MethodPost, "me/drafts/"+saved+"/send", alice, "", http.StatusOK)
	if again := request(http.MethodPost, "me/drafts/"+saved+"/send", alice, "", http.StatusOK); sent != again {
		t.Fatalf("repeat produced a new delivery: %s vs %s", sent, again)
	}
	inbox := sandboxDecode[larkmail.ListUserMailboxMessageResp](t, []byte(request(http.MethodGet, "me/messages", bob, "", http.StatusOK))).Data.Items
	if len(inbox) != 1 || inbox[0] == "" {
		t.Fatalf("expected a single delivered message: %+v", inbox)
	}
	path := "me/messages/" + inbox[0] + "?format=full"
	detail := request(http.MethodGet, path, bob, "", http.StatusOK)
	if !strings.Contains(detail, `"subject":"Test memo"`) || !strings.Contains(detail, `"bcc":[]`) ||
		!strings.Contains(detail, base64.RawURLEncoding.EncodeToString([]byte("Simulated delivery"))) {
		t.Fatalf("inbox message lost its subject, body, or recipient privacy: %s", detail)
	}
	detail = request(http.MethodGet, path, metadataOnly, "", http.StatusOK)
	for _, key := range []string{`"subject"`, `"head_from"`, `"body_plain_text"`} {
		if strings.Contains(detail, key) {
			t.Fatalf("metadata-only response leaked %s: %s", key, detail)
		}
	}
	request(http.MethodGet, path, alice, "", http.StatusNotFound)
}

func TestWorkResourceIsolationAndTimeWindows(t *testing.T) {
	cfg := sdkTestConfig()
	srv, web := sdkEventServer(t, cfg, event.Options{})
	const scope = "calendar:calendar:read calendar:calendar.event:create calendar:calendar.event:read task:task:write minutes:minutes.basic:read minutes:minutes.artifacts:read"
	app := cfg.Apps[0].AppID
	expiry := time.Now().Add(time.Minute)
	for i, value := range []string{"u-work-alice", "u-work-bob"} {
		requireSDKNetwork(t, srv.Store().PutToken(store.Token{
			Value: value, Kind: store.UserToken, AppID: app, UserOpenID: cfg.Users[i].OpenID, Scope: scope, ExpiresAt: expiry,
		}))
	}
	call := func(method, path, who, body string, status int) string {
		t.Helper()
		return string(contentCall(t, web, method, "/open-apis/"+path, "u-work-"+who, "application/json", strings.NewReader(body), status))
	}
	const events = "calendar/v4/calendars/primary/events"
	if body := call(http.MethodGet, "calendar/v4/calendars/primary", "alice", "", http.StatusOK); !strings.Contains(body, `"data":{"calendar_id":`) {
		t.Fatalf("calendar metadata response did not contain the ID at data.calendar_id: %s", body)
	}
	call(http.MethodPost, events, "alice", `{"start_time":{"timestamp":"300"},"end_time":{"timestamp":"299"}}`, http.StatusBadRequest)
	call(http.MethodPost, events, "alice", `{"summary":"Calendar block","start_time":{"timestamp":"300"},"end_time":{"timestamp":"400"}}`, http.StatusOK)
	rangePath := events + "/instance_view?start_time=350&end_time=450"
	if body := call(http.MethodGet, rangePath, "alice", "", http.StatusOK); !strings.Contains(body, "Calendar block") {
		t.Fatalf("overlapping event missing: %s", body)
	}
	for _, selector := range []struct{ path, actor string }{
		{rangePath, "bob"}, {events + "/instance_view?start_time=400&end_time=450", "alice"},
	} {
		if body := call(http.MethodGet, selector.path, selector.actor, "", http.StatusOK); !strings.Contains(body, `"items":[]`) {
			t.Fatalf("event crossed its owner or ending boundary: %s", body)
		}
	}
	const fixture = "minutes/v1/minutes/probetoken"
	sandboxRequest(t, web, http.MethodPost, "seed", map[string]any{"minutes": []any{
		map[string]any{"minute_token": "probetoken", "title": "Captured notes", "owner_open_id": cfg.Users[0].OpenID, "transcript": "The meeting is ready."},
	}}, http.StatusOK)
	call(http.MethodGet, fixture, "bob", "", http.StatusNotFound)
	call(http.MethodGet, fixture+"/artifacts", "bob", "", http.StatusNotFound)
	if body := call(http.MethodGet, fixture+"/artifacts", "alice", "", http.StatusOK); !strings.Contains(body, "The meeting is ready.") {
		t.Fatalf("fixture transcript missing: %s", body)
	}
	sandboxRequest(t, web, http.MethodPost, "reset", nil, http.StatusOK)
	if len(srv.Store().Minutes()) != 0 {
		t.Fatal("reset retained the runtime fixture")
	}
}
