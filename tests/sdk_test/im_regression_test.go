package sdk_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/server"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

func TestOfficialSDKIMAppMembership(t *testing.T) {
	cfg := sdkTestConfig()
	otherApp := config.App{AppID: "cli_review_second", AppSecret: cfg.Apps[0].AppSecret, BotOpenID: "ou_review_bot"}
	cfg.Apps = append(cfg.Apps, otherApp)
	primary := cfg.Apps[0]
	chatID := cfg.Chats[0].ChatID
	srv := server.New(cfg)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	first := lark.NewClient(primary.AppID, primary.AppSecret, lark.WithOpenBaseUrl(ts.URL), lark.WithHttpClient(ts.Client()))
	second := lark.NewClient(otherApp.AppID, otherApp.AppSecret, lark.WithOpenBaseUrl(ts.URL), lark.WithHttpClient(ts.Client()))
	requireSDKNetwork(t, srv.Store().PutToken(store.Token{
		Value: "u-review-alice", Kind: store.UserToken, AppID: otherApp.AppID, UserOpenID: cfg.Users[0].OpenID,
		ExpiresAt: srv.Store().Now().Add(time.Hour),
	}))
	asAlice := larkcore.WithUserAccessToken("u-review-alice")
	file := []byte("%PDF-1.7\nshared fixture\x00")
	up, err := first.Im.File.Create(t.Context(), larkim.NewCreateFileReqBuilder().
		Body(larkim.NewCreateFileReqBodyBuilder().FileType("pdf").FileName("fixture.pdf").File(bytes.NewReader(file)).Build()).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, up.CodeError, 0)
	fileKey := value(up.Data.FileKey)
	m := sdkSend(t, first, "chat_id", chatID, "file", fmt.Sprintf(`{"file_key":%q}`, fileKey))
	messageID := value(m.MessageId)
	getResource := func() *larkim.GetMessageResourceResp {
		t.Helper()
		resp, err := second.Im.MessageResource.Get(t.Context(), larkim.NewGetMessageResourceReqBuilder().MessageId(messageID).
			FileKey(fileKey).Type("file").Build(), asAlice)
		requireSDKNetwork(t, err)
		return resp
	}
	blocked := getResource()
	requireSDKCode(t, blocked.CodeError, 234004)
	chat, err := second.Im.Chat.Get(t.Context(), larkim.NewGetChatReqBuilder().ChatId(chatID).Build(), asAlice)
	requireSDKNetwork(t, err)
	requireSDKCode(t, chat.CodeError, 232011)
	members, err := second.Im.ChatMembers.Get(t.Context(), larkim.NewGetChatMembersReqBuilder().ChatId(chatID).Build(), asAlice)
	requireSDKNetwork(t, err)
	requireSDKCode(t, members.CodeError, 232011)
	message, err := second.Im.Message.Get(t.Context(), larkim.NewGetMessageReqBuilder().MessageId(messageID).Build(), asAlice)
	requireSDKNetwork(t, err)
	requireSDKCode(t, message.CodeError, 230002)
	joinRequest := func() *larkim.CreateChatMembersReq {
		return larkim.NewCreateChatMembersReqBuilder().ChatId(chatID).MemberIdType("app_id").
			Body(larkim.NewCreateChatMembersReqBodyBuilder().IdList([]string{otherApp.AppID}).Build()).Build()
	}
	mutation, err := second.Im.ChatMembers.Create(t.Context(), joinRequest(), asAlice)
	requireSDKNetwork(t, err)
	requireSDKCode(t, mutation.CodeError, 232011)
	join, err := first.Im.ChatMembers.Create(t.Context(), joinRequest())
	requireSDKNetwork(t, err)
	requireSDKCode(t, join.CodeError, 0)
	chat, err = second.Im.Chat.Get(t.Context(), larkim.NewGetChatReqBuilder().ChatId(chatID).Build(), asAlice)
	requireSDKNetwork(t, err)
	requireSDKCode(t, chat.CodeError, 0)
	joined := getResource()
	requireSDKCode(t, joined.CodeError, 0)
	assertSDKMedia(t, joined.ApiResp, joined.File, joined.FileName, "application/pdf", "fixture.pdf", file)

	// Even a public group requires app membership to retrieve attachments.
	public := cfg.Chats[0]
	public.ChatID = "oc_review_public"
	public.ChatType = "public"
	srv.Store().Chats().Put(public)
	publicMessage := sdkSend(t, first, "chat_id", public.ChatID, "file", fmt.Sprintf(`{"file_key":%q}`, fileKey))
	messageID = value(publicMessage.MessageId)
	requireSDKCode(t, getResource().CodeError, 234004)
}

