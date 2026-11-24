// Package fault matches and applies one-shot or persistent sandbox fault rules.
package fault

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
)

// Kind identifies the effect of a matching rule.
type Kind string

// Supported fault effects.
const (
	Latency        Kind = "latency"
	RateLimit      Kind = "rate_limit"
	HTTPError      Kind = "http_error"
	TokenExpiry    Kind = "token_expiry" //nolint:gosec // G101 mistakes the public rule name for a credential.
	DuplicateEvent Kind = "duplicate_event"
)

// Match describes the fields shared by rules and requests or deliveries.
type Match struct {
	AppID      string `json:"app_id,omitempty"`
	Method     string `json:"method,omitempty"`
	PathPrefix string `json:"path_prefix,omitempty"`
	EventType  string `json:"event_type,omitempty"`
	Channel    string `json:"channel,omitempty"`
}

// Spec is the JSON representation used to create a rule.
type Spec struct {
	Match
	Kind           Kind     `json:"kind"`
	RemainingHits  *int     `json:"remaining_hits,omitempty"`
	Probability    *float64 `json:"probability,omitempty"`
	DelayMS        *int     `json:"delay_ms,omitempty"`
	RetryAfter     *int     `json:"retry_after,omitempty"`
	StatusCode     *int     `json:"status_code,omitempty"`
	ErrorCode      *int     `json:"error_code,omitempty"`
	ErrorMsg       *string  `json:"error_msg,omitempty"`
	DuplicateCount *int     `json:"duplicate_count,omitempty"`
}

// Rule describes an active fault and its remaining budget.
type Rule struct {
	ID string `json:"fault_id"`
	Spec
}

// Effect contains the parameters of a matched fault.
type Effect struct {
	Kind           Kind
	Delay          time.Duration
	RetryAfter     int
	StatusCode     int
	ErrorCode      protocol.Code
	ErrorMsg       string
	DuplicateCount int
	done           <-chan struct{}
}

// Wait waits for the configured latency or cancellation by the caller, reset, or shutdown.
func (e Effect) Wait(ctx context.Context) error {
	timer := time.NewTimer(e.Delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-e.done:
		return context.Canceled
	case <-timer.C:
		return ctx.Err()
	}
}

// Engine owns an ordered list of active rules.
type Engine struct {
	mu      sync.RWMutex
	rules   []Rule
	random  func() float64
	done    chan struct{}
	stopped bool
}

// New creates an empty engine. A nil sampler uses the standard random source.
func New(sample func() float64) *Engine {
	if sample == nil {
		sample = rand.Float64
	}
	return &Engine{random: sample, done: make(chan struct{})}
}

