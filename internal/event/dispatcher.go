// Package event delivers Feishu Schema 2.0 events over WebSocket and HTTP.
package event

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
	"github.com/littlecrabzzz-alt/larkbox/internal/wsproto"
)

const (
	maxPendingEvents = 128
	defaultTimeout   = 3 * time.Second
)

// Options controls event delivery timeouts and retries. Zero values use defaults.
type Options struct {
	WebhookTimeout     time.Duration
	WebhookMaxAttempts int
	// RetryBackoff receives the failed attempt number, starting at 1.
	RetryBackoff func(int) time.Duration
	AckTimeout   time.Duration
	TicketTTL    time.Duration
}

// Delivery contains the outcome for one receiver. Data is the HTTP response
// body or the decoded WebSocket response data.
type Delivery struct {
	Channel string
	Status  int
	Headers http.Header
	Data    []byte
	Err     error
}

// Result identifies an event and contains the final outcome for each receiver.
type Result struct {
	EventID    string
	Deliveries []Delivery
}

type envelope struct {
	Schema string      `json:"schema"`
	Header eventHeader `json:"header"`
	Event  any         `json:"event"`
}

type eventHeader struct {
	ID         string `json:"event_id"`
	Type       string `json:"event_type"`
	CreateTime string `json:"create_time"`
	Token      string `json:"token"`
	AppID      string `json:"app_id"`
	TenantKey  string `json:"tenant_key"`
}

type deliveryJob struct {
	app  store.App
	id   string
	kind string
	body []byte
}

// Dispatcher manages event delivery and long connections for one server.
type Dispatcher struct {
	state  *store.Store
	opts   Options
	client *http.Client
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	mu      sync.Mutex
	closed  bool
	pending int
	active  sync.WaitGroup
	ws      gateway
}

// New creates a dispatcher. Call Shutdown to drain deliveries and close sockets.
func New(state *store.Store, opts Options) *Dispatcher {
	if opts.WebhookTimeout <= 0 {
		opts.WebhookTimeout = defaultTimeout
	}
	if opts.WebhookMaxAttempts <= 0 {
		opts.WebhookMaxAttempts = 5
	}
	if opts.AckTimeout <= 0 {
		opts.AckTimeout = defaultTimeout
	}
	if opts.TicketTTL <= 0 {
		opts.TicketTTL = 30 * time.Second
	}
	if opts.RetryBackoff == nil {
		opts.RetryBackoff = defaultRetryBackoff
	}
	ctx, cancel := context.WithCancel(context.Background())
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxResponseHeaderBytes = store.TraceBodyLimit
	return &Dispatcher{
		state: state, opts: opts, ctx: ctx, cancel: cancel, done: make(chan struct{}),
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		ws: gateway{
			tickets: make(map[string]ticket), sessions: make(map[string]map[*session]struct{}),
			upgrader: websocket.Upgrader{HandshakeTimeout: time.Second},
		},
	}
}

func defaultRetryBackoff(attempt int) time.Duration {
	delays := [...]time.Duration{15 * time.Second, 5 * time.Minute, time.Hour, 6 * time.Hour}
	return delays[min(attempt, len(delays))-1]
}

// Shutdown rejects new work, drains admitted deliveries, and closes long
// connections. If ctx expires, it cancels outstanding network operations.
func (d *Dispatcher) Shutdown(ctx context.Context) error {
	d.mu.Lock()
	if !d.closed {
		d.closed = true
		clear(d.ws.tickets)
		go d.finish()
	}
	d.mu.Unlock()

	select {
	case <-d.done:
		return nil
	case <-ctx.Done():
		d.cancel()
		d.closeSessions(false)
		return ctx.Err()
	}
}

func (d *Dispatcher) finish() {
	d.active.Wait()
	d.cancel()
	d.closeSessions(true)
	d.ws.readers.Wait()
	d.client.CloseIdleConnections()
	close(d.done)
}

func (d *Dispatcher) closeSessions(graceful bool) {
	d.mu.Lock()
	sessions := make([]*session, 0, d.ws.running)
	for _, appSessions := range d.ws.sessions {
		for s := range appSessions {
			sessions = append(sessions, s)
		}
	}
	d.mu.Unlock()

	deadline := time.Now().Add(100 * time.Millisecond)
	closeFrame := websocket.FormatCloseMessage(websocket.CloseGoingAway, "server shutting down")
	for _, s := range sessions {
		if graceful {
			_ = s.conn.WriteControl(websocket.CloseMessage, closeFrame, deadline)
		}
		_ = s.conn.Close()
	}
}

func (d *Dispatcher) begin(ctx context.Context) (context.Context, func(), error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil, nil, errors.New("event dispatcher is shutting down")
	}
	d.active.Add(1)
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(d.ctx, cancel)
	return ctx, func() {
		stop()
		cancel()
		d.active.Done()
	}, nil
}

