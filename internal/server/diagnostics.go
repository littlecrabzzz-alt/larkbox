package server

import (
	"fmt"
	"net/http"
	"net/http/pprof"
	"strconv"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/event"
	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/middleware"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

func operatorRoutes(cfg config.Config, events *event.Dispatcher, limits *middleware.RateLimiter) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_sandbox/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		value := events.Metrics()
		_, _ = fmt.Fprintf(w, "# TYPE larkbox_pending_deliveries gauge\nlarkbox_pending_deliveries %d\n"+
			"# TYPE larkbox_retry_waiters gauge\nlarkbox_retry_waiters %d\n"+
			"# TYPE larkbox_delivery_retries_total counter\nlarkbox_delivery_retries_total %d\n"+
			"# TYPE larkbox_delivery_attempts_total counter\nlarkbox_delivery_attempts_total %d\n"+
			"# TYPE larkbox_delivery_failures_total counter\nlarkbox_delivery_failures_total %d\n"+
			"# TYPE larkbox_rate_limited_requests_total counter\nlarkbox_rate_limited_requests_total %d\n",
			value.Pending, value.RetryWaiters, value.Retries, value.Attempts, value.Failures, limits.Rejected())
	})
	if cfg.Pprof && cfg.SandboxToken != "" {
		const prefix = "/_sandbox/debug/pprof/"
		for _, profile := range []string{"allocs", "block", "goroutine", "heap", "mutex", "threadcreate"} {
			mux.Handle("GET "+prefix+profile, shortProfile(pprof.Handler(profile)))
		}
		mux.HandleFunc("GET "+prefix+"{$}", func(w http.ResponseWriter, r *http.Request) {
			http.StripPrefix("/_sandbox", http.HandlerFunc(pprof.Index)).ServeHTTP(w, r)
		})
		mux.Handle("GET "+prefix+"profile", shortProfile(http.HandlerFunc(pprof.Profile)))
	}
	return mux
}

func shortProfile(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if raw := r.URL.Query().Get("seconds"); raw != "" {
			seconds, err := strconv.Atoi(raw)
			if err != nil || seconds < 1 || seconds > 30 {
				http.Error(w, "seconds must be between 1 and 30", http.StatusBadRequest)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func correlatedControl(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := protocol.NewID("req_")
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(store.WithRequestID(r.Context(), id)))
	})
}
