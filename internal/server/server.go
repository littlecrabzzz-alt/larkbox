// Package server wires the sandbox HTTP server and its route groups.
package server

import (
	"context"
	"net"
	"net/http"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/middleware"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

// Version identifies the current development build.
const Version = "0.1.0-dev"

// Server is both an HTTP handler and a gracefully stoppable HTTP server.
type Server struct {
	http  *http.Server
	store *store.Store
}

// RegisterRoutes attaches handlers to one route group. Patterns include the
// group's full path prefix, such as "POST /open-apis/auth/v3/...".
type RegisterRoutes func(*http.ServeMux, config.Config, *store.Store)

// Option customizes a server before it starts accepting requests.
type Option func(*options)

type options struct {
	feishu  []RegisterRoutes
	sandbox []RegisterRoutes
	fault   middleware.FaultHook
}

// WithFeishuRoutes registers routes under /open-apis/.
func WithFeishuRoutes(register RegisterRoutes) Option {
	return func(opts *options) { opts.feishu = append(opts.feishu, register) }
}

// WithSandboxRoutes registers routes under /_sandbox/.
func WithSandboxRoutes(register RegisterRoutes) Option {
	return func(opts *options) { opts.sandbox = append(opts.sandbox, register) }
}

// WithFaultHook installs a hook for Open API requests.
func WithFaultHook(hook middleware.FaultHook) Option {
	return func(opts *options) { opts.fault = hook }
}

// New creates and seeds a server and registers the configured routes.
func New(cfg config.Config, opts ...Option) *Server {
	settings := options{}
	for _, opt := range opts {
		opt(&settings)
	}
	state := store.New(nil, 0)
	state.Seed(cfg)

	feishu := http.NewServeMux()
	for _, register := range settings.feishu {
		register(feishu, cfg, state)
	}
	sandbox := http.NewServeMux()
	for _, register := range settings.sandbox {
		register(sandbox, cfg, state)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_ = protocol.WriteJSON(w, http.StatusOK, struct {
			Status     string `json:"status"`
			Version    string `json:"version"`
			StrictMode bool   `json:"strict_mode"`
		}{Status: "ok", Version: Version, StrictMode: cfg.StrictMode})
	})
	mux.Handle("/open-apis/", middleware.Audit(state, middleware.Fault(settings.fault, openAPIHandler(feishu))))
	mux.Handle("/_sandbox/", sandbox)

	return &Server{
		http: &http.Server{
			Addr: cfg.Addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
		},
		store: state,
	}
}

// An empty ServeMux pattern means either 404 or 405. Translate the former
// while letting ServeMux generate Allow and path parameters as usual.
func openAPIHandler(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler, pattern := mux.Handler(r)
		if pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}
		handler.ServeHTTP(&openAPIFallbackWriter{ResponseWriter: w}, r)
	})
}

type openAPIFallbackWriter struct {
	http.ResponseWriter
	translated bool
}

func (w *openAPIFallbackWriter) WriteHeader(status int) {
	if status != http.StatusNotFound {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.translated = true
	_ = protocol.WriteError(w.ResponseWriter, protocol.NewError(protocol.ResourceNotFound, nil))
}

func (w *openAPIFallbackWriter) Write(body []byte) (int, error) {
	if w.translated {
		return len(body), nil
	}
	return w.ResponseWriter.Write(body)
}

// Store returns the server's virtual state.
func (s *Server) Store() *store.Store { return s.store }

// ServeHTTP dispatches an HTTP request to the server routing tree.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.http.Handler.ServeHTTP(w, r)
}

// Serve accepts HTTP connections until the listener or server closes.
func (s *Server) Serve(listener net.Listener) error { return s.http.Serve(listener) }

// Shutdown stops accepting connections and waits for active requests to finish.
func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }
