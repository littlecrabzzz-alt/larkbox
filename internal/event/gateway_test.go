package event

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/simclock"
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
	d := testDispatcher(t, cfg, Options{AckTimeout: 250 * time.Millisecond, WebSocketMaxAttempts: 1}, nil)
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
		traces[0].Channel != "websocket" || !bytes.Contains(traces[0].ResponseBody, []byte("eyJ0b2FzdCI6e319")) ||
		traces[1].EventID != result.EventID || traces[1].Error == "" || traces[1].Attempt != 1 {
		t.Fatalf("unexpected traces: %+v", traces)
	}

	// The timed-out frame is still queued on the receiving socket.
	readDataFrame(t, conn)
	go func() {
		result, err := d.Dispatch(t.Context(), app.AppID, "test.created", struct{}{})
		got <- outcome{result, err}
	}()
	created := readDataFrame(t, conn)
	writeAck(t, conn, created, wsproto.Response{StatusCode: http.StatusCreated})
	ordinary := <-got
	requireNoError(t, ordinary.err)
	if len(ordinary.result.Deliveries) != 1 || ordinary.result.Deliveries[0].Status != http.StatusCreated {
		t.Fatalf("ordinary event did not accept HTTP 201: %+v", ordinary)
	}
}

func TestConcurrentSocketLifecycle(t *testing.T) {
	cfg := config.Default()
	app := cfg.Apps[0]
	d := testDispatcher(t, cfg, Options{AckTimeout: 200 * time.Millisecond, WebSocketMaxAttempts: 1}, nil)
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
			if len(result.Deliveries) > 1 {
				t.Error("concurrent dispatch selected more than one recipient")
			}
		}()
	}
	calls.Wait()
	waitForSessions(t, d, app.AppID, 1)
	result, err := d.Dispatch(t.Context(), app.AppID, "test.stable", struct{}{})
	requireNoError(t, err)
	if len(result.Deliveries) != 1 || result.Deliveries[0].Status != 200 {
		t.Fatalf("stable connection did not acknowledge: %+v", result)
	}
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

func noBackoff(int) time.Duration { return 0 }

func readDataFrame(t *testing.T, conn *websocket.Conn) wsproto.Frame {
	t.Helper()
	kind, wire, err := conn.ReadMessage()
	requireNoError(t, err)
	frame, err := wsproto.Decode(wire)
	requireNoError(t, err)
	if kind != websocket.BinaryMessage || frame.Method != wsproto.Data {
		t.Fatalf("unexpected event frame: %+v", frame)
	}
	return frame
}

func writeAck(t *testing.T, conn *websocket.Conn, frame wsproto.Frame, response wsproto.Response) {
	t.Helper()
	var err error
	frame.Payload, err = json.Marshal(response)
	requireNoError(t, err)
	wire, err := wsproto.Encode(&frame)
	requireNoError(t, err)
	requireNoError(t, conn.WriteMessage(websocket.BinaryMessage, wire))
}

func dispatchAsync(t *testing.T, d *Dispatcher, appID string) <-chan Result {
	t.Helper()
	done := make(chan Result, 1)
	go func() {
		result, err := d.Dispatch(t.Context(), appID, "test.delivery", struct{}{})
		if err != nil {
			t.Error(err)
		}
		done <- result
	}()
	return done
}