func TestOfficialSDKIMThreadModesAndMentions(t *testing.T) {
	cfg := sdkTestConfig()
	ts := httptest.NewServer(server.New(cfg))
	t.Cleanup(ts.Close)
	app := cfg.Apps[0]
	client := lark.NewClient(app.AppID, app.AppSecret, lark.WithOpenBaseUrl(ts.URL), lark.WithHttpClient(ts.Client()))
	api := client.Im
	reply := func(id string) *larkim.ReplyMessageRespData {
		t.Helper()
		resp, err := api.Message.Reply(t.Context(), larkim.NewReplyMessageReqBuilder().MessageId(id).
			Body(larkim.NewReplyMessageReqBodyBuilder().MsgType("text").Content(`{"text":"reply"}`).Build()).Build())
		requireSDKNetwork(t, err)
		requireSDKCode(t, resp.CodeError, 0)
		return resp.Data
	}
	chatID := cfg.Chats[0].ChatID
	root := sdkSend(t, client, "chat_id", chatID, "text", `{"text":"ordinary root"}`)
	ordinary := reply(value(root.MessageId))
	nested := reply(value(ordinary.MessageId))
	if value(nested.ParentId) != value(ordinary.MessageId) || value(nested.RootId) != value(root.MessageId) ||
		value(nested.ThreadId) != "" {
		t.Fatalf("ordinary nested reply lost its immediate parent: %+v", nested)
	}

	threadGroup, err := api.Chat.Create(t.Context(), larkim.NewCreateChatReqBuilder().
		Body(larkim.NewCreateChatReqBodyBuilder().GroupMessageType("thread").Build()).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, threadGroup.CodeError, 0)
	if value(threadGroup.Data.GroupMessageType) != "thread" {
		t.Fatalf("unexpected group type: %s", threadGroup.RawBody)
	}
	topic := sdkSend(t, client, "chat_id", value(threadGroup.Data.ChatId), "text", `{"text":"topic root"}`)
	topicID, threadID := value(topic.MessageId), value(topic.ThreadId)
	if !strings.HasPrefix(threadID, "omt_") {
		t.Fatalf("new top-level message has no topic: %+v", topic)
	}
	topicReply := reply(value(reply(topicID).MessageId))
	if value(topicReply.ParentId) != topicID || value(topicReply.RootId) != topicID || value(topicReply.ThreadId) != threadID {
		t.Fatalf("topic reply did not reference the topic root: %+v", topicReply)
	}

	alice := cfg.Users[0]
	content := fmt.Sprintf(`{"text":"<at user_id=\"%s\">Alice</at> and <at user_id=\"%s\">Bot</at>, <at user_id=\"%s\">again</at>"}`,
		alice.UserID, app.BotOpenID, alice.UnionID)
	sent, err := api.Message.Create(t.Context(), larkim.NewCreateMessageReqBuilder().ReceiveIdType("chat_id").
		Body(larkim.NewCreateMessageReqBodyBuilder().ReceiveId(chatID).MsgType("text").Content(content).Build()).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, sent.CodeError, 0)
	messageID := value(sent.Data.MessageId)
	want := `{"text":"@_user_1 and @_user_2, @_user_1"}`
	if value(sent.Data.Body.Content) != want {
		t.Fatalf("message send did not normalize mentions: %s", sent.RawBody)
	}
	assertMentions := func(items []*larkim.Message, idType, userID, botIDType, botID string) {
		t.Helper()
		for _, m := range items {
			if value(m.MessageId) != messageID {
				continue
			}
			if value(m.Body.Content) != want || len(m.Mentions) != 2 ||
				value(m.Mentions[0].Key) != "@_user_1" || value(m.Mentions[0].Id) != userID ||
				value(m.Mentions[0].IdType) != idType || value(m.Mentions[0].Name) != alice.Name ||
				value(m.Mentions[1].Key) != "@_user_2" || value(m.Mentions[1].Id) != botID ||
				value(m.Mentions[1].IdType) != botIDType || value(m.Mentions[1].Name) != app.BotName {
				t.Fatalf("unexpected mention mapping for %s: %+v", idType, m.Mentions)
			}
			return
		}
		t.Fatal("response did not contain the message with mentions")
	}
	for _, id := range []struct {
		param, idType, userID, botIDType, botID string
	}{
		{"", "open_id", alice.OpenID, "open_id", app.BotOpenID},
		{"open_id", "open_id", alice.OpenID, "app_id", app.AppID},
		{"union_id", "union_id", alice.UnionID, "app_id", app.AppID},
	} {
		req := larkim.NewGetMessageReqBuilder().MessageId(messageID)
		if id.param != "" {
			req.UserIdType(id.param)
		}
		got, err := api.Message.Get(t.Context(), req.Build())
		requireSDKNetwork(t, err)
		requireSDKCode(t, got.CodeError, 0)
		assertMentions(got.Data.Items, id.idType, id.userID, id.botIDType, id.botID)

		// The generated history request has no user_id_type setter.
		query := larkcore.QueryParams{"container_id_type": {"chat"}, "container_id": {chatID}}
		if id.param != "" {
			query.Set("user_id_type", id.param)
		}
		history, err := client.Do(t.Context(), &larkcore.ApiReq{
			HttpMethod: http.MethodGet, ApiPath: "/open-apis/im/v1/messages", QueryParams: query,
			SupportedAccessTokenTypes: []larkcore.AccessTokenType{larkcore.AccessTokenTypeTenant},
		})
		requireSDKNetwork(t, err)
		var list larkim.ListMessageResp
		requireSDKNetwork(t, json.Unmarshal(history.RawBody, &list))
		requireSDKCode(t, list.CodeError, 0)
		assertMentions(list.Data.Items, id.idType, id.userID, id.botIDType, id.botID)
	}
}

