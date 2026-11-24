package im

import (
	"cmp"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

var missingChat = protocol.APIError{Code: 232002, Message: "Chat does not exist.", Status: http.StatusBadRequest}

type chatRequest struct {
	config.ChatSettings
	Name    string   `json:"name"`
	Mode    string   `json:"chat_mode"`
	OwnerID string   `json:"owner_id"`
	UserIDs []string `json:"user_id_list"`
	BotIDs  []string `json:"bot_id_list"`
}

func (h handler) createChat(w http.ResponseWriter, r *http.Request) {
	kind, ok := userIDType(w, r)
	if !ok {
		return
	}
	manager, ok := queryChoice(w, r, "set_bot_manager", "false", "true", "false")
	if !ok {
		return
	}
	var req chatRequest
	if !readJSON(w, r, &req) || !req.validate(w) {
		return
	}

	actor := caller(r)
	app, _ := h.state.Apps().Get(actor.AppID)
	members := []string{actor.OpenID}
	for _, id := range req.UserIDs {
		user, found := h.findUser(kind, id)
		if !found {
			badField(w, "user_id_list", "Contains an unknown user.")
			return
		}
		members = appendUnique(members, user.OpenID)
	}
	owner := actor.OpenID
	if req.OwnerID != "" {
		user, found := h.findUser(kind, req.OwnerID)
		if !found {
			badField(w, "owner_id", "Expected a known user.")
			return
		}
		owner = user.OpenID
		members = appendUnique(members, owner)
	}
	if req.Mode == "group" && app.BotOpenID != "" {
		members = appendUnique(members, app.BotOpenID)
	}
	bots := h.botsByOpenID()
	for _, id := range req.BotIDs {
		bot, found := h.state.Apps().Get(id)
		if !found || bot.BotOpenID == "" {
			badField(w, "bot_id_list", "Contains an unknown bot app ID.")
			return
		}
		members = appendUnique(members, bot.BotOpenID)
	}
	chat := store.Chat{
		ChatID: protocol.NewID(protocol.ChatID), ChatMode: req.Mode, Name: req.Name,
		OwnerID: owner, MemberIDs: members, ChatSettings: req.ChatSettings, CreatedAt: h.state.Now(), AppID: actor.AppID,
	}
	if req.Mode == "p2p" {
		if len(members) != 2 || manager == "true" {
			badField(w, "chat_mode", "A p2p chat requires two participants and has no managers.")
			return
		}
		direct, err := h.state.EnsureP2PChat(actor, chat)
		if err != nil {
			writeFailure(w, err, missingChat)
			return
		}
		chat = direct
	} else {
		if manager == "true" && app.BotOpenID != "" && owner != app.BotOpenID {
			chat.BotManagers = []string{app.AppID}
		}
		h.state.Chats().Put(chat)
		for _, id := range members {
			if bot, found := bots[id]; found {
				h.emitMembership(r, chat.ChatID, bot, true)
			}
		}
	}
	data := h.chatDetails(chat, kind)
	data["chat_id"] = chat.ChatID
	success(w, data)
}

func (req *chatRequest) validate(w http.ResponseWriter) bool {
	req.Mode = cmp.Or(req.Mode, "group")
	req.ChatType = cmp.Or(req.ChatType, "private")
	switch {
	case !slices.Contains([]string{"group", "p2p"}, req.Mode):
		badField(w, "chat_mode", "Expected group or p2p.")
		return false
	case !slices.Contains([]string{"private", "public"}, req.ChatType):
		badField(w, "chat_type", "Expected private or public.")
		return false
	case utf8.RuneCountInString(req.Name) > 60 || (req.ChatType == "public" && utf8.RuneCountInString(strings.TrimSpace(req.Name)) < 2):
		badField(w, "name", "Expected at most 60 characters and at least two for a public group.")
		return false
	case utf8.RuneCountInString(req.Description) > 100:
		badField(w, "description", "Expected at most 100 characters.")
		return false
	case len(req.UserIDs) > 50:
		badField(w, "user_id_list", "Expected at most 50 users.")
		return false
	case len(req.BotIDs) > 5:
		badField(w, "bot_id_list", "Expected at most five bots.")
		return false
	}
	for _, option := range []struct {
		field   string
		value   *string
		allowed []string
	}{
		{"group_message_type", &req.GroupMessageType, []string{"chat", "thread"}},
		{"join_message_visibility", &req.JoinMessageVisibility, []string{"all_members", "only_owner", "not_anyone"}},
		{"leave_message_visibility", &req.LeaveMessageVisibility, []string{"all_members", "only_owner", "not_anyone"}},
		{"membership_approval", &req.MembershipApproval, []string{"no_approval_required", "approval_required"}},
		{"urgent_setting", &req.UrgentSetting, []string{"all_members", "only_owner"}},
		{"video_conference_setting", &req.VideoConferenceSetting, []string{"all_members", "only_owner"}},
		{"edit_permission", &req.EditPermission, []string{"all_members", "only_owner"}},
		{"pin_manage_setting", &req.PinManageSetting, []string{"all_members", "only_owner"}},
		{"hide_member_count_setting", &req.HideMemberCountSetting, []string{"all_members", "only_owner"}},
	} {
		*option.value = cmp.Or(*option.value, option.allowed[0])
		if !slices.Contains(option.allowed, *option.value) {
			badField(w, option.field, "Expected one of: "+strings.Join(option.allowed, ", ")+".")
			return false
		}
	}
	if req.Mode == "group" {
		req.Name = cmp.Or(strings.TrimSpace(req.Name), "(无主题)")
	}
	for lang := range req.I18nNames {
		if !slices.Contains([]string{"zh_cn", "en_us", "ja_jp"}, lang) {
			badField(w, "i18n_names", "Expected zh_cn, en_us, or ja_jp.")
			return false
		}
	}
	return true
}

func appendUnique(ids []string, id string) []string {
	if !slices.Contains(ids, id) {
		return append(ids, id)
	}
	return ids
}

func (h handler) listChats(w http.ResponseWriter, r *http.Request) {
	kind, ok := userIDType(w, r)
	if !ok {
		return
	}
	sortType, ok := queryChoice(w, r, "sort_type", "ByCreateTimeAsc", "ByCreateTimeAsc", "ByActiveTimeDesc")
	if !ok {
		return
	}
	types, ok := queryChoice(w, r, "types", "group", "group", "p2p", "group,p2p", "p2p,group")
	if !ok {
		return
	}
	size, ok := pageSize(w, r, 100)
	if !ok {
		return
	}
	actor := caller(r)
	chats := h.state.Chats().List()
	chats = slices.DeleteFunc(chats, func(c store.Chat) bool {
		return !actor.CanReadChat(c) || !slices.Contains(strings.Split(types, ","), c.ChatMode) || (c.ChatMode == "p2p" && !actor.IsUser)
	})
	if sortType == "ByActiveTimeDesc" {
		sortChatsByActivity(chats, h.state.Messages().List())
	} else {
		slices.SortFunc(chats, func(a, b store.Chat) int {
			return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), strings.Compare(a.ChatID, b.ChatID))
		})
	}
	result, ok := paginate(w, r, chats, size, func(c store.Chat) string { return c.ChatID })
	if !ok {
		return
	}
	items := make([]fields, 0, len(result.Items))
	for _, chat := range result.Items {
		item := fields{
			"chat_id": chat.ChatID, "name": chat.Name, "avatar": chat.Avatar, "description": chat.Description,
			"chat_mode": chat.ChatMode, "chat_status": "normal", "external": chat.External, "labels": emptySlice(chat.Labels),
		}
		h.addOwner(item, chat, kind)
		if chat.ChatMode == "p2p" {
			for _, id := range chat.MemberIDs {
				if id == actor.OpenID {
					continue
				}
				if user, found := h.state.Users().Get(id); found {
					item["p2p_target_type"], item["p2p_target_id"] = "user", userID(user, kind)
				} else if bot, found := h.botsByOpenID()[id]; found {
					item["p2p_target_type"], item["p2p_target_id"] = "bot", bot.AppID
				}
			}
		}
		items = append(items, item)
	}
	success(w, page[fields]{Items: items, HasMore: result.HasMore, PageToken: result.PageToken})
}

