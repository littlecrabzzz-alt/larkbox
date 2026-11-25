package sdk_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/core/httpserverext"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkappv6 "github.com/larksuite/oapi-sdk-go/v3/service/application/v6"
	larkappv7 "github.com/larksuite/oapi-sdk-go/v3/service/application/v7"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/littlecrabzzz-alt/larkbox/internal/event"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

func sdkCreateGroup(t *testing.T, client *lark.Client, members ...string) string {
	t.Helper()
	group, err := client.Im.Chat.Create(t.Context(), larkim.NewCreateChatReqBuilder().
		UserIdType("open_id").Body(larkim.NewCreateChatReqBodyBuilder().
		Name("Conversation lifecycle").ChatMode("group").UserIdList(members).Build()).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, group.CodeError, 0)
	return value(group.Data.ChatId)
}

func TestLifecycleEventsAndCommittedStateWithOfficialSDK(t *testing.T) {
	cfg := sdkTestConfig()
	cfg.Users = append(cfg.Users, store.User{OpenID: "ou_sdk_direct_peer", UserID: "direct-peer", Name: "Direct peer"})
	app := &cfg.Apps[0]
	app.EncryptKey = ""
	app.EventTypes = []string{
		"im.chat.member.user.added_v1", "im.chat.member.user.deleted_v1", "im.chat.disbanded_v1",
		"im.message.reaction.created_v1", "im.message.reaction.deleted_v1",
		"p2p_chat_create", "im.chat.access_event.bot_p2p_chat_entered_v1", "application.bot.menu_v6",
	}
	incoming := make(chan any, 16)
	d := dispatcher.NewEventDispatcher(app.VerificationToken, "").
		OnP2ChatMemberUserAddedV1(func(_ context.Context, e *larkim.P2ChatMemberUserAddedV1) error { incoming <- e; return nil }).
		OnP2ChatMemberUserDeletedV1(func(_ context.Context, e *larkim.P2ChatMemberUserDeletedV1) error { incoming <- e; return nil }).
		OnP2ChatDisbandedV1(func(_ context.Context, e *larkim.P2ChatDisbandedV1) error { incoming <- e; return nil }).
		OnP2MessageReactionCreatedV1(func(_ context.Context, e *larkim.P2MessageReactionCreatedV1) error { incoming <- e; return nil }).
		OnP2MessageReactionDeletedV1(func(_ context.Context, e *larkim.P2MessageReactionDeletedV1) error { incoming <- e; return nil }).
		OnP1P2PChatCreatedV1(func(_ context.Context, e *larkim.P1P2PChatCreatedV1) error { incoming <- e; return nil }).
		OnP2ChatAccessEventBotP2pChatEnteredV1(func(_ context.Context, e *larkim.P2ChatAccessEventBotP2pChatEnteredV1) error {
			incoming <- e
			return nil
		}).
		OnP2BotMenuV6(func(_ context.Context, e *larkappv6.P2BotMenuV6) error { incoming <- e; return nil })
	receiver := httptest.NewServer(http.HandlerFunc(httpserverext.NewEventHandlerFunc(d)))
	defer receiver.Close()
	app.WebhookURL = receiver.URL
	srv, web := sdkEventServer(t, cfg, event.Options{WebhookMaxAttempts: 1})
	client := sdkEventREST(web, *app)
	user := cfg.Users[1]
	chat := sdkCreateGroup(t, client)
	t.Run("join and leave", func(t *testing.T) {
		ids := []string{user.OpenID}
		joined, err := client.Im.ChatMembers.Create(t.Context(), larkim.NewCreateChatMembersReqBuilder().
			ChatId(chat).MemberIdType("open_id").Body(larkim.NewCreateChatMembersReqBodyBuilder().IdList(ids).Build()).Build())
		requireSDKNetwork(t, err)
		requireSDKCode(t, joined.CodeError, 0)
		e, ok := receiveSDKEvent(t, incoming).(*larkim.P2ChatMemberUserAddedV1)
		if !ok || value(e.Event.ChatId) != chat || len(e.Event.Users) != 1 || value(e.Event.Users[0].UserId.OpenId) != user.OpenID {
			t.Fatalf("unexpected join event: %+v", e)
		}
		assertMember := func(want bool) {
			t.Helper()
			members, err := client.Im.ChatMembers.Get(t.Context(), larkim.NewGetChatMembersReqBuilder().ChatId(chat).MemberIdType("open_id").Build())
			requireSDKNetwork(t, err)
			requireSDKCode(t, members.CodeError, 0)
			found := slices.ContainsFunc(members.Data.Items, func(m *larkim.ListMember) bool { return value(m.MemberId) == user.OpenID })
			if found != want {
				t.Fatalf("member presence after event = %v, want %v", found, want)
			}
		}
		assertMember(true)
		removed, err := client.Im.ChatMembers.Delete(t.Context(), larkim.NewDeleteChatMembersReqBuilder().
			ChatId(chat).MemberIdType("open_id").Body(larkim.NewDeleteChatMembersReqBodyBuilder().IdList(ids).Build()).Build())
		requireSDKNetwork(t, err)
		requireSDKCode(t, removed.CodeError, 0)
		deleted, ok := receiveSDKEvent(t, incoming).(*larkim.P2ChatMemberUserDeletedV1)
		if !ok || value(deleted.Event.ChatId) != chat || len(deleted.Event.Users) != 1 || value(deleted.Event.Users[0].UserId.OpenId) != user.OpenID {
			t.Fatalf("unexpected leave event: %+v", deleted)
		}
		assertMember(false)
	})

	member := cfg.Users[0]
	messageChat := sdkCreateGroup(t, client, member.OpenID)
	m := sdkSend(t, client, "chat_id", messageChat, "text", `{"text":"Check the state from the event handler"}`)
	messageID := value(m.MessageId)
	t.Run("user reaction", func(t *testing.T) {
		token := protocol.NewID("u-")
		requireSDKNetwork(t, srv.Store().PutToken(store.Token{Value: token, Kind: store.UserToken, AppID: app.AppID,
			UserOpenID: member.OpenID, ExpiresAt: time.Now().Add(time.Hour)}))
		reactions := client.Im.MessageReaction
		input := larkim.NewCreateMessageReactionReqBuilder().MessageId(messageID).
			Body(larkim.NewCreateMessageReactionReqBodyBuilder().ReactionType(larkim.NewEmojiBuilder().EmojiType("THUMBSUP").Build()).Build()).Build()
		created, err := reactions.Create(t.Context(), input, larkcore.WithUserAccessToken(token))
		requireSDKNetwork(t, err)
		requireSDKCode(t, created.CodeError, 0)
		e, ok := receiveSDKEvent(t, incoming).(*larkim.P2MessageReactionCreatedV1)
		if !ok || value(e.Event.MessageId) != messageID || value(e.Event.UserId.OpenId) != member.OpenID ||
			value(e.Event.OperatorType) != "user" || value(e.Event.ReactionType.EmojiType) != "THUMBSUP" {
			t.Fatalf("unexpected reaction event: %+v", e)
		}
		assertReactionCount := func(want int) {
			t.Helper()
			results, err := reactions.List(t.Context(), larkim.NewListMessageReactionReqBuilder().MessageId(messageID).Build())
			requireSDKNetwork(t, err)
			requireSDKCode(t, results.CodeError, 0)
			if len(results.Data.Items) != want {
				t.Fatalf("reaction count after event = %d, want %d", len(results.Data.Items), want)
			}
		}
		assertReactionCount(1)
		replay, err := reactions.Create(t.Context(), input, larkcore.WithUserAccessToken(token))
		requireSDKNetwork(t, err)
		requireSDKCode(t, replay.CodeError, 0)
		if value(replay.Data.ReactionId) != value(created.Data.ReactionId) {
			t.Fatal("duplicate add changed the reaction ID")
		}
		removed, err := reactions.Delete(t.Context(), larkim.NewDeleteMessageReactionReqBuilder().MessageId(messageID).
			ReactionId(value(created.Data.ReactionId)).Build(), larkcore.WithUserAccessToken(token))
		requireSDKNetwork(t, err)
		requireSDKCode(t, removed.CodeError, 0)
		deleted, ok := receiveSDKEvent(t, incoming).(*larkim.P2MessageReactionDeletedV1)
		if !ok || value(deleted.Event.MessageId) != messageID || value(deleted.Event.UserId.OpenId) != member.OpenID {
			t.Fatalf("unexpected reaction deletion event: %+v", deleted)
		}
		assertReactionCount(0)
	})

	t.Run("open and reenter direct chat", func(t *testing.T) {
		open := map[string]string{"app_id": app.AppID, "operator_open_id": user.OpenID}
		first := sandboxDecode[struct {
			ChatID  string `json:"chat_id"`
			ID      string `json:"event_id"`
			Created bool   `json:"created"`
		}](t, sandboxRequest(t, web, http.MethodPost, "chats/open", open, http.StatusOK))
		created, ok := receiveSDKEvent(t, incoming).(*larkim.P1P2PChatCreatedV1)
		if !ok || !first.Created || created.UUID != first.ID || created.Event.ChatID != first.ChatID ||
			created.Event.User.OpenId != user.OpenID || created.Event.Operator.OpenId != user.OpenID {
			t.Fatalf("unexpected first conversation: %+v, %+v", first, created)
		}
		last := sdkSend(t, client, "chat_id", first.ChatID, "text", `{"text":"Hello from the bot"}`)
		sandboxRequest(t, web, http.MethodPost, "chats/open", open, http.StatusOK)
		entered, ok := receiveSDKEvent(t, incoming).(*larkim.P2ChatAccessEventBotP2pChatEnteredV1)
		if !ok || value(entered.Event.ChatId) != first.ChatID || value(entered.Event.LastMessageId) != value(last.MessageId) {
			t.Fatalf("unexpected reentry event: %+v", entered)
		}
	})

	t.Run("bot send creates the first conversation", func(t *testing.T) {
		peer := cfg.Users[2]
		sent := sdkSend(t, client, "open_id", peer.OpenID, "text", `{"text":"First bot message"}`)
		created, ok := receiveSDKEvent(t, incoming).(*larkim.P1P2PChatCreatedV1)
		if !ok || created.Event.ChatID != value(sent.ChatId) || created.Event.User.OpenId != peer.OpenID ||
			created.Event.Operator.OpenId != app.BotOpenID {
			t.Fatalf("unexpected first conversation: %+v", created)
		}
	})

	t.Run("update and click menu", func(t *testing.T) {
		menu := larkappv7.NewBotMenuNodeBuilder().MenuId("status").DefaultName("Status").EventKey("check").MenuContentType(2).Build()
		resp, err := client.Application.V7.ApplicationAbility.Patch(t.Context(), larkappv7.NewPatchApplicationAbilityReqBuilder().AppId(app.AppID).
			Body(larkappv7.NewPatchApplicationAbilityReqBodyBuilder().Bot(larkappv7.NewAppAbilityBotBuilder().Enable(true).
				BotMenuEnable(true).BotMenus([]*larkappv7.BotMenuNode{menu}).Build()).Build()).Build())
		requireSDKNetwork(t, err)
		requireSDKCode(t, resp.CodeError, 0)
		path := "bots/" + app.AppID + "/menu"
		sandboxRequest(t, web, http.MethodPost, path+"/click", map[string]string{
			"menu_id": "status", "operator_open_id": user.OpenID,
		}, http.StatusOK)
		click, ok := receiveSDKEvent(t, incoming).(*larkappv6.P2BotMenuV6)
		if !ok || value(click.Event.EventKey) != "check" || value(click.Event.Operator.OperatorId.OpenId) != user.OpenID ||
			value(click.Event.Timestamp) < 1_000_000_000_000 {
			t.Fatalf("unexpected menu click: %+v", click)
		}
	})

	t.Run("disband group", func(t *testing.T) {
		resp, err := client.Im.Chat.Delete(t.Context(), larkim.NewDeleteChatReqBuilder().ChatId(messageChat).Build())
		requireSDKNetwork(t, err)
		requireSDKCode(t, resp.CodeError, 0)
		e, ok := receiveSDKEvent(t, incoming).(*larkim.P2ChatDisbandedV1)
		if !ok || value(e.Event.ChatId) != messageChat || value(e.Event.Name) == "" {
			t.Fatalf("unexpected disbanding event: %+v", e)
		}
		missing, err := client.Im.Chat.Get(t.Context(), larkim.NewGetChatReqBuilder().ChatId(messageChat).Build())
		requireSDKNetwork(t, err)
		requireSDKCode(t, missing.CodeError, 232009)
		history, err := client.Im.Message.Get(t.Context(), larkim.NewGetMessageReqBuilder().MessageId(messageID).Build())
		requireSDKNetwork(t, err)
		requireSDKCode(t, history.CodeError, 230001)
	})
}

