// Package im implements the Feishu IM v1 REST endpoints.
package im

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/middleware"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

// Event identifies an IM mutation for the app receiving the event.
type Event struct {
	Type      string
	AppID     string
	ChatID    string
	MessageID string
	ActorID   string
	BotOpenID string
	At        time.Time
}

// EventEmitter receives committed IM events. Implementations must support
// concurrent calls. A nil emitter disables dispatch.
type EventEmitter interface {
	Emit(context.Context, Event)
}

type noopEmitter struct{}

func (noopEmitter) Emit(context.Context, Event) {}

type handler struct {
	state   *store.Store
	emitter EventEmitter
}

// RegisterRoutes registers the IM API with the store and an optional event sink.
func RegisterRoutes(mux *http.ServeMux, state *store.Store, emitter EventEmitter) {
	if emitter == nil {
		emitter = noopEmitter{}
	}
	h := handler{state: state, emitter: emitter}
	anyActor := middleware.Bearer(state, middleware.TenantOrUser, true)
	bot := middleware.Bearer(state, middleware.Tenant, true)
	const base = "/open-apis/im/v1/"
	for _, route := range []struct {
		method  string
		path    string
		handle  http.HandlerFunc
		botOnly bool
	}{
		{"POST", "chats", h.createChat, false},
		{"GET", "chats", h.listChats, false},
		{"GET", "chats/{chat_id}", h.getChat, false},
		{"GET", "chats/{chat_id}/members", h.listMembers, false},
		{"POST", "chats/{chat_id}/members", h.changeMembers, false},
		{"DELETE", "chats/{chat_id}/members", h.changeMembers, false},
		{"POST", "messages", h.sendMessage, false},
		{"GET", "messages", h.listMessages, false},
		{"GET", "messages/{message_id}", h.getMessage, false},
		{"POST", "messages/{message_id}/reply", h.replyMessage, false},
		{"PATCH", "messages/{message_id}", h.editMessage, false},
		{"PUT", "messages/{message_id}", h.editMessage, true},
		{"DELETE", "messages/{message_id}", h.recallMessage, false},
		{"POST", "messages/{message_id}/reactions", h.createReaction, false},
		{"GET", "messages/{message_id}/reactions", h.listReactions, false},
		{"DELETE", "messages/{message_id}/reactions/{reaction_id}", h.deleteReaction, false},
		{"POST", "images", h.uploadImage, false},
		{"GET", "images/{image_key}", h.downloadImage, true},
		{"POST", "files", h.uploadFile, false},
		{"GET", "files/{file_key}", h.downloadFile, true},
		{"GET", "messages/{message_id}/resources/{file_key}", h.downloadResource, false},
	} {
		auth := anyActor
		if route.botOnly {
			auth = bot
		}
		mux.Handle(route.method+" "+base+route.path, auth(h.withActor(route.handle)))
	}
}

type actorKey struct{}

func (h handler) withActor(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, _ := middleware.AuthFromContext(r.Context())
		actor := store.IMActor{AppID: identity.AppID, OpenID: identity.UserOpenID, IsUser: identity.Kind == store.UserToken}
		if !actor.IsUser {
			app, _ := h.state.Apps().Get(identity.AppID)
			actor.OpenID = app.BotOpenID
			if actor.OpenID == "" {
				fail(w, 230006, "Bot ability is not activated.")
				return
			}
		}
		next(w, r.WithContext(context.WithValue(r.Context(), actorKey{}, actor)))
	})
}

func caller(r *http.Request) store.IMActor {
	actor, _ := r.Context().Value(actorKey{}).(store.IMActor)
	return actor
}

func success(w http.ResponseWriter, data any) {
	_ = protocol.WriteJSON(w, http.StatusOK, protocol.Envelope{Code: protocol.Success, Msg: "success", Data: data})
}

func fail(w http.ResponseWriter, code protocol.Code, msg string) {
	_ = protocol.WriteError(w, protocol.APIError{Code: code, Message: msg, Status: http.StatusBadRequest})
}

func badField(w http.ResponseWriter, field, description string) {
	_ = protocol.WriteError(w, protocol.NewError(protocol.FieldValidation, &protocol.ErrorDetails{
		FieldViolations: []protocol.FieldViolation{{Field: field, Description: description}},
	}))
}

var noPermission = protocol.APIError{Code: 230027, Message: "Lack of necessary permissions.", Status: http.StatusBadRequest}

func writeFailure(w http.ResponseWriter, err error, missing protocol.APIError) {
	var apiErr protocol.APIError
	switch {
	case errors.As(err, &apiErr):
		_ = protocol.WriteError(w, apiErr)
	case errors.Is(err, store.ErrOutsideChat):
		_ = protocol.WriteError(w, protocol.NewError(protocol.IMBotOutsideChat, nil))
	case errors.Is(err, store.ErrMessageDeleted):
		fail(w, 230011, "The message is recalled.")
	default:
		_ = protocol.WriteError(w, missing)
	}
}

func readJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil || len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, dst) != nil {
		badField(w, "body", "Expected a valid JSON object.")
		return false
	}
	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		badField(w, "body", "Expected one JSON object.")
		return false
	}
	return true
}

func queryChoice(w http.ResponseWriter, r *http.Request, field, fallback string, allowed ...string) (string, bool) {
	values, present := r.URL.Query()[field]
	if !present && fallback != "" {
		return fallback, true
	}
	if len(values) != 1 || !slices.Contains(allowed, values[0]) {
		badField(w, field, "Expected one of: "+strings.Join(allowed, ", ")+".")
		return "", false
	}
	return values[0], true
}

func userIDType(w http.ResponseWriter, r *http.Request) (string, bool) {
	return queryChoice(w, r, "user_id_type", "open_id", "open_id", "user_id", "union_id")
}

func (h handler) findUser(kind, id string) (store.User, bool) {
	if id == "" {
		return store.User{}, false
	}
	if kind == "open_id" {
		return h.state.Users().Get(id)
	}
	for _, user := range h.state.Users().List() {
		if userID(user, kind) == id || (kind == "email" && strings.EqualFold(user.Email, id)) {
			return user, true
		}
	}
	return store.User{}, false
}

func userID(user store.User, kind string) string {
	switch kind {
	case "user_id":
		return user.UserID
	case "union_id":
		return user.UnionID
	case "email":
		return user.Email
	default:
		return user.OpenID
	}
}

func milliseconds(t time.Time) string {
	return strconv.FormatInt(t.UnixMilli(), 10)
}

type page[T any] struct {
	Items     []T    `json:"items"`
	HasMore   bool   `json:"has_more"`
	PageToken string `json:"page_token,omitempty"`
}

func pageSize(w http.ResponseWriter, r *http.Request, maxSize int) (int, bool) {
	values, present := r.URL.Query()["page_size"]
	if !present {
		return 20, true
	}
	if len(values) == 1 {
		if size, err := strconv.Atoi(values[0]); err == nil && size > 0 && size <= maxSize {
			return size, true
		}
	}
	badField(w, "page_size", fmt.Sprintf("Expected an integer from 1 to %d.", maxSize))
	return 0, false
}

type sortKey struct {
	At    time.Time `json:"at,omitempty"`
	Order uint64    `json:"order,omitempty"`
	ID    string    `json:"id"`
}

type pageCursor struct {
	Scope    string  `json:"scope"`
	Last     sortKey `json:"last"`
	Activity *uint64 `json:"activity,omitempty"`
}

func readCursor(w http.ResponseWriter, r *http.Request) (pageCursor, bool) {
	actor := caller(r)
	query := r.URL.Query()
	query.Del("page_size")
	query.Del("page_token")
	hash := sha256.Sum256([]byte(actor.AppID + "\x00" + actor.OpenID + "\x00" + r.URL.Path + "?" + query.Encode()))
	scope := fmt.Sprintf("%x", hash[:8])
	if values, present := r.URL.Query()["page_token"]; present {
		if len(values) == 1 && len(values[0]) <= 1024 {
			var cursor pageCursor
			raw, err := base64.RawURLEncoding.DecodeString(values[0])
			if err == nil && json.Unmarshal(raw, &cursor) == nil && cursor.Scope == scope && cursor.Last.ID != "" {
				return cursor, true
			}
		}
		badField(w, "page_token", "Invalid page token.")
		return pageCursor{}, false
	}
	return pageCursor{Scope: scope}, true
}

func paginate[T any](items []T, size int, cursor pageCursor, descending bool, keyOf func(T) sortKey) page[T] {
	compare := func(a, b sortKey) int {
		order := cmp.Or(a.At.Compare(b.At), cmp.Compare(a.Order, b.Order), strings.Compare(a.ID, b.ID))
		if descending {
			return -order
		}
		return order
	}
	slices.SortFunc(items, func(a, b T) int { return compare(keyOf(a), keyOf(b)) })
	if cursor.Last.ID != "" {
		start := 0
		for start < len(items) && compare(keyOf(items[start]), cursor.Last) <= 0 {
			start++
		}
		items = items[start:]
	}
	result := page[T]{Items: items, HasMore: len(items) > size}
	if result.HasMore {
		cursor.Last = keyOf(items[size-1])
		raw, _ := json.Marshal(cursor)
		result.PageToken = base64.RawURLEncoding.EncodeToString(raw)
		result.Items = items[:size]
	}
	return result
}
