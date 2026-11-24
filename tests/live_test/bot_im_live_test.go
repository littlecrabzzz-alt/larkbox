//go:build live

package livetest

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"strings"
	"testing"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/littlecrabzzz-alt/larkbox/tests/fixturetest"
)

const (
	messages   = "/open-apis/im/v1/messages"
	testText   = `{"text":"larkbox differential test"}`
	replyText  = `{"text":"larkbox differential reply"}`
	imageShape = `{"code":0,"msg":"","data":{"image_key":""}}`
	botShape   = `{"code":0,"msg":"","bot":{"activate_status":0,"app_name":"","avatar_url?":"",
		"ip_white_list?":[""],"open_id":"fixture:id"}}`
	messageFields = `"message_id":"fixture:id","msg_type":"","chat_id":"fixture:id",
		"create_time":"fixture:millis","update_time?":"fixture:millis","deleted?":false,"updated?":false,
		"body":{"content":""},"sender":{"id":"fixture:id","id_type":"","sender_type":"","tenant_key?":"",
		"sender_name?":"","open_bot_id?":"","sender_i18n_names?":{"zh_cn?":"","en_us?":"","ja_jp?":""}},
		"mentions?":[{"key":"","id":"","id_type?":"","name?":"","tenant_key?":""}],
		"upper_message_id?":"","message_app_link?":"","message_position?":"","thread_message_position?":""`
	threadFields = `"root_id?":"","parent_id?":"","thread_id?":""`
	messageShape = `{"code":0,"msg":"","data":{` + messageFields + `,` + threadFields + `}}`
	replyShape   = `{"code":0,"msg":"","data":{` + messageFields + `,
		"root_id":"fixture:id","parent_id":"fixture:id","thread_id":"fixture:id"}}`
	getShape      = `{"code":0,"msg":"","data":{"items":[{` + messageFields + `,` + threadFields + `}]}}`
	reactionShape = `{"code":0,"msg":"","data":{"reaction_id":"fixture:id","action_time":"fixture:millis",
		"operator?":{"operator_id?":"","operator_type?":""},"reaction_type":{"emoji_type":""}}}`
)

func TestBotAndIMContracts(t *testing.T) {
	h := newHarness(t)
	token := h.tenantToken(t)
	tenant := larkcore.WithTenantAccessToken(token)
	t.Run("bot info", func(t *testing.T) {
		resp, err := h.sdk.Get(t.Context(), botInfoPath, nil, larkcore.AccessTokenTypeTenant, tenant)
		requireSDK(t, "bot info", err)
		h.checkResponse(t, "bot/bot_info_ok", resp, http.StatusOK, botShape)
	})
	t.Run("chat message lifecycle", func(t *testing.T) {
		if h.chatID == "" {
			t.Skip("set FEISHU_TEST_CHAT_ID to run message and reaction tests in a Feishu test chat")
		}
		testChat(t, h, token, tenant)
	})
	t.Run("direct message", func(t *testing.T) {
		if h.openID == "" {
			t.Skip("set FEISHU_TEST_OPEN_ID to run the optional direct message test")
		}
		_, recall := h.sendText(t, tenant, "open_id", h.openID)
		h.expectOK(t, recall())
	})
	t.Run("image round trip", func(t *testing.T) {
		testImage(t, h, tenant)
	})
}

func (h *harness) sendText(t *testing.T, tenant larkcore.RequestOptionFunc, kind, recipient string) (string, func() *larkcore.ApiResp) {
	t.Helper()
	sent, err := h.sdk.Im.Message.Create(t.Context(), larkim.NewCreateMessageReqBuilder().ReceiveIdType(kind).
		Body(larkim.NewCreateMessageReqBodyBuilder().ReceiveId(recipient).MsgType("text").Content(testText).Build()).Build(), tenant)
	requireSDK(t, "send text", err)
	var id string
	if sent.Data != nil {
		id = value(sent.Data.MessageId)
	}
	recall := h.registerRecall(t, tenant, id)
	h.checkResponse(t, "im/message_send_text_ok", sent.ApiResp, http.StatusOK, messageShape)
	if !strings.HasPrefix(id, "om_") || !sameText(sent.Data.Body, testText) ||
		(kind == "chat_id" && value(sent.Data.ChatId) != recipient) {
		t.Fatal("create message did not return the expected text and recipient")
	}
	return id, recall
}