func TestOfficialSDKIMMemberModes(t *testing.T) {
	cfg := sdkTestConfig()
	cfg.Apps = append(cfg.Apps, config.App{AppID: "cli_disabled"})
	cfg.Users = append(cfg.Users, config.User{OpenID: "ou_inactive", Inactive: true})
	app := cfg.Apps[0]
	ts := httptest.NewServer(server.New(cfg))
	t.Cleanup(ts.Close)
	client := lark.NewClient(app.AppID, app.AppSecret, lark.WithOpenBaseUrl(ts.URL), lark.WithHttpClient(ts.Client()))
	api := client.Im
	add := func(chat, kind string, mode int, ids ...string) *larkim.CreateChatMembersResp {
		t.Helper()
		result, err := api.ChatMembers.Create(t.Context(), larkim.NewCreateChatMembersReqBuilder().ChatId(chat).
			MemberIdType(kind).SucceedType(mode).Body(larkim.NewCreateChatMembersReqBodyBuilder().IdList(ids).Build()).Build())
		requireSDKNetwork(t, err)
		return result
	}
	chatID := cfg.Chats[0].ChatID
	for _, mode := range []int{0, 1, 2} {
		bad := add(chatID, "app_id", mode, "cli_disabled", "cli_unknown", "malformed")
		requireSDKCode(t, bad.CodeError, 232027)
		if bad.Data == nil || !slices.Equal(bad.Data.InvalidIdList, []string{"cli_disabled", "malformed"}) ||
			!slices.Equal(bad.Data.NotExistedIdList, []string{"cli_unknown"}) {
			t.Fatalf("mode %d did not classify known disabled and unknown app IDs: %s", mode, bad.RawBody)
		}
	}
	alice := cfg.Users[0].OpenID
	for _, mode := range []int{0, 1, 2} {
		mixed := add(chatID, "open_id", mode, alice, "ou_unknown", "malformed")
		want := 232001
		if mode == 1 {
			want = 0
		}
		requireSDKCode(t, mixed.CodeError, want)
		if mixed.Data == nil || !slices.Equal(mixed.Data.InvalidIdList, []string{"malformed"}) ||
			!slices.Equal(mixed.Data.NotExistedIdList, []string{"ou_unknown"}) {
			t.Fatalf("mode %d did not report invalid and unknown users in a mixed list: %s", mode, mixed.RawBody)
		}
	}
	defaultMode := add(chatID, "open_id", 0, alice, "ou_inactive")
	requireSDKCode(t, defaultMode.CodeError, 0)
	if !slices.Equal(defaultMode.Data.InvalidIdList, []string{"ou_inactive"}) {
		t.Fatalf("default mode did not report the inactive user: %s", defaultMode.RawBody)
	}
	requireSDKCode(t, add(chatID, "open_id", 2, alice, "ou_inactive").CodeError, 232001)
	requireSDKCode(t, add("oc_missing", "open_id", 0, alice).CodeError, 232006)
}

