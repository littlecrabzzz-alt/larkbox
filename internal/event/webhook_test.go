package event

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

func TestWebhookPayloadsAndChallenge(t *testing.T) {
	for _, mode := range []string{"plaintext", "encrypted"} {
		t.Run(mode, func(t *testing.T) {
			cfg := config.Default()
			app := &cfg.Apps[0]
			app.TenantKey = "tenant-demo"
			if mode == "plaintext" {
				app.EncryptKey = ""
			}
			var challengeOK, eventOK atomic.Bool
			receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				mustRead := raw
				if app.EncryptKey != "" {
					stamp, nonce, sig := r.Header.Get("X-Lark-Request-Timestamp"), r.Header.Get("X-Lark-Request-Nonce"), r.Header.Get("X-Lark-Signature")
					if stamp == "" || nonce == "" || !protocol.VerifyWebhookSignature(sig, stamp, nonce, app.EncryptKey, raw) {
						t.Error("invalid encrypted webhook signature")
					}
					signedAt, err := strconv.ParseInt(stamp, 10, 64)
					if err != nil || time.Since(time.Unix(signedAt, 0)).Abs() > 5*time.Minute {
						t.Error("signed webhook timestamp must use wall time")
					}
					mustRead, err = protocol.DecryptWebhook(app.EncryptKey, raw)
					if err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
				} else if r.Header.Get("X-Lark-Signature") != "" {
					t.Error("unexpected plaintext signature")
				}

				var payload struct {
					Schema    string          `json:"schema"`
					Header    eventHeader     `json:"header"`
					Event     json.RawMessage `json:"event"`
					Type      string          `json:"type"`
					Challenge string          `json:"challenge"`
					Token     string          `json:"token"`
				}
				if err := json.Unmarshal(mustRead, &payload); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if payload.Type == "url_verification" {
					if payload.Challenge == "" || payload.Token != app.VerificationToken {
						t.Error("invalid URL verification payload")
					}
					challengeOK.Store(true)
					_ = json.NewEncoder(w).Encode(map[string]string{"challenge": payload.Challenge})
					return
				}
				if payload.Schema != "2.0" || payload.Header.ID == "" || payload.Header.Type != "test.event" ||
					payload.Header.AppID != app.AppID || payload.Header.TenantKey != app.TenantKey ||
					payload.Header.Token != app.VerificationToken || payload.Header.CreateTime != "1700000000999" ||
					string(payload.Event) != `{"key":"value"}` {
					t.Errorf("invalid event: %s", mustRead)
				}
				eventOK.Store(true)
				_, _ = w.Write([]byte("accepted"))
			}))
			defer receiver.Close()
			app.WebhookURL = receiver.URL + "/events"
			d := testDispatcher(t, cfg, Options{}, func() time.Time { return time.UnixMilli(1700000000999) })
			requireNoError(t, d.VerifyWebhook(t.Context(), app.AppID))
			result, err := d.Dispatch(t.Context(), app.AppID, "test.event", map[string]string{"key": "value"})
			requireNoError(t, err)
			if !challengeOK.Load() || !eventOK.Load() || len(result.Deliveries) != 1 || string(result.Deliveries[0].Data) != "accepted" {
				t.Fatalf("missing delivery: %+v", result)
			}
			traces := d.state.Traces()
			if len(traces) != 2 || traces[0].EventType != "url_verification" || traces[1].EventType != "test.event" ||
				traces[1].Channel != "webhook" || traces[1].Direction != store.Outbound || traces[1].AppID != app.AppID ||
				traces[1].EventID != result.EventID || traces[1].Status != 200 || traces[1].Latency <= 0 ||
				traces[1].Path != "/events" || len(traces[1].ResponseBody) != 0 {
				t.Fatalf("unexpected traces: %+v", traces)
			}
		})
	}
}

func TestWebhookRetriesAndAuditCaps(t *testing.T) {
	cfg := config.Default()
	app := &cfg.Apps[0]
	app.EncryptKey = ""
	var attempts atomic.Int32
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Error(err)
			return
		}
		switch attempts.Add(1) {
		case 1:
			http.Error(w, "retry", http.StatusInternalServerError)
		case 2:
			select {
			case <-r.Context().Done():
			case <-time.After(time.Second):
				t.Error("delivery timeout failed to cancel request")
			}
		default:
			_, _ = w.Write(bytes.Repeat([]byte{'a'}, store.TraceBodyLimit+1))
		}
	}))
	defer receiver.Close()
	app.WebhookURL = receiver.URL
	d := testDispatcher(t, cfg, Options{WebhookTimeout: 100 * time.Millisecond, RetryBackoff: func(int) time.Duration { return 0 }}, nil)
	payload := map[string]string{"text": strings.Repeat("x", store.TraceBodyLimit)}
	result, err := d.Dispatch(t.Context(), app.AppID, "test.retry", payload)
	requireNoError(t, err)
	if len(result.Deliveries) != 1 || len(result.Deliveries[0].Data) != store.TraceBodyLimit+1 {
		t.Fatalf("lost response data: %+v", result)
	}
	traces := d.state.Traces()
	if len(traces) != 3 {
		t.Fatalf("got %d traces", len(traces))
	}
	for index, trace := range traces {
		if trace.Attempt != index+1 || trace.EventID != result.EventID || trace.Status != []int{500, 0, 200}[index] ||
			!trace.RequestBodyTruncated || len(trace.RequestBody) != 0 {
			t.Fatalf("invalid trace %d: status %d, id %s, attempt %d", index, trace.Status, trace.EventID, trace.Attempt)
		}
	}
	if traces[0].Error == "" || !strings.Contains(traces[1].Error, "deadline") || traces[2].Error != "" ||
		!traces[2].ResponseBodyTruncated || len(traces[2].ResponseBody) != 0 {
		t.Fatal("audit lost failure details or did not cap response")
	}
}