// Add validates and appends a rule.
func (e *Engine) Add(spec Spec) (Rule, error) {
	spec = spec.clone()
	spec.Method = strings.ToUpper(spec.Method)
	if err := spec.validate(); err != nil {
		return Rule{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopped {
		return Rule{}, errors.New("fault engine has stopped")
	}
	rule := Rule{ID: protocol.NewID("fault_"), Spec: spec}
	e.rules = append(e.rules, rule)
	rule.Spec = spec.clone()
	return rule, nil
}

// List snapshots all active rules in creation order.
func (e *Engine) List() []Rule {
	e.mu.RLock()
	defer e.mu.RUnlock()
	list := make([]Rule, 0, len(e.rules))
	for _, rule := range e.rules {
		rule.Spec = rule.clone()
		list = append(list, rule)
	}
	return list
}

// Delete removes a rule by ID.
func (e *Engine) Delete(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, rule := range e.rules {
		if rule.ID == id {
			e.remove(i)
			return true
		}
	}
	return false
}

func (e *Engine) remove(i int) {
	copy(e.rules[i:], e.rules[i+1:])
	e.rules[len(e.rules)-1] = Rule{}
	e.rules = e.rules[:len(e.rules)-1]
}

// Reset clears all rules and cancels active latency waits.
func (e *Engine) Reset() { e.clear(false) }

// Stop clears all rules and cancels waits permanently.
func (e *Engine) Stop() { e.clear(true) }

func (e *Engine) clear(stop bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.stopped {
		e.rules = nil
		close(e.done)
		e.done = make(chan struct{})
		e.stopped = stop
	}
}

// NeedsApp reports whether identifying the target app could change an inbound match.
func (e *Engine) NeedsApp(input Match) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, rule := range e.rules {
		if rule.AppID != "" && rule.Kind != TokenExpiry {
			input.AppID = rule.AppID
			if rule.matches(input) {
				return true
			}
		}
	}
	return false
}

// Match applies the first eligible rule in creation order. Token expiry
// requires a valid credential of a kind accepted by the route.
func (e *Engine) Match(input Match, expiryEligible bool) Effect {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.match(input, expiryEligible)
}

// MatchBeforeBearer defers selection when expiry could win and the route's
// Bearer requirement is hidden by a wrapper. The preflight spends no hits or
// random samples. The caller completes a deferred match at authentication.
func (e *Engine) MatchBeforeBearer(input Match) (effect Effect, deferred bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, rule := range e.rules {
		if !rule.matches(input) {
			continue
		}
		if rule.Kind == TokenExpiry {
			return Effect{}, true
		}
		if rule.Probability == nil || *rule.Probability == 1 {
			break
		}
	}
	return e.match(input, false), false
}

func (e *Engine) match(input Match, expiryEligible bool) Effect {
	for i, rule := range e.rules {
		if (rule.Kind == TokenExpiry && !expiryEligible) || !rule.matches(input) ||
			(rule.Probability != nil && *rule.Probability < 1 && e.random() >= *rule.Probability) {
			continue
		}
		if rule.RemainingHits != nil {
			(*rule.RemainingHits)--
			if *rule.RemainingHits == 0 {
				e.remove(i)
			}
		}
		return Effect{
			Kind: rule.Kind, Delay: time.Duration(or(rule.DelayMS, 0)) * time.Millisecond,
			RetryAfter: or(rule.RetryAfter, 1), StatusCode: or(rule.StatusCode, http.StatusInternalServerError),
			ErrorCode: protocol.Code(or(rule.ErrorCode, 99991401)), ErrorMsg: or(rule.ErrorMsg, "internal server error"),
			DuplicateCount: or(rule.DuplicateCount, 2), done: e.done,
		}
	}
	return Effect{}
}

func (s Spec) matches(m Match) bool {
	if (s.Channel != "" && s.Channel != m.Channel) || (s.AppID != "" && s.AppID != m.AppID) ||
		(s.Method != "" && s.Method != m.Method) || (s.EventType != "" && s.EventType != m.EventType) ||
		!strings.HasPrefix(m.PathPrefix, s.PathPrefix) {
		return false
	}
	switch s.Kind {
	case TokenExpiry, RateLimit, HTTPError:
		return m.Channel == "inbound"
	case DuplicateEvent:
		return m.Channel != "inbound"
	default:
		return true
	}
}

func (s Spec) validate() error {
	switch s.Channel {
	case "", "inbound", "webhook", "websocket":
	default:
		return errors.New("channel must be inbound, webhook, or websocket")
	}
	if s.PathPrefix != "" && !strings.HasPrefix(s.PathPrefix, "/") {
		return errors.New("path_prefix must start with /")
	}
	if strings.Trim(s.Method, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" {
		return errors.New("method must contain only letters")
	}
	if s.RemainingHits != nil && *s.RemainingHits <= 0 {
		return errors.New("remaining_hits must be positive")
	}
	if p := s.Probability; p != nil && (math.IsNaN(*p) || *p <= 0 || *p > 1) {
		return errors.New("probability must be in (0, 1]")
	}
	if (s.DelayMS != nil && s.Kind != Latency) || (s.RetryAfter != nil && s.Kind != RateLimit) ||
		((s.StatusCode != nil || s.ErrorCode != nil || s.ErrorMsg != nil) && s.Kind != HTTPError) ||
		(s.DuplicateCount != nil && s.Kind != DuplicateEvent) {
		return errors.New("fault parameters must match the rule kind")
	}
	switch s.Kind {
	case Latency:
		if ms := or(s.DelayMS, 0); ms < 1 || ms > 60000 {
			return errors.New("delay_ms must be between 1 and 60000")
		}
	case RateLimit, HTTPError, TokenExpiry:
		if (s.Channel != "" && s.Channel != "inbound") || s.EventType != "" {
			return errors.New("this fault kind only matches inbound requests")
		}
		if retry := or(s.RetryAfter, 1); retry < 0 || retry > 86400 {
			return errors.New("retry_after must be between 0 and 86400 seconds")
		}
		if code := or(s.StatusCode, 500); code < 400 || code > 599 {
			return errors.New("status_code must be between 400 and 599")
		}
		if or(s.ErrorCode, 1) <= 0 || (s.ErrorMsg != nil && strings.TrimSpace(*s.ErrorMsg) == "") {
			return errors.New("error_code and error_msg must be nonzero and nonempty")
		}
	case DuplicateEvent:
		if s.Channel == "inbound" || or(s.DuplicateCount, 2) < 2 || or(s.DuplicateCount, 2) > 10 {
			return errors.New("duplicate_event requires an outbound channel and duplicate_count between 2 and 10")
		}
	default:
		return errors.New("unknown fault kind")
	}
	return nil
}

func (s Spec) clone() Spec {
	s.RemainingHits, s.Probability = clone(s.RemainingHits), clone(s.Probability)
	s.DelayMS, s.RetryAfter = clone(s.DelayMS), clone(s.RetryAfter)
	s.StatusCode, s.ErrorCode, s.ErrorMsg = clone(s.StatusCode), clone(s.ErrorCode), clone(s.ErrorMsg)
	s.DuplicateCount = clone(s.DuplicateCount)
	return s
}

func or[T any](ptr *T, fallback T) T {
	if ptr == nil {
		return fallback
	}
	return *ptr
}

func clone[T any](ptr *T) *T {
	if ptr == nil {
		return nil
	}
	value := *ptr
	return &value
}