func TestOfficialSDKIMCardValidation(t *testing.T) {
	cfg := sdkTestConfig()
	ts := httptest.NewServer(server.New(cfg))
	t.Cleanup(ts.Close)
	app := cfg.Apps[0]
	client := lark.NewClient(app.AppID, app.AppSecret, lark.WithOpenBaseUrl(ts.URL), lark.WithHttpClient(ts.Client()))
	api := client.Im.Message
	chatID := cfg.Chats[0].ChatID
	card := `{"schema":"2.0","body":{"elements":[{"tag":"markdown","content":"original"}]}}`
	localized := `{
	  "schema":"1.0","config":{"update_multi":true},
	  "i18n_elements":{
	    "en_us":[{"tag":"column_set","columns":[{"tag":"column","width":"weighted","weight":1,"elements":[
	      {"tag":"div","text":{"tag":"plain_text","content":"Details"}},
	      {"tag":"note","elements":[{"tag":"plain_text","content":"Note"},{"tag":"img","img_key":"img_v3_example","alt":{"tag":"plain_text","content":"Preview"}}]},
	      {"tag":"action","actions":[{"tag":"button","text":{"tag":"plain_text","content":"Read"},"url":"https://example.test"}]}
	    ]}]}],
	    "ja_jp":[{"tag":"form","name":"feedback","elements":[
	      {"tag":"input","name":"reason","required":true},
	      {"tag":"button","name":"send","action_type":"form_submit","text":{"tag":"lark_md","content":"Submit"}}
	    ]}]
	  }
	}`
	original := sdkSend(t, client, "chat_id", chatID, "interactive", card)
	localizedMessage := sdkSend(t, client, "chat_id", chatID, "interactive", localized)
	sdkSend(t, client, "chat_id", chatID, "interactive", `{"type":"template","data":{"template_id":"AAfixture"}}`)
	id := value(original.MessageId)
	patch := func(id, content string) *larkim.PatchMessageResp {
		t.Helper()
		res, err := api.Patch(t.Context(), larkim.NewPatchMessageReqBuilder().MessageId(id).
			Body(larkim.NewPatchMessageReqBodyBuilder().Content(content).Build()).Build())
		requireSDKNetwork(t, err)
		return res
	}
	invalidCards := []string{
		`{}`, `{"elements":{}}`, `{"elements":[{"content":"no tag"}]}`,
		`{"elements":[],"header":[]}`, `{"elements":[],"config":"bad"}`,
		`{"schema":"2.0","body":[]}`, `{"schema":"2.0","config":{"update_multi":false},"body":{"elements":[]}}`,
		`{"schema":"2.0"}`, `{"schema":"2.0","header":{}}`,
		`{"schema":"2.0","header":{"title":{"tag":"plain_text","content":"Title"}},"body":null}`,
		`{"schema":"2.0","header":{"title":{"tag":"plain_text","content":"Title"}},"body":{"elements":"invalid"}}`,
		`{"schema":"2.0","body":{"elements":[{"tag":"button","text":{"tag":"plain_text","content":"Button"},"size":5}]}}`,
		`{"schema":"2.0","body":{"elements":[{"tag":"select_static","options":[{"value":"one"}],"width":10}]}}`,
		`{"schema":"2.0","body":{"elements":[{"tag":"select_static","options":{"value":"one"}}]}}`,
		`{"schema":"2.0","body":{"elements":[{"tag":"date_picker","initial_date":20260101}]}}`,
		`{"schema":"2.0","body":{"elements":[{"tag":"checker","checked":"true"}]}}`,
		`{"type":"template","data":{"template_id":7}}`,
		`{"i18n_elements":{}}`, `{"i18n_elements":{"en_us":[]}}`, `{"i18n_elements":{"en_us":{"tag":"hr"}}}`,
		`{"i18n_elements":{"en_us":[{"tag":"markdown","content":42}]}}`,
		`{"elements":[{"tag":"markdown","content":42}]}`,
		`{"elements":[{"tag":"div","text":{"tag":"plain_text","content":true}}]}`,
		`{"elements":[{"tag":"column_set","columns":[{"tag":"column","elements":[{"tag":"div","text":"bad"}]}]}]}`,
		`{"elements":[{"tag":"action","actions":[{"tag":"button","text":42}]}]}`,
		`{"elements":[{"tag":"img","img_key":null,"alt":{"tag":"plain_text","content":"Image"}}]}`,
		`{"elements":[{"tag":"note","elements":[{"tag":"plain_text","content":[] }]}]}`,
		`{"elements":[{"tag":"form","name":"feedback","elements":[{"tag":"input","name":"reason"}]}]}`,
		`{"elements":[{"tag":"collapsible_panel","header":{"title":{"tag":"plain_text","content":3}}}]}`,
	}
	nestedInput := func(fields string) string {
		return `{"schema":"2.0","body":{"elements":[{"tag":"column_set","columns":[{"tag":"column","elements":[{"tag":"input",` +
			fields + `}]}]}]}}`
	}
	for _, field := range []string{
		`"max_length":"123"`, `"max_length":1.0`, `"max_length":5.2`, `"max_length":0`, `"max_length":1001`,
		`"default_value":2`, `"required":"true"`, `"disabled":1`, `"input_type":true`, `"input_type":"number"`, `"rows":0`, `"rows":2.0`,
		`"max_rows":"3"`, `"max_rows":true`, `"max_rows":{}`, `"max_rows":1e1000`, `"show_icon":"false"`, `"width":20`, `"name":[]`,
		`"placeholder":"Plain text"`, `"label":{"tag":"plain_text","content":4}`,
	} {
		invalidCards = append(invalidCards, nestedInput(field))
	}
	for _, invalid := range invalidCards {
		sent, err := api.Create(t.Context(), larkim.NewCreateMessageReqBuilder().ReceiveIdType("chat_id").
			Body(larkim.NewCreateMessageReqBodyBuilder().ReceiveId(chatID).MsgType("interactive").Content(invalid).Build()).Build())
		requireSDKNetwork(t, err)
		requireSDKCode(t, sent.CodeError, 99992402)
		requireSDKCode(t, patch(id, invalid).CodeError, 99992402)
	}
	unshared := `{"config":{"update_multi":false},"elements":[]}`
	requireSDKCode(t, patch(id, unshared).CodeError, 230001)
	notShared := sdkSend(t, client, "chat_id", chatID, "interactive", unshared)
	requireSDKCode(t, patch(value(notShared.MessageId), card).CodeError, 230001)
	got, err := api.Get(t.Context(), larkim.NewGetMessageReqBuilder().MessageId(id).CardMsgContentType("user_card_content").Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, got.CodeError, 0)
	if len(got.Data.Items) != 1 || value(got.Data.Items[0].Body.Content) != card || value(got.Data.Items[0].Updated) ||
		value(got.Data.Items[0].UpdateTime) != value(original.UpdateTime) {
		t.Fatalf("invalid patch changed the original card: %s", got.RawBody)
	}
	localizedID := value(localizedMessage.MessageId)
	requireSDKCode(t, patch(localizedID,
		`{"config":{"update_multi":true},"i18n_elements":{"en_us":[{"tag":"markdown","content":42}]}}`).CodeError, 99992402)
	localizedDetail, err := api.Get(t.Context(), larkim.NewGetMessageReqBuilder().MessageId(localizedID).CardMsgContentType("user_card_content").Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, localizedDetail.CodeError, 0)
	if len(localizedDetail.Data.Items) != 1 || value(localizedDetail.Data.Items[0].Body.Content) != localized ||
		value(localizedDetail.Data.Items[0].Updated) {
		t.Fatalf("invalid patch changed the localized card: %s", localizedDetail.RawBody)
	}
	requireSDKCode(t, patch(value(localizedMessage.MessageId),
		`{"config":{"update_multi":true},"i18n_elements":{"en_us":[{"tag":"markdown","content":"Updated"}]}}`).CodeError, 0)
	titleOnly := `{"schema":"2.0","header":{"title":{"tag":"plain_text","content":"Initial title"}}}`
	newTitle := `{"schema":"2.0","header":{"title":{"tag":"plain_text","content":"Updated title"}}}`
	titleMessage := sdkSend(t, client, "chat_id", chatID, "interactive", titleOnly)
	titleID := value(titleMessage.MessageId)
	requireSDKCode(t, patch(titleID, newTitle).CodeError, 0)
	titleDetail, err := api.Get(t.Context(), larkim.NewGetMessageReqBuilder().MessageId(titleID).CardMsgContentType("user_card_content").Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, titleDetail.CodeError, 0)
	if len(titleDetail.Data.Items) != 1 || value(titleDetail.Data.Items[0].Body.Content) != newTitle ||
		!value(titleDetail.Data.Items[0].Updated) {
		t.Fatalf("title-only card PATCH was not persisted: %s", titleDetail.RawBody)
	}

	validInput := nestedInput(`"max_length":1000,"default_value":"Draft","required":true,"disabled":false,
		"input_type":"multiline_text","rows":1,"max_rows":2.5,"auto_resize":true,"show_icon":false,"width":"fill","name":"feedback",
		"placeholder":{"content":"Type here"},"label":{"tag":"plain_text","content":"Feedback"}`)
	inputMessage := sdkSend(t, client, "chat_id", chatID, "interactive", validInput)
	inputID := value(inputMessage.MessageId)
	replacement := nestedInput(`"input_type":"multiline_text","auto_resize":true,"max_length":1,"rows":1,"max_rows":2.5`)
	requireSDKCode(t, patch(inputID, replacement).CodeError, 0)
	inputDetail, err := api.Get(t.Context(), larkim.NewGetMessageReqBuilder().MessageId(inputID).CardMsgContentType("user_card_content").Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, inputDetail.CodeError, 0)
	if len(inputDetail.Data.Items) != 1 || value(inputDetail.Data.Items[0].Body.Content) != replacement {
		t.Fatalf("fractional max_rows was not preserved after PATCH: %s", inputDetail.RawBody)
	}
	requireSDKCode(t, patch(inputID, nestedInput(`"input_type":"multiline_text","auto_resize":true,"max_rows":-1`)).CodeError, 0)
	requireSDKCode(t, patch(id, `{"schema":"2.0","body":{"elements":[]}}`).CodeError, 0)
}
