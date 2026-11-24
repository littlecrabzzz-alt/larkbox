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
	WebhookTimeout       time.Duration
	VerificationTimeout  time.Duration
	WebhookMaxAttempts   int
	WebSocketMaxAttempts int
	MaxConnectionsPerApp int
	// RetryBackoff receives a failed attempt number, starting at 1, for either transport.
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
	state          *store.Store
	opts           Options
	client         *http.Client
	ctx            context.Context
	cancel         context.CancelFunc
	done           chan struct{}
	stopping       chan struct{}
	retriesStopped chan struct{}
	stopRetries    sync.Once

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
	if opts.VerificationTimeout <= 0 {
		opts.VerificationTimeout = time.Second
	}
	if opts.WebSocketMaxAttempts <= 0 {
		opts.WebSocketMaxAttempts = 5
	}
	if opts.MaxConnectionsPerApp <= 0 {
		opts.MaxConnectionsPerApp = 50
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
		state: state, opts: opts, ctx: ctx, cancel: cancel,
		done: make(chan struct{}), stopping: make(chan struct{}), retriesStopped: make(chan struct{}),
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		ws: gateway{
			tickets: make(map[string]ticket), apps: make(map[string]*appConnections),
			upgrader: websocket.Upgrader{HandshakeTimeout: time.Second},
		},
	}
}

func defaultRetryBackoff(attempt int) time.Duration {
	delays := [...]time.Duration{15 * time.Second, 5 * time.Minute, time.Hour, 6 * time.Hour}
	return delays[min(attempt, len(delays))-1]
}

// StopRetries abandons retry backoffs and prevents further retry attempts.
// New events and their first attempts can still be admitted while HTTP drains.
func (d *Dispatcher) StopRetries() {
	d.stopRetries.Do(func() { close(d.retriesStopped) })
}

// Shutdown closes admission and waits for admitted deliveries and connections.
// If ctx expires, it cancels outstanding network operations.
func (d *Dispatcher) Shutdown(ctx context.Context) error {
	d.StopRetries()
	d.mu.Lock()
	if !d.closed {
		d.closed = true
		close(d.stopping)
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
	d.ws.connections.Wait()
	d.client.CloseIdleConnections()
	close(d.done)
}

func (d *Dispatcher) closeSessions(graceful bool) {
	d.mu.Lock()
	var sessions []*session
	for _, app := range d.ws.apps {
		sessions = append(sessions, app.sessions...)
	}
	d.mu.Unlock()

	var deadline time.Time
	if graceful {
		deadline = time.Now().Add(100 * time.Millisecond)
	}
	for _, s := range sessions {
		closeSession(s, deadline)
	}
}

func closeSession(s *session, deadline time.Time) {
	if !deadline.IsZero() {
		frame := websocket.FormatCloseMessage(websocket.CloseGoingAway, "server shutting down")
		_ = s.conn.WriteControl(websocket.CloseMessage, frame, deadline)
	}
	s.disconnect()
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
	createTime := millis(at)
	if kind == cardActionTrigger {
		createTime = strconv.FormatInt(at.UnixMicro(), 10)
	}
	id := protocol.NewID(protocol.EventID)
	body, err := json.Marshal(envelope{
		Schema: "2.0",
		Header: eventHeader{
			ID: id, Type: kind, CreateTime: createTime,
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

// Dispatch delivers a Schema 2.0 event to one active session for the app
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
	case job.app.WebhookURL == "" && d.ws.apps[appID] == nil:
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
	result := Result{EventID: job.id}
	if job.kind == cardActionTrigger {
		delivered := d.deliverCardAction(ctx, job, nil)
		result.Deliveries = []Delivery{delivered}
		return result, delivered.Err
	}
	if job.app.WebhookURL == "" {
		result.Deliveries = appendDelivery(nil, d.deliverWebSocket(ctx, job))
	} else {
		// One transport can still deliver while the other is waiting for a
		// response or retrying.
		socket := make(chan Delivery, 1)
		go func() { socket <- d.deliverWebSocket(ctx, job) }()
		webhook := d.deliverWebhook(ctx, job)
		result.Deliveries = append(appendDelivery(nil, <-socket), webhook)
	}
	var err error
	for _, delivered := range result.Deliveries {
		err = errors.Join(err, delivered.Err)
	}
	return result, errors.Join(err, ctx.Err())
}

func appendDelivery(deliveries []Delivery, delivery Delivery) []Delivery {
	if delivery.Channel != "" {
		return append(deliveries, delivery)
	}
	return deliveries
}

func (d *Dispatcher) deliverWebSocket(ctx context.Context, job deliveryJob) Delivery {
	s := d.selectSession(ctx, job.app.AppID, true)
	if s == nil {
		return Delivery{}
	}
	var result Delivery
	attempts := 0
	for attempt := 1; ; attempt++ {
		if attempts > 0 && (!d.canRetry(ctx) || s.disconnected()) {
			return result
		}
		result = d.socketAttempt(ctx, s, job, attempt, nil)
		if attempts == 0 && errors.Is(result.Err, errDisconnectedBeforeWrite) {
			// No frame reached this replica. Another live session can still
			// receive the first delivery without using the ACK retry budget.
			if ctx.Err() == nil {
				if next := d.selectSession(ctx, job.app.AppID, false); next != nil {
					s = next
					continue
				}
			}
			return result
		}
		attempts++
		// Once a write may have reached a peer, keep retries on that replica.
		if result.Err == nil || attempts == d.opts.WebSocketMaxAttempts || !d.waitRetry(ctx, attempts, s.done) {
			return result
		}
	}
}

func (d *Dispatcher) canRetry(ctx context.Context) bool {
	select {
	case <-d.retriesStopped:
		return false
	default:
		return ctx.Err() == nil
	}
}

func (d *Dispatcher) waitRetry(ctx context.Context, attempt int, peerDone <-chan struct{}) bool {
	if !d.canRetry(ctx) {
		return false
	}
	timer := time.NewTimer(d.opts.RetryBackoff(attempt))
	defer timer.Stop()
	select {
	case <-timer.C:
		return d.canRetry(ctx)
	case <-ctx.Done():
	case <-d.retriesStopped:
	case <-peerDone:
	}
	return false
}

func (d *Dispatcher) socketAttempt(ctx context.Context, s *session, job deliveryJob, attempt int, verify func(context.Context, []byte) error) Delivery {
	trace := job.trace("websocket", attempt)
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
	trace.ResponseHeaders = headers
	if result.err == nil {
		// The acknowledgement embeds headers in its JSON body as well.
		// Store the same safe subset in both places.
		audit := result.response
		audit.Data = store.RedactTraceJSON(audit.Data)
		audit.Headers = make(map[string]string)
		for key, values := range store.TraceHeaders(headers) {
			audit.Headers[key] = values[0]
		}
		trace.ResponseBody, _ = json.Marshal(audit)
		switch {
		case !success(trace.Status) || (job.kind == cardActionTrigger && trace.Status != http.StatusOK):
			result.err = fmt.Errorf("websocket acknowledgement status %d", trace.Status)
		case ctx.Err() != nil:
			result.err = ctx.Err()
		case verify != nil:
			result.err = verify(ctx, result.response.Data)
		}
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
