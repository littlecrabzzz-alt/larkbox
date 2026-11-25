package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"net/url"
	"strings"

	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
)

func protectControl(secret string, next http.Handler) http.Handler {
	if secret == "" {
		return next
	}
	expected := sha256.Sum256([]byte(secret))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		headers := r.Header.Values("Authorization")
		var supplied string
		browser := false
		if len(headers) == 1 {
			scheme, value, _ := strings.Cut(headers[0], " ")
			switch {
			case strings.EqualFold(scheme, "Bearer"):
				supplied = value
			case strings.EqualFold(scheme, "Basic"):
				browser = true
				name, password, ok := r.BasicAuth()
				if ok && name == "sandbox" {
					supplied = password
				}
			}
		}
		sum := sha256.Sum256([]byte(supplied))
		if subtle.ConstantTimeCompare(sum[:], expected[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="Larkbox sandbox", charset="UTF-8"`)
			_ = protocol.WriteJSON(w, http.StatusUnauthorized, map[string]string{"error": "sandbox authentication required"})
			return
		}
		if browser && !sameOriginControl(r) {
			_ = protocol.WriteJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin sandbox request"})
			return
		}
		// Credentials stay in the control middleware, including for extensions.
		request := r.Clone(r.Context())
		request.Header.Del("Authorization")
		next.ServeHTTP(w, request)
	})
}

func sameOriginControl(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" || site == "same-site" {
		return false
	}
	raw := r.Header.Get("Origin")
	if raw == "" {
		return true
	}
	origin, err := url.Parse(raw)
	return err == nil && (origin.Scheme == "http" || origin.Scheme == "https") && strings.EqualFold(origin.Host, r.Host)
}