func TestDispatchSelectsOneReplicaAndIsolatesApps(t *testing.T) {
	cfg := config.Default()
	cfg.Apps[0].EncryptKey = ""
	captured := make(chan envelope, 4)
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
	d := testDispatcher(t, cfg, Options{WebSocketMaxAttempts: 1, WebhookMaxAttempts: 1}, nil)
	s := testGateway(t, d)
	host := strings.TrimPrefix(s.URL, "http://")
	clients := []*websocket.Conn{
		connect(t, ticketURL(t, d, host, app), http.StatusSwitchingProtocols),
		connect(t, ticketURL(t, d, host, app), http.StatusSwitchingProtocols),
		connect(t, ticketURL(t, d, host, other), http.StatusSwitchingProtocols),
	}
	waitForSessions(t, d, app.AppID, 2)
	waitForSessions(t, d, other.AppID, 1)

	type observation struct {
		replica int
		event   envelope
	}
	var readers sync.WaitGroup
	observed := make(chan observation, 6)
	for i, conn := range clients {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				_, wire, err := conn.ReadMessage()
				if err != nil {
					return
				}
				frame, err := wsproto.Decode(wire)
				if err != nil {
					t.Error(err)
					return
				}
				var e envelope
				if err := json.Unmarshal(frame.Payload, &e); err != nil {
					t.Error(err)
					return
				}
				observed <- observation{i, e}
				frame.Payload = []byte(`{"code":204}`)
				wire, err = wsproto.Encode(&frame)
				if err != nil {
					t.Error(err)
					return
				}
				if err := conn.WriteMessage(websocket.BinaryMessage, wire); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	seen := map[int]bool{}
	for range 2 {
		result, err := d.Dispatch(t.Context(), app.AppID, "test.delivery", struct{}{})
		requireNoError(t, err)
		if len(result.Deliveries) != 2 || result.Deliveries[0].Channel != "websocket" || result.Deliveries[1].Channel != "webhook" {
			t.Fatalf("unexpected deliveries: %+v", result.Deliveries)
		}
		select {
		case got := <-observed:
			if got.replica == 2 || seen[got.replica] || got.event.Header.ID != result.EventID || got.event.Header.AppID != app.AppID {
				t.Fatalf("unexpected replica delivery: %+v", got)
			}
			seen[got.replica] = true
		default:
			t.Fatal("no WebSocket recipient")
		}
		select {
		case got := <-captured:
			if got.Header.AppID != app.AppID || got.Header.ID != result.EventID {
				t.Fatalf("unexpected webhook event: %+v", got.Header)
			}
		default:
			t.Fatal("webhook not called")
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	requireNoError(t, d.Shutdown(ctx))
	readers.Wait()
	if len(observed) != 0 || len(d.state.Traces()) != 4 {
		t.Fatal("event reached an extra recipient")
	}
}

func waitForSessions(t *testing.T, d *Dispatcher, app string, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for d.SessionCount(app) != want {
		if time.Now().After(deadline) {
			t.Fatalf("app %s has %d sessions, want %d", app, d.SessionCount(app), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSocketRetriesFailedACKAndTimeout(t *testing.T) {
	cfg := config.Default()
	app := cfg.Apps[0]
	d := testDispatcher(t, cfg, Options{AckTimeout: 250 * time.Millisecond, WebSocketMaxAttempts: 3, RetryBackoff: noBackoff}, nil)
	srv := testGateway(t, d)
	host := strings.TrimPrefix(srv.URL, "http://")
	conn := connect(t, ticketURL(t, d, host, app), http.StatusSwitchingProtocols)
	waitForSessions(t, d, app.AppID, 1)
	replica := connect(t, ticketURL(t, d, host, app), http.StatusSwitchingProtocols)
	waitForSessions(t, d, app.AppID, 2)
	done := dispatchAsync(t, d, app.AppID)
	first := readDataFrame(t, conn)
	writeAck(t, conn, first, wsproto.Response{StatusCode: 500})
	second := readDataFrame(t, conn) // Leave this attempt unacknowledged.
	third := readDataFrame(t, conn)
	writeAck(t, conn, second, wsproto.Response{StatusCode: 500}) // A late ACK cannot fail the third attempt.
	writeAck(t, conn, third, wsproto.Response{StatusCode: 200})
	result := <-done
	if len(result.Deliveries) != 1 || result.Deliveries[0].Status != 200 {
		t.Fatalf("unexpected delivery: %+v", result)
	}
	ids := map[string]bool{}
	for _, frame := range []wsproto.Frame{first, second, third} {
		id := wsproto.HeaderValue(frame, wsproto.MessageID)
		if id == "" || ids[id] || wsproto.HeaderValue(frame, wsproto.TraceID) != result.EventID || !bytes.Equal(frame.Payload, first.Payload) {
			t.Fatal("retry changed event data or reused an ACK ID")
		}
		ids[id] = true
	}
	traces := d.state.Traces()
	if len(traces) != 3 {
		t.Fatalf("unexpected attempts: %+v", traces)
	}
	for i, trace := range traces {
		if trace.Attempt != i+1 || trace.EventID != result.EventID || trace.Status != []int{500, 0, 200}[i] ||
			(trace.Error == "") != (i == 2) {
			t.Fatalf("invalid attempt: %+v", trace)
		}
	}
	if !strings.Contains(traces[1].Error, "deadline") {
		t.Fatal("timeout was not audited")
	}
	next := dispatchAsync(t, d, app.AppID)
	nextFrame := readDataFrame(t, replica)
	if wsproto.HeaderValue(nextFrame, wsproto.TraceID) == result.EventID {
		t.Fatal("a second replica received a retry for the same event")
	}
	writeAck(t, replica, nextFrame, wsproto.Response{StatusCode: 200})
	<-next
}

func TestOversizedACKHeadersAreSanitizedOnlyInAudit(t *testing.T) {
	cfg := config.Default()
	app := cfg.Apps[0]
	d := testDispatcher(t, cfg, Options{}, nil)
	srv := testGateway(t, d)
	conn := connect(t, ticketURL(t, d, strings.TrimPrefix(srv.URL, "http://"), app), http.StatusSwitchingProtocols)
	done := dispatchAsync(t, d, app.AppID)
	frame := readDataFrame(t, conn)
	long := strings.Repeat("x", store.TraceHeaderValueLimit*2)
	headers := map[string]string{"Authorization": "private credential", "Set-Cookie": "private cookie", "X-Request-Id": long}
	for i := range 100 {
		headers[fmt.Sprintf("X-Extra-%d", i)] = long
	}
	writeAck(t, conn, frame, wsproto.Response{StatusCode: 200, Headers: headers, Data: []byte("unchanged")})
	result := <-done
	if len(result.Deliveries) != 1 {
		t.Fatalf("unexpected delivery: %+v", result)
	}
	delivery := result.Deliveries[0]
	if len(delivery.Headers) != len(headers) || delivery.Headers.Get("Authorization") != headers["Authorization"] ||
		delivery.Headers.Get("X-Request-Id") != long || string(delivery.Data) != "unchanged" {
		t.Fatal("audit modified the caller's ACK")
	}
	trace := d.state.Traces()[0]
	if len(trace.ResponseHeaders) != 1 || trace.ResponseHeaders.Get("X-Request-Id") != long[:store.TraceHeaderValueLimit] ||
		bytes.Contains(trace.ResponseBody, []byte("private")) || len(trace.ResponseBody) > store.TraceHeaderLimit {
		t.Fatalf("audit retained sensitive or oversized ACK headers: %+v", trace.ResponseHeaders)
	}
}

type gatedUpgradeWriter struct {
	http.ResponseWriter
	release <-chan struct{}
}

func (w gatedUpgradeWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	return &gatedUpgradeConn{Conn: conn, release: w.release}, rw, nil
}

type gatedUpgradeConn struct {
	net.Conn
	first   sync.Once
	release <-chan struct{}
}

func (c *gatedUpgradeConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.first.Do(func() { <-c.release })
	return n, err
}

func TestUpgradeReservationAndShutdown(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(map[bool]string{false: "first delivery", true: "shutdown"}[shutdown], func(t *testing.T) {
			cfg := config.Default()
			app := cfg.Apps[0]
			other := app
			other.AppID = "cli_other"
			cfg.Apps = append(cfg.Apps, other)
			d := testDispatcher(t, cfg, Options{MaxConnectionsPerApp: 1}, nil)
			release := make(chan struct{})
			resume := sync.OnceFunc(func() { close(release) })
			defer time.AfterFunc(3*time.Second, resume).Stop()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("gate") == "1" {
					w = gatedUpgradeWriter{w, release}
				}
				d.upgrade(w, r)
			}))
			t.Cleanup(srv.Close)
			t.Cleanup(resume)
			host := strings.TrimPrefix(srv.URL, "http://")
			ticket := ticketURL(t, d, host, app)
			ticket.RawQuery += "&gate=1"
			conn := connect(t, ticket, http.StatusSwitchingProtocols)
			if d.SessionCount(app.AppID) != 0 {
				t.Fatal("session was registered before upgrade completed")
			}

			limitTicket := ticketURL(t, d, host, app)
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			c, resp, err := websocket.DefaultDialer.DialContext(ctx, limitTicket.String(), nil)
			if c != nil {
				_ = c.Close()
			}
			if resp != nil {
				_ = resp.Body.Close()
			}
			if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden ||
				resp.Header.Get(larkws.HeaderHandshakeAuthErrCode) != strconv.Itoa(larkws.ExceedConnLimit) {
				t.Fatalf("reservation did not count toward app limit: %v, %v", resp, err)
			}
			connect(t, ticketURL(t, d, host, other), http.StatusSwitchingProtocols)
			if shutdown {
				done := make(chan error, 1)
				go func() { done <- d.Shutdown(ctx) }()
				<-d.stopping
				select {
				case err := <-done:
					t.Fatalf("shutdown returned before the upgrade finished: %v", err)
				default:
				}
				resume()
				requireNoError(t, <-done)
				if d.SessionCount(app.AppID) != 0 {
					t.Fatal("shutdown registered a pending session")
				}
				_, _, err := conn.ReadMessage()
				if !websocket.IsCloseError(err, websocket.CloseGoingAway) {
					t.Fatalf("pending upgrade did not close cleanly: %v", err)
				}
				return
			}
			done := dispatchAsync(t, d, app.AppID)
			select {
			case <-done:
				t.Fatal("event was lost before the session registered")
			case <-time.After(25 * time.Millisecond):
			}
			resume()
			frame := readDataFrame(t, conn)
			writeAck(t, conn, frame, wsproto.Response{StatusCode: 200})
			if result := <-done; len(result.Deliveries) != 1 || result.Deliveries[0].Status != 200 {
				t.Fatalf("first event failed: %+v", result)
			}
			connect(t, limitTicket, http.StatusForbidden)
			_ = conn.Close()
			waitForSessions(t, d, app.AppID, 0)
			connect(t, limitTicket, http.StatusSwitchingProtocols)
		})
	}
}

func TestShutdownInterruptsWebSocketBackoff(t *testing.T) {
	cfg := config.Default()
	app := cfg.Apps[0]
	backoff := make(chan time.Duration, 1)
	d := testDispatcher(t, cfg, Options{RetryBackoff: func(attempt int) time.Duration {
		delay := defaultRetryBackoff(attempt)
		backoff <- delay
		return delay
	}}, nil)
	srv := testGateway(t, d)
	conn := connect(t, ticketURL(t, d, strings.TrimPrefix(srv.URL, "http://"), app), http.StatusSwitchingProtocols)
	waitForSessions(t, d, app.AppID, 1)
	d.EnqueueMessageRecalled(t.Context(), app.AppID, "oc_test", "om_test", "message_owner", d.state.Now())
	frame := readDataFrame(t, conn)
	writeAck(t, conn, frame, wsproto.Response{StatusCode: 500})
	select {
	case delay := <-backoff:
		if delay != 15*time.Second {
			t.Fatalf("expected the default backoff, got %v", delay)
		}
	case <-time.After(time.Second):
		t.Fatal("delivery did not enter retry backoff")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	requireNoError(t, d.Shutdown(ctx))
	if ctx.Err() != nil || len(d.state.Traces()) != 1 {
		t.Fatal("shutdown waited for the backoff or started another attempt")
	}
}

func TestFirstSocketWriteFailsOverIfSelectedReplicaDisconnects(t *testing.T) {
	for _, hasReplica := range []bool{false, true} {
		t.Run(map[bool]string{false: "last replica", true: "another replica"}[hasReplica], func(t *testing.T) {
			cfg := config.Default()
			app := cfg.Apps[0]
			d := testDispatcher(t, cfg, Options{WebSocketMaxAttempts: 1, AckTimeout: 2 * time.Second}, nil)
			srv := testGateway(t, d)
			host := strings.TrimPrefix(srv.URL, "http://")
			conn := connect(t, ticketURL(t, d, host, app), http.StatusSwitchingProtocols)
			waitForSessions(t, d, app.AppID, 1)
			d.mu.Lock()
			selected := d.ws.apps[app.AppID].sessions[0]
			d.mu.Unlock()
			var other *websocket.Conn
			if hasReplica {
				other = connect(t, ticketURL(t, d, host, app), http.StatusSwitchingProtocols)
				waitForSessions(t, d, app.AppID, 2)
			}

			// Hold the selected replica's writer until dispatch has selected
			// it and is waiting to write its first frame.
			selected.writer.Lock()
			release := sync.OnceFunc(selected.writer.Unlock)
			defer release()
			type outcome struct {
				result Result
				err    error
			}
			finished := make(chan outcome, 1)
			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
			defer cancel()
			go func() {
				result, err := d.Dispatch(ctx, app.AppID, "test.disconnect_before_write", struct{}{})
				finished <- outcome{result, err}
			}()
			waitForPendingACK(t, selected)
			requireNoError(t, conn.Close())
			select {
			case <-selected.done:
			case <-ctx.Done():
				t.Fatal("selected replica did not disconnect")
			}
			release()

			if hasReplica {
				frame := readDataFrame(t, other)
				writeAck(t, other, frame, wsproto.Response{StatusCode: http.StatusOK})
			}
			got := <-finished
			if len(got.result.Deliveries) != 1 || got.result.Deliveries[0].Channel != "websocket" {
				t.Fatalf("expected an explicit websocket outcome, got %+v", got.result)
			}
			traces := d.state.Traces()
			if len(traces) == 0 || traces[0].Attempt != 1 || traces[0].Status != 0 ||
				traces[0].Error != errDisconnectedBeforeWrite.Error() || traces[0].EventID != got.result.EventID {
				t.Fatalf("first disconnect was not audited: %+v", traces)
			}
			if hasReplica {
				requireNoError(t, got.err)
				if got.result.Deliveries[0].Status != http.StatusOK || len(traces) != 2 ||
					traces[1].Attempt != 2 || traces[1].Status != http.StatusOK || traces[1].EventID != got.result.EventID {
					t.Fatalf("replacement delivery was not audited: %+v", traces)
				}
			} else if !errors.Is(got.err, errDisconnectedBeforeWrite) || len(traces) != 1 ||
				!errors.Is(got.result.Deliveries[0].Err, errDisconnectedBeforeWrite) {
				t.Fatalf("last-replica disconnect returned %+v, %v", got.result, got.err)
			}
		})
	}
}

func waitForPendingACK(t *testing.T, s *session) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		count := len(s.pending)
		s.mu.Unlock()
		if count > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("dispatch did not reach the selected replica")
}

func TestTicketExpirySampledWhileHoldingReservationLock(t *testing.T) {
	cfg := config.Default()
	var d *Dispatcher
	now, expired := time.Unix(1, 0), false
	d = testDispatcher(t, cfg, Options{TicketTTL: time.Second}, func() time.Time {
		if expired {
			if d.mu.TryLock() {
				d.mu.Unlock()
				t.Error("expiry sampled outside the reservation lock")
				return now
			}
			return now.Add(time.Second)
		}
		return now
	})
	ticket := ticketURL(t, d, "127.0.0.1", cfg.Apps[0])
	expired = true
	_, rejection := d.reserveSession(ticket.Query())
	if rejection == nil {
		d.ws.connections.Done() // Release the unexpected reservation.
		t.Fatal("expired ticket was accepted")
	}
	if rejection.status != http.StatusUnauthorized {
		t.Fatalf("unexpected rejection: %+v", rejection)
	}
}

func TestSocketRetryUsesManualClockAndResetCancelsIt(t *testing.T) {
	cfg := config.Default()
	app := cfg.Apps[0]
	clock := simclock.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	d := testDispatcher(t, cfg, Options{
		ManualClock: clock, WebSocketMaxAttempts: 2,
		RetryBackoff: func(int) time.Duration { return time.Hour },
	}, clock.Now)
	srv := testGateway(t, d)
	host := strings.TrimPrefix(srv.URL, "http://")
	conn := connect(t, ticketURL(t, d, host, app), http.StatusSwitchingProtocols)
	waitForSessions(t, d, app.AppID, 1)

	done := dispatchAsync(t, d, app.AppID)
	first := readDataFrame(t, conn)
	writeAck(t, conn, first, wsproto.Response{StatusCode: 500})
	waitForAttempts := func(want int) {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for len(d.state.Traces()) < want {
			if time.Now().After(deadline) {
				t.Fatal("acknowledgement did not reach the trace ring")
			}
			time.Sleep(time.Millisecond)
		}
	}
	waitForAttempts(1)
	_, err := clock.Advance(time.Hour - time.Second)
	requireNoError(t, err)
	if len(d.state.Traces()) != 1 {
		t.Fatal("retry ran before its virtual deadline")
	}
	_, err = clock.Advance(time.Second)
	requireNoError(t, err)
	retry := readDataFrame(t, conn)
	if wsproto.HeaderValue(first, wsproto.TraceID) != wsproto.HeaderValue(retry, wsproto.TraceID) ||
		wsproto.HeaderValue(first, wsproto.MessageID) == wsproto.HeaderValue(retry, wsproto.MessageID) {
		t.Fatal("retry must retain the event ID and use a fresh acknowledgement ID")
	}
	writeAck(t, conn, retry, wsproto.Response{StatusCode: 200})
	if result := <-done; len(result.Deliveries) != 1 || result.Deliveries[0].Status != 200 {
		t.Fatalf("retry did not succeed: %+v", result)
	}

	pending := make(chan error, 1)
	go func() {
		_, err := d.Dispatch(t.Context(), app.AppID, "im.message.receive_v1", map[string]string{"text": "pending"})
		pending <- err
	}()
	writeAck(t, conn, readDataFrame(t, conn), wsproto.Response{StatusCode: 500})
	waitForAttempts(3)
	d.Reset(func() {
		clock.Reset()
		d.state.Reset()
	})
	select {
	case err := <-pending:
		if err == nil {
			t.Fatal("reset unexpectedly completed an unacknowledged event")
		}
	case <-time.After(time.Second):
		t.Fatal("reset did not release an event waiting on the clock")
	}
	if len(d.state.Traces()) != 0 {
		t.Fatal("old retry traces survived reset")
	}
}

func TestSocketRetryRestoresOnReconnectedPeer(t *testing.T) {
	cfg := config.Default()
	app := cfg.Apps[0]
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := simclock.New(at)
	d := testDispatcher(t, cfg, Options{
		ManualClock: clock, WebSocketMaxAttempts: 2,
		RetryBackoff: func(int) time.Duration { return time.Minute },
	}, clock.Now)
	host := strings.TrimPrefix(testGateway(t, d).URL, "http://")
	initialPeer := connect(t, ticketURL(t, d, host, app), http.StatusSwitchingProtocols)
	waitForSessions(t, d, app.AppID, 1)
	d.mu.Lock()
	selected := d.ws.apps[app.AppID].sessions[0]
	d.mu.Unlock()
	peer := connect(t, ticketURL(t, d, host, app), http.StatusSwitchingProtocols)
	waitForSessions(t, d, app.AppID, 2)
	selected.writer.Lock()
	release := sync.OnceFunc(selected.writer.Unlock)
	defer release()
	d.Enqueue(t.Context(), app.AppID, "im.message.receive_v1", at, map[string]string{"text": "pending"})
	waitForPendingACK(t, selected)
	requireNoError(t, initialPeer.Close())
	select {
	case <-selected.done:
	case <-time.After(time.Second):
		t.Fatal("initial replica did not disconnect")
	}
	release()
	first := readDataFrame(t, peer)
	writeAck(t, peer, first, wsproto.Response{StatusCode: 500})
	deadline := time.Now().Add(time.Second)
	for d.Metrics().RetryWaiters == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if d.Metrics().RetryWaiters != 1 {
		t.Fatal("retry did not enter the wait queue")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	requireNoError(t, d.Shutdown(ctx))
	var pending []PendingRetry
	requireNoError(t, d.ExportCheckpoint(func(retries []PendingRetry) error {
		pending = retries
		return nil
	}))
	if len(pending) != 1 || pending[0].Channel != "websocket" || pending[0].Attempt != 3 ||
		pending[0].AttemptsUsed != 1 || !pending[0].Due.Equal(at.Add(time.Minute)) {
		t.Fatalf("graceful checkpoint lost the pending WebSocket attempt: %+v", pending)
	}

	restoredClock := simclock.New(at)
	restored := testDispatcher(t, cfg, Options{ManualClock: restoredClock, WebSocketMaxAttempts: 2}, restoredClock.Now)
	restored.RestoreCheckpoint(func() {}, pending)
	_, err := restoredClock.Advance(time.Minute)
	requireNoError(t, err)
	host = strings.TrimPrefix(testGateway(t, restored).URL, "http://")
	peer = connect(t, ticketURL(t, restored, host, app), http.StatusSwitchingProtocols)
	retry := readDataFrame(t, peer)
	if wsproto.HeaderValue(first, wsproto.TraceID) != wsproto.HeaderValue(retry, wsproto.TraceID) ||
		wsproto.HeaderValue(first, wsproto.MessageID) == wsproto.HeaderValue(retry, wsproto.MessageID) {
		t.Fatal("restored retry lost its event ID or reused its acknowledgement ID")
	}
	writeAck(t, peer, retry, wsproto.Response{StatusCode: 200})
	deadline = time.Now().Add(time.Second)
	for restored.Metrics().Pending != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	trace := restored.state.Traces()
	if len(trace) != 1 || trace[0].Attempt != 3 || trace[0].Status != 200 || trace[0].EventID != pending[0].EventID {
		t.Fatalf("restored WebSocket delivery did not acknowledge at attempt 3: %+v", trace)
	}
	requireNoError(t, restored.ExportCheckpoint(func(retries []PendingRetry) error {
		if len(retries) != 0 {
			t.Errorf("acknowledged event retained in the retry snapshot: %+v", retries)
		}
		return nil
	}))
}
