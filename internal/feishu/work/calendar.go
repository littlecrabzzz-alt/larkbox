package work

import (
	"cmp"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

type eventTime struct {
	Timestamp string `json:"timestamp"`
	Timezone  string `json:"timezone,omitempty"`
}

func (t eventTime) parse() (time.Time, bool) {
	seconds, err := strconv.ParseInt(t.Timestamp, 10, 64)
	return time.Unix(seconds, 0).UTC(), err == nil && seconds > 0 && seconds <= 253402300799
}

func primaryData(owner store.ResourceOwner) fields {
	return fields{"calendar_id": store.CalendarID(owner), "summary": "Primary calendar", "type": "primary", "role": "owner"}
}

func (h handler) ownCalendar(w http.ResponseWriter, r *http.Request) (store.ResourceOwner, bool) {
	owner := h.owner(r)
	calendar := r.PathValue("calendar_id")
	if calendar != "" && calendar != "primary" && calendar != store.CalendarID(owner) {
		fail(w, protocol.ResourceNotFound)
		return store.ResourceOwner{}, false
	}
	return owner, true
}

func (h handler) calendar(w http.ResponseWriter, r *http.Request) {
	if owner, ok := h.ownCalendar(w, r); ok {
		reply(w, fields{"calendar": primaryData(owner)})
	}
}

func (h handler) primaryCalendar(w http.ResponseWriter, r *http.Request) {
	owner := h.owner(r)
	reply(w, fields{"calendars": []fields{{"calendar": primaryData(owner), "user_id": owner.UserOpenID}}})
}

func (h handler) createEvent(w http.ResponseWriter, r *http.Request) {
	owner, ok := h.ownCalendar(w, r)
	if !ok {
		return
	}
	var input struct {
		Summary     string          `json:"summary"`
		Description string          `json:"description"`
		Rich        string          `json:"description_rich"`
		Start       eventTime       `json:"start_time"`
		End         eventTime       `json:"end_time"`
		Notify      bool            `json:"need_notification"`
		Visibility  string          `json:"visibility"`
		Ability     string          `json:"attendee_ability"`
		FreeBusy    string          `json:"free_busy_status"`
		VChat       json.RawMessage `json:"vchat"`
		Reminders   json.RawMessage `json:"reminders"`
	}
	if !read(w, r, &input, false) {
		return
	}
	start, startOK := input.Start.parse()
	end, endOK := input.End.parse()
	if !startOK || !endOK || !end.After(start) {
		fail(w, protocol.InvalidParameter)
		return
	}
	entry, err := store.Commit(r.Context(), h.state, func() (store.CalendarEvent, error) {
		return h.state.CreateCalendarEvent(store.CalendarEvent{
			Owner: owner, Summary: input.Summary, Description: cmp.Or(input.Rich, input.Description), Start: start, End: end,
			Timezone: cmp.Or(input.Start.Timezone, "UTC"), FreeBusy: input.FreeBusy, Ability: input.Ability,
			VChat: input.VChat, Reminders: input.Reminders,
		}), nil
	})
	if err != nil {
		stateError(w, err)
		return
	}
	reply(w, fields{"event": calendarEventData(entry)})
}

func calendarEventData(entry store.CalendarEvent) fields {
	return fields{
		"event_id": entry.ID, "calendar_id": store.CalendarID(entry.Owner), "summary": entry.Summary, "description": entry.Description,
		"start_time": eventTime{strconv.FormatInt(entry.Start.Unix(), 10), entry.Timezone},
		"end_time":   eventTime{strconv.FormatInt(entry.End.Unix(), 10), entry.Timezone}, "status": "confirmed",
		"attendee_ability": entry.Ability, "free_busy_status": entry.FreeBusy, "vchat": entry.VChat, "reminders": entry.Reminders,
	}
}

func (h handler) readEvent(w http.ResponseWriter, r *http.Request) {
	owner, ok := h.ownCalendar(w, r)
	if !ok {
		return
	}
	entry, found := h.state.CalendarEvent(owner, r.PathValue("event_id"))
	if !found {
		fail(w, protocol.ResourceNotFound)
		return
	}
	reply(w, fields{"event": calendarEventData(entry)})
}

func (h handler) agenda(w http.ResponseWriter, r *http.Request) {
	owner, ok := h.ownCalendar(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	start, startOK := (eventTime{Timestamp: query.Get("start_time")}).parse()
	end, endOK := (eventTime{Timestamp: query.Get("end_time")}).parse()
	if !startOK || !endOK || !end.After(start) {
		fail(w, protocol.InvalidParameter)
		return
	}
	if end.Sub(start) > 40*24*time.Hour {
		_ = protocol.WriteError(w, protocol.APIError{Code: 193103, Status: http.StatusBadRequest, Message: "time range exceeds 40 days"})
		return
	}
	entries := h.state.CalendarEvents(owner, start, end)
	if len(entries) > 1000 {
		_ = protocol.WriteError(w, protocol.APIError{Code: 193104, Status: http.StatusBadRequest, Message: "too many calendar events"})
		return
	}
	items := make([]fields, 0, len(entries))
	for _, entry := range entries {
		items = append(items, calendarEventData(entry))
	}
	reply(w, fields{"items": items})
}

func (h handler) deleteEvent(w http.ResponseWriter, r *http.Request) {
	owner, ok := h.ownCalendar(w, r)
	if !ok {
		return
	}
	if err := h.state.Mutate(r.Context(), func() error { return h.state.DeleteCalendarEvent(owner, r.PathValue("event_id")) }); err != nil {
		stateError(w, err)
		return
	}
	reply(w, nil)
}
