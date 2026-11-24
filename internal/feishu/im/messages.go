package im

import (
	"cmp"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

var missingMessage = protocol.NewError(protocol.IMInvalidParameter, nil)

type messageRequest struct {
	ReceiveID     string          `json:"receive_id"`
	MsgType       string          `json:"msg_type"`
	Content       *string         `json:"content"`
	UUID          string          `json:"uuid"`
	ReplyInThread json.RawMessage `json:"reply_in_thread"`
}

func (h handler) sendMessage(w http.ResponseWriter, r *http.Request) {
	kind, ok := queryChoice(w, r, "receive_id_type", "", "open_id", "user_id", "union_id", "email", "chat_id")
	if !ok {
		return
	}
	var req messageRequest
	if !readJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.ReceiveID) == "" {
		badField(w, "receive_id", "A recipient ID is required.")
		return
	}
	input, ok := h.newMessage(w, req)
	if !ok {
		return
	}
	if kind == "chat_id" {
		input.ChatID = req.ReceiveID
	} else {
		user, found := h.findUser(kind, req.ReceiveID)
		if !found {
			fail(w, 230013, "Bot has NO availability to this user.")
			return
		}
		input.Peer = user.OpenID
	}
	message, err := h.state.CreateIMMessage(caller(r), input)
	if err != nil {
		writeFailure(w, err, missingChat)
		return
	}
	success(w, h.messageData(message, "open_id"))
}

func (h handler) replyMessage(w http.ResponseWriter, r *http.Request) {
	var req messageRequest
	if !readJSON(w, r, &req) {
		return
	}
	input, ok := h.newMessage(w, req)
	if !ok {
		return
	}
	switch string(req.ReplyInThread) {
	case "true":
		input.ReplyInThread = true
	case "", "false":
	default:
		badField(w, "reply_in_thread", "Expected a boolean.")
		return
	}
	input.ParentID = r.PathValue("message_id")
	message, err := h.state.CreateIMMessage(caller(r), input)
	if err != nil {
		writeFailure(w, err, missingMessage)
		return
	}
	success(w, h.messageData(message, "open_id"))
}

func (h handler) newMessage(w http.ResponseWriter, req messageRequest) (store.NewMessage, bool) {
	if len(req.UUID) > 50 {
		badField(w, "uuid", "Expected at most 50 bytes.")
		return store.NewMessage{}, false
	}
	content, ok := checkContent(w, req.MsgType, req.Content)
	if !ok {
		return store.NewMessage{}, false
	}
	return store.NewMessage{
		MsgType: req.MsgType, Content: *req.Content, UUID: req.UUID,
		Mentions: h.mentions(req.MsgType, content),
	}, true
}

func checkContent(w http.ResponseWriter, kind string, encoded *string) (map[string]any, bool) {
	var content map[string]any
	if encoded == nil || json.Unmarshal([]byte(*encoded), &content) != nil || content == nil {
		badField(w, "content", "Expected a JSON-encoded object in a string.")
		return nil, false
	}

	switch kind {
	case "text":
		_, ok := content["text"].(string)
		if ok {
			return content, true
		}
	case "post":
		if validPost(content) {
			return content, true
		}
	case "interactive":
		return content, true
	case "image":
		if nonemptyString(content, "image_key") {
			return content, true
		}
	case "file", "audio", "sticker":
		if nonemptyString(content, "file_key") {
			return content, true
		}
	case "media":
		if nonemptyString(content, "file_key") && optionalString(content, "image_key") {
			return content, true
		}
	case "share_chat":
		if nonemptyString(content, "chat_id") {
			return content, true
		}
	case "share_user":
		if nonemptyString(content, "user_id") {
			return content, true
		}
	case "system":
		if nonemptyString(content, "template") {
			return content, true
		}
	default:
		badField(w, "msg_type", "Unsupported message type.")
		return nil, false
	}
	badField(w, "content", "Invalid content for message type "+kind+".")
	return nil, false
}

func nonemptyString(obj map[string]any, key string) bool {
	value, _ := obj[key].(string)
	return strings.TrimSpace(value) != ""
}

func optionalString(obj map[string]any, key string) bool {
	value, exists := obj[key]
	_, isString := value.(string)
	return !exists || isString
}

func validPost(post map[string]any) bool {
	if _, hasContent := post["content"]; hasContent {
		return validPostBody(post)
	}
	if len(post) == 0 {
		return false
	}
	for lang, value := range post {
		localized, ok := value.(map[string]any)
		if !slices.Contains([]string{"zh_cn", "en_us", "ja_jp"}, lang) || !ok || !validPostBody(localized) {
			return false
		}
	}
	return true
}

func validPostBody(post map[string]any) bool {
	paragraphs, ok := post["content"].([]any)
	if !ok || len(paragraphs) == 0 {
		return false
	}
	if title, present := post["title"]; present {
		if _, ok := title.(string); !ok {
			return false
		}
	}
	for _, row := range paragraphs {
		elements, ok := row.([]any)
		if !ok {
			return false
		}
		for _, value := range elements {
			element, ok := value.(map[string]any)
			if !ok || !validPostElement(element, len(elements)) {
				return false
			}
		}
	}
	return true
}

