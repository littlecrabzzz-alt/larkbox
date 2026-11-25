// Package event delivers Feishu events over WebSocket and HTTP.
package event

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/sandbox/fault"
	"github.com/littlecrabzzz-alt/larkbox/internal/simclock"
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
	Faults       *fault.Engine
	ManualClock  *simclock.Manual
	Logger       *slog.Logger
}

// Delivery contains the outcome for one receiver. Data is the HTTP response
// body or the decoded WebSocket response data.
type Delivery struct {
	Channel     string
	Status      int
	Headers     http.Header
	Data        []byte
	Err         error
	completedAt time.Time
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

type legacyEnvelope struct {
	Timestamp string `json:"ts"`
	ID        string `json:"uuid"`
	Token     string `json:"token"`
	Type      string `json:"type"`
	Event     any    `json:"event"`
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
	redactor  store.Redactor
	app       store.App
	id        string
	kind      string
	requestID string
	body      []byte
	wireBody  []byte
	epoch     uint64
	copy      int
	delayedAt time.Time
}

// ApprovalInstance is the legacy webhook event for native approval state changes.
const ApprovalInstance = "approval_instance"

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

	mu            sync.Mutex
	closed        bool
	resetCtx      context.Context
	resetCancel   context.CancelFunc
	generation    uint64
	pending       int
	active        sync.WaitGroup
	ws            gateway
	retryWaiters  atomic.Int64
	retries       atomic.Uint64
	attempts      atomic.Uint64
	failures      atomic.Uint64
	retryJournal  map[retryKey]PendingRetry
	socketChanged chan struct{}
}

// Metrics contains current queue sizes and cumulative delivery counters.
type Metrics struct {
	Pending, RetryWaiters       int64
	Retries, Attempts, Failures uint64
}

