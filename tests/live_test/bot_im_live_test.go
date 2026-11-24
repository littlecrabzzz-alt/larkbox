//go:build live

package livetest

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/littlecrabzzz-alt/larkbox/tests/fixturetest"
)

const (
	messages   = "/open-apis/im/v1/messages"
	testText   = `{"text":"larkbox differential test"}`
	replyText  = `{"text":"larkbox differential reply"}`
	imageShape = `{"code":0,"msg":"","data":{"image_key":""}}`
)

func TestBotAndIMContracts(t *testing.T) {
	h := newHarness(t)
	token := h.tenantToken(t)
	tenant := larkcore.WithTenantAccessToken(token)
	t.Run("bot info", func(t *testing.T) {
		resp, err := h.sdk.Get(t.Context(), botInfoPath, nil, larkcore.AccessTokenTypeTenant, tenant)
		requireSDK(t, "bot info", err)
		golden(t, "bot/bot_info_ok", resp)
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
		expectOK(t, recall())
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
	golden(t, "im/message_send_text_ok", sent.ApiResp)
	if !strings.HasPrefix(id, "om_") || sent.Data.Body == nil || value(sent.Data.Body.Content) != testText ||
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
	fixturetest.Golden(t, "im/message_send_invalid_content_99992402", status, raw, false)

	api := h.sdk.Im.V1
	reply, err := api.Message.Reply(t.Context(), larkim.NewReplyMessageReqBuilder().MessageId(rootID).
		Body(larkim.NewReplyMessageReqBodyBuilder().MsgType("text").Content(replyText).ReplyInThread(true).Build()).Build(), tenant)
	requireSDK(t, "reply in thread", err)
	var replyID string
	if reply.Data != nil {
		replyID = value(reply.Data.MessageId)
	}
	recallReply := h.registerRecall(t, tenant, replyID)
	golden(t, "im/message_reply_thread_ok", reply.ApiResp)
	if !strings.HasPrefix(replyID, "om_") || value(reply.Data.ParentId) != rootID || value(reply.Data.RootId) != rootID ||
		!strings.HasPrefix(value(reply.Data.ThreadId), "omt_") {
		t.Fatal("reply did not retain the message and thread relationships")
	}

	got, err := api.Message.Get(t.Context(), larkim.NewGetMessageReqBuilder().MessageId(rootID).Build(), tenant)
	requireSDK(t, "get message", err)
	golden(t, "im/message_get_ok", got.ApiResp)
	if got.Data == nil || len(got.Data.Items) != 1 || got.Data.Items[0] == nil || value(got.Data.Items[0].MessageId) != rootID {
		t.Fatal("message detail did not contain the sent message")
	}

	list, err := api.Message.List(t.Context(), larkim.NewListMessageReqBuilder().ContainerIdType("chat").
		ContainerId(h.chatID).SortType("ByCreateTimeDesc").PageSize(50).Build(), tenant)
	requireSDK(t, "list chat messages", err)
	listShape := `{"code":0,"msg":"","data":{"has_more":false,"items":[]}}`
	if list.Data != nil && list.Data.PageToken != nil {
		listShape = `{"code":0,"msg":"","data":{"has_more":false,"items":[],"page_token":""}}`
	}
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
	expectOK(t, recallReply())
	expectOK(t, recallRoot())
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
		remove = registerRemoval(t, "delete reaction", func(ctx context.Context) (*larkcore.ApiResp, error) {
			deleted, err := api.Delete(ctx, larkim.NewDeleteMessageReactionReqBuilder().MessageId(messageID).ReactionId(id).Build(), tenant)
			if err != nil {
				return nil, err
			}
			return deleted.ApiResp, nil
		})
	}
	golden(t, "im/reaction_create_ok", created.ApiResp)
	if remove == nil || created.Data.ReactionType == nil || value(created.Data.ReactionType.EmojiType) != "THUMBSUP" {
		t.Fatal("created reaction has no ID or unexpected emoji")
	}
	golden(t, "im/reaction_create_ok", remove())
}

func (h *harness) registerRecall(t *testing.T, tenant larkcore.RequestOptionFunc, id string) func() *larkcore.ApiResp {
	t.Helper()
	if id == "" {
		return nil
	}
	return registerRemoval(t, "recall message", func(ctx context.Context) (*larkcore.ApiResp, error) {
		resp, err := h.sdk.Im.Message.Delete(ctx, larkim.NewDeleteMessageReqBuilder().MessageId(id).Build(), tenant)
		if err != nil {
			return nil, err
		}
		return resp.ApiResp, nil
	})
}

func registerRemoval(t *testing.T, operation string, remove func(context.Context) (*larkcore.ApiResp, error)) func() *larkcore.ApiResp {
	t.Helper()
	done := false
	run := func(ctx context.Context) *larkcore.ApiResp {
		t.Helper()
		response, err := remove(ctx)
		requireSDK(t, operation, err)
		var result struct {
			Code *int `json:"code"`
		}
		if json.Unmarshal(response.RawBody, &result) != nil || result.Code == nil {
			t.Fatalf("%s: missing business code (HTTP %d)", operation, response.StatusCode)
		}
		if response.StatusCode != http.StatusOK || *result.Code != 0 {
			t.Fatalf("%s: HTTP %d, business code %d", operation, response.StatusCode, *result.Code)
		}
		done = true
		return response
	}
	t.Cleanup(func() {
		if !done {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			run(ctx)
		}
	})
	return func() *larkcore.ApiResp { return run(t.Context()) }
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
	if !strings.HasPrefix(key, "img_v3_") || key == "img_v3_" {
		t.Fatal("upload did not return an img_v3_ key")
	}
	downloaded, err := api.Image.Get(t.Context(), larkim.NewGetImageReqBuilder().ImageKey(key).Build(), tenant)
	requireSDK(t, "download test PNG", err)
	if downloaded.StatusCode != http.StatusOK || downloaded.File == nil || !downloaded.Success() {
		t.Fatalf("image download: HTTP %d, business code %d", downloaded.StatusCode, downloaded.Code)
	}
	got, err := io.ReadAll(downloaded.File)
	requireSDK(t, "read downloaded PNG", err)
	decoded, err := png.Decode(bytes.NewReader(got))
	requireSDK(t, "decode downloaded PNG", err)
	if decoded.Bounds() != pixel.Bounds() || color.NRGBAModel.Convert(decoded.At(0, 0)) != pixel.NRGBAAt(0, 0) {
		t.Fatal("downloaded PNG did not preserve the uploaded pixel")
	}
}