func sortChatsByActivity(chats []store.Chat, messages []store.Message) {
	latest := make(map[string]store.Message)
	for _, m := range messages {
		if m.CreatedAt.After(latest[m.ChatID].CreatedAt) {
			latest[m.ChatID] = m
		}
	}
	slices.SortFunc(chats, func(a, b store.Chat) int {
		aTime := latest[a.ChatID].CreatedAt
		if aTime.IsZero() {
			aTime = a.CreatedAt
		}
		bTime := latest[b.ChatID].CreatedAt
		if bTime.IsZero() {
			bTime = b.CreatedAt
		}
		return cmp.Or(bTime.Compare(aTime), strings.Compare(a.ChatID, b.ChatID))
	})
}

type fields = map[string]any

func emptySlice[T any](items []T) []T {
	if items == nil {
		return []T{}
	}
	return items
}

func (h handler) getChat(w http.ResponseWriter, r *http.Request) {
	kind, ok := userIDType(w, r)
	if !ok {
		return
	}
	chat, err := h.state.IMChat(caller(r), r.PathValue("chat_id"))
	if err != nil {
		writeFailure(w, err, missingChat)
		return
	}
	data := h.chatDetails(chat, kind)
	bots := h.botsByOpenID()
	botCount, userCount := 0, 0
	for _, id := range chat.MemberIDs {
		if _, isBot := bots[id]; isBot {
			botCount++
		} else if _, isUser := h.state.Users().Get(id); isUser {
			userCount++
		}
	}
	data["user_count"], data["bot_count"] = strconv.Itoa(userCount), strconv.Itoa(botCount)
	data["chat_status"] = "normal"
	if chat.ChatMode != "p2p" {
		data["bot_manager_id_list"] = emptySlice(chat.BotManagers)
		data["user_manager_id_list"] = []string{}
	}
	success(w, data)
}