func TestForwardMessageAndReadResourcesWithOfficialSDK(t *testing.T) {
	cfg := sdkEventConfig()
	_, web := sdkEventServer(t, cfg, event.Options{})
	owner, forwarder := sdkEventREST(web, cfg.Apps[0]), sdkEventREST(web, cfg.Apps[1])
	source := sdkCreateGroup(t, owner)
	target := sdkCreateGroup(t, forwarder)
	sdkBotJoin(t, owner, source, cfg.Apps[1].AppID)
	original := []byte("\x89PNG\r\n\x1a\nimage bytes")
	uploaded, err := owner.Im.Image.Create(t.Context(), larkim.NewCreateImageReqBuilder().
		Body(larkim.NewCreateImageReqBodyBuilder().ImageType("message").Image(bytes.NewReader(original)).Build()).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, uploaded.CodeError, 0)
	key := value(uploaded.Data.ImageKey)
	image := sdkSend(t, owner, "chat_id", source, "image", fmt.Sprintf(`{"image_key":%q}`, key))
	text := sdkSend(t, owner, "chat_id", source, "text", `{"text":"A caption"}`)
	single, err := forwarder.Im.Message.Forward(t.Context(), larkim.NewForwardMessageReqBuilder().MessageId(value(image.MessageId)).
		ReceiveIdType("chat_id").Body(larkim.NewForwardMessageReqBodyBuilder().ReceiveId(target).Build()).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, single.CodeError, 0)
	if value(single.Data.ChatId) != target || value(single.Data.MessageId) == value(image.MessageId) {
		t.Fatalf("unexpected forwarded message: %s", single.RawBody)
	}
	checkResource := func(id string) {
		t.Helper()
		req := larkim.NewGetMessageResourceReqBuilder().MessageId(id).Type("image").FileKey(key).Build()
		got, err := forwarder.Im.MessageResource.Get(t.Context(), req)
		requireSDKNetwork(t, err)
		requireSDKCode(t, got.CodeError, 0)
		assertSDKMedia(t, got.ApiResp, got.File, got.FileName, "image/png", "", original)
		denied, err := owner.Im.MessageResource.Get(t.Context(), req)
		requireSDKNetwork(t, err)
		requireSDKCode(t, denied.CodeError, 234004)
	}
	checkResource(value(single.Data.MessageId))

	merge := larkim.NewMergeForwardMessageReqBuilder().ReceiveIdType("chat_id").Uuid("bundle-idempotency").
		Body(larkim.NewMergeForwardMessageReqBodyBuilder().ReceiveId(target).
			MessageIdList([]string{value(image.MessageId), "om_unknown", value(text.MessageId)}).Build()).Build()
	first, err := forwarder.Im.Message.MergeForward(t.Context(), merge)
	requireSDKNetwork(t, err)
	requireSDKCode(t, first.CodeError, 0)
	parent := value(first.Data.Message.MessageId)
	if !slices.Equal(first.Data.InvalidMessageIdList, []string{"om_unknown"}) ||
		value(first.Data.Message.Body.Content) != "Merged and Forwarded Message" || value(first.Data.Message.MsgType) != "merge_forward" {
		t.Fatalf("unexpected merge response: %s", first.RawBody)
	}
	disband, err := owner.Im.Chat.Delete(t.Context(), larkim.NewDeleteChatReqBuilder().ChatId(source).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, disband.CodeError, 0)
	replay, err := forwarder.Im.Message.MergeForward(t.Context(), merge)
	requireSDKNetwork(t, err)
	requireSDKCode(t, replay.CodeError, 0)
	if value(replay.Data.Message.MessageId) != parent || !slices.Equal(replay.Data.InvalidMessageIdList, first.Data.InvalidMessageIdList) {
		t.Fatalf("replay changed the merge outcome: %s", replay.RawBody)
	}
	get, err := forwarder.Im.Message.Get(t.Context(), larkim.NewGetMessageReqBuilder().MessageId(parent).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, get.CodeError, 0)
	items := get.Data.Items
	if len(items) != 3 || value(items[1].UpperMessageId) != parent || value(items[1].MsgType) != "image" ||
		value(items[2].UpperMessageId) != parent || value(items[2].Body.Content) != `{"text":"A caption"}` {
		t.Fatalf("unexpected bundle items: %s", get.RawBody)
	}
	checkResource(value(items[1].MessageId))
	history, err := forwarder.Im.Message.List(t.Context(), larkim.NewListMessageReqBuilder().ContainerIdType("chat").ContainerId(target).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, history.CodeError, 0)
	if len(history.Data.Items) != 2 {
		t.Fatalf("forwarded children leaked into group history: %s", history.RawBody)
	}
	sdkRecallMessage(t, forwarder, parent)
	recalled, err := forwarder.Im.MessageResource.Get(t.Context(), larkim.NewGetMessageResourceReqBuilder().
		MessageId(value(items[1].MessageId)).Type("image").FileKey(key).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, recalled.CodeError, 230110)
}

