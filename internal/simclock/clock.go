// Package simclock keeps sandbox state and retry scheduling on a shared test clock.
package simclock

import (
	"errors"
	"sync"
	"time"
)

// Manual is a frozen clock advanced explicitly by a sandbox client.
type Manual struct {
	mu         sync.Mutex
	start, now time.Time
	changed    chan struct{}
}

// New freezes the clock at start.
func New(start time.Time) *Manual {
	start = start.UTC()
	return &Manual{start: start, now: start, changed: make(chan struct{})}
}

// Now returns the current simulated time.
func (c *Manual) Now() time.Time {
	now, _ := c.Snapshot()
	return now
}

// Snapshot returns a time and a channel that closes on the next clock change.
func (c *Manual) Snapshot() (time.Time, <-chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now, c.changed
}

// Advance moves the clock forwards and wakes waiters.
func (c *Manual) Advance(by time.Duration) (time.Time, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := c.now.Add(by)
	if by <= 0 || next.Year() > 9999 {
		return c.now, errors.New("advance must be positive and keep the clock in the RFC3339 range")
	}
	c.set(next)
	return next, nil
}

// Reset restores the initial time and wakes waiters.
func (c *Manual) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.set(c.start)
}

func (c *Manual) set(now time.Time) {
	c.now = now
	close(c.changed)
	c.changed = make(chan struct{})
}