func (h handler) addOwner(data fields, chat store.Chat, kind string) {
	if owner, ok := h.state.Users().Get(chat.OwnerID); ok && chat.ChatMode != "p2p" {
		data["owner_id"], data["owner_id_type"], data["tenant_key"] = userID(owner, kind), kind, owner.TenantKey
	}
}

func (h handler) chatDetails(chat store.Chat, kind string) fields {
	data := fields{
		"chat_mode": chat.ChatMode, "name": chat.Name, "avatar": chat.Avatar,
		"description": chat.Description, "external": chat.External,
	}
	h.addOwner(data, chat, kind)
	if chat.ChatMode == "p2p" {
		return data
	}
	data["chat_type"] = cmp.Or(chat.ChatType, "private")
	data["group_message_type"] = cmp.Or(chat.GroupMessageType, "chat")
	data["add_member_permission"] = "all_members"
	data["at_all_permission"] = "all_members"
	data["share_card_permission"] = "allowed"
	data["moderation_permission"] = "all_members"
	data["join_message_visibility"] = cmp.Or(chat.JoinMessageVisibility, "all_members")
	data["leave_message_visibility"] = cmp.Or(chat.LeaveMessageVisibility, "all_members")
	data["membership_approval"] = cmp.Or(chat.MembershipApproval, "no_approval_required")
	data["urgent_setting"] = cmp.Or(chat.UrgentSetting, "all_members")
	data["video_conference_setting"] = cmp.Or(chat.VideoConferenceSetting, "all_members")
	data["edit_permission"] = cmp.Or(chat.EditPermission, "all_members")
	data["pin_manage_setting"] = cmp.Or(chat.PinManageSetting, "all_members")
	data["hide_member_count_setting"] = cmp.Or(chat.HideMemberCountSetting, "all_members")
	data["labels"], data["toolkit_ids"] = emptySlice(chat.Labels), emptySlice(chat.ToolkitIDs)
	for _, tag := range []string{"inner", "tenant", "department", "edu", "meeting", "customer_service"} {
		if slices.Contains(chat.ChatTags, tag) {
			data["chat_tag"] = tag
			break
		}
	}
	if len(chat.I18nNames) > 0 {
		data["i18n_names"] = chat.I18nNames
	}
	if chat.RestrictedModeSetting != nil {
		data["restricted_mode_setting"] = chat.RestrictedModeSetting
	}
	return data
}