func TestForwardedCardsUseOriginalAppPolicy(t *testing.T) {
	cfg := sdkEventConfig()
	origin := &cfg.Apps[0]
	origin.EncryptKey = ""
	callbacks := make(chan string, 2)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := sdkReadWebhook(t, w, r, origin.EncryptKey)
		if !ok {
			return
		}
		var packet struct {
			Header struct {
				AppID string `json:"app_id"`
			} `json:"header"`
		}
		if err := json.Unmarshal(body, &packet); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		callbacks <- packet.Header.AppID
		_, _ = w.Write([]byte(`{"toast":{"type":"success","content":"Received by original app"}}`))
	}))
	defer receiver.Close()
	origin.WebhookURL = receiver.URL
	_, web := sdkEventServer(t, cfg, event.Options{WebhookMaxAttempts: 1})
	owner, forwarder := sdkEventREST(web, *origin), sdkEventREST(web, cfg.Apps[1])
	operator := cfg.Users[0].OpenID
	source, target := sdkCreateGroup(t, owner), sdkCreateGroup(t, forwarder, operator)
	sdkBotJoin(t, owner, source, cfg.Apps[1].AppID)

	send := func(cardConfig string) string {
		t.Helper()
		card := fmt.Sprintf(`{"schema":"2.0","config":%s,"body":{"elements":[{"tag":"markdown","content":"Review"}]}}`, cardConfig)
		return value(sdkSend(t, owner, "chat_id", source, "interactive", card).MessageId)
	}
	forward := func(id string, code int) string {
		t.Helper()
		reply, err := forwarder.Im.Message.Forward(t.Context(), larkim.NewForwardMessageReqBuilder().MessageId(id).
			ReceiveIdType("chat_id").Body(larkim.NewForwardMessageReqBodyBuilder().ReceiveId(target).Build()).Build())
		requireSDKNetwork(t, err)
		requireSDKCode(t, reply.CodeError, code)
		if reply.Data == nil {
			return ""
		}
		return value(reply.Data.MessageId)
	}

	protected := send(`{"enable_forward":false}`)
	forward(protected, 230070)
	merge, err := forwarder.Im.Message.MergeForward(t.Context(), larkim.NewMergeForwardMessageReqBuilder().ReceiveIdType("chat_id").
		Body(larkim.NewMergeForwardMessageReqBodyBuilder().ReceiveId(target).MessageIdList([]string{protected}).Build()).Build())
	requireSDKNetwork(t, err)
	requireSDKCode(t, merge.CodeError, 230070)

	action := func(id string, status int) []byte {
		t.Helper()
		return sandboxRequest(t, web, http.MethodPost, "cards/action", map[string]any{
			"message_id": id, "operator_open_id": operator, "action": map[string]string{"tag": "button"},
		}, status)
	}
	action(forward(send("{}"), 0), http.StatusBadRequest)
	toast := action(forward(send(`{"enable_forward_interaction":true}`), 0), http.StatusOK)
	if receiveSDKEvent(t, callbacks) != origin.AppID || !bytes.Contains(toast, []byte("Received by original app")) {
		t.Fatalf("forwarded card callback missing or sent incorrectly: %s", toast)
	}
}
