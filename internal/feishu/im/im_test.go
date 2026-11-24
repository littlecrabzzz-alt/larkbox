package im

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/middleware"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

const (
	imPath     = "/open-apis/im/v1"
	demoGroup  = "oc_larkbox_group"
	demoBot    = "t-demo"
	demoUser   = "u-alice"
	foreignBot = "t-second"
	otherBotID = "cli_other"
)

type setup struct {
	state *store.Store
	mux   http.Handler
	cfg   config.Config
	clock *atomic.Int64
}

func newIM(t *testing.T, emitter EventEmitter) setup {
	t.Helper()
	cfg := config.Default()
	cfg.Apps = append(cfg.Apps, config.App{AppID: otherBotID, BotOpenID: "ou_other_bot", BotName: "Other"})
	clock := &atomic.Int64{}
	clock.Store(1_700_000_000_123)
	state := store.New(func() time.Time { return time.UnixMilli(clock.Load()) }, 0)
	state.Seed(cfg)
	s := setup{state: state, cfg: cfg, clock: clock}
	s.tokens(t)
	mux := http.NewServeMux()
	RegisterRoutes(mux, state, emitter)
	s.mux = middleware.Audit(state, mux)
	return s
}

func (s setup) tokens(t *testing.T) {
	t.Helper()
	expiry := s.state.Now().Add(2 * time.Hour)
	if err := s.state.PutTokens(
		store.Token{Value: demoBot, AppID: s.cfg.Apps[0].AppID, Kind: store.TenantToken, ExpiresAt: expiry},
		store.Token{Value: demoUser, AppID: s.cfg.Apps[0].AppID, UserOpenID: s.cfg.Users[0].OpenID, Kind: store.UserToken, ExpiresAt: expiry},
		store.Token{Value: foreignBot, AppID: otherBotID, Kind: store.TenantToken, ExpiresAt: expiry},
		store.Token{Value: "a-demo", AppID: s.cfg.Apps[0].AppID, Kind: store.AppToken, ExpiresAt: expiry},
	); err != nil {
		t.Fatal(err)
	}
}

func (s setup) request(method, route, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, imPath+route, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return s.serve(r, token)
}

func (s setup) serve(r *http.Request, token string) *httptest.ResponseRecorder {
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)
	return w
}

type response struct {
	Code *int            `json:"code"`
	Data json.RawMessage `json:"data"`
}

func expect(t *testing.T, w *httptest.ResponseRecorder, code int, data any) {
	t.Helper()
	var wire response
	if err := json.Unmarshal(w.Body.Bytes(), &wire); err != nil {
		t.Fatalf("HTTP %d: %v", w.Code, err)
	}
	if wire.Code == nil || *wire.Code != code || (code == 0 && w.Code != http.StatusOK) || (code != 0 && w.Code < 400) {
		t.Fatalf("want code %d, got HTTP %d: %s", code, w.Code, w.Body)
	}
	if data != nil {
		if err := json.Unmarshal(wire.Data, data); err != nil {
			t.Fatalf("decode response data: %s: %v", wire.Data, err)
		}
	}
}

func (s setup) send(t *testing.T, token, idType, target, kind, content, uuid string) wireMessage {
	t.Helper()
	body, err := json.Marshal(map[string]string{
		"receive_id": target, "msg_type": kind, "content": content, "uuid": uuid,
	})
	if err != nil {
		t.Fatal(err)
	}
	var m wireMessage
	expect(t, s.request(http.MethodPost, "/messages?receive_id_type="+idType, token, string(body)), 0, &m)
	if !strings.HasPrefix(m.ID, "om_") || m.Body.Content != content {
		t.Fatalf("unexpected message: %+v", m)
	}
	return m
}

type wireMessage struct {
	ID         string `json:"message_id"`
	ChatID     string `json:"chat_id"`
	CreateTime string `json:"create_time"`
	UpdateTime string `json:"update_time"`
	Sender     struct {
		ID   string `json:"id"`
		Type string `json:"sender_type"`
	} `json:"sender"`
	Body struct {
		Content string `json:"content"`
	} `json:"body"`
}

