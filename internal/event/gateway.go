package event

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/wsproto"
)

const (
	socketPath = "/ws/v2"
	serviceID  = 1
	maxTickets = 1024
	maxFrame   = 1 << 20

	bootstrapBadRequest   = 9499
	bootstrapEmptyCreds   = 1000040344
	bootstrapInvalidCreds = 1000040345
	bootstrapInvalidAppID = 1000040346
)

var clientConfig = wsproto.ClientConfig{
	ReconnectCount: -1, ReconnectInterval: 120, ReconnectNonce: 30, PingInterval: 120,
}

type ticket struct {
	appID, deviceID string
	reservation     *appConnections
	expires         time.Time
}

type gateway struct {
	tickets     map[string]ticket
	apps        map[string]*appConnections
	connections sync.WaitGroup
	upgrader    websocket.Upgrader
}

// appConnections is guarded by the dispatcher mutex.
type appConnections struct {
	sessions  []*session
	next      int
	upgrading int
	changed   chan struct{}
}

// RegisterRoutes installs the SDK endpoint discovery and socket routes.
func (d *Dispatcher) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST "+larkws.GenEndpointUri, d.bootstrap)
	mux.HandleFunc("GET "+socketPath, d.upgrade)
}

func (d *Dispatcher) bootstrap(w http.ResponseWriter, r *http.Request) {
	const bodyLimit = 4096
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, bodyLimit))
	// Distinguish missing fields (bad JSON request) from explicitly empty
	// credentials, as the endpoint does before checking credential validity.
	var request struct {
		AppID           *string
		AppSecret       *string
		ClientAssertion string
	}
	if err := dec.Decode(&request); err != nil || dec.Decode(new(json.RawMessage)) != io.EOF ||
		request.AppID == nil || request.AppSecret == nil {
		_ = protocol.WriteJSON(w, http.StatusBadRequest, protocol.Envelope{
			Code: bootstrapBadRequest, Msg: "Bad Request", Data: struct{}{},
		})
		return
	}
	if *request.AppID == "" || *request.AppSecret == "" {
		writeBootstrapError(w, bootstrapEmptyCreds, "app_id or app_secret is null")
		return
	}
	d.mu.Lock()
	generation := d.generation
	d.mu.Unlock()
	app, found := d.state.Apps().Get(*request.AppID)
	provided, expected := sha256.Sum256([]byte(*request.AppSecret)), sha256.Sum256([]byte(app.AppSecret))
	validSecret := subtle.ConstantTimeCompare(provided[:], expected[:]) == 1
	if !found && !feishuAppID(*request.AppID) {
		writeBootstrapError(w, bootstrapInvalidAppID, "app_id is invalid")
		return
	}
	if !found || !validSecret || request.ClientAssertion != "" {
		writeBootstrapError(w, bootstrapInvalidCreds, "app_id or app_secret is invalid")
		return
	}
	token, device, ok := d.issueTicket(app.AppID, generation)
	if !ok {
		_ = protocol.WriteJSON(w, http.StatusOK, larkws.EndpointResp{Code: larkws.SystemBusy, Msg: "system busy"})
		return
	}
	scheme := "ws"
	if r.TLS != nil || r.URL.Scheme == "https" {
		scheme = "wss"
	}
	endpoint := url.URL{Scheme: scheme, Host: r.Host, Path: socketPath, RawQuery: url.Values{
		"ticket": {token}, larkws.DeviceID: {device}, larkws.ServiceID: {strconv.Itoa(serviceID)},
	}.Encode()}
	w.Header().Set("Cache-Control", "no-store")
	_ = protocol.WriteJSON(w, http.StatusOK, larkws.EndpointResp{Msg: "ok", Data: &larkws.Endpoint{
		Url: endpoint.String(), ClientConfig: &clientConfig,
	}})
}

func writeBootstrapError(w http.ResponseWriter, code protocol.Code, message string) {
	_ = protocol.WriteJSON(w, http.StatusOK, protocol.Envelope{
		Code: code, Msg: message, Data: map[string]string{"URL": ""},
	})
}

func feishuAppID(appID string) bool {
	suffix, ok := strings.CutPrefix(appID, "cli_")
	if !ok || len(suffix) != 16 {
		return false
	}
	_, err := hex.DecodeString(suffix)
	return err == nil
}

func (d *Dispatcher) issueTicket(appID string, generation uint64) (token, device string, ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.state.Now()
	for id, saved := range d.ws.tickets {
		if !now.Before(saved.expires) {
			delete(d.ws.tickets, id)
		}
	}
	if d.closed || generation != d.generation || len(d.ws.tickets) >= maxTickets {
		return "", "", false
	}
	token, device = protocol.NewID(""), protocol.NewID("")
	d.ws.tickets[token] = ticket{appID: appID, deviceID: device, expires: now.Add(d.opts.TicketTTL)}
	return token, device, true
}

