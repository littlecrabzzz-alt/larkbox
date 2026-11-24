package event

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/sandbox/fault"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

func TestWebhookDuplicateUsesSameEncryptedPayload(t *testing.T) {
	cfg := config.Default()
	payloads := make(chan []byte, 4)
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		payloads <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer web.Close()
	cfg.Apps[0].WebhookURL = web.URL + "/hook"
	state := store.New(nil, 0)
	state.Seed(cfg)
	engine := fault.New(nil)
	one := 1
	_, err := engine.Add(fault.Spec{Kind: fault.DuplicateEvent, RemainingHits: &one, Match: fault.Match{
		Channel: "webhook", EventType: "test.duplicate", PathPrefix: "/hook", Method: "POST",
	}})
	if err != nil {
		t.Fatal(err)
	}
	d := New(state, Options{Faults: engine, WebhookMaxAttempts: 1})
	defer func() {
		if err := d.Shutdown(t.Context()); err != nil {
			t.Error(err)
		}
	}()
	res, err := d.Dispatch(t.Context(), cfg.Apps[0].AppID, "test.duplicate", map[string]bool{"ok": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(payloads) != 2 {
		t.Fatalf("delivered %d copies", len(payloads))
	}
	first, second := <-payloads, <-payloads
	var wire struct {
		Encrypt string `json:"encrypt"`
	}
	if !bytes.Equal(first, second) || json.Unmarshal(first, &wire) != nil {
		t.Fatalf("wire payload differs: %s vs %s", first, second)
	}
	raw, err := protocol.DecryptWebhook(cfg.Apps[0].EncryptKey, first)
	if err != nil || !bytes.Contains(raw, []byte(res.EventID)) {
		t.Fatalf("missing event ID in encrypted event: %v", err)
	}
	_, err = d.Dispatch(t.Context(), cfg.Apps[0].AppID, "test.duplicate", nil)
	if err != nil || len(payloads) != 1 {
		t.Fatalf("expected one subsequent delivery: %v, %d copies", err, len(payloads))
	}
	traces := state.QueryTraces(store.TraceFilter{EventID: res.EventID, Channel: "webhook"})
	if len(traces) != 2 || traces[0].Copy != 0 || traces[1].Copy != 1 {
		t.Fatalf("unexpected duplicate traces: %+v", traces)
	}
}

func TestDispatcherResetCancelsOutboundLatency(t *testing.T) {
	cfg := config.Default()
	cfg.Apps[0].WebhookURL = "http://127.0.0.1:1"
	state := store.New(nil, 0)
	state.Seed(cfg)
	engine := fault.New(nil)
	one, delay := 1, 60000
	_, err := engine.Add(fault.Spec{Kind: fault.Latency, DelayMS: &delay, RemainingHits: &one, Match: fault.Match{Channel: "webhook"}})
	if err != nil {
		t.Fatal(err)
	}
	d := New(state, Options{Faults: engine})
	defer func() {
		if err := d.Shutdown(t.Context()); err != nil {
			t.Error(err)
		}
	}()
	errs := make(chan error, 1)
	go func() {
		_, err := d.Dispatch(t.Context(), cfg.Apps[0].AppID, "test.latency", nil)
		errs <- err
	}()
	timeout := time.After(2 * time.Second)
	for len(engine.List()) != 0 {
		select {
		case <-timeout:
			t.Fatal("delivery did not enter latency")
		case <-time.After(time.Millisecond):
		}
	}
	d.Reset(state.Reset)
	select {
	case err := <-errs:
		if err == nil || len(state.Traces()) != 0 {
			t.Fatalf("old delivery survived the reset: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("reset did not cancel outbound latency")
	}
}

func TestResetIgnoresNotificationsFromOldOpenAPIRequests(t *testing.T) {
	cfg := config.Default()
	cfg.Apps[0].WebhookURL = "http://127.0.0.1:1"
	d := testDispatcher(t, cfg, Options{}, nil)
	state := d.state
	before, _ := state.Admit(t.Context())
	d.Reset(state.Reset)
	d.EnqueueMessageRecalled(before, cfg.Apps[0].AppID, cfg.Chats[0].ChatID, "om_previous", "message_owner", state.Now())
	d.EnqueueBotMembership(before, cfg.Apps[0].AppID, cfg.Chats[0].ChatID, cfg.Users[0].OpenID, "", state.Now(), true)
	d.mu.Lock()
	pending := d.pending
	d.mu.Unlock()
	if pending != 0 || len(state.Traces()) != 0 {
		t.Fatalf("pre-reset notifications produced %d jobs or %d traces", pending, len(state.Traces()))
	}
}