func (h handler) botsByOpenID() map[string]store.App {
	result := make(map[string]store.App)
	for _, app := range h.state.Apps().List() {
		if app.BotOpenID != "" {
			result[app.BotOpenID] = app
		}
	}
	return result
}

func (h handler) listMembers(w http.ResponseWriter, r *http.Request) {
	kind, ok := queryChoice(w, r, "member_id_type", "open_id", "open_id", "user_id", "union_id")
	if !ok {
		return
	}
	size, ok := pageSize(w, r, 100)
	if !ok {
		return
	}
	chat, err := h.state.IMChat(caller(r), r.PathValue("chat_id"))
	if err != nil {
		writeFailure(w, err, missingChat)
		return
	}
	users := make([]store.User, 0, len(chat.MemberIDs))
	for _, id := range chat.MemberIDs {
		if user, found := h.state.Users().Get(id); found {
			users = append(users, user)
		}
	}
	result, ok := paginate(w, r, users, size, func(u store.User) string { return u.OpenID })
	if !ok {
		return
	}
	items := make([]fields, 0, len(result.Items))
	for _, user := range result.Items {
		items = append(items, fields{
			"member_id_type": kind, "member_id": userID(user, kind), "name": user.Name, "tenant_key": user.TenantKey,
		})
	}
	success(w, struct {
		page[fields]
		MemberTotal int `json:"member_total"`
	}{page[fields]{Items: items, HasMore: result.HasMore, PageToken: result.PageToken}, len(users)})
}

type memberChange struct {
	invalid []string
	missing []string
	pending []string
}

