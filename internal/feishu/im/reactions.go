package im

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

var reactionMessageMissing = protocol.APIError{Code: 231003, Message: "The message is not found, maybe not exist or deleted.", Status: http.StatusBadRequest}
var reactionMissing = protocol.APIError{Code: 231001, Message: "The reaction does not exist.", Status: http.StatusBadRequest}

func (h handler) createReaction(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ReactionType struct {
			EmojiType string `json:"emoji_type"`
		} `json:"reaction_type"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	emoji := req.ReactionType.EmojiType
	if strings.TrimSpace(emoji) == "" {
		badField(w, "reaction_type.emoji_type", "An emoji type is required.")
		return
	}
	actor := caller(r)
	reaction := store.Reaction{ID: protocol.NewID(protocol.ReactionID), OperatorOpenID: actor.OpenID, EmojiType: emoji}
	if !actor.IsUser {
		reaction.OperatorAppID = actor.AppID
	}
	_, err := h.state.ChangeIMMessage(actor, r.PathValue("message_id"), func(m *store.Message, _ store.Chat) error {
		if m.MsgType == "system" {
			return protocol.APIError{Code: 231017, Message: "This operation is not supported for this message type.", Status: http.StatusBadRequest}
		}
		for _, existing := range m.Reactions {
			if existing.OperatorAppID == reaction.OperatorAppID && existing.OperatorOpenID == actor.OpenID && existing.EmojiType == emoji {
				reaction = existing
				return nil
			}
		}
		reaction.CreatedAt = h.state.Now()
		m.Reactions = append(m.Reactions, reaction)
		return nil
	})
	if err != nil {
		reactionError(w, err)
		return
	}
	success(w, h.reactionData(reaction, "open_id"))
}

func (h handler) deleteReaction(w http.ResponseWriter, r *http.Request) {
	actor := caller(r)
	id := r.PathValue("reaction_id")
	var reaction store.Reaction
	_, err := h.state.ChangeIMMessage(actor, r.PathValue("message_id"), func(m *store.Message, _ store.Chat) error {
		index := slices.IndexFunc(m.Reactions, func(candidate store.Reaction) bool { return candidate.ID == id })
		if index < 0 {
			return reactionMissing
		}
		reaction = m.Reactions[index]
		if reaction.OperatorOpenID != actor.OpenID || (reaction.OperatorAppID == "") != actor.IsUser ||
			(!actor.IsUser && reaction.OperatorAppID != actor.AppID) {
			return noPermission
		}
		m.Reactions = slices.Delete(m.Reactions, index, index+1)
		return nil
	})
	if err != nil {
		reactionError(w, err)
		return
	}
	success(w, h.reactionData(reaction, "open_id"))
}

func (h handler) listReactions(w http.ResponseWriter, r *http.Request) {
	kind, ok := userIDType(w, r)
	if !ok {
		return
	}
	size, ok := pageSize(w, r, 100)
	if !ok {
		return
	}
	m, err := h.state.IMMessage(caller(r), r.PathValue("message_id"))
	if err != nil {
		reactionError(w, err)
		return
	}
	if m.Deleted {
		reactionError(w, store.ErrMessageDeleted)
		return
	}
	emoji := r.URL.Query().Get("reaction_type")
	reactions := emptySlice(m.Reactions)
	if emoji != "" {
		reactions = slices.DeleteFunc(reactions, func(reaction store.Reaction) bool { return reaction.EmojiType != emoji })
	}
	result, ok := paginate(w, r, reactions, size, func(reaction store.Reaction) string { return reaction.ID })
	if !ok {
		return
	}
	items := make([]fields, 0, len(result.Items))
	for _, reaction := range result.Items {
		items = append(items, h.reactionData(reaction, kind))
	}
	success(w, page[fields]{Items: items, HasMore: result.HasMore, PageToken: result.PageToken})
}

func (h handler) reactionData(reaction store.Reaction, kind string) fields {
	operator := fields{"operator_id": reaction.OperatorOpenID, "operator_type": "user"}
	if reaction.OperatorAppID != "" {
		operator["operator_id"], operator["operator_type"] = reaction.OperatorAppID, "app"
	} else if user, found := h.state.Users().Get(reaction.OperatorOpenID); found {
		operator["operator_id"] = userID(user, kind)
	}
	return fields{
		"reaction_id": reaction.ID, "operator": operator, "action_time": milliseconds(reaction.CreatedAt),
		"reaction_type": fields{"emoji_type": reaction.EmojiType},
	}
}

func reactionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrOutsideChat):
		fail(w, 231002, "The operator has no permission to react on the specific message.")
	case errors.Is(err, store.ErrMessageDeleted):
		_ = protocol.WriteError(w, reactionMessageMissing)
	default:
		writeFailure(w, err, reactionMessageMissing)
	}
}