func rejectHandshake(w http.ResponseWriter, status, code int, message string) {
	w.Header().Set(larkws.HeaderHandshakeStatus, strconv.Itoa(code))
	w.Header().Set(larkws.HeaderHandshakeMsg, message)
	http.Error(w, message, status)
}

func (d *Dispatcher) upgrade(w http.ResponseWriter, r *http.Request) {
	ticket, rejection := d.reserveSession(r.URL.Query())
	if rejection != nil {
		if rejection.code == larkws.AuthFailed && rejection.status == http.StatusForbidden {
			w.Header().Set(larkws.HeaderHandshakeAuthErrCode, strconv.Itoa(larkws.ExceedConnLimit))
		}
		rejectHandshake(w, rejection.status, rejection.code, rejection.message)
		return
	}
	defer d.ws.connections.Done()
	conn, err := d.ws.upgrader.Upgrade(w, r, nil)
	var s *session
	if err == nil {
		conn.SetReadLimit(maxFrame)
		s = &session{conn: conn, done: make(chan struct{}), pending: make(map[string]chan ack),
			info: SessionInfo{ID: protocol.NewID("ws_"), AppID: ticket.appID, DeviceID: ticket.deviceID, ServiceID: serviceID, ConnectedAt: d.state.Now()}}
	}
	if !d.registerSession(ticket, s) {
		if s != nil {
			closeSession(s, time.Now().Add(100*time.Millisecond))
		}
		return
	}
	defer d.removeSession(ticket, s)
	s.read(d.ctx)
}

type handshakeRejection struct {
	status, code int
	message      string
}

func (d *Dispatcher) reserveSession(q url.Values) (ticket, *handshakeRejection) {
	token := q.Get("ticket")
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.state.Now()
	saved, found := d.ws.tickets[token]
	switch {
	case d.closed:
		return ticket{}, &handshakeRejection{http.StatusServiceUnavailable, larkws.SystemBusy, "system busy"}
	case !found || !now.Before(saved.expires):
		delete(d.ws.tickets, token)
		return ticket{}, &handshakeRejection{http.StatusUnauthorized, larkws.AuthFailed, "invalid ticket"}
	case q.Get(larkws.ServiceID) != strconv.Itoa(serviceID) || q.Get(larkws.DeviceID) != saved.deviceID:
		return ticket{}, &handshakeRejection{http.StatusForbidden, larkws.Forbidden, "ticket binding mismatch"}
	}
	app := d.ws.apps[saved.appID]
	if app == nil {
		app = &appConnections{changed: make(chan struct{})}
		d.ws.apps[saved.appID] = app
	}
	if len(app.sessions)+app.upgrading >= d.opts.MaxConnectionsPerApp {
		return ticket{}, &handshakeRejection{http.StatusForbidden, larkws.AuthFailed, "app connection limit exceeded"}
	}
	delete(d.ws.tickets, token)
	app.upgrading++
	d.ws.connections.Add(1)
	saved.reservation = app
	return saved, nil
}

func (d *Dispatcher) registerSession(ticket ticket, s *session) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	app := d.ws.apps[ticket.appID]
	if app != ticket.reservation {
		return false
	}
	app.upgrading--
	registered := s != nil && !d.closed
	if registered {
		app.sessions = append(app.sessions, s)
		close(d.socketChanged)
		d.socketChanged = make(chan struct{})
	}
	close(app.changed)
	if len(app.sessions) == 0 && app.upgrading == 0 {
		delete(d.ws.apps, ticket.appID)
	} else {
		app.changed = make(chan struct{})
	}
	return registered
}

func (d *Dispatcher) removeSession(ticket ticket, s *session) {
	s.disconnect()
	d.mu.Lock()
	defer d.mu.Unlock()
	app := d.ws.apps[ticket.appID]
	if app != ticket.reservation {
		return
	}
	idx := slices.Index(app.sessions, s)
	app.sessions = slices.Delete(app.sessions, idx, idx+1)
	if idx < app.next {
		app.next--
	}
	if len(app.sessions) == 0 && app.upgrading == 0 {
		delete(d.ws.apps, ticket.appID)
	}
}

// SessionCount reports the number of active long connections for an app.
func (d *Dispatcher) SessionCount(appID string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	if app := d.ws.apps[appID]; app != nil {
		return len(app.sessions)
	}
	return 0
}

