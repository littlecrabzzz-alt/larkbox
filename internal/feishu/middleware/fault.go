package middleware

import "net/http"

// FaultHook decides whether to intercept an Open API request. BeforeRequest
// returns true if it has written a response and the downstream handler can stop.
type FaultHook interface {
	BeforeRequest(http.ResponseWriter, *http.Request) bool
}

// Fault activates the supplied fault hook. A nil hook leaves next as-is.
func Fault(hook FaultHook, next http.Handler) http.Handler {
	if hook == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hook.BeforeRequest(w, r) {
			next.ServeHTTP(w, r)
		}
	})
}
