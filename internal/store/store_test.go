package store

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
)

func TestTokenValidity(t *testing.T) {
	issued := time.Unix(1700000000, 0)
	for _, tc := range []struct {
		name    string
		elapsed time.Duration
		revoked bool
		missing bool
		want    error
	}{
		{"valid", 0, false, false, nil},
		{"last nanosecond", time.Second - time.Nanosecond, false, false, nil},
		{"expiry boundary", time.Second, false, false, ErrTokenExpired},
		{"expired", 2 * time.Second, false, false, ErrTokenExpired},
		{"revoked", 0, true, false, ErrTokenRevoked},
		{"missing", 0, false, true, ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := New(func() time.Time { return issued.Add(tc.elapsed) }, 0)
			state.Apps().Put(App{AppID: "cli_demo"})
			state.Users().Put(User{OpenID: "ou_demo"})
			for _, kind := range []TokenKind{TenantToken, AppToken, UserToken, RefreshToken, OAuthCode} {
				token := Token{Value: string(kind), Kind: kind, AppID: "cli_demo", UserOpenID: "ou_demo", ExpiresAt: issued.Add(time.Second)}
				if !tc.missing {
					if err := state.PutToken(token); err != nil {
						t.Fatal(err)
					}
				}
				if tc.revoked {
					if !state.RevokeToken(token.Value) {
						t.Fatal("revoke failed")
					}
					token.Revoked = true
				}
				got, err := state.CheckToken(token.Value)
				if !errors.Is(err, tc.want) || !tc.missing && got != token {
					t.Fatalf("CheckToken for %s = %+v, %v, want %v", kind, got, err, tc.want)
				}
			}
		})
	}
}

func TestCardSequence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sequence int64
		exists   bool
		want     error
	}{
		{"next", 4, true, nil},
		{"skip", 10, true, nil},
		{"duplicate", 3, true, ErrStaleSequence},
		{"decreasing", 2, true, ErrStaleSequence},
		{"missing", 4, false, ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := New(nil, 0)
			card := Card{ID: "card_test", Sequence: 3, JSON: []byte("{}"), Elements: map[string]string{"text": "start"}}
			if tc.exists {
				if err := state.CreateCard(card); err != nil {
					t.Fatal(err)
				}
				if err := state.CreateCard(card); !errors.Is(err, ErrAlreadyExists) {
					t.Fatalf("duplicate create error = %v", err)
				}
			}
			card.Sequence = tc.sequence
			card.Elements["text"] = "update"
			if err := state.UpdateCard(card); !errors.Is(err, tc.want) {
				t.Fatalf("UpdateCard error = %v, want %v", err, tc.want)
			}
			got, ok := state.Card(card.ID)
			if ok != tc.exists {
				t.Fatalf("Card exists = %t, want %t", ok, tc.exists)
			}
			if !ok {
				return
			}
			wantSequence, wantText := tc.sequence, "update"
			if tc.want != nil {
				wantSequence, wantText = 3, "start"
			}
			card.Elements["text"], card.JSON[0] = "changed input", 'x'
			got.Elements["text"], got.JSON[0] = "changed output", 'y'
			got, _ = state.Card(card.ID)
			if got.Sequence != wantSequence || got.Elements["text"] != wantText || string(got.JSON) != "{}" {
				t.Fatalf("sequence or card data changed unexpectedly: %+v", got)
			}
			if !state.DeleteCard(card.ID) || len(state.Cards()) != 0 {
				t.Fatal("deleted card remained visible")
			}
		})
	}
}