func validPostElement(element map[string]any, paragraphSize int) bool {
	text, hasText := element["text"].(string)
	tag, _ := element["tag"].(string)
	switch tag {
	case "text", "code_block":
		return hasText
	case "a":
		return hasText && nonemptyString(element, "href")
	case "at":
		return nonemptyString(element, "user_id")
	case "emotion":
		return nonemptyString(element, "emoji_type")
	case "img":
		return paragraphSize == 1 && nonemptyString(element, "image_key")
	case "media":
		return paragraphSize == 1 && nonemptyString(element, "file_key") && optionalString(element, "image_key")
	case "md":
		return paragraphSize == 1 && hasText && strings.TrimSpace(text) != ""
	default:
		return tag == "hr"
	}
}

var textMention = regexp.MustCompile(`<at user_id="([^"]+)">([^<]*)</at>`)

func (h handler) mentions(kind string, content map[string]any) []store.Mention {
	if kind != "text" && kind != "post" {
		return nil
	}
	type reference struct{ id, name string }
	var refs []reference
	if kind == "text" {
		value, _ := content["text"].(string)
		for _, match := range textMention.FindAllStringSubmatch(value, -1) {
			refs = append(refs, reference{match[1], match[2]})
		}
	} else {
		posts := []map[string]any{content}
		if _, ok := content["content"]; !ok {
			posts = nil
			for _, lang := range []string{"zh_cn", "en_us", "ja_jp"} {
				if post, ok := content[lang].(map[string]any); ok {
					posts = append(posts, post)
				}
			}
		}
		for _, post := range posts {
			rows, _ := post["content"].([]any)
			for _, row := range rows {
				elements, _ := row.([]any)
				for _, v := range elements {
					element, _ := v.(map[string]any)
					if element["tag"] == "at" {
						id, _ := element["user_id"].(string)
						name, _ := element["user_name"].(string)
						refs = append(refs, reference{id, name})
					}
				}
			}
		}
	}
	result := make([]store.Mention, 0, len(refs))
	seen := make(map[string]bool)
	for _, ref := range refs {
		if ref.id == "" || seen[ref.id] {
			continue
		}
		seen[ref.id] = true
		mention := store.Mention{Key: "@_user_" + strconv.Itoa(len(result)+1), OpenID: ref.id, Name: ref.name}
		for _, idType := range []string{"open_id", "user_id", "union_id"} {
			if user, ok := h.findUser(idType, ref.id); ok {
				mention.OpenID, mention.UserID, mention.Name = user.OpenID, user.UserID, user.Name
				break
			}
		}
		result = append(result, mention)
	}
	return result
}

func (h handler) messageData(m store.Message, kind string) fields {
	sender := fields{"id": m.AppID, "id_type": "app_id", "sender_type": "app", "tenant_key": ""}
	if m.SenderIsUser || m.AppID == "" {
		user, found := h.state.Users().Get(m.SenderOpenID)
		if found {
			sender["id"], sender["id_type"], sender["sender_type"] = userID(user, kind), kind, "user"
			sender["tenant_key"] = user.TenantKey
			sender["sender_name"] = user.Name
		} else {
			sender["id"], sender["id_type"], sender["sender_type"] = m.SenderOpenID, "open_id", "user"
		}
	} else if app, ok := h.state.Apps().Get(m.AppID); ok {
		sender["open_bot_id"], sender["sender_name"] = app.BotOpenID, app.BotName
	}
	mentions := make([]fields, 0, len(m.Mentions))
	for _, mention := range m.Mentions {
		id, idType, tenant := mention.OpenID, "open_id", ""
		if user, ok := h.state.Users().Get(id); ok {
			id, idType, tenant = userID(user, kind), kind, user.TenantKey
		}
		mentions = append(mentions, fields{"key": mention.Key, "id": id, "id_type": idType, "name": mention.Name, "tenant_key": tenant})
	}
	payload := m.Content
	if m.Deleted {
		payload, mentions = "", []fields{}
	}
	result := fields{
		"message_id": m.ID, "msg_type": m.MsgType, "create_time": milliseconds(m.CreatedAt), "update_time": milliseconds(m.UpdatedAt),
		"deleted": m.Deleted, "updated": m.Updated, "chat_id": m.ChatID, "sender": sender,
		"body": fields{"content": payload}, "mentions": mentions,
	}
	for key, value := range map[string]string{"root_id": m.RootID, "parent_id": m.ParentID, "thread_id": m.ThreadID} {
		if value != "" {
			result[key] = value
		}
	}
	return result
}

func (h handler) getMessage(w http.ResponseWriter, r *http.Request) {
	kind, ok := userIDType(w, r)
	if !ok {
		return
	}
	m, err := h.state.IMMessage(caller(r), r.PathValue("message_id"))
	if err != nil {
		writeFailure(w, err, missingMessage)
		return
	}
	success(w, fields{"items": []fields{h.messageData(m, kind)}})
}

