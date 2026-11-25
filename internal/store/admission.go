package store

import (
	"context"
	"errors"
)

type admissionKey struct{}
type requestIDKey struct{}

type admission struct {
	store *Store
	reset uint64
}

// WithRequestID associates a generated correlation ID with a request.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestID returns the correlation ID inherited from the incoming request.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// ErrReset indicates that a request was admitted before the current reset.
var ErrReset = errors.New("sandbox reset during request")

// Admit binds an Open API request to the current reset and audit generations.
func (s *Store) Admit(ctx context.Context) (context.Context, uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return context.WithValue(ctx, admissionKey{}, admission{s, s.resetEpoch}), s.traceEpoch
}

// Current reports whether a scoped request still belongs to this sandbox state.
func (s *Store) Current(ctx context.Context) bool {
	scope, ok := ctx.Value(admissionKey{}).(admission)
	if !ok {
		return true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return scope.store == s && scope.reset == s.resetEpoch
}

// ResetEpoch identifies the current runtime state generation.
func (s *Store) ResetEpoch() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.resetEpoch
}

// Commit fences a store mutation against reset. Callers perform request I/O
// before Commit and send responses or enqueue events after Commit returns.
func Commit[T any](ctx context.Context, s *Store, change func() (T, error)) (T, error) {
	s.resetGate.RLock()
	defer s.resetGate.RUnlock()
	if !s.Current(ctx) {
		var zero T
		return zero, ErrReset
	}
	return change()
}

// Mutate fences a store mutation that returns only an error.
func (s *Store) Mutate(ctx context.Context, change func() error) error {
	_, err := Commit(ctx, s, func() (struct{}, error) { return struct{}{}, change() })
	return err
}