func TestIMStrictMessagesAndQueries(t *testing.T) {
	s := newIM(t, nil)
	base := `{"receive_id":"oc_larkbox_group","msg_type":"text","content":"{\"text\":\"hello\"}"}`
	post := func(body string) string { return strings.Replace(base, `"{\"text\":\"hello\"}"`, body, 1) }
	for _, test := range []struct {
		name, method, route, token, body string
		code                             int
	}{
		{"missing bearer", "POST", "/messages?receive_id_type=chat_id", "", base, 99991661},
		{"app bearer", "POST", "/messages?receive_id_type=chat_id", "a-demo", base, 99991664},
		{"missing receive type", "POST", "/messages", demoBot, base, 99992402},
		{"invalid receive type", "POST", "/messages?receive_id_type=invalid", demoBot, base, 99992402},
		{"missing receive id", "POST", "/messages?receive_id_type=chat_id", demoBot, `{"msg_type":"text","content":"{}"}`, 99992402},
		{"null JSON body", "POST", "/chats", demoBot, "null", 99992402},
		{"raw content object", "POST", "/messages?receive_id_type=chat_id", demoBot, post(`{"text":"hello"}`), 99992402},
		{"raw numeric content", "POST", "/messages?receive_id_type=chat_id", demoBot, post("5"), 99992402},
		{"null content", "POST", "/messages?receive_id_type=chat_id", demoBot, post("null"), 99992402},
		{"inner invalid JSON", "POST", "/messages?receive_id_type=chat_id", demoBot, post(`"{"`), 99992402},
		{"inner string", "POST", "/messages?receive_id_type=chat_id", demoBot, post(`"\"text\""`), 99992402},
		{"text not a string", "POST", "/messages?receive_id_type=chat_id", demoBot, post(`"{\"text\":true}"`), 99992402},
		{"trailing data", "POST", "/messages?receive_id_type=chat_id", demoBot, base + "{}", 99992402},
		{"missing user", "POST", "/messages?receive_id_type=open_id", demoBot, strings.Replace(base, demoGroup, "ou_missing", 1), 230013},
		{"missing chat", "GET", "/chats/oc_missing", demoBot, "", 232006},
		{"foreign chat", "GET", "/chats/" + demoGroup, foreignBot, "", 232011},
		{"chat page above maximum", "GET", "/chats?page_size=101", demoBot, "", 99992402},
		{"chat page below minimum", "GET", "/chats?page_size=0", demoBot, "", 99992402},
		{"missing container", "GET", "/messages?container_id_type=chat", demoBot, "", 99992402},
		{"bad container", "GET", "/messages?container_id_type=group&container_id=" + demoGroup, demoBot, "", 99992402},
		{"bad sort", "GET", "/messages?container_id_type=chat&container_id=" + demoGroup + "&sort_type=desc", demoBot, "", 99992402},
		{"oversize history page", "GET", "/messages?container_id_type=chat&container_id=" + demoGroup + "&page_size=51", demoBot, "", 99992402},
		{"invalid history cursor", "GET", "/messages?container_id_type=chat&container_id=" + demoGroup + "&page_token=bad", demoBot, "", 99992402},
		{"thread time filter", "GET", "/messages?container_id_type=thread&container_id=omt_missing&end_time=1700000000", demoBot, "", 99992402},
		{"milliseconds in filter", "GET", "/messages?container_id_type=chat&container_id=" + demoGroup + "&start_time=1700000000123", demoBot, "", 99992402},
		{"reversed time range", "GET", "/messages?container_id_type=chat&container_id=" + demoGroup + "&start_time=2&end_time=1", demoBot, "", 99992402},
		{"bad member mode", "POST", "/chats/" + demoGroup + "/members?succeed_type=3", demoBot, `{"id_list":["alice"]}`, 99992402},
		{"empty member list", "POST", "/chats/" + demoGroup + "/members", demoBot, `{"id_list":[]}`, 232027},
		{"public group name", "POST", "/chats", demoBot, `{"chat_type":"public","name":"a"}`, 99992402},
		{"invalid owner", "POST", "/chats", demoBot, `{"owner_id":"ou_missing"}`, 99992402},
		{"too many p2p members", "POST", "/chats?user_id_type=user_id", demoBot, `{"chat_mode":"p2p","user_id_list":["alice","bob"]}`, 99992402},
		{"invalid emoji", "POST", "/messages/om_any/reactions", demoBot, `{"reaction_type":{"emoji_type":""}}`, 99992402},
		{"missing reaction message", "GET", "/messages/om_any/reactions", demoBot, "", 231003},
	} {
		t.Run(test.name, func(t *testing.T) {
			expect(t, s.request(test.method, test.route, test.token, test.body), test.code, nil)
		})
	}
}

