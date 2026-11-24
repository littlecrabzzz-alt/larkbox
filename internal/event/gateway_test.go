package event

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
	"github.com/littlecrabzzz-alt/larkbox/internal/wsproto"
)

func testDispatcher(t *testing.T, cfg config.Config, opts Options, now func() time.Time) *Dispatcher {
	t.Helper()
	s := store.New(now, 0)
	s.Seed(cfg)
	d := New(s, opts)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := d.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return d
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func testGateway(t *testing.T, d *Dispatcher) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	d.RegisterRoutes(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func endpoint(t *testing.T, d *Dispatcher, host string, secure bool, body string) (int, larkws.EndpointResp) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "http://"+host+larkws.GenEndpointUri, strings.NewReader(body))
	if secure {
		r.TLS = &tls.ConnectionState{}
	}
	w := httptest.NewRecorder()
	d.bootstrap(w, r)
	var response larkws.EndpointResp
	requireNoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	return w.Code, response
}

func ticketURL(t *testing.T, d *Dispatcher, host string, app store.App) *url.URL {
	t.Helper()
	body, err := json.Marshal(larkws.BootstrapRequest{AppID: app.AppID, AppSecret: app.AppSecret})
	requireNoError(t, err)
	status, resp := endpoint(t, d, host, false, string(body))
	if status != http.StatusOK || resp.Code != 0 || resp.Data == nil || resp.Data.ClientConfig == nil || *resp.Data.ClientConfig != clientConfig {
		t.Fatalf("invalid endpoint: %+v", resp)
	}
	u, err := url.Parse(resp.Data.Url)
	requireNoError(t, err)
	return u
}

func connect(t *testing.T, u *url.URL, status int) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, u.String(), nil)
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if status == http.StatusSwitchingProtocols {
		requireNoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		requireNoError(t, conn.SetReadDeadline(time.Now().Add(4*time.Second)))
		return conn
	}
	if err == nil || resp == nil || resp.StatusCode != status || resp.Header.Get(larkws.HeaderHandshakeStatus) == "" {
		t.Fatalf("wanted HTTP %d WebSocket rejection, got %v, %v", status, resp, err)
	}
	return nil
}

func TestBootstrapCredentialsAndTickets(t *testing.T) {
	cfg := config.Default()
	app := cfg.Apps[0]
	var elapsed atomic.Int64
	d := testDispatcher(t, cfg, Options{TicketTTL: time.Second}, func() time.Time { return time.Unix(elapsed.Load(), 0) })
	s := testGateway(t, d)
	host := strings.TrimPrefix(s.URL, "http://")
	for _, tc := range []struct {
		name, body   string
		status, code int
	}{
		{"invalid JSON", `{`, 400, 9499},
		{"trailing JSON", `{"AppID":"x","AppSecret":"y"}{}`, 400, 9499},
		{"missing secret", `{"AppID":"cli_larkbox"}`, 400, 9499},
		{"empty secret", `{"AppID":"cli_larkbox","AppSecret":""}`, 200, 1000040344},
		{"wrong secret", `{"AppID":"cli_larkbox","AppSecret":"wrong"}`, 200, 1000040345},
		{"unknown app", `{"AppID":"cli_a1b2c3d4e5f67890","AppSecret":"wrong"}`, 200, 1000040345},
		{"invalid app format", `{"AppID":"wrong","AppSecret":"wrong"}`, 200, 1000040346},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, response := endpoint(t, d, host, false, tc.body)
			if status != tc.status || response.Code != tc.code || response.Data == nil || response.Data.Url != "" {
				t.Fatalf("unexpected response: HTTP %d %+v", status, response)
			}
		})
	}
	body, err := json.Marshal(larkws.BootstrapRequest{AppID: app.AppID, AppSecret: app.AppSecret})
	requireNoError(t, err)
	if status, got := endpoint(t, d, host, true, string(body)); status != 200 || got.Code != 0 || got.Data == nil || !strings.HasPrefix(got.Data.Url, "wss://"+host+socketPath+"?") {
		t.Fatalf("invalid TLS bootstrap: %+v", got)
	}
	expired := ticketURL(t, d, host, app)
	elapsed.Add(1)
	connect(t, expired, http.StatusUnauthorized)

	valid := ticketURL(t, d, host, app)
	for _, field := range []string{larkws.DeviceID, larkws.ServiceID, "ticket"} {
		wrong := *valid
		q := wrong.Query()
		q.Set(field, "wrong")
		wrong.RawQuery = q.Encode()
		status := http.StatusForbidden
		if field == "ticket" {
			status = http.StatusUnauthorized
		}
		connect(t, &wrong, status)
	}
	conn := connect(t, valid, http.StatusSwitchingProtocols)
	connect(t, valid, http.StatusUnauthorized)
	ping := larkws.NewPingFrame(serviceID)
	ping.SeqID = 77
	wire, err := ping.Marshal()
	requireNoError(t, err)
	requireNoError(t, conn.WriteMessage(websocket.BinaryMessage, []byte{0xff}))
	requireNoError(t, conn.WriteMessage(websocket.BinaryMessage, wire))
	kind, wire, err := conn.ReadMessage()
	requireNoError(t, err)
	pong, err := wsproto.Decode(wire)
	requireNoError(t, err)
	var opts larkws.ClientConfig
	requireNoError(t, json.Unmarshal(pong.Payload, &opts))
	if kind != websocket.BinaryMessage || pong.Method != wsproto.Control || pong.SeqID != ping.SeqID ||
		wsproto.HeaderValue(pong, wsproto.Type) != wsproto.Pong || opts != clientConfig {
		t.Fatalf("invalid pong: %+v", pong)
	}
}