func sentBy(actor store.IMActor, m *store.Message) bool {
	if actor.IsUser {
		return (m.SenderIsUser || m.AppID == "") && m.SenderOpenID == actor.OpenID
	}
	return !m.SenderIsUser && actor.AppID == m.AppID
}

func (h handler) editMessage(w http.ResponseWriter, r *http.Request) {
	var req messageRequest
	if !readJSON(w, r, &req) {
		return
	}
	patch := r.Method == http.MethodPatch
	if patch {
		req.MsgType = "interactive"
	} else if req.MsgType != "text" && req.MsgType != "post" {
		badField(w, "msg_type", "PUT supports text and post.")
		return
	}
	content, ok := checkContent(w, req.MsgType, req.Content)
	if !ok {
		return
	}
	actor := caller(r)
	mentions := h.mentions(req.MsgType, content)
	m, err := h.state.ChangeIMMessage(actor, r.PathValue("message_id"), func(m *store.Message, _ store.Chat) error {
		if !sentBy(actor, m) {
			return noPermission
		}
		if (patch && m.MsgType != "interactive") || (!patch && m.MsgType != "text" && m.MsgType != "post") {
			return protocol.NewError(protocol.IMInvalidParameter, nil)
		}
		m.MsgType, m.Content, m.Mentions = req.MsgType, *req.Content, mentions
		m.Updated, m.UpdatedAt = true, h.state.Now()
		return nil
	})
	if err != nil {
		writeFailure(w, err, missingMessage)
		return
	}
	if patch {
		_ = protocol.WriteSuccess(w, nil)
		return
	}
	success(w, h.messageData(m, "open_id"))
}

func (h handler) recallMessage(w http.ResponseWriter, r *http.Request) {
	actor := caller(r)
	m, err := h.state.ChangeIMMessage(actor, r.PathValue("message_id"), func(m *store.Message, chat store.Chat) error {
		if !sentBy(actor, m) && (chat.ChatMode != "group" || !managesChat(actor, chat)) {
			return noPermission
		}
		m.Deleted, m.UpdatedAt = true, h.state.Now()
		return nil
	})
	if err != nil {
		writeFailure(w, err, missingMessage)
		return
	}
	h.emitter.Emit(r.Context(), Event{
		Type: "im.message.recalled_v1", AppID: m.AppID, ChatID: m.ChatID,
		MessageID: m.ID, ActorID: actor.OpenID, At: m.UpdatedAt,
	})
	_ = protocol.WriteSuccess(w, nil)
}

func (h handler) listMessages(w http.ResponseWriter, r *http.Request) {
	kind, ok := queryChoice(w, r, "container_id_type", "", "chat", "thread")
	if !ok {
		return
	}
	containerID := r.URL.Query().Get("container_id")
	if strings.TrimSpace(containerID) == "" {
		badField(w, "container_id", "A container ID is required.")
		return
	}
	idType, ok := userIDType(w, r)
	if !ok {
		return
	}
	order, ok := queryChoice(w, r, "sort_type", "ByCreateTimeAsc", "ByCreateTimeAsc", "ByCreateTimeDesc")
	if !ok {
		return
	}
	size, ok := pageSize(w, r, 50)
	if !ok {
		return
	}
	start, ok := historyTime(w, r, "start_time")
	if !ok {
		return
	}
	end, ok := historyTime(w, r, "end_time")
	if !ok {
		return
	}
	if end != nil && start != nil && *end < *start {
		badField(w, "end_time", "Expected end_time at or after start_time.")
		return
	}
	messages, err := h.state.IMHistory(caller(r), kind, containerID)
	if err != nil {
		missing := missingChat
		if kind == "thread" {
			missing = protocol.APIError{Code: 230019, Message: "The topic does NOT exist.", Status: http.StatusBadRequest}
		}
		writeFailure(w, err, missing)
		return
	}
	messages = slices.DeleteFunc(messages, func(m store.Message) bool {
		unix := m.CreatedAt.Unix()
		return (start != nil && unix < *start) || (end != nil && unix > *end)
	})
	slices.SortFunc(messages, func(a, b store.Message) int {
		ordering := cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.Order, b.Order), strings.Compare(a.ID, b.ID))
		if order == "ByCreateTimeDesc" {
			return -ordering
		}
		return ordering
	})
	result, ok := paginate(w, r, messages, size, func(m store.Message) string { return m.ID })
	if !ok {
		return
	}
	items := make([]fields, 0, len(result.Items))
	for _, m := range result.Items {
		items = append(items, h.messageData(m, idType))
	}
	success(w, page[fields]{Items: items, HasMore: result.HasMore, PageToken: result.PageToken})
}

func historyTime(w http.ResponseWriter, r *http.Request, name string) (*int64, bool) {
	values, present := r.URL.Query()[name]
	if !present {
		return nil, true
	}
	if len(values) == 1 && len(values[0]) > 0 && len(values[0]) <= 10 &&
		strings.Trim(values[0], "0123456789") == "" {
		value, err := strconv.ParseInt(values[0], 10, 64)
		if err == nil {
			return &value, true
		}
	}
	badField(w, name, "Expected a Unix timestamp in seconds.")
	return nil, false
}
