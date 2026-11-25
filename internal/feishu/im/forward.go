package im

import (
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

func (h handler) forwardMessage(w http.ResponseWriter, r *http.Request) {
	kind, ok := queryChoice(w, r, "receive_id_type", "", "open_id", "user_id", "union_id", "email", "chat_id", "thread_id")
	if !ok {
		return
	}
	var req struct {
		ReceiveID string   `json:"receive_id"`
		IDs       []string `json:"message_id_list"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.ReceiveID) == "" {
		badField(w, "receive_id", "A recipient ID is required.")
		return
	}
	messageID := r.PathValue("message_id")
	merged := messageID == ""
	if !merged {
		req.IDs = []string{messageID}
	}
	if len(req.IDs) == 0 || len(req.IDs) > 100 {
		badField(w, "message_id_list", "Expected between 1 and 100 source IDs.")
		return
	}
	target := store.NewMessage{UUID: r.URL.Query().Get("uuid")}
	if utf8.RuneCountInString(target.UUID) > 50 {
		badField(w, "uuid", "Expected at most 50 characters.")
		return
	}
	switch kind {
	case "chat_id":
		target.ChatID = req.ReceiveID
	case "thread_id":
		target.TargetThreadID = req.ReceiveID
	default:
		user, found := h.findUser(kind, req.ReceiveID)
		if !found || user.Inactive {
			fail(w, 230013, "Bot has NO availability to this user.")
			return
		}
		target.Peer = user.OpenID
	}
	actor := caller(r)
	var invalid []string
	var created bool
	message, err := store.Commit(r.Context(), h.state, func() (store.Message, error) {
		m, invalidIDs, first, err := h.state.ForwardIMMessages(actor, target, req.IDs, merged)
		invalid = invalidIDs
		created = first
		return m, err
	})
	if err != nil {
		writeForwardFailure(w, err, merged)
		return
	}
	if created {
		h.emitFirstBotChat(r, message.ChatID)
	}
	data := h.messageData(cardForRead(message, actor.AppID, false), "open_id", false)
	if merged {
		data = fields{"message": data, "invalid_message_id_list": emptySlice(invalid)}
	}
	success(w, data)
}

func writeForwardFailure(w http.ResponseWriter, err error, merged bool) {
	switch {
	case errors.Is(err, store.ErrForwardThreadMissing):
		fail(w, 230019, "The thread does NOT exist.")
	case errors.Is(err, store.ErrForwardRestricted):
		fail(w, 230070, "Forwarding of messages in restricted mode is not allowed.")
	case errors.Is(err, store.ErrForwardMixedChats):
		fail(w, 230069, "Does not support forwarding messages from different groups.")
	case errors.Is(err, store.ErrForwardMixedThreads):
		fail(w, 230067, "The source of the messages to be merged and forwarded are not in compliance.")
	case errors.Is(err, store.ErrForwardRecalled):
		fail(w, 230065, "All messages to be forwarded have been recalled.")
	case errors.Is(err, store.ErrForwardType):
		fail(w, 230061, "The message type does not support forwarding.")
	case errors.Is(err, store.ErrForwardSource) && merged:
		fail(w, 230064, "All messages to be forwarded are invalid.")
	case errors.Is(err, store.ErrForwardSource):
		fail(w, 230064, "The message to be forwarded is invalid.")
	case errors.Is(err, store.ErrForwardTooLarge):
		fail(w, 230001, err.Error())
	default:
		writeFailure(w, err, protocol.APIError{Code: 230063, Status: http.StatusBadRequest, Message: "The chat_id of group to forward is invalid."})
	}
}