func TestSocketAcknowledgementCorrelationAndTimeout(t *testing.T) {
	cfg := config.Default()
	app := cfg.Apps[0]
	d := testDispatcher(t, cfg, Options{AckTimeout: 250 * time.Millisecond}, nil)
	s := testGateway(t, d)
	conn := connect(t, ticketURL(t, d, strings.TrimPrefix(s.URL, "http://"), app), http.StatusSwitchingProtocols)
	type outcome struct {
		result Result
		err    error
	}
	got := make(chan outcome, 1)
	go func() {
		result, err := d.DispatchCardAction(t.Context(), app.AppID, map[string]string{"action": "click"})
		got <- outcome{result, err}
	}()
	_, wire, err := conn.ReadMessage()
	requireNoError(t, err)
	frame, err := wsproto.Decode(wire)
	requireNoError(t, err)
	ackFrame := frame
	ackFrame.Headers = []wsproto.Header{{Key: wsproto.MessageID, Value: "wrong-message-id"}}
	ackFrame.Payload = []byte(`{"code":500}`)
	wire, err = wsproto.Encode(&ackFrame)
	requireNoError(t, err)
	requireNoError(t, conn.WriteMessage(websocket.BinaryMessage, wire))

	frame.Payload = []byte(`{"code":200,"headers":{"x-result":"ok"},"data":"eyJ0b2FzdCI6e319"}`)
	wire, err = wsproto.Encode(&frame)
	requireNoError(t, err)
	requireNoError(t, conn.WriteMessage(websocket.BinaryMessage, wire))
	delivered := <-got
	requireNoError(t, delivered.err)
	if len(delivered.result.Deliveries) != 1 {
		t.Fatalf("got %+v", delivered.result)
	}
	response := delivered.result.Deliveries[0]
	if response.Status != 200 || response.Headers.Get("X-Result") != "ok" || string(response.Data) != `{"toast":{}}` {
		t.Fatalf("got %+v", response)
	}

	result, err := d.Dispatch(t.Context(), app.AppID, "test.timeout", struct{}{})
	if !errors.Is(err, context.DeadlineExceeded) || len(result.Deliveries) != 1 {
		t.Fatalf("expected acknowledgement timeout, got %+v, %v", result, err)
	}
	traces := d.state.Traces()
	if len(traces) != 2 || traces[0].EventID != delivered.result.EventID || traces[0].EventType != "card.action.trigger" ||
		traces[0].Channel != "websocket" || !bytes.Equal(traces[0].ResponseBody, frame.Payload) ||
		traces[1].EventID != result.EventID || traces[1].Error == "" || traces[1].Attempt != 1 {
		t.Fatalf("unexpected traces: %+v", traces)
	}
}

func TestConcurrentSocketLifecycle(t *testing.T) {
	cfg := config.Default()
	app := cfg.Apps[0]
	d := testDispatcher(t, cfg, Options{AckTimeout: 200 * time.Millisecond}, nil)
	s := testGateway(t, d)
	host := strings.TrimPrefix(s.URL, "http://")
	stable := connect(t, ticketURL(t, d, host, app), http.StatusSwitchingProtocols)
	readerDone := make(chan error, 1)
	go func() {
		defer close(readerDone)
		for {
			_, wire, err := stable.ReadMessage()
			if err != nil {
				readerDone <- err
				return
			}
			frame, err := wsproto.Decode(wire)
			if err != nil {
				t.Error(err)
				return
			}
			frame.Payload = []byte(`{"code":200}`)
			wire, err = wsproto.Encode(&frame)
			if err != nil {
				t.Error(err)
				return
			}
			if err := stable.WriteMessage(websocket.BinaryMessage, wire); err != nil {
				return
			}
		}
	}()
	var calls sync.WaitGroup
	for range 12 {
		u := ticketURL(t, d, host, app)
		calls.Add(2)
		go func() {
			defer calls.Done()
			c, resp, err := websocket.DefaultDialer.DialContext(t.Context(), u.String(), nil)
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			if err != nil {
				t.Error(err)
				return
			}
			_ = c.Close()
		}()
		go func() {
			defer calls.Done()
			result, _ := d.Dispatch(t.Context(), app.AppID, "test.concurrent", struct{}{})
			for _, delivery := range result.Deliveries {
				if delivery.Status == 200 {
					return
				}
			}
			t.Error("stable connection did not acknowledge dispatch")
		}()
	}
	calls.Wait()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	requireNoError(t, d.Shutdown(ctx))
	if d.SessionCount(app.AppID) != 0 {
		t.Fatal("shutdown left sessions registered")
	}
	select {
	case err := <-readerDone:
		if !websocket.IsCloseError(err, websocket.CloseGoingAway) {
			t.Errorf("expected a going-away close frame, got %v", err)
		}
	case <-ctx.Done():
		t.Fatal("shutdown left the WebSocket open")
	}
	if _, err := d.Dispatch(ctx, app.AppID, "test.stopped", struct{}{}); err == nil {
		t.Fatal("shutdown left delivery admission open")
	}
}