// Metrics returns the dispatcher's current observations for an operator.
func (d *Dispatcher) Metrics() Metrics {
	d.mu.Lock()
	defer d.mu.Unlock()
	return Metrics{int64(d.pending), d.retryWaiters.Load(), d.retries.Load(), d.attempts.Load(), d.failures.Load()}
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
	resetCtx, resetCancel := context.WithCancel(ctx)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxResponseHeaderBytes = store.TraceBodyLimit
	return &Dispatcher{
		state: state, opts: opts, ctx: ctx, cancel: cancel, resetCtx: resetCtx, resetCancel: resetCancel,
		done: make(chan struct{}), stopping: make(chan struct{}), retriesStopped: make(chan struct{}),
		retryJournal: make(map[retryKey]PendingRetry), socketChanged: make(chan struct{}),
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

type admissionKey struct{}

type admission struct {
	reset context.Context
	epoch uint64
}

func (d *Dispatcher) begin(ctx context.Context) (context.Context, func(), error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil, nil, errors.New("event dispatcher is shutting down")
	}
	if scope, ok := ctx.Value(admissionKey{}).(admission); ok && scope.reset != d.resetCtx {
		return nil, nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	d.active.Add(1)
	ctx = context.WithValue(ctx, admissionKey{}, admission{d.resetCtx, d.state.TraceEpoch()})
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(d.resetCtx, cancel)
	if d.resetCtx.Err() != nil {
		cancel()
	}
	return ctx, func() {
		stop()
		cancel()
		d.active.Done()
	}, nil
}

func (d *Dispatcher) newJob(ctx context.Context, appID, kind string, at time.Time, payload any) (deliveryJob, error) {
	epoch := ctx.Value(admissionKey{}).(admission).epoch
	app, ok := d.state.Apps().Get(appID)
	if !ok {
		return deliveryJob{epoch: epoch}, fmt.Errorf("unknown event app %q", appID)
	}
	if kind == "" {
		return deliveryJob{epoch: epoch}, errors.New("event type is required")
	}
	if at.IsZero() {
		at = d.state.Now()
	}
	createTime := millis(at)
	if kind == cardActionTrigger {
		createTime = strconv.FormatInt(at.UnixMicro(), 10)
	}
	id := protocol.NewID(protocol.EventID)
	var packet any = envelope{
		Schema: "2.0",
		Header: eventHeader{
			ID: id, Type: kind, CreateTime: createTime,
			AppID: appID, Token: app.VerificationToken, TenantKey: tenantKey(app),
		},
		Event: payload,
	}
	if kind == FirstBotConversation || kind == ApprovalInstance {
		payloadJSON, err := json.Marshal(payload)
		if err != nil {
			return deliveryJob{epoch: epoch}, err
		}
		var legacyData map[string]json.RawMessage
		if json.Unmarshal(payloadJSON, &legacyData) != nil || legacyData == nil {
			return deliveryJob{epoch: epoch}, errors.New("legacy event requires an event object")
		}
		for name, value := range map[string]string{"type": kind, "app_id": appID, "tenant_key": tenantKey(app)} {
			legacyData[name], _ = json.Marshal(value)
		}
		packet = legacyEnvelope{
			Timestamp: fmt.Sprintf("%d.%09d", at.Unix(), at.Nanosecond()), ID: id,
			Token: app.VerificationToken, Type: "event_callback", Event: legacyData,
		}
	}
	body, err := json.Marshal(packet)
	if err != nil {
		return deliveryJob{epoch: epoch}, err
	}
	if len(body) > maxFrame {
		return deliveryJob{epoch: epoch}, errors.New("event payload exceeds size limit")
	}
	return deliveryJob{app: app, id: id, kind: kind, requestID: store.RequestID(ctx), body: body, epoch: epoch, redactor: store.AppRedactor(app)}, nil
}

func tenantKey(app store.App) string {
	if app.TenantKey != "" {
		return app.TenantKey
	}
	return "larkbox"
}

func millis(at time.Time) string { return strconv.FormatInt(at.UnixMilli(), 10) }

// Dispatch delivers an event to one active session for the app
// and to its configured webhook. It waits for each receiver's outcome.
func (d *Dispatcher) Dispatch(ctx context.Context, appID, kind string, payload any) (Result, error) {
	return d.dispatch(ctx, appID, kind, payload, false)
}

// DispatchSubscribed delivers a user interaction if the app subscribes to it.
func (d *Dispatcher) DispatchSubscribed(ctx context.Context, appID, kind string, payload any) (Result, error) {
	return d.dispatch(ctx, appID, kind, payload, true)
}

func (d *Dispatcher) dispatch(ctx context.Context, appID, kind string, payload any, requireSubscription bool) (Result, error) {
	ctx, finish, err := d.begin(ctx)
	if err != nil {
		return Result{}, err
	}
	defer finish()
	job, err := d.newJob(ctx, appID, kind, time.Time{}, payload)
	if err != nil {
		return Result{}, err
	}
	if requireSubscription && !subscribed(job.app, kind) {
		return Result{}, nil
	}
	return d.deliver(ctx, job)
}

// Enqueue delivers a committed state change to an app subscribed to its event type.
func (d *Dispatcher) Enqueue(ctx context.Context, appID, kind string, at time.Time, payload any) {
	d.enqueue(ctx, appID, kind, at, payload)
}

func (d *Dispatcher) enqueue(from context.Context, appID, kind string, at time.Time, payload any) {
	d.mu.Lock()
	if !d.state.Current(from) {
		d.mu.Unlock()
		return
	}
	ctx := context.WithValue(d.resetCtx, admissionKey{}, admission{d.resetCtx, d.state.TraceEpoch()})
	ctx = store.WithRequestID(ctx, store.RequestID(from))
	job, err := d.newJob(ctx, appID, kind, at, payload)
	if err != nil {
		d.mu.Unlock()
		d.record(job, store.Trace{StartedAt: time.Now(), AppID: appID, EventType: kind, Channel: "dispatcher"}, err)
		return
	}
	switch {
	case d.closed:
		err = errors.New("event dispatcher is shutting down")
	case !subscribed(job.app, kind) || (job.app.WebhookURL == "" && d.ws.apps[appID] == nil):
		// Nothing is subscribed for this app.
	case d.pending >= maxPendingEvents:
		err = errors.New("too many pending events")
	default:
		d.pending++
		d.active.Add(1)
		go d.runJob(ctx, job)
	}
	d.mu.Unlock()
	if err != nil {
		d.record(job, job.trace("dispatcher", 0), err)
	}
}

func (d *Dispatcher) runJob(ctx context.Context, job deliveryJob) {
	defer d.active.Done()
	_, _ = d.deliver(ctx, job)
	d.mu.Lock()
	d.pending--
	d.mu.Unlock()
}

func (d *Dispatcher) deliver(ctx context.Context, job deliveryJob) (Result, error) {
	result := Result{EventID: job.id}
	if err := ctx.Err(); err != nil {
		return result, err
	}
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
	if job.kind == ApprovalInstance {
		return Delivery{}
	}
	s := d.selectSession(ctx, job.app.AppID, true)
	if s == nil {
		return Delivery{}
	}
	return d.withDeliveryFault(ctx, job, "websocket", "", socketPath, func(job deliveryJob) Delivery {
		return d.socketDelivery(ctx, s, job)
	})
}

func (d *Dispatcher) socketDelivery(ctx context.Context, s *session, job deliveryJob) Delivery {
	return d.socketDeliveryFrom(ctx, s, job, 1)
}

func (d *Dispatcher) socketDeliveryFrom(ctx context.Context, s *session, job deliveryJob, start int) (result Delivery) {
	defer func() { d.finishRetry(ctx, job, "websocket", result.Err) }()
	attempts := start - 1
	for attempt := start; ; attempt++ {
		if attempts > 0 && (!d.canRetry(ctx) || s.disconnected()) {
			if result.Err == nil {
				result.Err = errDisconnectedBeforeWrite
			}
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
		if result.Err == nil || attempts >= d.opts.WebSocketMaxAttempts || !d.waitRetry(ctx, job, "websocket", attempts, result.completedAt, s.done) {
			return result
		}
	}
}

func (d *Dispatcher) canRetry(ctx context.Context) bool {
	if scope, ok := ctx.Value(admissionKey{}).(admission); ok && scope.reset.Err() != nil {
		return false
	}
	select {
	case <-d.retriesStopped:
		return false
	default:
		return ctx.Err() == nil
	}
}

func (d *Dispatcher) waitRetry(ctx context.Context, job deliveryJob, channel string, attempt int, completedAt time.Time, peerDone <-chan struct{}) bool {
	due := completedAt.Add(d.opts.RetryBackoff(attempt))
	d.scheduleRetry(ctx, job, channel, attempt, due)
	return d.waitUntilRetry(ctx, due, peerDone)
}

func (d *Dispatcher) waitUntilRetry(ctx context.Context, due time.Time, peerDone <-chan struct{}) (ready bool) {
	if !d.canRetry(ctx) {
		return false
	}
	d.retryWaiters.Add(1)
	defer func() {
		if ready {
			d.retries.Add(1)
		}
		d.retryWaiters.Add(-1)
	}()
	if clock := d.opts.ManualClock; clock != nil {
		for {
			now, changed := clock.Snapshot()
			if !now.Before(due) {
				return d.canRetry(ctx)
			}
			select {
			case <-changed:
			case <-ctx.Done():
				return false
			case <-d.retriesStopped:
				return false
			case <-peerDone:
				return false
			}
		}
	}
	timer := time.NewTimer(time.Until(due))
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
		audit.Data = job.redactor.JSON(audit.Data)
		audit.Headers = make(map[string]string)
		for key, values := range job.redactor.Headers(headers) {
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
	completedAt := d.state.Now()
	d.record(job, trace, result.err)
	return Delivery{Channel: trace.Channel, Status: trace.Status, Headers: headers, Data: result.response.Data, Err: result.err, completedAt: completedAt}
}

func (job deliveryJob) trace(channel string, attempt int) store.Trace {
	started := time.Now()
	if job.copy == 0 && attempt <= 1 && !job.delayedAt.IsZero() {
		started = job.delayedAt
	}
	return store.Trace{
		Direction: store.Outbound, RequestID: job.requestID, AppID: job.app.AppID, EventID: job.id, EventType: job.kind,
		Channel: channel, Attempt: attempt, Copy: job.copy, RequestBody: job.body, StartedAt: started,
	}
}

func (d *Dispatcher) record(job deliveryJob, trace store.Trace, err error) {
	trace.Direction = store.Outbound
	trace.Latency = time.Since(trace.StartedAt)
	if err != nil {
		trace.Error = err.Error()
	}
	if trace.Attempt > 0 {
		d.attempts.Add(1)
		if err != nil || !success(trace.Status) {
			d.failures.Add(1)
		}
	}
	d.state.AppendTraceAt(job.epoch, trace, job.redactor)
	if logger := d.opts.Logger; logger != nil {
		logger.Info("event_delivery", "request_id", trace.RequestID, "event_id", trace.EventID,
			"app_id", job.redactor.Replace(trace.AppID), "event_type", job.redactor.Replace(trace.EventType),
			"channel", trace.Channel, "attempt", trace.Attempt, "status", trace.Status,
			"failed", err != nil || !success(trace.Status), "latency_ms", trace.Latency.Milliseconds())
	}
}

func success(status int) bool { return status >= 200 && status < 300 }

// Reset cancels deliveries, invalidates tickets and sessions, and replaces state
// under the same admission lock. The reset callback must not call the dispatcher.
func (d *Dispatcher) Reset(reset func()) {
	d.mu.Lock()
	d.resetCancel()
	d.resetCtx, d.resetCancel = context.WithCancel(d.ctx)
	d.generation++
	clear(d.ws.tickets)
	clear(d.retryJournal)
	var sessions []*session
	for _, app := range d.ws.apps {
		sessions = append(sessions, app.sessions...)
		close(app.changed)
	}
	clear(d.ws.apps)
	reset()
	d.mu.Unlock()
	for _, s := range sessions {
		s.disconnect()
	}
}

// Scope binds a control request to the current reset generation.
func (d *Dispatcher) Scope(ctx context.Context) context.Context {
	d.mu.Lock()
	defer d.mu.Unlock()
	return context.WithValue(ctx, admissionKey{}, admission{reset: d.resetCtx})
}

func (d *Dispatcher) withDeliveryFault(ctx context.Context, job deliveryJob, channel, method, path string, deliver func(deliveryJob) Delivery) Delivery {
	input := fault.Match{AppID: job.app.AppID, EventType: job.kind, Channel: channel, Method: method, PathPrefix: path}
	d.mu.Lock()
	scope := ctx.Value(admissionKey{}).(admission)
	err := scope.reset.Err()
	var effect fault.Effect
	if engine := d.opts.Faults; engine != nil && err == nil && ctx.Err() == nil {
		effect = engine.Match(input, false)
	}
	d.mu.Unlock()
	if err != nil {
		return Delivery{Channel: channel, Err: err}
	}
	count := 1
	switch effect.Kind {
	case fault.Latency:
		job.delayedAt = time.Now()
		if err := effect.Wait(ctx); err != nil {
			trace := job.trace(channel, 0)
			trace.Method, trace.Path = method, path
			d.record(job, trace, err)
			return Delivery{Channel: channel, Err: err}
		}
	case fault.DuplicateEvent:
		count = effect.DuplicateCount
	}
	if err := ctx.Err(); err != nil {
		return Delivery{Channel: channel, Err: err}
	}
	first := deliver(job)
	for i := 1; i < count && ctx.Err() == nil && scope.reset.Err() == nil; i++ {
		job.copy = i
		_ = deliver(job)
	}
	return first
}