func TestMessageContentSchema(t *testing.T) {
	for _, test := range []struct{ kind, good, bad string }{
		{"text", `{"text":""}`, `{"text":null}`},
		{"post", `{"en_us":{"title":"Test","content":[[{"tag":"text","text":"content"}]]}}`, `{"en_us":{"content":[[{}]]}}`},
		{"interactive", `{"elements":[]}`, `[]`},
		{"image", `{"image_key":"img_v3_example"}`, `{"image_key":""}`},
		{"file", `{"file_key":"file_v3_example"}`, `{"file_key":null}`},
		{"share_chat", `{"chat_id":"oc_example"}`, `{"open_id":"ou_example"}`},
		{"share_user", `{"user_id":"ou_example"}`, `{"user_id":123}`},
		{"audio", `{"file_key":"file_v3_example"}`, `{}`},
		{"media", `{"file_key":"file_v3_example","image_key":"img_v3_example"}`, `{"image_key":"img_v3_example"}`},
		{"sticker", `{"file_key":"file_v3_example"}`, `{}`},
		{"system", `{"template":"test"}`, `{"text":"test"}`},
	} {
		t.Run(test.kind, func(t *testing.T) {
			w := httptest.NewRecorder()
			if _, ok := checkContent(w, test.kind, &test.good); !ok {
				t.Fatalf("valid content rejected: %s", w.Body)
			}
			if _, ok := checkContent(w, test.kind, &test.bad); ok {
				t.Fatal("invalid content accepted")
			}
			expect(t, w, 99992402, nil)
		})
	}
}

type eventLog struct {
	mu     sync.Mutex
	events []Event
}