func (d *Dispatcher) selectSession(ctx context.Context, appID string, waitForUpgrade bool) *session {
	for ctx.Err() == nil {
		d.mu.Lock()
		app := d.ws.apps[appID]
		if app == nil {
			d.mu.Unlock()
			return nil
		}
		for offset := range len(app.sessions) {
			i := (app.next + offset) % len(app.sessions)
			s := app.sessions[i]
			if !s.disconnected() {
				app.next = i + 1
				d.mu.Unlock()
				return s
			}
		}
		if !waitForUpgrade || app.upgrading == 0 {
			d.mu.Unlock()
			return nil
		}
		// A client can dispatch as soon as it receives the 101. Wait for
		// its reserved session to register or fail.
		changed := app.changed
		d.mu.Unlock()
		select {
		case <-changed:
		case <-d.stopping:
			return nil
		case <-ctx.Done():
			return nil
		}
	}
	return nil
}

var errDisconnectedBeforeWrite = errors.New("websocket disconnected before event write")

type session struct {
	info      SessionInfo
	conn      *websocket.Conn
	done      chan struct{}
	closeOnce sync.Once
	writer    sync.Mutex
	mu        sync.Mutex
	pending   map[string]chan ack
}

func (s *session) disconnected() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (s *session) disconnect() {
	s.closeOnce.Do(func() {
		close(s.done)
		_ = s.conn.Close()
	})
}

type ack struct {
	response wsproto.Response
	err      error
}

func (s *session) write(ctx context.Context, frame *wsproto.Frame) error {
	wire, err := wsproto.Encode(frame)
	if err != nil {
		return err
	}
	s.writer.Lock()
	defer s.writer.Unlock()
	if s.disconnected() {
		return errDisconnectedBeforeWrite
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, _ := ctx.Deadline()
	if err := s.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	if err := s.conn.WriteMessage(websocket.BinaryMessage, wire); err != nil {
		// Gorilla cannot write to the connection again after a write timeout.
		s.disconnect()
		if errors.Is(err, websocket.ErrCloseSent) {
			return errDisconnectedBeforeWrite
		}
		return err
	}
	return nil
}

func (s *session) read(ctx context.Context) {
	for {
		// A healthy SDK sends a protobuf ping every PingInterval.
		if err := s.conn.SetReadDeadline(time.Now().Add(3 * time.Duration(clientConfig.PingInterval) * time.Second)); err != nil {
			return
		}
		kind, wire, err := s.conn.ReadMessage()
		if err != nil {
			return
		}
		if kind != websocket.BinaryMessage {
			continue
		}
		frame, err := wsproto.Decode(wire)
		if err != nil || frame.Service != serviceID {
			continue
		}
		if err := s.handleFrame(ctx, frame); err != nil {
			return
		}
	}
}

func (s *session) handleFrame(ctx context.Context, frame wsproto.Frame) error {
	switch frame.Method {
	case wsproto.Control:
		if wsproto.HeaderValue(frame, wsproto.Type) != wsproto.Ping {
			return nil
		}
		pong, err := wsproto.ControlFrame(frame.Service, wsproto.Pong, &clientConfig)
		if err != nil {
			return err
		}
		pong.SeqID, pong.LogID, pong.LogIDNew = frame.SeqID, frame.LogID, frame.LogIDNew
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		return s.write(ctx, &pong)
	case wsproto.Data:
		id := wsproto.HeaderValue(frame, wsproto.MessageID)
		s.mu.Lock()
		defer s.mu.Unlock()
		if ch, ok := s.pending[id]; ok {
			delete(s.pending, id)
			response, err := wsproto.DecodeResponse(frame.Payload)
			ch <- ack{response: response, err: err}
		}
	}
	return nil
}

func (s *session) exchange(ctx context.Context, frame *wsproto.Frame, messageID string) ack {
	ch := make(chan ack, 1)
	s.mu.Lock()
	s.pending[messageID] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, messageID)
		s.mu.Unlock()
	}()
	if err := s.write(ctx, frame); err != nil {
		return ack{err: err}
	}
	select {
	case result := <-ch:
		return result
	case <-s.done:
		// The peer may have sent the ACK and immediately closed the socket.
		select {
		case result := <-ch:
			return result
		default:
			return ack{err: errors.New("websocket disconnected before acknowledgement")}
		}
	case <-ctx.Done():
		return ack{err: ctx.Err()}
	}
}

// SessionInfo exposes connection metadata without exposing upgrade credentials.
type SessionInfo struct {
	ID          string    `json:"session_id"`
	AppID       string    `json:"app_id"`
	DeviceID    string    `json:"device_id"`
	ServiceID   int       `json:"service_id"`
	ConnectedAt time.Time `json:"connected_at"`
}

// Sessions snapshots all active long connections.
func (d *Dispatcher) Sessions() []SessionInfo {
	d.mu.Lock()
	defer d.mu.Unlock()
	sessions := make([]SessionInfo, 0)
	for _, app := range d.ws.apps {
		for _, s := range app.sessions {
			if !s.disconnected() {
				sessions = append(sessions, s.info)
			}
		}
	}
	slices.SortFunc(sessions, func(a, b SessionInfo) int { return strings.Compare(a.ID, b.ID) })
	return sessions
}
