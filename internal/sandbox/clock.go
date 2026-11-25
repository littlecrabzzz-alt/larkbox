package sandbox

import (
	"net/http"
	"time"
)

func (c *control) clockStatus(w http.ResponseWriter, _ *http.Request) {
	c.mu.RLock()
	now := c.state.Now()
	c.mu.RUnlock()
	reply(w, http.StatusOK, data{"now": now.UTC(), "manual": c.clock != nil})
}

func (c *control) advanceClock(w http.ResponseWriter, r *http.Request) {
	var input struct {
		By string `json:"by"`
	}
	if !read(w, r, &input, false) {
		return
	}
	by, err := time.ParseDuration(input.By)
	if err != nil || by <= 0 {
		fail(w, http.StatusBadRequest, "by must be a positive duration such as 15s")
		return
	}
	if c.clock == nil {
		fail(w, http.StatusConflict, "set clock_start to enable manual time advancement")
		return
	}
	c.mu.Lock()
	now, err := c.clock.Advance(by)
	c.mu.Unlock()
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	reply(w, http.StatusOK, data{"now": now, "manual": true})
}