func TestSeedAndReset(t *testing.T) {
	state := New(nil, 0)
	cfg := config.Default()
	state.Seed(cfg)

	chatID := cfg.Chats[0].ChatID
	owner := cfg.Chats[0].OwnerID
	cfg.Chats[0].MemberIDs[0] = "changed"
	if !state.AddChatMember(chatID, "ou_added") {
		t.Fatal("seeded chat not found")
	}
	if !state.AddChatMember(chatID, "ou_added") || !state.RemoveChatMember(chatID, owner) {
		t.Fatal("seeded chat not found")
	}
	chat, _ := state.Chats().Get(chatID)
	wantMembers := []string{cfg.Users[1].OpenID, cfg.Apps[0].BotOpenID, "ou_added"}
	if !slices.Equal(chat.MemberIDs, wantMembers) {
		t.Fatalf("chat members = %v, want %v", chat.MemberIDs, wantMembers)
	}
	if err := state.PutToken(Token{Value: "t-test", AppID: cfg.Apps[0].AppID, Kind: TenantToken}); err != nil {
		t.Fatal(err)
	}
	state.Apps().Delete(cfg.Apps[0].AppID)
	state.Users().Delete(cfg.Users[0].OpenID)
	state.Messages().Put(Message{ID: "om_test"})
	state.Media().Put(Media{Key: "file_v3_test"})
	if err := state.CreateCard(Card{ID: "card_test"}); err != nil {
		t.Fatal(err)
	}
	state.AppendTrace(Trace{Direction: Inbound})

	state.Reset()
	app, appFound := state.Apps().Get(cfg.Apps[0].AppID)
	user, userFound := state.Users().Get(cfg.Users[0].OpenID)
	chat, chatFound := state.Chats().Get(chatID)
	if !appFound || !reflect.DeepEqual(app, cfg.Apps[0]) || !userFound || user != cfg.Users[0] || !chatFound || chat.MemberIDs[0] != owner || slices.Contains(chat.MemberIDs, "ou_added") {
		t.Fatal("reset did not restore the original seed")
	}
	if _, err := state.CheckToken("t-test"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reset retained credential: %v", err)
	}
	if len(state.Messages().List())+len(state.Media().List())+len(state.Cards())+len(state.Traces()) != 0 {
		t.Fatal("reset retained runtime state")
	}
	state.Seed(config.Config{})
	state.Reset()
	if len(state.Apps().List())+len(state.Users().List())+len(state.Chats().List()) != 0 {
		t.Fatal("empty seed did not replace the old seed")
	}
}

func TestMessageAndMediaSnapshots(t *testing.T) {
	state := New(nil, 0)
	message := Message{ID: "om_reply", RootID: "om_root", ParentID: "om_parent", ThreadID: "omt_test", Updated: true,
		Mentions: []Mention{{OpenID: "ou_test"}}, Reactions: []Reaction{{EmojiType: "SMILE"}}}
	media := Media{Key: "img_v3_test", Bytes: []byte("payload"), ContentType: "image/jpeg"}
	state.Messages().Put(message)
	state.Media().Put(media)
	message.Mentions[0].OpenID = "ou_changed"
	message.Reactions[0].EmojiType = "SAD"
	media.Bytes[0] = 'x'

	list := state.Messages().List()
	got, ok := state.Media().Get(media.Key)
	if !ok || string(got.Bytes) != "payload" || list[0].Mentions[0].OpenID != "ou_test" || list[0].Reactions[0].EmojiType != "SMILE" ||
		list[0].RootID != message.RootID || list[0].ParentID != message.ParentID || list[0].ThreadID != message.ThreadID || !list[0].Updated {
		t.Fatalf("unexpected snapshots: %+v, %+v", list, got)
	}
	list[0].Mentions[0].OpenID = "output changed"
	got.Bytes[0] = 'y'
	keptMessage, _ := state.Messages().Get(message.ID)
	keptMedia, _ := state.Media().Get(media.Key)
	if keptMessage.Mentions[0].OpenID != "ou_test" || string(keptMedia.Bytes) != "payload" {
		t.Fatal("mutating snapshots changed backing state")
	}
}

func TestTraceRing(t *testing.T) {
	state := New(nil, 2)
	code := 230001
	for _, trace := range []Trace{
		{Direction: Inbound, Path: "/old"},
		{Direction: Outbound, Path: "/callback", EventID: "evt_test", Channel: "webhook", Attempt: 2, RequestBody: []byte(`{"event":"test"}`)},
		{Direction: Inbound, Path: "/open-apis/demo", Code: &code, RequestHeaders: http.Header{"Content-Type": []string{"application/json"}}},
	} {
		state.AppendTrace(trace)
	}
	code = 42
	got := state.Traces()
	if len(got) != 2 || got[0].Direction != Outbound || got[0].EventID != "evt_test" || got[0].Attempt != 2 ||
		got[1].Direction != Inbound || got[1].Code == nil || *got[1].Code != 230001 {
		t.Fatalf("unexpected trace order or contents: %+v", got)
	}
	got[0].RequestBody[0] = 'x'
	got[1].RequestHeaders.Set("Content-Type", "text/plain")
	*got[1].Code = 13
	got = state.Traces()
	if string(got[0].RequestBody) != `{"event":"test"}` || got[1].RequestHeaders.Get("Content-Type") != "application/json" || *got[1].Code != 230001 {
		t.Fatal("mutating trace copies changed backing state")
	}
}

