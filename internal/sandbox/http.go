// Package sandbox exposes the local larkbox control plane.
package sandbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/event"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/sandbox/fault"
	"github.com/littlecrabzzz-alt/larkbox/internal/simclock"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

const maxBody = 1 << 20

type control struct {
	cfg     config.Config
	version string
	state   *store.Store
	events  *event.Dispatcher
	faults  *fault.Engine
	clock   *simclock.Manual
	mu      sync.RWMutex // Serialize control-plane state preparation with reset.
}

type data = map[string]any

// New creates the unauthenticated local control plane.
func New(cfg config.Config, version string, state *store.Store, events *event.Dispatcher, faults *fault.Engine, clock *simclock.Manual) http.Handler {
	c := &control{cfg: cfg, version: version, state: state, events: events, faults: faults, clock: clock}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_sandbox/state", c.snapshot)
	mux.HandleFunc("GET /_sandbox/openapi.json", describeOpenAPI)
	mux.HandleFunc("GET /_sandbox/clock", c.clockStatus)
	mux.HandleFunc("POST /_sandbox/clock/advance", c.advanceClock)
	mux.HandleFunc("POST /_sandbox/reset", c.reset)
	mux.HandleFunc("POST /_sandbox/seed", c.seed)
	mux.HandleFunc("GET /_sandbox/chats", c.chats)
	mux.HandleFunc("POST /_sandbox/chats/open", c.openBotChat)
	mux.HandleFunc("GET /_sandbox/chats/{chat_id}/messages", c.messages)
	mux.HandleFunc("GET /_sandbox/cards", c.cards)
	mux.HandleFunc("GET /_sandbox/cards/{card_id}", c.card)
	mux.HandleFunc("POST /_sandbox/cards/validate", validateCard)
	mux.HandleFunc("GET /_sandbox/images/{image_key}", c.cardImage)
	mux.HandleFunc("GET /_sandbox/ws/sessions", c.sessions)
	mux.HandleFunc("POST /_sandbox/messages/send", c.send)
	mux.HandleFunc("POST /_sandbox/cards/action", c.cardAction)
	mux.HandleFunc("POST /_sandbox/events/webhook/verify", c.verifyWebhook)
	mux.HandleFunc("POST /_sandbox/events/inject", c.injectEvent)
	mux.HandleFunc("GET /_sandbox/bots/{app_id}/menu", c.getBotMenu)
	mux.HandleFunc("POST /_sandbox/bots/{app_id}/menu/click", c.clickBotMenu)
	mux.HandleFunc("GET /_sandbox/traces", c.traces)
	mux.HandleFunc("DELETE /_sandbox/traces", c.clearTraces)
	mux.HandleFunc("GET /_sandbox/faults", c.listFaults)
	mux.HandleFunc("POST /_sandbox/faults", c.addFault)
	mux.HandleFunc("DELETE /_sandbox/faults/{fault_id}", c.deleteFault)
	mux.HandleFunc("DELETE /_sandbox/faults", c.clearFaults)
	mux.HandleFunc("POST /_sandbox/tokens/expire", c.expireTokens)
	mux.HandleFunc("GET /_sandbox/apps/{app_id}/grants", c.authorizedUsers)
	mux.HandleFunc("POST /_sandbox/auth/devices/decision", c.decideDevice)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		_, pattern := mux.Handler(r)
		if pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}
		var allow []string
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodDelete} {
			probe := *r
			probe.Method = method
			if _, pattern := mux.Handler(&probe); pattern != "" {
				allow = append(allow, method)
			}
		}
		if len(allow) == 0 {
			fail(w, http.StatusNotFound, "unknown sandbox endpoint")
			return
		}
		for _, method := range allow {
			w.Header().Add("Allow", method)
		}
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
	})
}

func reply(w http.ResponseWriter, status int, value any) {
	_ = protocol.WriteJSON(w, status, value)
}

func fail(w http.ResponseWriter, status int, message string) {
	reply(w, status, data{"error": message})
}

func read(w http.ResponseWriter, r *http.Request, dst any, optional bool) bool {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		var oversized *http.MaxBytesError
		if errors.As(err, &oversized) {
			fail(w, http.StatusRequestEntityTooLarge, "request body exceeds 1 MiB")
		} else {
			fail(w, http.StatusBadRequest, "could not read request body")
		}
		return false
	}
	raw = bytes.TrimSpace(raw)
	if optional && len(raw) == 0 {
		return true
	}
	if len(raw) == 0 || raw[0] != '{' || !json.Valid(raw) {
		fail(w, http.StatusBadRequest, "request body must be one JSON object")
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(dst); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return false
	}
	return true
}

func stateError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrMessageCardNotFound):
		fail(w, http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrAlreadyExists), errors.Is(err, store.ErrMessageDeleted), errors.Is(err, store.ErrCardExpired):
		fail(w, http.StatusConflict, err.Error())
	default:
		fail(w, http.StatusBadRequest, err.Error())
	}
}