func (l *eventLog) Emit(_ context.Context, e Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func TestIMEventsAndOwnership(t *testing.T) {
	log := &eventLog{}
	s := newIM(t, log)
	chatMembers := "/chats/" + demoGroup + "/members"
	add := `{"id_list":["cli_other"]}`
	expect(t, s.request("POST", chatMembers+"?member_id_type=app_id", demoBot, add), 0, nil)
	expect(t, s.request("POST", chatMembers+"?member_id_type=app_id", demoBot, add), 0, nil)
	before := s.send(t, demoBot, "chat_id", demoGroup, "text", `{"text":"original"}`, "")
	edit := `{"msg_type":"text","content":"{\"text\":\"edit\"}"}`
	expect(t, s.request("PUT", "/messages/"+before.ID, foreignBot, edit), 230027, nil)
	expect(t, s.request("PUT", "/messages/"+before.ID, demoUser, edit), 99991668, nil)
	expect(t, s.request("DELETE", "/messages/"+before.ID, foreignBot, ""), 230027, nil)
	expect(t, s.request("DELETE", chatMembers+"?member_id_type=open_id", demoUser, `{"id_list":["ou_other_bot"]}`), 0, nil)
	expect(t, s.request("GET", "/messages/"+before.ID, foreignBot, ""), 230002, nil)
	expect(t, s.request("DELETE", chatMembers+"?member_id_type=app_id", demoUser, add), 0, nil)

	s.clock.Add(2000)
	var edited wireMessage
	expect(t, s.request("PUT", "/messages/"+before.ID, demoBot, edit), 0, &edited)
	if edited.UpdateTime != "1700000002123" || edited.CreateTime != "1700000000123" {
		t.Fatalf("unexpected timestamps: %+v", edited)
	}
	expect(t, s.request("POST", "/messages/"+before.ID+"/reply", demoBot, `{"msg_type":"text","content":"{\"text\":\"reply\"}","reply_in_thread":null}`), 99992402, nil)
	expect(t, s.request("DELETE", "/messages/"+before.ID, demoBot, ""), 0, nil)
	expect(t, s.request("DELETE", "/messages/"+before.ID, demoBot, ""), 230011, nil)
	expect(t, s.request("PUT", "/messages/"+before.ID, demoBot, edit), 230011, nil)
	expect(t, s.request("POST", "/messages/"+before.ID+"/reply", demoBot, `{"msg_type":"text","content":"{\"text\":\"reply\"}"}`), 230011, nil)
	expect(t, s.request("POST", "/messages/"+before.ID+"/reactions", demoBot, `{"reaction_type":{"emoji_type":"SMILE"}}`), 231003, nil)
	if len(log.events) != 3 || log.events[0].Type != "im.chat.member.bot.added_v1" ||
		log.events[1].Type != "im.chat.member.bot.deleted_v1" || log.events[0].AppID != otherBotID ||
		log.events[1].BotOpenID != s.cfg.Apps[1].BotOpenID || log.events[2].Type != "im.message.recalled_v1" ||
		log.events[2].ChatID != demoGroup || log.events[2].MessageID != before.ID ||
		log.events[2].ActorID != s.cfg.Apps[0].BotOpenID || !log.events[2].At.Equal(s.state.Now()) {
		t.Fatalf("unexpected events: %+v", log.events)
	}

	fromUser := s.send(t, demoUser, "union_id", s.cfg.Users[1].UnionID, "text", `{"text":"user chat"}`, "")
	var data struct {
		Items []wireMessage `json:"items"`
	}
	expect(t, s.request("GET", "/messages/"+fromUser.ID+"?user_id_type=user_id", demoUser, ""), 0, &data)
	if len(data.Items) != 1 || data.Items[0].Sender.ID != s.cfg.Users[0].UserID || data.Items[0].Sender.Type != "user" {
		t.Fatalf("unexpected user identity: %+v", data)
	}
	expect(t, s.request("GET", "/messages/"+fromUser.ID, demoBot, ""), 230002, nil)
}

func TestMemberFailureIsAtomic(t *testing.T) {
	s := newIM(t, nil)
	r := "/chats/" + demoGroup + "/members?member_id_type=app_id"
	body := `{"id_list":["cli_other","cli_missing","bad"]}`
	for _, mode := range []string{"", "&succeed_type=2"} {
		var result struct {
			Invalid []string `json:"invalid_id_list"`
			Missing []string `json:"not_existed_id_list"`
		}
		expect(t, s.request("POST", r+mode, demoBot, body), 232001, &result)
		if !slices.Equal(result.Invalid, []string{"bad"}) || !slices.Equal(result.Missing, []string{"cli_missing"}) {
			t.Fatalf("unexpected invalid or missing member lists: %+v", result)
		}
		expect(t, s.request("GET", "/chats/"+demoGroup, foreignBot, ""), 232011, nil)
	}
	expect(t, s.request("POST", r+"&succeed_type=1", demoBot, body), 0, nil)
	expect(t, s.request("GET", "/chats/"+demoGroup, foreignBot, ""), 0, nil)
}

func TestIMConcurrentDedupAndReset(t *testing.T) {
	s := newIM(t, nil)
	const workers = 32
	var wg sync.WaitGroup
	results := make(chan wireMessage, workers)
	for index := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			targetType, target := "open_id", s.cfg.Users[1].OpenID
			if index%2 == 0 {
				targetType, target = "email", s.cfg.Users[1].Email
			}
			m := s.send(t, demoBot, targetType, target, "text", `{"text":"deduplicate"}`, "request-1")
			results <- m
		}()
	}
	wg.Wait()
	close(results)
	var id, directID string
	for m := range results {
		if id == "" {
			id, directID = m.ID, m.ChatID
		}
		if m.ID != id || m.ChatID != directID {
			t.Fatalf("parallel callers received different messages or direct chats: %+v", m)
		}
	}
	if len(s.state.Messages().List()) != 1 || len(s.state.Chats().List()) != len(s.cfg.Chats)+1 {
		t.Fatal("parallel duplicate operations committed more than once")
	}
	// Different requests reuse the same direct chat under the same contention.
	for index := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := s.send(t, demoBot, "union_id", s.cfg.Users[1].UnionID, "text", `{"text":"unique"}`, fmt.Sprint(index))
			if m.ChatID != directID {
				t.Errorf("direct chat changed: %s", m.ChatID)
			}
		}()
	}
	wg.Wait()
	if len(s.state.Messages().List()) != workers+1 || len(s.state.Chats().List()) != len(s.cfg.Chats)+1 {
		t.Fatal("parallel unique operations lost state")
	}
	s.state.Reset()
	s.tokens(t)
	again := s.send(t, demoBot, "user_id", s.cfg.Users[1].UserID, "text", `{"text":"deduplicate"}`, "request-1")
	if again.ChatID != directID || again.ID == id || len(s.state.Messages().List()) != 1 {
		t.Fatal("reset did not clear messages and deduplication or direct chat ID was not deterministic")
	}
}

