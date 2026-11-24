package sdk_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/server"
)

func sdkTestConfig() config.Config {
	cfg := config.Default()
	// The SDK caches tenant tokens globally by app ID.
	cfg.Apps[0].AppID = protocol.NewID("cli_")
	return cfg
}

func TestOfficialSDKIM(t *testing.T) {
	cfg := sdkTestConfig()
	srv := server.New(cfg)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	app := cfg.Apps[0]
	client := lark.NewClient(app.AppID, app.AppSecret, lark.WithOpenBaseUrl(ts.URL), lark.WithHttpClient(ts.Client()))

	t.Run("bot info", func(t *testing.T) {
		// The SDK's channel package also uses generic Get for this ungenerated v3 route.
		resp, err := client.Get(t.Context(), "/open-apis/bot/v3/info", nil, larkcore.AccessTokenTypeTenant)
		requireSDKNetwork(t, err)
		var wire struct {
			Code *int            `json:"code"`
			Msg  string          `json:"msg"`
			Data json.RawMessage `json:"data"`
			Bot  struct {
				OpenID  string   `json:"open_id"`
				AppName string   `json:"app_name"`
				Status  int      `json:"activate_status"`
				IPs     []string `json:"ip_white_list"`
				Avatar  *string  `json:"avatar_url"`
			} `json:"bot"`
		}
		requireSDKNetwork(t, json.Unmarshal(resp.RawBody, &wire))
		if resp.StatusCode != 200 || wire.Code == nil || *wire.Code != 0 || wire.Msg != "ok" ||
			wire.Bot.OpenID != app.BotOpenID || wire.Bot.AppName != app.BotName || wire.Bot.Status != 2 ||
			wire.Bot.IPs == nil || len(wire.Bot.IPs) != 0 || wire.Bot.Avatar == nil || len(wire.Data) != 0 {
			t.Fatalf("unexpected bot profile: %s", resp.RawBody)
		}
		traces := srv.Store().Traces()
		trace := traces[len(traces)-1]
		if trace.AppID != app.AppID || trace.Code == nil || *trace.Code != 0 {
			t.Fatalf("bot trace has no result metadata: %+v", trace)
		}
	})

	var chatID string
	t.Run("chat and members", func(t *testing.T) {
		chatID = sdkChats(t, client, cfg)
	})
	if chatID == "" {
		t.Fatal("chat setup failed")
	}
	t.Run("messages", func(t *testing.T) { sdkMessages(t, client, cfg, chatID) })
	t.Run("reactions", func(t *testing.T) { sdkReactions(t, client, chatID) })
	t.Run("media", func(t *testing.T) { sdkMedia(t, client, chatID) })
	t.Run("strict validation", func(t *testing.T) { sdkInvalidIM(t, client, chatID) })
}

