// Package server wires the sandbox HTTP server and its route groups.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/event"
	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/approval"
	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/auth"
	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/bot"
	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/cardkit"
	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/contact"
	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/content"
	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/im"
	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/middleware"
	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/work"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/sandbox"
	"github.com/littlecrabzzz-alt/larkbox/internal/sandbox/fault"
	"github.com/littlecrabzzz-alt/larkbox/internal/simclock"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
	"github.com/littlecrabzzz-alt/larkbox/internal/webui"
)

// Version identifies the current development build.
var Version = "0.1.0-dev"

// Server is both an HTTP handler and a gracefully stoppable HTTP server.
type Server struct {
	http   *http.Server
	store  *store.Store
	events *event.Dispatcher
	faults *fault.Engine
}

// RegisterRoutes attaches handlers to one route group. Patterns include the
// group's full path prefix, such as "POST /open-apis/auth/v3/...".
type RegisterRoutes func(*http.ServeMux, config.Config, *store.Store)

// Option customizes a server before it starts accepting requests.
type Option func(*options)

type options struct {
	feishu       []RegisterRoutes
	sandbox      []RegisterRoutes
	fault        middleware.FaultHook
	emitter      im.EventEmitter
	eventOptions event.Options
	logger       *slog.Logger
	noUI         bool
}

// WithoutWebUI disables the embedded inspector.
func WithoutWebUI() Option {
	return func(opts *options) { opts.noUI = true }
}

// WithFeishuRoutes registers routes under /open-apis/. Annotate outer-wrapped
// Bearer handlers with middleware.WithBearerPolicy to enable expiry faults.
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

// WithEventEmitter subscribes to committed IM mutations.
func WithEventEmitter(emitter im.EventEmitter) Option {
	return func(opts *options) { opts.emitter = emitter }
}

// WithEventOptions configures the built-in event dispatcher.
func WithEventOptions(opts event.Options) Option {
	return func(settings *options) { settings.eventOptions = opts }
}

// WithLogger records request and delivery metadata to logger.
func WithLogger(logger *slog.Logger) Option {
	return func(opts *options) { opts.logger = logger }
}

type eventEmitter struct {
	state      *store.Store
	dispatcher *event.Dispatcher
	next       im.EventEmitter
}

func (e eventEmitter) Emit(ctx context.Context, notification im.Event) {
	switch notification.Type {
	case "im.message.recalled_v1":
		e.dispatcher.EnqueueMessageRecalled(ctx, notification.AppID, notification.ChatID, notification.MessageID, notification.RecallType, notification.At)
	case "im.chat.member.bot.added_v1", "im.chat.member.bot.deleted_v1":
		e.dispatcher.EnqueueBotMembership(ctx, notification.AppID, notification.ChatID, notification.ActorID, notification.ActorAppID, notification.At, notification.Type == "im.chat.member.bot.added_v1")
	default:
		if notification.Payload != nil {
			e.dispatcher.Enqueue(ctx, notification.AppID, notification.Type, notification.At, notification.Payload)
		}
	}
	if e.next != nil && e.state.Current(ctx) {
		e.next.Emit(ctx, notification)
	}
}