func TestIMMultipartBoundsAndAudit(t *testing.T) {
	s := newIM(t, nil)
	upload := func(route, partName string, fields map[string]string, contents []byte) *httptest.ResponseRecorder {
		t.Helper()
		var buffer bytes.Buffer
		writer := multipart.NewWriter(&buffer)
		for key, val := range fields {
			if err := writer.WriteField(key, val); err != nil {
				t.Fatal(err)
			}
		}
		if partName != "" {
			part, err := writer.CreateFormFile(partName, "fixture.dat")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(part, bytes.NewReader(contents)); err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("POST", imPath+route, &buffer)
		r.Header.Set("Content-Type", writer.FormDataContentType())
		return s.serve(r, demoBot)
	}
	imageFields := map[string]string{"image_type": "message"}
	expect(t, upload("/images", "", imageFields, nil), 99992402, nil)
	expect(t, upload("/images", "image", imageFields, nil), 99992402, nil)
	expect(t, upload("/images", "image", map[string]string{"image_type": "unsupported"}, []byte{1}), 99992402, nil)
	expect(t, upload("/images", "image", imageFields, make([]byte, maxImageSize+1)), 99992402, nil)
	expect(t, upload("/files", "file", map[string]string{"file_type": "stream", "file_name": "file", "duration": "-1"}, []byte{1}), 99992402, nil)
	expect(t, upload("/files", "file", map[string]string{"file_type": "stream", "file_name": ".."}, []byte{1}), 99992402, nil)
	expect(t, upload("/files", "file", map[string]string{"file_type": "stream", "file_name": strings.Repeat("é", 128)}, []byte{1}), 99992402, nil)

	// A binary attachment can contain JSON without becoming a business envelope.
	body := []byte(`{"code":1234,"msg":"attachment"}`)
	var data struct {
		Key string `json:"file_key"`
	}
	expect(t, upload("/files", "file", map[string]string{"file_type": "stream", "file_name": "folder/file.json"}, body), 0, &data)
	w := s.request("GET", "/files/"+data.Key, demoBot, "")
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), body) ||
		w.Header().Get("Content-Disposition") != "attachment; filename=file.json" {
		t.Fatalf("download changed binary payload or headers: %v, %q", w.Header(), w.Body.Bytes())
	}
	traces := s.state.Traces()
	trace := traces[len(traces)-1]
	if trace.Code != nil || len(trace.ResponseBody) != 0 || trace.ResponseHeaders.Get("Content-Disposition") == "" {
		t.Fatalf("download audit did not distinguish binary from JSON: %+v", trace)
	}
	expect(t, s.request("GET", "/files/"+data.Key, foreignBot, ""), 234002, nil)
	expect(t, s.request("GET", "/files/"+data.Key, demoUser, ""), 99991668, nil)
	resource := s.send(t, demoBot, "chat_id", demoGroup, "post",
		fmt.Sprintf(`{"en_us":{"content":[[{"tag":"text","text":"%s"}]]}}`, data.Key), "")
	expect(t, s.request("GET", "/messages/"+resource.ID+"/resources/"+data.Key+"?type=file", demoBot, ""), 234003, nil)
	s.state.Reset()
	s.tokens(t)
	expect(t, s.request("GET", "/files/"+data.Key, demoBot, ""), 234001, nil)
}