func testChat(t *testing.T, h *harness, token string, tenant larkcore.RequestOptionFunc) {
	t.Helper()
	rootID, recallRoot := h.sendText(t, tenant, "chat_id", h.chatID)
	invalid, err := json.Marshal(map[string]any{
		"receive_id": h.chatID, "msg_type": "text", "content": map[string]string{"text": "invalid content"},
	})
	requireSDK(t, "encode invalid content", err)
	status, raw := fixturetest.Request(t, h.http, h.base, http.MethodPost, messages+"?receive_id_type=chat_id", token, invalid)
	// A server regression could accept the invalid request and create a message.
	var unexpected struct {
		Data larkim.CreateMessageRespData `json:"data"`
	}
	if json.Unmarshal(raw, &unexpected) == nil {
		h.registerRecall(t, tenant, value(unexpected.Data.MessageId))
	}
	h.checkWire(t, "im/message_send_invalid_content_99992402", status, raw, http.StatusBadRequest, liveErrorShape(99992402))

	api := h.sdk.Im.V1
	reply, err := api.Message.Reply(t.Context(), larkim.NewReplyMessageReqBuilder().MessageId(rootID).
		Body(larkim.NewReplyMessageReqBodyBuilder().MsgType("text").Content(replyText).ReplyInThread(true).Build()).Build(), tenant)
	requireSDK(t, "reply in thread", err)
	var replyID string
	if reply.Data != nil {
		replyID = value(reply.Data.MessageId)
	}
	recallReply := h.registerRecall(t, tenant, replyID)
	h.checkResponse(t, "im/message_reply_thread_ok", reply.ApiResp, http.StatusOK, replyShape)
	if !strings.HasPrefix(replyID, "om_") || value(reply.Data.ParentId) != rootID || value(reply.Data.RootId) != rootID ||
		!strings.HasPrefix(value(reply.Data.ThreadId), "omt_") || !sameText(reply.Data.Body, replyText) {
		t.Fatal("reply did not retain the message and thread relationships")
	}

	got, err := api.Message.Get(t.Context(), larkim.NewGetMessageReqBuilder().MessageId(rootID).Build(), tenant)
	requireSDK(t, "get message", err)
	h.checkResponse(t, "im/message_get_ok", got.ApiResp, http.StatusOK, getShape)
	if got.Data == nil || len(got.Data.Items) != 1 || got.Data.Items[0] == nil || value(got.Data.Items[0].MessageId) != rootID || !sameText(got.Data.Items[0].Body, testText) {
		t.Fatal("message detail did not contain the sent message")
	}

	list, err := api.Message.List(t.Context(), larkim.NewListMessageReqBuilder().ContainerIdType("chat").
		ContainerId(h.chatID).SortType("ByCreateTimeDesc").PageSize(50).Build(), tenant)
	requireSDK(t, "list chat messages", err)
	listShape := `{"code":0,"msg":"","data":{"has_more":false,"items":[],"page_token?":""}}`
	fixturetest.Envelope(t, list.StatusCode, list.RawBody, http.StatusOK, listShape)
	found := false
	for _, item := range list.Data.Items {
		if item != nil && value(item.MessageId) == rootID {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("chat history did not contain the sent message")
	}

	testReaction(t, h, tenant, rootID)
	h.expectOK(t, recallReply())
	h.expectOK(t, recallRoot())
}

func testReaction(t *testing.T, h *harness, tenant larkcore.RequestOptionFunc, messageID string) {
	t.Helper()
	api := h.sdk.Im.MessageReaction
	created, err := api.Create(t.Context(), larkim.NewCreateMessageReactionReqBuilder().MessageId(messageID).
		Body(larkim.NewCreateMessageReactionReqBodyBuilder().ReactionType(larkim.NewEmojiBuilder().EmojiType("THUMBSUP").Build()).Build()).Build(), tenant)
	requireSDK(t, "add reaction", err)
	var id string
	if created.Data != nil {
		id = value(created.Data.ReactionId)
	}
	var remove func() *larkcore.ApiResp
	if id != "" {
		remove = registerMutation(t, "delete reaction", func(ctx context.Context) (*larkcore.ApiResp, error) {
			deleted, err := api.Delete(ctx, larkim.NewDeleteMessageReactionReqBuilder().MessageId(messageID).ReactionId(id).Build(), tenant)
			if err != nil {
				return nil, err
			}
			return deleted.ApiResp, nil
		}, nil)
	}
	h.checkResponse(t, "im/reaction_create_ok", created.ApiResp, http.StatusOK, reactionShape)
	if remove == nil || created.Data.ReactionType == nil || value(created.Data.ReactionType.EmojiType) != "THUMBSUP" {
		t.Fatal("created reaction has no ID or unexpected emoji")
	}
	h.checkResponse(t, "im/reaction_create_ok", remove(), http.StatusOK, reactionShape)
}

func (h *harness) registerRecall(t *testing.T, tenant larkcore.RequestOptionFunc, id string) func() *larkcore.ApiResp {
	t.Helper()
	if id == "" {
		return nil
	}
	return registerMutation(t, "recall message", func(ctx context.Context) (*larkcore.ApiResp, error) {
		resp, err := h.sdk.Im.Message.Delete(ctx, larkim.NewDeleteMessageReqBuilder().MessageId(id).Build(), tenant)
		if err != nil {
			return nil, err
		}
		return resp.ApiResp, nil
	}, nil)
}

func sameText(body *larkim.MessageBody, expected string) bool {
	if body == nil {
		return false
	}
	var got, want struct {
		Text *string `json:"text"`
	}
	return json.Unmarshal([]byte(value(body.Content)), &got) == nil &&
		json.Unmarshal([]byte(expected), &want) == nil && got.Text != nil && want.Text != nil && *got.Text == *want.Text
}

func testImage(t *testing.T, h *harness, tenant larkcore.RequestOptionFunc) {
	t.Helper()
	var encoded bytes.Buffer
	pixel := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	pixel.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 255})
	requireSDK(t, "encode test PNG", png.Encode(&encoded, pixel))
	api := h.sdk.Im.V1
	uploaded, err := api.Image.Create(t.Context(), larkim.NewCreateImageReqBuilder().
		Body(larkim.NewCreateImageReqBodyBuilder().ImageType("message").Image(bytes.NewReader(encoded.Bytes())).Build()).Build(), tenant)
	requireSDK(t, "upload test PNG", err)
	fixturetest.Envelope(t, uploaded.StatusCode, uploaded.RawBody, http.StatusOK, imageShape)
	key := value(uploaded.Data.ImageKey)
	if !strings.HasPrefix(key, "img_") || len(key) == len("img_") {
		t.Fatal("upload did not return a non-empty img_ key")
	}
	downloaded, err := api.Image.Get(t.Context(), larkim.NewGetImageReqBuilder().ImageKey(key).Build(), tenant)
	requireSDK(t, "download test PNG", err)
	if downloaded.StatusCode != http.StatusOK || downloaded.File == nil || !downloaded.Success() {
		t.Fatalf("image download: HTTP %d, business code %d", downloaded.StatusCode, downloaded.Code)
	}
	got, err := png.Decode(downloaded.File)
	requireSDK(t, "decode downloaded PNG", err)
	bounds := pixel.Bounds()
	if got.Bounds() != bounds {
		t.Fatalf("downloaded PNG bounds = %v, want %v", got.Bounds(), bounds)
	}
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			gotR, gotG, gotB, gotA := got.At(x, y).RGBA()
			wantR, wantG, wantB, wantA := pixel.NRGBAAt(x, y).RGBA()
			if gotR != wantR || gotG != wantG || gotB != wantB || gotA != wantA {
				t.Fatalf("downloaded PNG pixel differs at (%d, %d)", x, y)
			}
		}
	}
}
