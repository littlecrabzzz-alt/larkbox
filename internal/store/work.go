package store

import (
	"cmp"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
)

// CalendarEvent is a time block on the requesting user's virtual calendar.
type CalendarEvent struct {
	Owner                    ResourceOwner
	ID, Summary, Description string
	Start, End               time.Time
	Timezone                 string
	FreeBusy, Ability        string
	VChat, Reminders         json.RawMessage
}

// Task stores one actionable item and its completion time.
type Task struct {
	Owner                       ResourceOwner
	ID, Summary, Description    string
	Created, Updated, Completed time.Time
}

type workState struct {
	events  map[string]CalendarEvent
	tasks   map[string]Task
	minutes map[string]config.Minute
	drafts  map[string]MailDraft
	mail    map[string]MailMessage
}

func newWork() workState {
	return workState{
		events: make(map[string]CalendarEvent), tasks: make(map[string]Task), minutes: make(map[string]config.Minute),
		drafts: make(map[string]MailDraft), mail: make(map[string]MailMessage),
	}
}

// CalendarID identifies the owner of the sandbox primary calendar.
func CalendarID(owner ResourceOwner) string { return "cal_" + cmp.Or(owner.UserOpenID, owner.AppID) }

// CreateCalendarEvent records an interval under the creator's calendar.
func (s *Store) CreateCalendarEvent(entry CalendarEvent) CalendarEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry.ID = protocol.NewID("evt_") + "_0"
	s.work.events[entry.ID] = cloneCalendarEvent(entry)
	return entry
}

// CalendarEvent reads an event visible to its calendar owner.
func (s *Store) CalendarEvent(owner ResourceOwner, id string) (CalendarEvent, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.work.events[id]
	if !ok || entry.Owner != owner {
		return CalendarEvent{}, false
	}
	return cloneCalendarEvent(entry), true
}

// CalendarEvents returns events overlapping the requested half-open interval.
func (s *Store) CalendarEvents(owner ResourceOwner, start, end time.Time) []CalendarEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]CalendarEvent, 0)
	for _, entry := range s.work.events {
		if entry.Owner == owner && entry.Start.Before(end) && entry.End.After(start) {
			result = append(result, cloneCalendarEvent(entry))
		}
	}
	slices.SortFunc(result, func(a, b CalendarEvent) int {
		if order := a.Start.Compare(b.Start); order != 0 {
			return order
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return result
}

func cloneCalendarEvent(entry CalendarEvent) CalendarEvent {
	entry.VChat, entry.Reminders = slices.Clone(entry.VChat), slices.Clone(entry.Reminders)
	return entry
}

// DeleteCalendarEvent removes one visible event from its calendar.
func (s *Store) DeleteCalendarEvent(owner ResourceOwner, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	event, ok := s.work.events[id]
	if !ok || event.Owner != owner {
		return ErrNotFound
	}
	delete(s.work.events, id)
	return nil
}

// CreateTask records an incomplete task for the requesting app or user.
func (s *Store) CreateTask(owner ResourceOwner, summary, description string) Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.Now()
	entry := Task{Owner: owner, ID: strings.ToLower(resourceUUID()), Summary: summary, Description: description, Created: now, Updated: now}
	s.work.tasks[entry.ID] = entry
	return entry
}

// Task reads an existing task by its GUID.
func (s *Store) Task(owner ResourceOwner, id string) (Task, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.work.tasks[id]
	if !ok || entry.Owner != owner {
		return Task{}, false
	}
	return entry, true
}

// UpdateTask applies selected updates together to an existing task.
func (s *Store) UpdateTask(owner ResourceOwner, id string, summary, description *string, completed *time.Time) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.work.tasks[id]
	if !ok || entry.Owner != owner {
		return Task{}, ErrNotFound
	}
	if summary != nil {
		entry.Summary = *summary
	}
	if description != nil {
		entry.Description = *description
	}
	if completed != nil {
		entry.Completed = *completed
	}
	entry.Updated = s.Now()
	s.work.tasks[id] = entry
	return entry, nil
}
