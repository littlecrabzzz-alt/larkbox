package middleware

import (
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

// RateLimiter maintains an application's request budgets for each sandbox generation.
type RateLimiter struct {
	settings config.RateLimits
	mu       sync.Mutex
	epoch    uint64
	apps     map[string]appLimits
	rejected atomic.Uint64
}

type rateWindow struct {
	end  time.Time
	used int
}

type appLimits struct {
	tenant             string
	second, day, month rateWindow
}

type rateLease struct {
	app, tenant string
	epoch       uint64
	day, month  time.Time
	limit       int
	wait        int64
	err         protocol.APIError
}

// NewRateLimiter configures budgets for authenticated Open API calls.
func NewRateLimiter(settings config.RateLimits) *RateLimiter {
	return &RateLimiter{settings: settings.Effective(), apps: make(map[string]appLimits)}
}

// Rejected returns the total requests declined by the configured budgets.
func (l *RateLimiter) Rejected() uint64 { return l.rejected.Load() }

// Wrap meters authenticated Bearer routes before their handlers mutate state.
func (l *RateLimiter) Wrap(state *store.Store, routes *http.ServeMux, next http.Handler) http.Handler {
	if l.settings.Profile != "realistic" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler, _ := routes.Handler(r)
		route, metered := handler.(interface{ bearerPolicy() RequiredToken })
		if !metered {
			next.ServeHTTP(w, r)
			return
		}
		identity, invalid := authenticate(state, route.bearerPolicy(), r.Header.Values("Authorization"))
		if invalid.Code != protocol.Success {
			next.ServeHTTP(w, r)
			return
		}
		SetTraceApp(r, identity.AppID)
		lease, err := store.Commit(r.Context(), state, func() (rateLease, error) {
			return l.reserve(identity, state.Now, state.ResetEpoch()), nil
		})
		if WriteResetConflict(w, err) {
			return
		}
		if lease.err.Code != protocol.Success {
			l.rejected.Add(1)
			wait := strconv.FormatInt(lease.wait, 10)
			w.Header().Set("X-Ogw-Ratelimit-Limit", strconv.Itoa(lease.limit))
			w.Header().Set("X-Ogw-Ratelimit-Reset", wait)
			w.Header().Set("Retry-After", wait)
			_ = protocol.WriteError(w, lease.err)
			return
		}
		if l.settings.DailyQuota == 0 && l.settings.MonthlyQuota == 0 {
			next.ServeHTTP(w, r)
			return
		}
		observed := &rateWriter{ResponseWriter: w}
		defer func() {
			if !observed.finished || observed.status >= 400 || observed.code != protocol.Success {
				l.refund(lease)
			}
		}()
		var wrapped http.ResponseWriter = observed
		if _, ok := w.(http.Flusher); ok {
			wrapped = &flushRateWriter{observed}
		}
		next.ServeHTTP(wrapped, r)
		observed.finished = true
	})
}

func (l *RateLimiter) reserve(auth Auth, clock func() time.Time, epoch uint64) rateLease {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.epoch != epoch {
		clear(l.apps)
		l.epoch = epoch
	}
	current := l.apps[auth.AppID]
	if current.tenant != auth.TenantKey {
		current = appLimits{tenant: auth.TenantKey}
	}
	now := clock().UTC()
	y, month, day := now.Date()
	current.second = openWindow(current.second, now.Truncate(time.Second).Add(time.Second))
	current.day = openWindow(current.day, time.Date(y, month, day+1, 0, 0, 0, 0, time.UTC))
	current.month = openWindow(current.month, time.Date(y, month+1, 1, 0, 0, 0, 0, time.UTC))
	for _, budget := range []struct {
		window rateWindow
		limit  int
		code   protocol.Code
		daily  bool
	}{
		{current.month, l.settings.MonthlyQuota, protocol.QuotaExceeded, false},
		{current.day, l.settings.DailyQuota, protocol.QuotaExceeded, true},
		{current.second, l.settings.QPS, protocol.RateLimited, false},
	} {
		if budget.limit > 0 && budget.window.used >= budget.limit {
			failure := protocol.NewError(budget.code, nil)
			if budget.daily {
				failure.Message = "sandbox daily API call quota has been exceeded"
			}
			wait := (budget.window.end.Sub(now) + time.Second - 1) / time.Second
			return rateLease{limit: budget.limit, wait: int64(wait), err: failure}
		}
	}
	current.second.used++
	if l.settings.DailyQuota > 0 {
		current.day.used++
	}
	if l.settings.MonthlyQuota > 0 {
		current.month.used++
	}
	l.apps[auth.AppID] = current
	return rateLease{app: auth.AppID, tenant: auth.TenantKey, epoch: epoch, day: current.day.end, month: current.month.end}
}

func openWindow(old rateWindow, until time.Time) rateWindow {
	if old.end.Equal(until) {
		return old
	}
	return rateWindow{end: until}
}

func (l *RateLimiter) refund(lease rateLease) {
	l.mu.Lock()
	defer l.mu.Unlock()
	current, ok := l.apps[lease.app]
	if !ok || l.epoch != lease.epoch || current.tenant != lease.tenant {
		return
	}
	if current.day.end.Equal(lease.day) && current.day.used > 0 {
		current.day.used--
	}
	if current.month.end.Equal(lease.month) && current.month.used > 0 {
		current.month.used--
	}
	l.apps[lease.app] = current
}

type rateWriter struct {
	http.ResponseWriter
	status   int
	code     protocol.Code
	finished bool
}

func (w *rateWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *rateWriter) WriteHeader(status int) {
	if w.status == 0 && status >= 200 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *rateWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(body)
}

func (w *rateWriter) SetFeishuCode(code protocol.Code) {
	if w.status == 0 {
		w.code = code
		if sink, ok := w.ResponseWriter.(interface{ SetFeishuCode(protocol.Code) }); ok {
			sink.SetFeishuCode(code)
		}
	}
}

type flushRateWriter struct{ *rateWriter }

func (w *flushRateWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}