func TestConcurrentStoreAccess(t *testing.T) {
	const workers = 32
	state := New(nil, 8)
	state.Seed(config.Default())
	if err := state.CreateCard(Card{ID: "card_test"}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 1; i <= workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("member-%d", i)
			state.Users().Put(User{OpenID: id})
			if err := state.PutToken(Token{Value: id, Kind: UserToken, AppID: "cli_larkbox", UserOpenID: id, ExpiresAt: state.Now().Add(time.Hour)}); err != nil {
				t.Errorf("put token: %v", err)
			}
			state.AddChatMember("oc_larkbox_group", id)
			state.AppendTrace(Trace{Path: id})
			if err := state.UpdateCard(Card{ID: "card_test", Sequence: int64(i)}); err != nil && !errors.Is(err, ErrStaleSequence) {
				t.Errorf("unexpected card update error: %v", err)
			}
			state.Users().List()
			state.Traces()
			if _, err := state.CheckToken(id); err != nil {
				t.Errorf("missing token: %v", err)
			}
		}()
	}
	wg.Wait()
	card, _ := state.Card("card_test")
	chat, _ := state.Chats().Get("oc_larkbox_group")
	if card.Sequence != workers || len(chat.MemberIDs) != len(config.Default().Chats[0].MemberIDs)+workers ||
		len(state.Traces()) != 8 {
		t.Fatal("concurrent writers lost updates or exceeded trace capacity")
	}
}

func TestTokenOwnersAndBinding(t *testing.T) {
	now := time.Unix(1700000000, 0)
	for _, tc := range []struct {
		name                  string
		kind                  TokenKind
		app, user             string
		deleteApp, deleteUser bool
		expectApp, expectUser string
		putErr, lookupErr     error
	}{
		{"valid", UserToken, "cli_a", "ou_a", false, false, "cli_a", "ou_a", nil, nil},
		{"missing app", TenantToken, "", "", false, false, "", "", ErrTokenOwner, ErrNotFound},
		{"unknown app", OAuthCode, "cli_unknown", "", false, false, "", "", ErrTokenOwner, ErrNotFound},
		{"missing user", UserToken, "cli_a", "", false, false, "", "", ErrTokenOwner, ErrNotFound},
		{"unknown user", UserToken, "cli_a", "ou_unknown", false, false, "", "", ErrTokenOwner, ErrNotFound},
		{"unknown attached user", RefreshToken, "cli_a", "ou_unknown", false, false, "", "", ErrTokenOwner, ErrNotFound},
		{"deleted app", AppToken, "cli_a", "", true, false, "", "", nil, ErrTokenOwner},
		{"deleted user", UserToken, "cli_a", "ou_a", false, true, "", "", nil, ErrTokenOwner},
		{"cross-app", UserToken, "cli_a", "ou_a", false, false, "cli_b", "ou_a", nil, ErrTokenBinding},
		{"cross-user", UserToken, "cli_a", "ou_a", false, false, "cli_a", "ou_b", nil, ErrTokenBinding},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := New(func() time.Time { return now }, 0)
			state.Apps().Put(App{AppID: "cli_a"})
			state.Apps().Put(App{AppID: "cli_b"})
			state.Users().Put(User{OpenID: "ou_a"})
			state.Users().Put(User{OpenID: "ou_b"})
			token := Token{Value: "test-token", Kind: tc.kind, AppID: tc.app, UserOpenID: tc.user, ExpiresAt: now.Add(time.Hour)}
			if err := state.PutToken(token); !errors.Is(err, tc.putErr) {
				t.Fatalf("PutToken error = %v, want %v", err, tc.putErr)
			}
			if tc.deleteApp {
				state.Apps().Delete(tc.app)
			}
			if tc.deleteUser {
				state.Users().Delete(tc.user)
			}
			got, err := state.CheckTokenBinding(token.Value, tc.expectApp, tc.expectUser)
			if !errors.Is(err, tc.lookupErr) || (err == nil && got != token) {
				t.Fatalf("CheckTokenBinding = %+v, %v, want %v", got, err, tc.lookupErr)
			}
			if state.DeleteToken(token.Value) != (tc.putErr == nil) || state.DeleteToken(token.Value) {
				t.Fatal("unexpected result deleting token")
			}
		})
	}
}

