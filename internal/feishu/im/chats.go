package im

import (
	"cmp"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/event"
	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/middleware"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

var (
	missingChat          = protocol.APIError{Code: 232006, Message: "Your request specifies a chat_id which is invalid.", Status: http.StatusBadRequest}
	invalidChatParameter = protocol.APIError{Code: 232001, Message: "Your request contains an invalid request parameter.", Status: http.StatusBadRequest}
	chatPermission       = protocol.APIError{Code: 232014, Message: "The token used in the request does NOT have the permissions necessary to complete the request.", Status: http.StatusBadRequest}
)

func writeChatFailure(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrOutsideChat) {
		fail(w, 232011, "Operator can NOT be out of the chat.")
		return
	}
	writeFailure(w, err, missingChat)
}

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
		if !found || user.Inactive {
			badField(w, "user_id_list", "Contains an unknown user.")
			return
		}
		members = appendUnique(members, user.OpenID)
	}
	owner := actor.OpenID
	if req.OwnerID != "" {
		user, found := h.findUser(kind, req.OwnerID)
		if !found || user.Inactive {
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
		OwnerID: owner, CreatorID: actor.OpenID, MemberIDs: members, ChatSettings: req.ChatSettings, CreatedAt: h.state.Now(), AppID: actor.AppID,
	}
	if req.Mode == "p2p" {
		if len(members) != 2 || manager == "true" {
			badField(w, "chat_mode", "A p2p chat requires two participants and has no managers.")
			return
		}
		var created bool
		direct, err := store.Commit(r.Context(), h.state, func() (store.Chat, error) {
			direct, first, err := h.state.EnsureP2PChat(actor, chat)
			created = first
			return direct, err
		})
		if err != nil {
			writeChatFailure(w, err)
			return
		}
		chat = direct
		if created {
			h.emitFirstBotChat(r, chat.ChatID)
		}
	} else {
		if manager == "true" && app.BotOpenID != "" && owner != app.BotOpenID {
			chat.BotManagers = []string{app.AppID}
		}
		if err := h.state.Mutate(r.Context(), func() error {
			h.state.Chats().Put(chat)
			return nil
		}); middleware.WriteResetConflict(w, err) {
			return
		}
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
	chat := config.Chat{ChatMode: req.Mode, Name: req.Name, ChatSettings: req.ChatSettings}
	if field, description := chat.NormalizeSettings(); field != "" {
		badField(w, field, description)
		return false
	}
	req.Mode, req.Name, req.ChatSettings = chat.ChatMode, chat.Name, chat.ChatSettings
	switch {
	case len(req.UserIDs) > 50:
		badField(w, "user_id_list", "Expected at most 50 users.")
		return false
	case len(req.BotIDs) > 5:
		badField(w, "bot_id_list", "Expected at most five bots.")
		return false
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
	cursor, ok := readCursor(w, r)
	if !ok {
		return
	}
	actor := caller(r)
	chats := h.state.IMChats(actor)
	modes := strings.Split(types, ",")
	chats = slices.DeleteFunc(chats, func(c store.Chat) bool {
		return !slices.Contains(modes, c.ChatMode) || (c.ChatMode == "p2p" && !actor.IsUser)
	})
	var activity map[string]time.Time
	if sortType == "ByActiveTimeDesc" {
		watermark, latest := h.state.IMActivity(chats, cursor.Activity)
		cursor.Activity, activity = &watermark, latest
	}
	result := paginate(chats, size, cursor, sortType == "ByActiveTimeDesc", func(c store.Chat) sortKey {
		at := c.CreatedAt
		if activity != nil {
			at = activity[c.ChatID]
		}
		return sortKey{At: at, ID: c.ChatID}
	})
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
		writeChatFailure(w, err)
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
		writeChatFailure(w, err)
		return
	}
	users := make([]store.User, 0, len(chat.MemberIDs))
	for _, id := range chat.MemberIDs {
		if user, found := h.state.Users().Get(id); found {
			users = append(users, user)
		}
	}
	cursor, ok := readCursor(w, r)
	if !ok {
		return
	}
	result := paginate(users, size, cursor, false, func(u store.User) sortKey {
		return sortKey{Order: chat.MemberOrder[u.OpenID], ID: u.OpenID}
	})
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
	invalid, missing, pending []string
	users                     []string
	available, unavailable    int
}

func (m memberChange) failure(mode string) protocol.APIError {
	if m.available == 0 {
		return protocol.APIError{
			Code: 232027, Status: http.StatusBadRequest,
			Message: "There are no valid members in the ID list specified in your request.",
		}
	}
	// The default skips inactive users but fails on other unavailable IDs.
	if (mode == "0" && m.unavailable > 0) || (mode == "2" && len(m.invalid)+len(m.missing) > 0) {
		return invalidChatParameter
	}
	return protocol.APIError{}
}

type memberTarget struct {
	openID   string
	invalid  bool
	inactive bool
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
	if req.IDs == nil || len(req.IDs) > limit || (!add && len(req.IDs) == 0) {
		badField(w, "id_list", "Invalid number of members.")
		return
	}

	bots := h.botsByOpenID()
	ids := make(map[string]memberTarget, len(req.IDs))
	for _, id := range req.IDs {
		ids[id] = h.resolveMember(kind, id, bots)
	}
	var result memberChange
	var selectionFailure protocol.APIError
	var changedBots []store.App
	var recipients []string
	actor, id := caller(r), r.PathValue("chat_id")
	chat, err := store.Commit(r.Context(), h.state, func() (store.Chat, error) {
		updated, receivedBy, err := h.state.ChangeIMChat(actor, id, func(chat *store.Chat) error {
			if chat.ChatMode == "p2p" {
				return invalidChatParameter
			}
			if !add && !managesChat(actor, *chat) {
				for _, member := range req.IDs {
					if target := ids[member].openID; target != "" && target != actor.OpenID && slices.Contains(chat.MemberIDs, target) {
						return chatPermission
					}
				}
			}
			result, changedBots = applyMembers(chat, req.IDs, ids, bots, actor, add)
			if add {
				selectionFailure = result.failure(mode)
				if selectionFailure.Code != protocol.Success {
					return selectionFailure
				}
			}
			return nil
		})
		recipients = receivedBy
		return updated, err
	})

	data := fields{"invalid_id_list": emptySlice(result.invalid)}
	if add {
		data["not_existed_id_list"] = emptySlice(result.missing)
		data["pending_approval_id_list"] = emptySlice(result.pending)
	}
	if err != nil {
		if selectionFailure.Code != protocol.Success {
			_ = protocol.WriteJSON(w, selectionFailure.Status, protocol.Envelope{Code: selectionFailure.Code, Msg: selectionFailure.Message, Data: data})
			return
		}
		writeChatFailure(w, err)
		return
	}
	for _, bot := range changedBots {
		h.emitMembership(r, id, bot, add)
	}
	if len(result.users) > 0 {
		kind := "im.chat.member.user.deleted_v1"
		if add {
			kind = "im.chat.member.user.added_v1"
		}
		h.emitChatLifecycle(r, chat, kind, recipients, result.users)
	}
	success(w, data)
}

func (h handler) resolveMember(kind, id string, bots map[string]store.App) memberTarget {
	if kind == "app_id" {
		if bot, found := h.state.Apps().Get(id); found {
			return memberTarget{openID: bot.BotOpenID, invalid: bot.BotOpenID == ""}
		}
	} else if user, found := h.findUser(kind, id); found {
		return memberTarget{openID: user.OpenID, inactive: user.Inactive}
	} else if kind == "open_id" && bots[id].AppID != "" {
		return memberTarget{openID: id}
	}
	return memberTarget{invalid: !validMemberID(kind, id)}
}

func applyMembers(chat *store.Chat, requested []string, ids map[string]memberTarget, bots map[string]store.App, actor store.IMActor, add bool) (memberChange, []store.App) {
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
		target := ids[id]
		openID := target.openID
		bot, isBot := bots[openID]
		present := slices.Contains(chat.MemberIDs, openID)
		switch {
		case add && target.inactive:
			result.invalid = append(result.invalid, id)
		case target.invalid || openID == "":
			result.unavailable++
			if add && !target.invalid {
				result.missing = append(result.missing, id)
			} else {
				result.invalid = append(result.invalid, id)
			}
		case add && present:
			result.available++
		case add && isBot && (botCount >= 15 || len(changed) >= 5):
			result.invalid = append(result.invalid, id)
			result.unavailable++
		case add && !isBot && chat.MembershipApproval == "approval_required" &&
			!managesChat(actor, *chat):
			result.pending = append(result.pending, id)
			result.available++
		case add:
			result.available++
			chat.MemberIDs = append(chat.MemberIDs, openID)
			if isBot {
				botCount++
				changed = append(changed, bot)
			} else {
				result.users = append(result.users, openID)
			}
		case !present || chat.OwnerID == openID || (isBot && len(changed) >= 5):
			result.invalid = append(result.invalid, id)
		default:
			chat.MemberIDs = slices.DeleteFunc(chat.MemberIDs, func(member string) bool { return member == openID })
			if isBot {
				chat.BotManagers = slices.DeleteFunc(chat.BotManagers, func(manager string) bool { return manager == bot.AppID })
				changed = append(changed, bot)
			} else {
				result.users = append(result.users, openID)
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
	actor := caller(r)
	h.emitter.Emit(r.Context(), Event{
		Type: kind, AppID: bot.AppID, ChatID: chatID, ActorID: actor.OpenID, ActorAppID: actor.AppID,
		BotOpenID: bot.BotOpenID, At: h.state.Now(),
	})
}

func (h handler) disbandChat(w http.ResponseWriter, r *http.Request) {
	actor := caller(r)
	var recipients []string
	chat, err := store.Commit(r.Context(), h.state, func() (store.Chat, error) {
		removed, receivedBy, err := h.state.DeleteIMChat(actor, r.PathValue("chat_id"), func(chat store.Chat) error {
			if chat.ChatMode != "group" {
				return protocol.APIError{Code: 232008, Status: http.StatusBadRequest, Message: "Your request specifies a chat whose type is NOT supported currently."}
			}
			if chat.OwnerID != actor.OpenID && (actor.IsUser || chat.CreatorID != actor.OpenID) {
				return protocol.APIError{Code: 232017, Status: http.StatusBadRequest, Message: "No Permission: the operator must be the owner or authorized creator."}
			}
			return nil
		})
		recipients = receivedBy
		return removed, err
	})
	if err != nil {
		writeChatFailure(w, err)
		return
	}
	h.emitChatLifecycle(r, chat, "im.chat.disbanded_v1", recipients, nil)
	success(w, fields{})
}

func (h handler) emitChatLifecycle(r *http.Request, chat store.Chat, kind string, recipients, members []string) {
	if len(recipients) == 0 {
		return
	}
	actor := caller(r)
	app, _ := h.state.Apps().Get(actor.AppID)
	tenant := app.TenantKey
	if tenant == "" {
		tenant = "larkbox"
	}
	operator, _ := h.state.Users().Get(actor.OpenID)
	operatorTenant := operator.TenantKey
	if operatorTenant == "" {
		operatorTenant = tenant
	}
	payload := fields{
		"chat_id": chat.ChatID, "name": chat.Name, "external": chat.External,
		"operator_id": h.eventIdentity(actor.OpenID), "operator_tenant_key": operatorTenant,
	}
	if len(chat.I18nNames) > 0 {
		payload["i18n_names"] = chat.I18nNames
	}
	if len(members) > 0 {
		users := make([]fields, 0, len(members))
		for _, id := range members {
			user, _ := h.state.Users().Get(id)
			userTenant := user.TenantKey
			if userTenant == "" {
				userTenant = tenant
			}
			users = append(users, fields{"name": user.Name, "tenant_key": userTenant, "user_id": h.eventIdentity(id)})
		}
		payload["users"] = users
	}
	now := h.state.Now()
	for _, appID := range recipients {
		h.emitter.Emit(r.Context(), Event{Type: kind, AppID: appID, ChatID: chat.ChatID,
			ActorID: actor.OpenID, ActorAppID: actor.AppID, At: now, Payload: payload})
	}
}

func (h handler) emitFirstBotChat(r *http.Request, chatID string) {
	actor := caller(r)
	app, _ := h.state.Apps().Get(actor.AppID)
	chat, ok := h.state.Chats().Get(chatID)
	if !ok || chat.ChatMode != "p2p" || len(chat.MemberIDs) != 2 || !slices.Contains(chat.MemberIDs, app.BotOpenID) {
		return
	}
	peer := chat.MemberIDs[0]
	if peer == app.BotOpenID {
		peer = chat.MemberIDs[1]
	}
	user, exists := h.state.Users().Get(peer)
	if !exists {
		return
	}
	operator, _ := h.state.Users().Get(actor.OpenID)
	operator.OpenID = actor.OpenID
	h.emitter.Emit(r.Context(), Event{
		Type: event.FirstBotConversation, AppID: actor.AppID, ChatID: chatID,
		ActorID: actor.OpenID, ActorAppID: actor.AppID, At: chat.CreatedAt,
		Payload: event.FirstBotConversationPayload(chatID, operator, user),
	})
}