func TestConcurrentTicketReplay(t *testing.T) {
	cfg := config.Default()
	d := testDispatcher(t, cfg, Options{}, nil)
	s := testGateway(t, d)
	ticket := ticketURL(t, d, strings.TrimPrefix(s.URL, "http://"), cfg.Apps[0])
	statuses := make(chan int, 8)
	var contenders sync.WaitGroup
	for range cap(statuses) {
		contenders.Add(1)
		go func() {
			defer contenders.Done()
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			conn, resp, err := websocket.DefaultDialer.DialContext(ctx, ticket.String(), nil)
			if resp != nil {
				_ = resp.Body.Close()
				statuses <- resp.StatusCode
			} else {
				t.Errorf("missing handshake response: %v", err)
			}
			if conn != nil {
				_ = conn.Close()
			}
		}()
	}
	contenders.Wait()
	close(statuses)
	successes := 0
	for status := range statuses {
		switch status {
		case http.StatusSwitchingProtocols:
			successes++
		case http.StatusUnauthorized:
		default:
			t.Errorf("unexpected handshake HTTP status %d", status)
		}
	}
	if successes != 1 {
		t.Errorf("single-use ticket opened %d sessions", successes)
	}
}

func TestDispatchFanoutAndAppIsolation(t *testing.T) {
	cfg := config.Default()
	cfg.Apps[0].EncryptKey = ""
	captured := make(chan envelope, 1)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e envelope
		if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		captured <- e
		w.WriteHeader(http.StatusNoContent)
	}))
	defer receiver.Close()
	app := cfg.Apps[0]
	app.WebhookURL = receiver.URL
	other := app
	other.AppID, other.WebhookURL = "cli_other", ""
	cfg.Apps = []config.App{app, other}
	d := testDispatcher(t, cfg, Options{WebhookMaxAttempts: 1}, nil)
	s := testGateway(t, d)
	host := strings.TrimPrefix(s.URL, "http://")
	connect(t, ticketURL(t, d, host, other), http.StatusSwitchingProtocols)
	clients := []*websocket.Conn{
		connect(t, ticketURL(t, d, host, app), http.StatusSwitchingProtocols),
		connect(t, ticketURL(t, d, host, app), http.StatusSwitchingProtocols),
	}

	done := make(chan Result, 1)
	go func() {
		result, err := d.Dispatch(t.Context(), app.AppID, "test.fanout", map[string]string{"text": "hello"})
		if err != nil {
			t.Error(err)
		}
		done <- result
	}()
	var socketIDs []string
	for _, conn := range clients {
		_, wire, err := conn.ReadMessage()
		requireNoError(t, err)
		frame, err := wsproto.Decode(wire)
		requireNoError(t, err)
		var e envelope
		requireNoError(t, json.Unmarshal(frame.Payload, &e))
		if e.Header.AppID != app.AppID || e.Header.Type != "test.fanout" {
			t.Errorf("unexpected websocket event: %+v", e.Header)
		}
		socketIDs = append(socketIDs, e.Header.ID)
		frame.Payload = []byte(`{"code":204}`)
		wire, err = wsproto.Encode(&frame)
		requireNoError(t, err)
		requireNoError(t, conn.WriteMessage(websocket.BinaryMessage, wire))
	}
	result := <-done
	if len(result.Deliveries) != 3 {
		t.Fatalf("expected two sockets and one webhook, got %+v", result.Deliveries)
	}
	select {
	case e := <-captured:
		if e.Header.AppID != app.AppID || e.Header.ID != result.EventID {
			t.Errorf("unexpected webhook event: %+v", e.Header)
		}
	default:
		t.Fatal("webhook not called")
	}
	for _, id := range socketIDs {
		if id != result.EventID {
			t.Errorf("websocket saw event ID %q, want %q", id, result.EventID)
		}
	}
	traces := d.state.Traces()
	if len(traces) != 3 {
		t.Fatalf("expected three attempts, got %d", len(traces))
	}
	for _, trace := range traces {
		if trace.Status != 204 || trace.EventID != result.EventID || trace.AppID != app.AppID || trace.Attempt != 1 {
			t.Errorf("invalid fanout trace: %+v", trace)
		}
	}
}