func (d *Dispatcher) newJob(appID, kind string, at time.Time, payload any) (deliveryJob, error) {
	app, ok := d.state.Apps().Get(appID)
	if !ok {
		return deliveryJob{}, fmt.Errorf("unknown event app %q", appID)
	}
	if kind == "" {
		return deliveryJob{}, errors.New("event type is required")
	}
	if at.IsZero() {
		at = d.state.Now()
	}
	id := protocol.NewID(protocol.EventID)
	body, err := json.Marshal(envelope{
		Schema: "2.0",
		Header: eventHeader{
			ID: id, Type: kind, CreateTime: millis(at),
			AppID: appID, Token: app.VerificationToken, TenantKey: tenantKey(app),
		},
		Event: payload,
	})
	if err != nil {
		return deliveryJob{}, err
	}
	if len(body) > maxFrame {
		return deliveryJob{}, errors.New("event payload exceeds size limit")
	}
	return deliveryJob{app: app, id: id, kind: kind, body: body}, nil
}

func tenantKey(app store.App) string {
	if app.TenantKey != "" {
		return app.TenantKey
	}
	return "larkbox"
}

func millis(at time.Time) string { return strconv.FormatInt(at.UnixMilli(), 10) }

// Dispatch delivers a Schema 2.0 event to every active session for the app
// and to its configured webhook. It waits for each receiver's outcome.
func (d *Dispatcher) Dispatch(ctx context.Context, appID, kind string, payload any) (Result, error) {
	job, err := d.newJob(appID, kind, time.Time{}, payload)
	if err != nil {
		return Result{}, err
	}
	ctx, finish, err := d.begin(ctx)
	if err != nil {
		return Result{}, err
	}
	defer finish()
	return d.deliver(ctx, job)
}

func (d *Dispatcher) enqueue(appID, kind string, at time.Time, payload any) {
	job, err := d.newJob(appID, kind, at, payload)
	if err != nil {
		d.record(store.Trace{StartedAt: time.Now(), AppID: appID, EventType: kind, Channel: "dispatcher"}, err)
		return
	}
	d.mu.Lock()
	switch {
	case d.closed:
		err = errors.New("event dispatcher is shutting down")
	case job.app.WebhookURL == "" && len(d.ws.sessions[appID]) == 0:
		// Nothing is subscribed for this app.
	case d.pending >= maxPendingEvents:
		err = errors.New("too many pending events")
	default:
		d.pending++
		d.active.Add(1)
		go d.runJob(job)
	}
	d.mu.Unlock()
	if err != nil {
		d.record(job.trace("dispatcher", 0), err)
	}
}

func (d *Dispatcher) runJob(job deliveryJob) {
	defer d.active.Done()
	_, _ = d.deliver(d.ctx, job)
	d.mu.Lock()
	d.pending--
	d.mu.Unlock()
}

func (d *Dispatcher) deliver(ctx context.Context, job deliveryJob) (Result, error) {
	d.mu.Lock()
	sessions := make([]*session, 0, len(d.ws.sessions[job.app.AppID]))
	for session := range d.ws.sessions[job.app.AppID] {
		sessions = append(sessions, session)
	}
	d.mu.Unlock()

	result := Result{EventID: job.id, Deliveries: make([]Delivery, len(sessions))}
	if job.app.WebhookURL != "" {
		result.Deliveries = append(result.Deliveries, Delivery{})
	}
	var sockets sync.WaitGroup
	for i, s := range sessions {
		sockets.Add(1)
		go func() {
			defer sockets.Done()
			result.Deliveries[i] = d.deliverSocket(ctx, s, job)
		}()
	}
	if job.app.WebhookURL != "" {
		result.Deliveries[len(sessions)] = d.deliverWebhook(ctx, job)
	}
	sockets.Wait()
	var err error
	for _, delivered := range result.Deliveries {
		err = errors.Join(err, delivered.Err)
	}
	return result, err
}

func (d *Dispatcher) deliverSocket(ctx context.Context, s *session, job deliveryJob) Delivery {
	trace := job.trace("websocket", 1)
	trace.Path = socketPath
	ctx, cancel := context.WithTimeout(ctx, d.opts.AckTimeout)
	defer cancel()

	messageID := protocol.NewID(protocol.FrameID)
	// The pinned Go SDK dispatches callbacks by event_type inside an event
	// frame. Its transport ignores the legacy "card" frame type.
	frame := wsproto.DataFrame(serviceID, wsproto.Event, messageID, job.id, job.body)
	result := s.exchange(ctx, &frame, messageID)
	headers := make(http.Header, len(result.response.Headers))
	for k, v := range result.response.Headers {
		headers.Add(k, v)
	}
	trace.Status = result.response.StatusCode
	trace.ResponseBody = result.wire
	trace.ResponseHeaders = headers
	if result.err == nil && !success(trace.Status) {
		result.err = fmt.Errorf("websocket acknowledgement status %d", trace.Status)
	}
	d.record(trace, result.err)
	return Delivery{Channel: trace.Channel, Status: trace.Status, Headers: headers, Data: result.response.Data, Err: result.err}
}

func (job deliveryJob) trace(channel string, attempt int) store.Trace {
	return store.Trace{
		Direction: store.Outbound, AppID: job.app.AppID, EventID: job.id, EventType: job.kind,
		Channel: channel, Attempt: attempt, RequestBody: job.body, StartedAt: time.Now(),
	}
}

func (d *Dispatcher) record(trace store.Trace, err error) {
	trace.Direction = store.Outbound
	trace.Latency = time.Since(trace.StartedAt)
	if err != nil {
		trace.Error = err.Error()
	}
	d.state.AppendTrace(trace)
}

func success(status int) bool { return status >= 200 && status < 300 }