func (h handler) changeMembers(w http.ResponseWriter, r *http.Request) {
	kind, ok := queryChoice(w, r, "member_id_type", "open_id", "open_id", "user_id", "union_id", "app_id")
	if !ok {
		return
	}
	add := r.Method == http.MethodPost
	mode := "1"
	if add {
		mode, ok = queryChoice(w, r, "succeed_type", "0", "0", "1", "2")
		if !ok {
			return
		}
	}
	var req struct {
		IDs []string `json:"id_list"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	limit := 50
	if kind == "app_id" {
		limit = 5
	}
	if len(req.IDs) == 0 || len(req.IDs) > limit {
		badField(w, "id_list", "Invalid number of members.")
		return
	}

	bots := h.botsByOpenID()
	ids := make(map[string]string, len(req.IDs))
	for _, id := range req.IDs {
		ids[id] = h.resolveMember(kind, id, bots)
	}
	var result memberChange
	var changedBots []store.App
	actor, id := caller(r), r.PathValue("chat_id")
	err := h.state.ChangeIMChat(actor, id, func(chat *store.Chat) error {
		if chat.ChatMode == "p2p" {
			return protocol.NewError(protocol.IMInvalidParameter, nil)
		}
		if !add && !managesChat(actor, *chat) {
			for _, member := range req.IDs {
				if target := ids[member]; target != "" && target != actor.OpenID && slices.Contains(chat.MemberIDs, target) {
					return protocol.APIError{Code: 232014, Status: http.StatusBadRequest, Message: "The token used in the request does NOT have the permissions necessary to complete the request."}
				}
			}
		}
		result, changedBots = applyMembers(chat, req.IDs, ids, bots, actor, kind, add)
		if add && mode != "1" && len(result.invalid)+len(result.missing) > 0 {
			return protocol.NewError(protocol.IMInvalidParameter, nil)
		}
		return nil
	})

	data := fields{"invalid_id_list": emptySlice(result.invalid)}
	if add {
		data["not_existed_id_list"] = emptySlice(result.missing)
		data["pending_approval_id_list"] = emptySlice(result.pending)
	}
	if err != nil {
		if add && len(result.invalid)+len(result.missing) > 0 {
			failure := protocol.NewError(protocol.IMInvalidParameter, nil)
			_ = protocol.WriteJSON(w, failure.Status, protocol.Envelope{Code: failure.Code, Msg: failure.Message, Data: data})
			return
		}
		writeFailure(w, err, missingChat)
		return
	}
	for _, bot := range changedBots {
		h.emitMembership(r, id, bot, add)
	}
	success(w, data)
}

func (h handler) resolveMember(kind, id string, bots map[string]store.App) string {
	if user, found := h.findUser(kind, id); kind != "app_id" && found {
		return user.OpenID
	}
	if kind == "app_id" {
		bot, _ := h.state.Apps().Get(id)
		return bot.BotOpenID
	}
	if kind == "open_id" && bots[id].AppID != "" {
		return id
	}
	return ""
}

func applyMembers(chat *store.Chat, requested []string, ids map[string]string, bots map[string]store.App, actor store.IMActor, kind string, add bool) (memberChange, []store.App) {
	var result memberChange
	var changed []store.App
	seen := make(map[string]bool)
	botCount := 0
	for _, id := range chat.MemberIDs {
		if bots[id].AppID != "" {
			botCount++
		}
	}
	for _, id := range requested {
		if seen[id] {
			continue
		}
		seen[id] = true
		openID := ids[id]
		bot, isBot := bots[openID]
		present := slices.Contains(chat.MemberIDs, openID)
		switch {
		case openID == "":
			if add && validMemberID(kind, id) {
				result.missing = append(result.missing, id)
			} else {
				result.invalid = append(result.invalid, id)
			}
		case add && present:
			continue
		case add && isBot && (botCount >= 15 || len(changed) >= 5):
			result.invalid = append(result.invalid, id)
		case add && !isBot && chat.MembershipApproval == "approval_required" &&
			!managesChat(actor, *chat):
			result.pending = append(result.pending, id)
		case add:
			chat.MemberIDs = append(chat.MemberIDs, openID)
			if isBot {
				botCount++
				changed = append(changed, bot)
			}
		case !present || chat.OwnerID == openID || (isBot && len(changed) >= 5):
			result.invalid = append(result.invalid, id)
		default:
			chat.MemberIDs = slices.DeleteFunc(chat.MemberIDs, func(member string) bool { return member == openID })
			if isBot {
				chat.BotManagers = slices.DeleteFunc(chat.BotManagers, func(manager string) bool { return manager == bot.AppID })
				changed = append(changed, bot)
			}
		}
	}
	return result, changed
}

func managesChat(actor store.IMActor, chat store.Chat) bool {
	return chat.OwnerID == actor.OpenID ||
		(!actor.IsUser && (chat.AppID == actor.AppID || slices.Contains(chat.BotManagers, actor.AppID)))
}

func validMemberID(kind, id string) bool {
	if id == "" || strings.ContainsAny(id, " \t\r\n") {
		return false
	}
	switch kind {
	case "open_id":
		return strings.HasPrefix(id, "ou_") && len(id) > 3
	case "union_id":
		return strings.HasPrefix(id, "on_") && len(id) > 3
	case "app_id":
		return strings.HasPrefix(id, "cli_") && len(id) > 4
	default:
		return true
	}
}

func (h handler) emitMembership(r *http.Request, chatID string, bot store.App, add bool) {
	kind := "im.chat.member.bot.deleted_v1"
	if add {
		kind = "im.chat.member.bot.added_v1"
	}
	h.emitter.Emit(r.Context(), Event{
		Type: kind, AppID: bot.AppID, ChatID: chatID, ActorID: caller(r).OpenID,
		BotOpenID: bot.BotOpenID, At: h.state.Now(),
	})
}
