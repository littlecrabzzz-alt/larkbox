// Package work implements scheduling and work management APIs.
package work

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/middleware"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

type fields = map[string]any

type handler struct{ state *store.Store }

// RegisterRoutes serves calendar, task, minutes, and mail workflows.
func RegisterRoutes(mux *http.ServeMux, cfg config.Config, state *store.Store) {
	h := handler{state}
	withToken := func(required middleware.RequiredToken, path string, action http.HandlerFunc, scopes ...string) {
		mux.Handle(path, middleware.OwnedResource(state, required, cfg.StrictMode, action, scopes...))
	}
	register := func(path string, action http.HandlerFunc, scopes ...string) {
		withToken(middleware.TenantOrUser, path, action, scopes...)
	}
	const calendars = "/open-apis/calendar/v4/calendars/"
	register("GET "+calendars+"{calendar_id}", h.calendar, "calendar:calendar:read", "calendar:calendar:readonly", "calendar:calendar")
	register("POST "+calendars+"primary", h.primaryCalendar, "calendar:calendar:read", "calendar:calendar:readonly", "calendar:calendar", "calendar:calendar.calendar:readonly")
	register("POST "+calendars+"{calendar_id}/events", h.createEvent, "calendar:calendar.event:create", "calendar:calendar")
	register("GET "+calendars+"{calendar_id}/events/{event_id}", h.readEvent, "calendar:calendar.event:read", "calendar:calendar:readonly", "calendar:calendar")
	register("GET "+calendars+"{calendar_id}/events/instance_view", h.agenda, "calendar:calendar.event:read", "calendar:calendar:readonly", "calendar:calendar")
	register("DELETE "+calendars+"{calendar_id}/events/{event_id}", h.deleteEvent, "calendar:calendar.event:delete", "calendar:calendar")
	const tasks = "/open-apis/task/v2/tasks"
	register("POST "+tasks, h.createTask, "task:task:write")
	register("GET "+tasks+"/{task_guid}", h.readTask, "task:task:read", "task:task:write")
	register("PATCH "+tasks+"/{task_guid}", h.updateTask, "task:task:write")
	const minutes = "/open-apis/minutes/v1/minutes/{minute_token}"
	withToken(middleware.User, "GET "+minutes, h.minute, "minutes:minutes.basic:read")
	withToken(middleware.User, "GET "+minutes+"/artifacts", h.minuteArtifacts, "minutes:minutes.artifacts:read")
	const mailbox = "/open-apis/mail/v1/user_mailboxes/{mailbox_id}"
	mail := func(method, suffix string, action http.HandlerFunc, scopes ...string) {
		withToken(middleware.User, method+" "+mailbox+suffix, action, scopes...)
	}
	mail(http.MethodGet, "/profile", h.mailProfile, "mail:user_mailbox:readonly")
	mail(http.MethodPost, "/drafts", h.createMailDraft, "mail:user_mailbox.message:modify")
	mail(http.MethodGet, "/drafts/{draft_id}", h.readMailDraft, mailReadScope)
	mail(http.MethodPost, "/drafts/{draft_id}/send", h.sendMailDraft, "mail:user_mailbox.message:send")
	mail(http.MethodGet, "/messages", h.listMail, mailReadScope)
	mail(http.MethodGet, "/messages/{message_id}", h.readMail, mailReadScope)
	mail(http.MethodPost, "/messages/batch_get", h.batchReadMail, mailReadScope)
	mail(http.MethodPost, "/search", h.searchMail, mailReadScope)
}

func reply(w http.ResponseWriter, data any) {
	_ = protocol.WriteJSON(w, http.StatusOK, protocol.Envelope{Code: protocol.Success, Msg: "success", Data: data})
}

func fail(w http.ResponseWriter, code protocol.Code) {
	err := protocol.NewError(code, nil)
	if code == protocol.InvalidParameter {
		err.Status = http.StatusBadRequest
	}
	_ = protocol.WriteError(w, err)
}

func stateError(w http.ResponseWriter, err error) {
	if middleware.WriteResetConflict(w, err) {
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		fail(w, protocol.ResourceNotFound)
		return
	}
	fail(w, protocol.InvalidParameter)
}

func read(w http.ResponseWriter, r *http.Request, into any, allowEmpty bool) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); allowEmpty && errors.Is(err, io.EOF) {
		return true
	} else if err != nil || !errors.Is(dec.Decode(new(json.RawMessage)), io.EOF) {
		fail(w, protocol.MalformedRequest)
		return false
	}
	return true
}
