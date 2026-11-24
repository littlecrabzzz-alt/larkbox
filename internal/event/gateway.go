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
	socketPath  = "/ws/v2"
	serviceID   = 1
	maxTickets  = 1024
	maxSessions = 128
	maxFrame    = 1 << 20

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
	expires         time.Time
}

type gateway struct {
	tickets  map[string]ticket
	sessions map[string]map[*session]struct{}
	running  int
	readers  sync.WaitGroup
	upgrader websocket.Upgrader
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
	token, device, ok := d.issueTicket(app.AppID)
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

func (d *Dispatcher) issueTicket(appID string) (token, device string, ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.state.Now()
	for id, saved := range d.ws.tickets {
		if !now.Before(saved.expires) {
			delete(d.ws.tickets, id)
		}
	}
	if d.closed || len(d.ws.tickets) >= maxTickets {
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
	session, appID := d.openSession(w, r)
	if session == nil {
		return
	}
	defer d.ws.readers.Done()
	defer d.removeSession(appID, session)
	session.read(d.ctx)
}

func (d *Dispatcher) openSession(w http.ResponseWriter, r *http.Request) (*session, string) {
	q := r.URL.Query()
	token := q.Get("ticket")
	d.mu.Lock()
	defer d.mu.Unlock()
	saved, found := d.ws.tickets[token]
	switch {
	case d.closed:
		rejectHandshake(w, http.StatusServiceUnavailable, larkws.SystemBusy, "system busy")
	case !found || !d.state.Now().Before(saved.expires):
		delete(d.ws.tickets, token)
		rejectHandshake(w, http.StatusUnauthorized, larkws.AuthFailed, "invalid ticket")
	case q.Get(larkws.ServiceID) != strconv.Itoa(serviceID) || q.Get(larkws.DeviceID) != saved.deviceID:
		rejectHandshake(w, http.StatusForbidden, larkws.Forbidden, "ticket binding mismatch")
	case d.ws.running >= maxSessions:
		w.Header().Set(larkws.HeaderHandshakeAuthErrCode, strconv.Itoa(larkws.ExceedConnLimit))
		rejectHandshake(w, http.StatusForbidden, larkws.AuthFailed, "connection limit exceeded")
	default:
		delete(d.ws.tickets, token)
		// Keep registration atomic with respect to delivery snapshots. The
		// handshake write is bounded by the upgrader's handshake timeout.
		conn, err := d.ws.upgrader.Upgrade(w, r, nil)
		if err != nil {
			return nil, ""
		}
		conn.SetReadLimit(maxFrame)
		s := &session{conn: conn, done: make(chan struct{}), pending: make(map[string]chan ack)}
		if d.ws.sessions[saved.appID] == nil {
			d.ws.sessions[saved.appID] = make(map[*session]struct{})
		}
		d.ws.sessions[saved.appID][s] = struct{}{}
		d.ws.running++
		d.ws.readers.Add(1)
		return s, saved.appID
	}
	return nil, ""
}

func (d *Dispatcher) removeSession(appID string, s *session) {
	_ = s.conn.Close()
	close(s.done)
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.ws.sessions[appID], s)
	if len(d.ws.sessions[appID]) == 0 {
		delete(d.ws.sessions, appID)
	}
	d.ws.running--
}

// SessionCount reports the number of active long connections for an app.
func (d *Dispatcher) SessionCount(appID string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.ws.sessions[appID])
}

type session struct {
	conn    *websocket.Conn
	done    chan struct{}
	writer  sync.Mutex
	mu      sync.Mutex
	pending map[string]chan ack
}

type ack struct {
	response wsproto.Response
	wire     []byte
	err      error
}

func (s *session) write(ctx context.Context, frame *wsproto.Frame) error {
	wire, err := wsproto.Encode(frame)
	if err != nil {
		return err
	}
	s.writer.Lock()
	defer s.writer.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, _ := ctx.Deadline()
	if err := s.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	if err := s.conn.WriteMessage(websocket.BinaryMessage, wire); err != nil {
		// Gorilla cannot write to the connection again after a write timeout.
		_ = s.conn.Close()
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
			ch <- ack{response: response, wire: frame.Payload, err: err}
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
