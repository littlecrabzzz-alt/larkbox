package sandbox

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/littlecrabzzz-alt/larkbox/internal/event"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

func (c *control) injectEvent(w http.ResponseWriter, r *http.Request) {
	var input struct {
		AppID     string          `json:"app_id"`
		EventType string          `json:"event_type"`
		Event     json.RawMessage `json:"event"`
	}
	if !read(w, r, &input, false) {
		return
	}
	payload := bytes.TrimSpace(input.Event)
	if input.AppID == "" || input.EventType == "" || strings.ContainsAny(input.EventType, " \t\r\n") ||
		len(payload) == 0 || payload[0] != '{' {
		fail(w, http.StatusBadRequest, "app_id, event_type, and a JSON event object are required")
		return
	}
	c.mu.RLock()
	ctx := c.events.Scope(r.Context())
	_, exists := c.state.Apps().Get(input.AppID)
	c.mu.RUnlock()
	if !exists {
		stateError(w, store.ErrNotFound)
		return
	}
	result, err := c.events.Dispatch(ctx, input.AppID, input.EventType, input.Event)
	if err != nil && result.EventID == "" {
		stateError(w, err)
		return
	}
	reply(w, http.StatusOK, eventReceipt(result, err))
}

func eventReceipt(result event.Result, err error) data {
	outcomes := make([]data, 0, len(result.Deliveries))
	for _, delivery := range result.Deliveries {
		outcomes = append(outcomes, deliveryData(delivery))
	}
	return data{"event_id": result.EventID, "deliveries": outcomes, "error": errorText(err)}
}