// New creates and seeds a server and registers the configured routes.
func New(cfg config.Config, opts ...Option) *Server {
	settings := options{eventOptions: configuredDelivery(cfg.EventDelivery)}
	for _, opt := range opts {
		opt(&settings)
	}
	var clock *simclock.Manual
	var now func() time.Time
	if cfg.ClockStart != nil {
		clock = simclock.New(*cfg.ClockStart)
		now = clock.Now
	}
	state := store.New(now, 0)
	state.Seed(cfg)
	faults := fault.New(nil)
	settings.eventOptions.Faults = faults
	settings.eventOptions.ManualClock = clock
	if settings.logger != nil {
		settings.eventOptions.Logger = settings.logger
	}
	events := event.New(state, settings.eventOptions)
	limits := middleware.NewRateLimiter(cfg.RateLimits)

	feishu := http.NewServeMux()
	auth.RegisterRoutes(feishu, cfg, state)
	auth.RegisterAccountsRoutes(feishu, cfg, state, events)
	bot.RegisterRoutes(feishu, state)
	cardkit.RegisterRoutes(feishu, state)
	contact.RegisterRoutes(feishu, cfg, state)
	content.RegisterRoutes(feishu, cfg, state)
	work.RegisterRoutes(feishu, cfg, state)
	approval.RegisterRoutes(feishu, cfg, state, events)
	im.RegisterRoutes(feishu, state, eventEmitter{state: state, dispatcher: events, next: settings.emitter})
	for _, register := range settings.feishu {
		register(feishu, cfg, state)
	}
	extensions := http.NewServeMux()
	for _, register := range settings.sandbox {
		register(extensions, cfg, state)
	}

	mux := http.NewServeMux()
	events.RegisterRoutes(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_ = protocol.WriteJSON(w, http.StatusOK, struct {
			Status     string `json:"status"`
			Version    string `json:"version"`
			StrictMode bool   `json:"strict_mode"`
		}{Status: "ok", Version: Version, StrictMode: cfg.StrictMode})
	})
	api := middleware.AuditWithLogger(state, limits.Wrap(state, feishu,
		middleware.Fault(settings.fault, middleware.SandboxFaults(faults, state, feishu, openAPIHandler(feishu)))), settings.logger)
	mux.Handle("/open-apis/", api)
	mux.Handle("/oauth/", api)
	mux.Handle("/page/cli", api)
	control := sandbox.New(cfg, Version, state, events, faults, clock)
	operator := operatorRoutes(cfg, events, limits)
	controlPlane := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := operator.Handler(r); pattern != "" {
			operator.ServeHTTP(w, r)
			return
		}
		if _, pattern := extensions.Handler(r); pattern != "" {
			extensions.ServeHTTP(w, r)
			return
		}
		control.ServeHTTP(w, r)
	})
	mux.Handle("/_sandbox/", protectControl(cfg.SandboxToken, correlatedControl(controlPlane)))

	return &Server{
		http: &http.Server{
			Addr: cfg.Addr, Handler: withWebUI(mux, settings.noUI, cfg.SandboxToken), ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
		},
		store: state, events: events, faults: faults,
	}
}

func configuredDelivery(settings config.EventDelivery) event.Options {
	opts := event.Options{
		WebhookTimeout:       time.Duration(settings.WebhookTimeoutMS) * time.Millisecond,
		VerificationTimeout:  time.Duration(settings.VerificationTimeoutMS) * time.Millisecond,
		AckTimeout:           time.Duration(settings.AckTimeoutMS) * time.Millisecond,
		WebhookMaxAttempts:   settings.WebhookMaxAttempts,
		WebSocketMaxAttempts: settings.WebSocketMaxAttempts,
	}
	if len(settings.RetryBackoffMS) > 0 {
		backoff := append([]int64(nil), settings.RetryBackoffMS...)
		opts.RetryBackoff = func(attempt int) time.Duration {
			return time.Duration(backoff[min(attempt, len(backoff))-1]) * time.Millisecond
		}
	}
	return opts
}

func withWebUI(routes http.Handler, disabled bool, secret string) http.Handler {
	if disabled {
		return routes
	}
	inspector := protectControl(secret, http.HandlerFunc(webui.ServeHTTP))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/page/cli" {
			routes.ServeHTTP(w, r)
			return
		}
		prefix, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		switch prefix {
		case "open-apis", "oauth", "_sandbox", "callback", "ws", "healthz":
			routes.ServeHTTP(w, r)
		default:
			inspector.ServeHTTP(w, r)
		}
	})
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

// Events returns the built-in event dispatcher.
func (s *Server) Events() *event.Dispatcher { return s.events }

// TriggerCardAction simulates a user action on an interactive message.
func (s *Server) TriggerCardAction(ctx context.Context, input event.CardActionTriggerInput) (event.CardActionResult, error) {
	return s.events.TriggerCardAction(ctx, input)
}

// Handler returns the server routing tree.
func (s *Server) Handler() http.Handler { return s.http.Handler }

// ServeHTTP dispatches an HTTP request to the server routing tree.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.Handler().ServeHTTP(w, r)
}

// Serve accepts HTTP connections until the listener or server closes.
func (s *Server) Serve(listener net.Listener) error { return s.http.Serve(listener) }

// Shutdown stops accepting connections and waits for active requests to finish.
func (s *Server) Shutdown(ctx context.Context) error {
	s.faults.Stop()
	s.events.StopRetries()
	httpErr := s.http.Shutdown(ctx)
	return errors.Join(httpErr, s.events.Shutdown(ctx))
}
