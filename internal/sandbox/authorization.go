package sandbox

import (
	"errors"
	"net/http"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

func (c *control) authorizedUsers(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("app_id")
	entries, err := c.state.AuthorizedUsers(id)
	if err != nil {
		stateError(w, err)
		return
	}
	type grant struct {
		OpenID string    `json:"open_id"`
		Scope  string    `json:"scope"`
		At     time.Time `json:"authorized_at"`
	}
	result := make([]grant, 0, len(entries))
	for _, entry := range entries {
		result = append(result, grant{entry.OpenID, entry.Scope, entry.At})
	}
	reply(w, http.StatusOK, data{"app_id": id, "items": result})
}

func (c *control) decideDevice(w http.ResponseWriter, r *http.Request) {
	var input struct {
		UserCode string `json:"user_code"`
		OpenID   string `json:"open_id"`
		Decision string `json:"decision"`
	}
	if !read(w, r, &input, false) {
		return
	}
	if input.UserCode == "" || (input.Decision != "approve" && input.Decision != "deny") {
		fail(w, http.StatusBadRequest, "user_code and a decision of approve or deny are required")
		return
	}
	c.mu.RLock()
	err := c.state.DecideDeviceFlow(input.UserCode, input.OpenID, input.Decision == "approve")
	c.mu.RUnlock()
	if errors.Is(err, store.ErrDeviceExpired) {
		fail(w, http.StatusNotFound, "device code unavailable")
		return
	}
	if err != nil {
		stateError(w, err)
		return
	}
	reply(w, http.StatusOK, data{"decision": input.Decision})
}