func TestWebhookChallengeMismatchAndConnectFailure(t *testing.T) {
	cfg := config.Default()
	app := &cfg.Apps[0]
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"challenge":"wrong"}`))
	}))
	app.WebhookURL = receiver.URL
	d := testDispatcher(t, cfg, Options{WebhookMaxAttempts: 2, RetryBackoff: func(int) time.Duration { return 0 }}, nil)
	if err := d.VerifyWebhook(t.Context(), app.AppID); err == nil || err.Error() != "webhook challenge mismatch" {
		t.Fatalf("expected challenge mismatch, got %v", err)
	}
	receiver.Close()
	result, err := d.Dispatch(t.Context(), app.AppID, "test.connect", struct{}{})
	if err == nil {
		t.Fatal("connection failure was ignored")
	}
	traces := d.state.Traces()
	if len(traces) != 3 || traces[0].Status != 200 || traces[0].Error != "webhook challenge mismatch" ||
		traces[1].EventID != result.EventID || traces[2].EventID != result.EventID || traces[2].Attempt != 2 || traces[2].Error == "" {
		t.Fatalf("invalid failed webhook attempts: %+v", traces)
	}
}

func TestShutdownDrainsHookDeliveriesAndCancelsOnDeadline(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(map[bool]string{false: "drain", true: "cancel"}[force], func(t *testing.T) {
			cfg := config.Default()
			started, resume := make(chan struct{}), make(chan struct{})
			receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				close(started)
				select {
				case <-resume:
					w.WriteHeader(http.StatusNoContent)
				case <-r.Context().Done():
				}
			}))
			defer receiver.Close()
			cfg.Apps[0].WebhookURL = receiver.URL
			d := testDispatcher(t, cfg, Options{}, nil)
			d.EnqueueMessageRecalled(t.Context(), cfg.Apps[0].AppID, "oc_test", "om_test", "message_owner", d.state.Now())
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("hook delivery never started")
			}

			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if force {
				canceled, stop := context.WithCancel(ctx)
				stop()
				if err := d.Shutdown(canceled); !errors.Is(err, context.Canceled) {
					t.Fatalf("shutdown error = %v", err)
				}
			} else {
				drained := make(chan error, 1)
				go func() { drained <- d.Shutdown(ctx) }()
				<-d.stopping
				close(resume)
				requireNoError(t, <-drained)
			}
			requireNoError(t, d.Shutdown(ctx))
			traces := d.state.Traces()
			if len(traces) != 1 || (force && traces[0].Error == "") || (!force && traces[0].Status != http.StatusNoContent) {
				t.Fatalf("invalid shutdown traces: %+v", traces)
			}
		})
	}
}

func TestVerificationUsesItsOwnTimeoutWithoutRetries(t *testing.T) {
	cfg := config.Default()
	app := &cfg.Apps[0]
	app.EncryptKey = ""
	var challenges, events atomic.Int32
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Type      string `json:"type"`
			Challenge string `json:"challenge"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if body.Type == "url_verification" {
			challenges.Add(1)
		} else {
			events.Add(1)
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(150 * time.Millisecond):
		}
		if body.Type == "url_verification" {
			_ = json.NewEncoder(w).Encode(map[string]string{"challenge": body.Challenge})
		} else {
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer receiver.Close()
	app.WebhookURL = receiver.URL
	defaults := testDispatcher(t, cfg, Options{}, nil)
	if defaults.opts.VerificationTimeout != time.Second || defaults.opts.WebhookTimeout != 3*time.Second {
		t.Fatalf("unexpected timeout defaults: %v and %v", defaults.opts.VerificationTimeout, defaults.opts.WebhookTimeout)
	}
	d := testDispatcher(t, cfg, Options{VerificationTimeout: 50 * time.Millisecond, RetryBackoff: noBackoff}, nil)
	if err := d.VerifyWebhook(t.Context(), app.AppID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("slow challenge returned %v", err)
	}
	result, err := d.Dispatch(t.Context(), app.AppID, "test.event", struct{}{})
	requireNoError(t, err)
	if challenges.Load() != 1 || events.Load() != 1 || len(result.Deliveries) != 1 || result.Deliveries[0].Status != 204 {
		t.Fatalf("unexpected attempts: %d verification, %d events, %+v", challenges.Load(), events.Load(), result)
	}
}

func TestShutdownInterruptsWebhookBackoff(t *testing.T) {
	cfg := config.Default()
	var requests atomic.Int32
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer receiver.Close()
	cfg.Apps[0].WebhookURL = receiver.URL
	backoff := make(chan time.Duration, 1)
	d := testDispatcher(t, cfg, Options{RetryBackoff: func(attempt int) time.Duration {
		delay := defaultRetryBackoff(attempt)
		backoff <- delay
		return delay
	}}, nil)
	d.EnqueueMessageRecalled(t.Context(), cfg.Apps[0].AppID, "oc_test", "om_test", "message_owner", d.state.Now())
	select {
	case delay := <-backoff:
		if delay != 15*time.Second {
			t.Fatalf("expected the default backoff, got %v", delay)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("delivery did not enter retry backoff")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	requireNoError(t, d.Shutdown(ctx))
	if ctx.Err() != nil || requests.Load() != 1 || len(d.state.Traces()) != 1 {
		t.Fatal("shutdown waited for the backoff or started another attempt")
	}
}