func TestIMUUIDExpiresAndValidatesTargets(t *testing.T) {
	s := newIM(t, nil)
	s.state.AddChatMember(demoGroup, s.cfg.Apps[1].BotOpenID)
	send := func(token string) wireMessage {
		t.Helper()
		return s.send(t, token, "chat_id", demoGroup, "text", `{"text":"one UUID"}`, "shared")
	}
	initial := send(demoBot)
	if other := send(foreignBot); other.ID == initial.ID {
		t.Fatal("different apps shared a deduplication record")
	}
	originalChats := len(s.state.Chats().List())
	originalMessages := len(s.state.Messages().List())
	conflict := `{"receive_id":"ou_larkbox_bob","msg_type":"text","content":"{\"text\":\"other target\"}","uuid":"shared"}`
	expect(t, s.request("POST", "/messages?receive_id_type=open_id", demoBot, conflict), 230001, nil)
	expect(t, s.request("POST", "/messages?receive_id_type=chat_id", demoUser,
		`{"receive_id":"oc_larkbox_group","msg_type":"text","content":"{\"text\":\"other sender\"}","uuid":"shared"}`), 230001, nil)
	if len(s.state.Chats().List()) != originalChats || len(s.state.Messages().List()) != originalMessages {
		t.Fatal("conflicting UUID reuse created a conversation or message")
	}

	// Email and chat ID identify the same seeded direct conversation.
	alice := s.cfg.Users[0]
	direct := s.send(t, demoBot, "email", alice.Email, "text", `{"text":"direct"}`, "direct-uuid")
	if retry := s.send(t, demoBot, "chat_id", direct.ChatID, "text", `{"text":"direct"}`, "direct-uuid"); retry.ID != direct.ID {
		t.Fatal("deduplication did not recognize the canonical direct chat")
	}
	s.clock.Add((time.Hour - time.Millisecond).Milliseconds())
	if retry := send(demoBot); retry.ID != initial.ID {
		t.Fatal("deduplication expired before one hour")
	}
	s.clock.Add(1)
	if fresh := send(demoBot); fresh.ID == initial.ID {
		t.Fatal("deduplication persisted at the one-hour boundary")
	}

	reply := `{"msg_type":"text","content":"{\"text\":\"reply\"}","uuid":"reply-uuid"}`
	var replyOriginal, replyRepeat wireMessage
	expect(t, s.request("POST", "/messages/"+initial.ID+"/reply", demoBot, reply), 0, &replyOriginal)
	expect(t, s.request("POST", "/messages/"+initial.ID+"/reply", demoBot, reply), 0, &replyRepeat)
	if replyRepeat.ID != replyOriginal.ID {
		t.Fatal("repeated reply was not deduplicated")
	}
	otherParent := s.send(t, demoBot, "chat_id", demoGroup, "text", `{"text":"another parent"}`, "")
	expect(t, s.request("POST", "/messages/"+otherParent.ID+"/reply", demoBot, reply), 230001, nil)
	expect(t, s.request("POST", "/messages/"+otherParent.ID+"/reply", demoBot,
		strings.Replace(reply, "reply-uuid", "shared", 1)), 0, nil)
	expect(t, s.request("POST", "/messages/om_missing/reply", demoBot, reply), 230001, nil)
	expect(t, s.request("DELETE", "/messages/"+initial.ID, demoBot, ""), 0, nil)
	expect(t, s.request("POST", "/messages/"+initial.ID+"/reply", demoBot, reply), 230011, nil)
	expect(t, s.request("POST", "/messages?receive_id_type=chat_id", demoBot,
		`{"receive_id":"oc_missing","msg_type":"text","content":"{\"text\":\"test\"}","uuid":"shared"}`), 232006, nil)
}

func TestIMMemberCursorResumesAfterRemoval(t *testing.T) {
	s := newIM(t, nil)
	var chat struct {
		ID string `json:"chat_id"`
	}
	expect(t, s.request("POST", "/chats?user_id_type=user_id", demoBot,
		`{"user_id_list":["alice","bob"]}`), 0, &chat)
	path := "/chats/" + chat.ID + "/members?member_id_type=user_id"
	type membersPage struct {
		PageToken string              `json:"page_token"`
		HasMore   bool                `json:"has_more"`
		Items     []map[string]string `json:"items"`
	}
	var first, next membersPage
	expect(t, s.request("GET", path+"&page_size=1", demoBot, ""), 0, &first)
	if len(first.Items) != 1 || first.Items[0]["member_id"] != "alice" || first.PageToken == "" {
		t.Fatalf("unexpected first member page: %+v", first)
	}
	expect(t, s.request("DELETE", path, demoBot, `{"id_list":["alice"]}`), 0, nil)
	expect(t, s.request("GET", path+"&page_size=1&page_token="+first.PageToken, demoBot, ""), 0, &next)
	if len(next.Items) != 1 || next.Items[0]["member_id"] != "bob" || next.HasMore {
		t.Fatalf("unexpected second member page after deletion: %+v", next)
	}
}

func TestIMReactionFilterValidation(t *testing.T) {
	s := newIM(t, nil)
	m := s.send(t, demoBot, "chat_id", demoGroup, "text", `{"text":"reaction fixture"}`, "")
	path := "/messages/" + m.ID + "/reactions"
	for _, query := range []string{"", "invalid-type", "SMILE&reaction_type=SMILE"} {
		expect(t, s.request("GET", path+"?reaction_type="+query, demoBot, ""), 99992402, nil)
	}
	expect(t, s.request("POST", path, demoBot, `{"reaction_type":{"emoji_type":"SMILE NOW"}}`), 99992402, nil)
	expect(t, s.request("POST", path, demoBot, `{"reaction_type":{"emoji_type":"UNKNOWN_EMOJI_XYZ"}}`), 231001, nil)
	expect(t, s.request("GET", path+"?reaction_type=UNKNOWN_EMOJI_XYZ", demoBot, ""), 231001, nil)
}
