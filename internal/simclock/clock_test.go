package simclock

import (
	"testing"
	"time"
)

func TestAdvancePastLastCardExpiryTimeLeavesClockUnchanged(t *testing.T) {
	start := time.Date(9999, time.December, 17, 23, 59, 58, 0, time.UTC)
	clock := New(start)
	if !ValidTime(start) {
		t.Fatal("the initial time must allow a full CardKit lifetime")
	}
	_, changed := clock.Snapshot()
	now, err := clock.Advance(time.Second)
	if err != nil || !now.Equal(start.Add(time.Second)) {
		t.Fatalf("valid advance failed: %s, %v", now, err)
	}
	select {
	case <-changed:
	default:
		t.Fatal("valid advance did not signal observers")
	}
	if _, err := clock.Advance(time.Second); err == nil || !clock.Now().Equal(now) {
		t.Fatal("accepted a time that cannot represent the resulting card expiry")
	}
}