func TestQueryTracesAfterRingWrap(t *testing.T) {
	state := New(nil, 6)
	for _, tc := range []struct{ app, method, path string }{
		{"cli_a", http.MethodGet, "/open-apis/a/evicted"},
		{"cli_a", http.MethodGet, "/open-apis/a/oldest"},
		{"cli_b", http.MethodGet, "/open-apis/a/foreign"},
		{"cli_a", http.MethodPost, "/open-apis/a/post"},
		{"cli_a", http.MethodGet, "/open-apis/a/previous"},
		{"cli_a", http.MethodGet, "/open-apis/b/other"},
		{"cli_a", http.MethodGet, "/open-apis/a/latest"},
	} {
		state.AppendTrace(Trace{AppID: tc.app, Method: tc.method, Path: tc.path, RequestBody: []byte(`{"body":"test"}`)})
	}
	filter := TraceFilter{AppID: "cli_a", Method: http.MethodGet, PathPrefix: "/open-apis/a/"}
	for _, tc := range []struct {
		limit int
		want  []string
	}{
		{0, []string{"/open-apis/a/oldest", "/open-apis/a/previous", "/open-apis/a/latest"}},
		{2, []string{"/open-apis/a/previous", "/open-apis/a/latest"}},
	} {
		filter.Limit = tc.limit
		traces := state.QueryTraces(filter)
		if len(traces) != len(tc.want) {
			t.Fatalf("limit %d: got %d results, want %d", tc.limit, len(traces), len(tc.want))
		}
		for i, trace := range traces {
			if trace.Path != tc.want[i] || string(trace.RequestBody) != `{"body":"test"}` {
				t.Fatalf("limit %d: unexpected trace %+v", tc.limit, trace)
			}
			trace.RequestBody[0] = 'x'
		}
	}
}

func TestAppRedirectURISnapshots(t *testing.T) {
	cfg := config.Default()
	state := New(nil, 0)
	state.Seed(cfg)
	id, original := cfg.Apps[0].AppID, cfg.Apps[0].RedirectURIs[0]
	cfg.Apps[0].RedirectURIs[0] = "https://changed.test/seed"
	read := func(want string) App {
		t.Helper()
		app, ok := state.Apps().Get(id)
		if !ok || app.RedirectURIs[0] != want {
			t.Fatalf("app did not preserve callback %q", want)
		}
		return app
	}
	app := read(original)
	app.RedirectURIs[0] = "https://changed.test/runtime"
	read(original)
	state.Apps().Put(app)
	app.RedirectURIs[0] = "https://changed.test/after-put"
	read("https://changed.test/runtime")
	listed := state.Apps().List()
	listed[0].RedirectURIs[0] = "https://changed.test/list"
	read("https://changed.test/runtime")
	state.Reset()
	read(original)
}

func TestSandboxMessageCapturesRecipientsAtCommit(t *testing.T) {
	cfg := config.Default()
	initial := cfg.Apps[0]
	replacement := config.App{AppID: "cli_other", BotOpenID: "ou_other"}
	cfg.Apps = append(cfg.Apps, replacement)
	clockEntered, releaseClock := make(chan struct{}), make(chan struct{})
	var once sync.Once
	state := New(func() time.Time {
		once.Do(func() {
			close(clockEntered)
			<-releaseClock
		})
		return time.Unix(1700000000, 0)
	}, 0)
	state.Seed(cfg)
	chatID := cfg.Chats[0].ChatID
	actor := IMActor{OpenID: cfg.Users[0].OpenID, IsUser: true, Sandbox: true}
	req := NewMessage{ChatID: chatID, MsgType: "text", Content: `{"text":"hello"}`}
	type result struct {
		message    Message
		recipients []string
		err        error
	}
	committed := make(chan result, 1)
	go func() {
		message, recipients, _, err := state.CreateSandboxMessage(actor, req)
		committed <- result{message, recipients, err}
	}()
	<-clockEntered
	changed := make(chan error, 1)
	go func() {
		_, _, err := state.ChangeIMChat(actor, chatID, func(chat *Chat) error {
			chat.MemberIDs = append(slices.DeleteFunc(chat.MemberIDs, func(id string) bool { return id == initial.BotOpenID }), replacement.BotOpenID)
			return nil
		})
		changed <- err
	}()
	close(releaseClock)
	first := <-committed
	if err := <-changed; err != nil {
		t.Fatal(err)
	}
	if first.err != nil || first.message.ID == "" || !slices.Equal(first.recipients, []string{initial.AppID}) {
		t.Fatalf("before membership change: %+v", first)
	}
	_, after, _, err := state.CreateSandboxMessage(actor, req)
	if err != nil || !slices.Equal(after, []string{replacement.AppID}) {
		t.Fatalf("after membership change: got %v, %v", after, err)
	}
}