func requireSDKNetwork(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func requireSDKCode(t *testing.T, result larkcore.CodeError, want int) {
	t.Helper()
	if result.Code != want {
		t.Fatalf("SDK result = %+v, want code %d", result, want)
	}
}

func value[T any](ptr *T) T {
	if ptr != nil {
		return *ptr
	}
	var zero T
	return zero
}

func sdkChats(t *testing.T, client *lark.Client, cfg config.Config) string {
	alice, bob := cfg.Users[0], cfg.Users[1]
	api := client.Im.V1
	created, err := api.Chat.Create(t.Context(), larkim.NewCreateChatReqBuilder().UserIdType("user_id").SetBotManager(true).
		Body(larkim.NewCreateChatReqBodyBuilder().Name("SDK chat").Description("SDK compatibility").
			OwnerId(alice.UserID).Build()).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, created.CodeError, 0)
	id := value(created.Data.ChatId)
	if !strings.HasPrefix(id, "oc_") || value(created.Data.ChatMode) != "group" ||
		value(created.Data.OwnerId) != alice.UserID || value(created.Data.OwnerIdType) != "user_id" {
		t.Fatalf("unexpected create chat response: %s", created.RawBody)
	}
	list, err := api.Chat.List(t.Context(), larkim.NewListChatReqBuilder().SortType("ByActiveTimeDesc").PageSize(1).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, list.CodeError, 0)
	if len(list.Data.Items) != 1 || value(list.Data.Items[0].ChatId) != id || !value(list.Data.HasMore) || value(list.Data.PageToken) == "" {
		t.Fatalf("unexpected first chat page: %s", list.RawBody)
	}
	// New activity would move the second chat before the cursor if sorting
	// were recalculated from current activity on every request.
	sdkSend(t, client, "chat_id", cfg.Chats[0].ChatID, "text", `{"text":"recent activity"}`)
	next, err := api.Chat.List(t.Context(), larkim.NewListChatReqBuilder().SortType("ByActiveTimeDesc").
		PageSize(1).PageToken(*list.Data.PageToken).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, next.CodeError, 0)
	if len(next.Data.Items) != 1 || value(next.Data.Items[0].ChatId) != cfg.Chats[0].ChatID || value(next.Data.HasMore) {
		t.Fatalf("unexpected second chat page: %s", next.RawBody)
	}

	add, err := api.ChatMembers.Create(t.Context(), larkim.NewCreateChatMembersReqBuilder().ChatId(id).
		MemberIdType("union_id").SucceedType(1).
		Body(larkim.NewCreateChatMembersReqBodyBuilder().IdList([]string{bob.UnionID, "on_missing", "invalid"}).Build()).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, add.CodeError, 0)
	if !slices.Equal(add.Data.InvalidIdList, []string{"invalid"}) || !slices.Equal(add.Data.NotExistedIdList, []string{"on_missing"}) {
		t.Fatalf("unexpected member addition: %s", add.RawBody)
	}
	detail, err := api.Chat.Get(t.Context(), larkim.NewGetChatReqBuilder().ChatId(id).UserIdType("union_id").Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, detail.CodeError, 0)
	if value(detail.Data.Description) != "SDK compatibility" || value(detail.Data.OwnerId) != alice.UnionID ||
		value(detail.Data.UserCount) != "2" || value(detail.Data.BotCount) != "1" ||
		!slices.Equal(detail.Data.BotManagerIdList, []string{cfg.Apps[0].AppID}) {
		t.Fatalf("unexpected chat detail: %s", detail.RawBody)
	}

	first, err := api.ChatMembers.Get(t.Context(), larkim.NewGetChatMembersReqBuilder().ChatId(id).
		MemberIdType("user_id").PageSize(1).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, first.CodeError, 0)
	if len(first.Data.Items) != 1 || value(first.Data.Items[0].MemberId) != alice.UserID ||
		value(first.Data.MemberTotal) != 2 || !value(first.Data.HasMore) || value(first.Data.PageToken) == "" {
		t.Fatalf("unexpected member page: %s", first.RawBody)
	}
	second, err := api.ChatMembers.Get(t.Context(), larkim.NewGetChatMembersReqBuilder().ChatId(id).
		MemberIdType("user_id").PageSize(1).PageToken(*first.Data.PageToken).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, second.CodeError, 0)
	if len(second.Data.Items) != 1 || value(second.Data.Items[0].MemberId) != bob.UserID || value(second.Data.HasMore) {
		t.Fatalf("unexpected second member page: %s", second.RawBody)
	}
	deleteBody, err := larkim.NewDeleteChatMembersPathReqBodyBuilder().IdList([]string{bob.OpenID, "ou_missing"}).Build()
	requireSDKNetwork(t, err)
	deleted, err := api.ChatMembers.Delete(t.Context(), larkim.NewDeleteChatMembersReqBuilder().ChatId(id).Body(deleteBody).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, deleted.CodeError, 0)
	if !slices.Equal(deleted.Data.InvalidIdList, []string{"ou_missing"}) {
		t.Fatalf("unexpected member removal: %s", deleted.RawBody)
	}
	return id
}

func sdkSend(t *testing.T, client *lark.Client, recipientType, recipient, kind, content string) *larkim.CreateMessageRespData {
	t.Helper()
	resp, err := client.Im.Message.Create(t.Context(), larkim.NewCreateMessageReqBuilder().ReceiveIdType(recipientType).
		Body(larkim.NewCreateMessageReqBodyBuilder().ReceiveId(recipient).MsgType(kind).Content(content).Build()).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, resp.CodeError, 0)
	if resp.Data == nil || !strings.HasPrefix(value(resp.Data.MessageId), "om_") ||
		resp.Data.Body == nil || value(resp.Data.Body.Content) != content || value(resp.Data.MsgType) != kind ||
		resp.Data.Deleted == nil || *resp.Data.Deleted || resp.Data.Updated == nil || *resp.Data.Updated {
		t.Fatalf("unexpected send response: %s", resp.RawBody)
	}
	return resp.Data
}

func sdkMessages(t *testing.T, client *lark.Client, cfg config.Config, chatID string) {
	api := client.Im.Message
	root := sdkSend(t, client, "chat_id", chatID, "text", `{"text":"root"}`)
	rootID := value(root.MessageId)
	ms, err := strconv.ParseInt(value(root.CreateTime), 10, 64)
	if err != nil || ms < 1_000_000_000_000 || value(root.UpdateTime) != value(root.CreateTime) ||
		root.Sender == nil || value(root.Sender.IdType) != "app_id" || value(root.Sender.Id) != cfg.Apps[0].AppID {
		t.Fatalf("unexpected creation time or sender: %+v", root)
	}
	alice := cfg.Users[0]
	direct := sdkSend(t, client, "email", alice.Email, "text", `{"text":"by email"}`)
	sameDirect := sdkSend(t, client, "user_id", alice.UserID, "text", `{"text":"by user ID"}`)
	if value(direct.ChatId) != cfg.Chats[1].ChatID || value(sameDirect.ChatId) != value(direct.ChatId) {
		t.Fatal("direct chat was not reused across ID types")
	}
	reply := func(id, content string, inThread bool) *larkim.ReplyMessageRespData {
		t.Helper()
		result, err := api.Reply(t.Context(), larkim.NewReplyMessageReqBuilder().MessageId(id).
			Body(larkim.NewReplyMessageReqBodyBuilder().MsgType("text").Content(content).ReplyInThread(inThread).Build()).Build())
		requireSDKNetwork(t, err)
		requireSDKCode(t, result.CodeError, 0)
		return result.Data
	}
	first := reply(rootID, `{"text":"in thread"}`, true)
	second := reply(value(first.MessageId), `{"text":"nested reply"}`, false)
	threadID := value(first.ThreadId)
	if value(first.RootId) != rootID || value(first.ParentId) != rootID || !strings.HasPrefix(threadID, "omt_") ||
		value(second.ParentId) != rootID || value(second.RootId) != rootID || value(second.ThreadId) != threadID {
		t.Fatal("reply IDs did not preserve the thread hierarchy")
	}
	card := sdkSend(t, client, "chat_id", chatID, "interactive", `{"config":{"update_multi":true},"elements":[]}`)
	patchedContent := `{"config":{"update_multi":true},"elements":[{"tag":"markdown","content":"updated"}]}`
	patched, err := api.Patch(t.Context(), larkim.NewPatchMessageReqBuilder().MessageId(value(card.MessageId)).
		Body(larkim.NewPatchMessageReqBodyBuilder().Content(patchedContent).Build()).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, patched.CodeError, 0)
	get := func(id string) *larkim.Message {
		t.Helper()
		result, err := api.Get(t.Context(), larkim.NewGetMessageReqBuilder().MessageId(id).UserIdType("union_id").Build())
		requireSDKNetwork(t, err)
		requireSDKCode(t, result.CodeError, 0)
		if result.Data == nil || len(result.Data.Items) != 1 || value(result.Data.Items[0].MessageId) != id {
			t.Fatalf("unexpected get message wrapper: %s", result.RawBody)
		}
		return result.Data.Items[0]
	}
	if updated := get(value(card.MessageId)); !value(updated.Updated) || value(updated.Body.Content) != patchedContent {
		t.Fatal("PATCH did not persist the interactive message")
	}

	editContent := `{"text":"edited"}`
	edited, err := api.Update(t.Context(), larkim.NewUpdateMessageReqBuilder().MessageId(rootID).
		Body(larkim.NewUpdateMessageReqBodyBuilder().MsgType("text").Content(editContent).Build()).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, edited.CodeError, 0)
	if value(edited.Data.Body.Content) != editContent || !value(edited.Data.Updated) ||
		value(edited.Data.ThreadId) != threadID || value(get(rootID).Body.Content) != editContent {
		t.Fatalf("unexpected PUT result: %s", edited.RawBody)
	}

	list := func(kind, id, order, token string, size int) *larkim.ListMessageRespData {
		t.Helper()
		builder := larkim.NewListMessageReqBuilder().ContainerIdType(kind).ContainerId(id).SortType(order).PageSize(size)
		if token != "" {
			builder.PageToken(token)
		}
		if kind == "chat" {
			builder.StartTime(strconv.FormatInt(ms/1000, 10)).EndTime(strconv.FormatInt(ms/1000+60, 10))
		}
		result, err := api.List(t.Context(), builder.Build())
		requireSDKNetwork(t, err)
		requireSDKCode(t, result.CodeError, 0)
		return result.Data
	}
	asc := list("chat", chatID, "ByCreateTimeAsc", "", 1)
	if len(asc.Items) != 1 || value(asc.Items[0].MessageId) != rootID || !value(asc.HasMore) || value(asc.PageToken) == "" {
		t.Fatalf("unexpected ascending history: %+v", asc)
	}
	next := list("chat", chatID, "ByCreateTimeAsc", *asc.PageToken, 1)
	desc := list("chat", chatID, "ByCreateTimeDesc", "", 1)
	if len(next.Items) != 1 || value(next.Items[0].MessageId) != value(card.MessageId) || value(next.HasMore) ||
		len(desc.Items) != 1 || value(desc.Items[0].MessageId) != value(card.MessageId) {
		t.Fatal("chat history sort, cursor, or thread exclusion did not match")
	}
	thread := list("thread", threadID, "ByCreateTimeAsc", "", 20)
	if len(thread.Items) != 3 || value(thread.Items[0].MessageId) != rootID ||
		value(thread.Items[1].MessageId) != value(first.MessageId) || value(thread.Items[2].MessageId) != value(second.MessageId) {
		t.Fatal("thread history did not preserve message order")
	}

	recalled, err := api.Delete(t.Context(), larkim.NewDeleteMessageReqBuilder().MessageId(rootID).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, recalled.CodeError, 0)
	if !value(get(rootID).Deleted) {
		t.Fatal("DELETE did not mark the message recalled")
	}
}

func sdkReactions(t *testing.T, client *lark.Client, chatID string) {
	message := sdkSend(t, client, "chat_id", chatID, "text", `{"text":"react here"}`)
	id := value(message.MessageId)
	api := client.Im.MessageReaction
	add := func(emoji string) *larkim.CreateMessageReactionRespData {
		t.Helper()
		resp, err := api.Create(t.Context(), larkim.NewCreateMessageReactionReqBuilder().MessageId(id).
			Body(larkim.NewCreateMessageReactionReqBodyBuilder().ReactionType(larkim.NewEmojiBuilder().EmojiType(emoji).Build()).Build()).Build())
		requireSDKNetwork(t, err)
		requireSDKCode(t, resp.CodeError, 0)
		ms, err := strconv.ParseInt(value(resp.Data.ActionTime), 10, 64)
		if err != nil || ms < 1_000_000_000_000 || value(resp.Data.ReactionType.EmojiType) != emoji ||
			value(resp.Data.ReactionId) == "" || value(resp.Data.Operator.OperatorId) == "" {
			t.Fatalf("unexpected reaction response: %s", resp.RawBody)
		}
		return resp.Data
	}
	like, clap := add("THUMBSUP"), add("APPLAUSE")
	first, err := api.List(t.Context(), larkim.NewListMessageReactionReqBuilder().MessageId(id).PageSize(1).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, first.CodeError, 0)
	if len(first.Data.Items) != 1 || value(first.Data.Items[0].ReactionId) != value(like.ReactionId) ||
		!value(first.Data.HasMore) || value(first.Data.PageToken) == "" {
		t.Fatalf("unexpected first reaction page: %s", first.RawBody)
	}
	deleted, err := api.Delete(t.Context(), larkim.NewDeleteMessageReactionReqBuilder().MessageId(id).
		ReactionId(value(like.ReactionId)).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, deleted.CodeError, 0)
	if value(deleted.Data.ReactionId) != value(like.ReactionId) ||
		value(deleted.Data.ActionTime) != value(like.ActionTime) || value(deleted.Data.ReactionType.EmojiType) != "THUMBSUP" {
		t.Fatalf("unexpected deleted reaction: %s", deleted.RawBody)
	}
	next, err := api.List(t.Context(), larkim.NewListMessageReactionReqBuilder().MessageId(id).PageSize(1).
		PageToken(*first.Data.PageToken).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, next.CodeError, 0)
	if len(next.Data.Items) != 1 || value(next.Data.Items[0].ReactionId) != value(clap.ReactionId) || value(next.Data.HasMore) {
		t.Fatalf("unexpected second reaction page: %s", next.RawBody)
	}
	filtered, err := api.List(t.Context(), larkim.NewListMessageReactionReqBuilder().MessageId(id).ReactionType("APPLAUSE").Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, filtered.CodeError, 0)
	if len(filtered.Data.Items) != 1 || value(filtered.Data.Items[0].ReactionId) != value(clap.ReactionId) {
		t.Fatalf("unexpected filtered reaction page: %s", filtered.RawBody)
	}

	for _, size := range []int{50, 51} {
		limit, err := api.List(t.Context(), larkim.NewListMessageReactionReqBuilder().MessageId(id).PageSize(size).Build())
		requireSDKNetwork(t, err)
		want := 0
		if size == 51 {
			want = 99992402
		}
		requireSDKCode(t, limit.CodeError, want)
	}

	for _, emoji := range []string{"OnIt", "2022"} {
		add(emoji)
	}
	unknown, err := api.Create(t.Context(), larkim.NewCreateMessageReactionReqBuilder().MessageId(id).
		Body(larkim.NewCreateMessageReactionReqBodyBuilder().ReactionType(
			larkim.NewEmojiBuilder().EmojiType("UNKNOWN_EMOJI_XYZ").Build()).Build()).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, unknown.CodeError, 231001)
	badFilter, err := api.List(t.Context(), larkim.NewListMessageReactionReqBuilder().MessageId(id).
		ReactionType("UNKNOWN_EMOJI_XYZ").Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, badFilter.CodeError, 231001)

}

func sdkMedia(t *testing.T, client *lark.Client, chatID string) {
	api := client.Im.V1
	image := []byte("\x89PNG\r\n\x1a\nfixture image\x00")
	file := []byte("%PDF-1.7\nfixture file\x00")
	imageResp, err := api.Image.Create(t.Context(), larkim.NewCreateImageReqBuilder().
		Body(larkim.NewCreateImageReqBodyBuilder().ImageType("message").Image(bytes.NewReader(image)).Build()).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, imageResp.CodeError, 0)
	imageKey := value(imageResp.Data.ImageKey)
	if !strings.HasPrefix(imageKey, "img_v3_") {
		t.Fatalf("unexpected image key: %q", imageKey)
	}
	fileResp, err := api.File.Create(t.Context(), larkim.NewCreateFileReqBuilder().
		Body(larkim.NewCreateFileReqBodyBuilder().FileType("pdf").FileName(`..\fixtures\résumé.pdf`).
			File(bytes.NewReader(file)).Build()).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, fileResp.CodeError, 0)
	fileKey := value(fileResp.Data.FileKey)
	if !strings.HasPrefix(fileKey, "file_v3_") {
		t.Fatalf("unexpected file key: %q", fileKey)
	}

	gotImage, err := api.Image.Get(t.Context(), larkim.NewGetImageReqBuilder().ImageKey(imageKey).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, gotImage.CodeError, 0)
	assertSDKMedia(t, gotImage.ApiResp, gotImage.File, gotImage.FileName, "image/png", "", image)
	gotFile, err := api.File.Get(t.Context(), larkim.NewGetFileReqBuilder().FileKey(fileKey).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, gotFile.CodeError, 0)
	assertSDKMedia(t, gotFile.ApiResp, gotFile.File, gotFile.FileName, "application/pdf", "résumé.pdf", file)

	for _, resource := range []struct {
		kind, key, mime, name string
		data                  []byte
	}{
		{"image", imageKey, "image/png", gotImage.FileName, image},
		{"file", fileKey, "application/pdf", gotFile.FileName, file},
	} {
		payload, err := json.Marshal(map[string]string{resource.kind + "_key": resource.key})
		requireSDKNetwork(t, err)
		message := sdkSend(t, client, "chat_id", chatID, resource.kind, string(payload))
		got, err := api.MessageResource.Get(t.Context(), larkim.NewGetMessageResourceReqBuilder().
			MessageId(value(message.MessageId)).FileKey(resource.key).Type(resource.kind).Build())
		requireSDKNetwork(t, err)
		requireSDKCode(t, got.CodeError, 0)
		assertSDKMedia(t, got.ApiResp, got.File, got.FileName, resource.mime, resource.name, resource.data)
	}
	for _, fixture := range []struct {
		post, kind, key, mime, name string
		data                        []byte
		code                        int
	}{
		{fmt.Sprintf(`{"en_us":{"content":[[{"tag":"img","image_key":%q}]]}}`, imageKey),
			"image", imageKey, "image/png", gotImage.FileName, image, 0},
		{fmt.Sprintf(`{"content":[[{"tag":"media","file_key":%q,"image_key":%q}]]}`, fileKey, imageKey),
			"file", fileKey, "application/pdf", gotFile.FileName, file, 0},
		{fmt.Sprintf(`{"content":[[{"tag":"text","text":"safe"}]],"en_us":{"content":[[{"tag":"img","image_key":%q}]]}}`, imageKey),
			"image", imageKey, "", "", nil, 234003},
		{fmt.Sprintf(`{"en_us":{"content":[[{"tag":"text","text":"safe"}]]},"metadata":{"content":[[{"tag":"img","image_key":%q}]]}}`, imageKey),
			"image", imageKey, "", "", nil, 234003},
	} {
		m := sdkSend(t, client, "chat_id", chatID, "post", fixture.post)
		res, err := api.MessageResource.Get(t.Context(), larkim.NewGetMessageResourceReqBuilder().
			MessageId(value(m.MessageId)).FileKey(fixture.key).Type(fixture.kind).Build())
		requireSDKNetwork(t, err)
		requireSDKCode(t, res.CodeError, fixture.code)
		if fixture.code == 0 {
			assertSDKMedia(t, res.ApiResp, res.File, res.FileName, fixture.mime, fixture.name, fixture.data)
		} else if res.File != nil {
			t.Fatal("ignored rich-text metadata exposed an attachment")
		}
	}
	unrelated := sdkSend(t, client, "chat_id", chatID, "text", `{"text":"This message contains no attachment."}`)
	blocked, err := api.MessageResource.Get(t.Context(), larkim.NewGetMessageResourceReqBuilder().
		MessageId(value(unrelated.MessageId)).FileKey(fileKey).Type("file").Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, blocked.CodeError, 234003)
	if blocked.StatusCode != http.StatusBadRequest || blocked.File != nil {
		t.Fatalf("unreferenced file returned HTTP %d", blocked.StatusCode)
	}
}

func assertSDKMedia(t *testing.T, resp *larkcore.ApiResp, reader io.Reader, gotName, mime, wantName string, want []byte) {
	t.Helper()
	if resp.StatusCode != http.StatusOK || reader == nil || gotName == "" ||
		(wantName != "" && gotName != wantName) || resp.Header.Get("Content-Type") != mime {
		t.Fatalf("unexpected download metadata: %d %v", resp.StatusCode, resp.Header)
	}
	got, err := io.ReadAll(reader)
	requireSDKNetwork(t, err)
	if !bytes.Equal(got, want) {
		t.Fatalf("download bytes = %q, want %q", got, want)
	}
}

func sdkInvalidIM(t *testing.T, client *lark.Client, chatID string) {
	generic, err := client.Post(t.Context(), "/open-apis/im/v1/messages?receive_id_type=chat_id", map[string]any{
		"receive_id": chatID, "msg_type": "text", "content": map[string]string{"text": "raw object"},
	}, larkcore.AccessTokenTypeTenant)
	requireSDKNetwork(t, err)
	var invalid larkcore.CodeError
	requireSDKNetwork(t, json.Unmarshal(generic.RawBody, &invalid))
	requireSDKCode(t, invalid, 99992402)
	if generic.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid JSON encoding returned HTTP %d", generic.StatusCode)
	}
	badType, err := client.Im.Message.Create(t.Context(), larkim.NewCreateMessageReqBuilder().ReceiveIdType("wrong").
		Body(larkim.NewCreateMessageReqBodyBuilder().ReceiveId(chatID).MsgType("text").Content(`{"text":"hi"}`).Build()).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, badType.CodeError, 99992402)
	missingChat, err := client.Im.Chat.Get(t.Context(), larkim.NewGetChatReqBuilder().ChatId("oc_missing").Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, missingChat.CodeError, 232006)

	textMessage := sdkSend(t, client, "chat_id", chatID, "text", `{"text":"not a card"}`)
	notCard, err := client.Im.Message.Patch(t.Context(),
		larkim.NewPatchMessageReqBuilder().MessageId(value(textMessage.MessageId)).
			Body(larkim.NewPatchMessageReqBodyBuilder().Content(`{"elements":[]}`).Build()).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, notCard.CodeError, 230001)

	missing, err := client.Im.Message.Delete(t.Context(), larkim.NewDeleteMessageReqBuilder().MessageId("om_missing").Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, missing.CodeError, 230001)
}
