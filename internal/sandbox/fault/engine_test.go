package fault

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMatchingProbabilityAndRemainingHits(t *testing.T) {
	samples := 0
	engine := New(func() float64 {
		samples++
		if samples == 1 {
			return 0.75
		}
		return 0.25
	})
	hits, chance := 17, 0.5
	input := Match{AppID: "cli_test", Method: "GET", PathPrefix: "/open-apis/im/v1/messages", Channel: "inbound"}
	_, err := engine.Add(Spec{Kind: RateLimit, Match: Match{
		AppID: input.AppID, Method: "get", PathPrefix: "/open-apis/im/", Channel: input.Channel,
	}, RemainingHits: &hits, Probability: &chance})
	if err != nil {
		t.Fatal(err)
	}
	wrong := input
	wrong.Method = "POST"
	if engine.Match(wrong, false).Kind != "" ||
		engine.Match(input, false).Kind != "" || *engine.List()[0].RemainingHits != hits {
		t.Fatal("an unmatched request or probability miss consumed a hit")
	}
	var matched atomic.Int32
	var workers sync.WaitGroup
	for range 100 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 10 {
				if engine.Match(input, false).Kind == RateLimit {
					matched.Add(1)
				}
			}
		}()
	}
	workers.Wait()
	if matched.Load() != int32(hits) || len(engine.List()) != 0 || samples != hits+1 {
		t.Fatalf("budget = %d, matched %d in %d samples, rules: %+v", hits, matched.Load(), samples, engine.List())
	}
}

func TestLatencyCancellation(t *testing.T) {
	for _, cancelBy := range []string{"request", "reset", "stop"} {
		t.Run(cancelBy, func(t *testing.T) {
			engine := New(nil)
			delay := 60000
			_, err := engine.Add(Spec{Kind: Latency, DelayMS: &delay})
			if err != nil {
				t.Fatal(err)
			}
			effect := engine.Match(Match{Channel: "inbound"}, false)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- effect.Wait(ctx) }()
			switch cancelBy {
			case "request":
				cancel()
			case "reset":
				engine.Reset()
			case "stop":
				engine.Stop()
			}
			select {
			case err := <-done:
				if err != context.Canceled {
					t.Fatalf("latency returned %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("latency did not unblock")
			}
		})
	}
}
