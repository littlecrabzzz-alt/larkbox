package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

// auditBodyLimit caps memory used to record each HTTP body.
const auditBodyLimit = 64 * 1024

// Audit records bounded request and response snapshots for Open API calls.
// It omits credential headers, redacts JSON credentials, and skips body
// formats that cannot be safely redacted, such as multipart forms.
func Audit(state *store.Store, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		trace := store.Trace{
			Direction: store.Inbound, StartedAt: started, Method: r.Method, Path: r.URL.EscapedPath(),
			RequestHeaders: auditHeaders(r.Header),
		}
		if r.Body != nil && r.Body != http.NoBody {
			body, err := io.ReadAll(io.LimitReader(r.Body, auditBodyLimit+1))
			trace.RequestBodyTruncated = len(body) > auditBodyLimit || err != nil
			trace.RequestBody = auditBody(body, r.Header.Get("Content-Type"), trace.RequestBodyTruncated)
			r.Body = replayBody(r.Body, body, err)
		}
		capture := &auditWriter{ResponseWriter: w}
		next.ServeHTTP(capture, r)
		trace.Status = capture.status
		if trace.Status == 0 {
			trace.Status = http.StatusOK
		}
		trace.Latency = time.Since(started)
		trace.ResponseHeaders = auditHeaders(capture.Header())
		trace.ResponseBodyTruncated = capture.truncated
		trace.ResponseBody = auditBody(capture.body.Bytes(), capture.Header().Get("Content-Type"), capture.truncated)
		if !capture.truncated {
			var envelope struct {
				Code *int `json:"code"`
			}
			if json.Unmarshal(capture.body.Bytes(), &envelope) == nil {
				trace.Code = envelope.Code
			}
		}
		state.AppendTrace(trace)
	})
}

type readCloser struct {
	io.Reader
	io.Closer
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

func replayBody(original io.ReadCloser, prefix []byte, readErr error) io.ReadCloser {
	var tail io.Reader = original
	if readErr != nil {
		tail = errorReader{err: readErr}
	}
	return readCloser{Reader: io.MultiReader(bytes.NewReader(prefix), tail), Closer: original}
}

type auditWriter struct {
	http.ResponseWriter
	body      bytes.Buffer
	status    int
	truncated bool
}

func (w *auditWriter) WriteHeader(status int) {
	if w.status == 0 && (status == http.StatusSwitchingProtocols || status >= 200) {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *auditWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(body)
	remaining := auditBodyLimit - w.body.Len()
	_, _ = w.body.Write(body[:min(n, remaining)])
	w.truncated = w.truncated || n > remaining
	return n, err
}

func (w *auditWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *auditWriter) FlushError() error {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func auditHeaders(headers http.Header) http.Header {
	result := make(http.Header)
	for _, name := range []string{
		"Accept", "Content-Type", "Content-Length", "Content-Encoding", "User-Agent",
		"Retry-After", "X-Ogw-Ratelimit-Limit", "X-Ogw-Ratelimit-Reset",
		"X-Request-Id", "X-Tt-Logid", "X-Lark-Request-Timestamp",
	} {
		for _, value := range headers.Values(name) {
			result.Add(name, value)
		}
	}
	return result
}

func auditBody(body []byte, contentType string, truncated bool) []byte {
	if len(body) == 0 {
		return nil
	}
	body = body[:min(len(body), auditBodyLimit)]
	format, _, _ := mime.ParseMediaType(contentType)
	var value any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if !truncated && json.Valid(body) && decoder.Decode(&value) == nil {
		if redactJSON(value) {
			redacted, err := json.Marshal(value)
			if err != nil || len(redacted) > auditBodyLimit {
				return nil
			}
			return redacted
		}
		return body
	}
	trimmed := bytes.TrimSpace(body)
	looksLikeJSON := len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[')
	if looksLikeJSON || format == "application/json" || strings.HasSuffix(format, "+json") ||
		strings.HasPrefix(format, "multipart/") || format == "application/x-www-form-urlencoded" {
		return nil
	}
	return body
}

func redactJSON(value any) bool {
	changed := false
	switch value := value.(type) {
	case map[string]any:
		for name, child := range value {
			if credentialField(name, child) {
				value[name] = "[REDACTED]"
				changed = true
			} else if redactJSON(child) {
				changed = true
			}
		}
	case []any:
		for _, child := range value {
			if redactJSON(child) {
				changed = true
			}
		}
	}
	return changed
}

func credentialField(name string, value any) bool {
	switch strings.ToLower(name) {
	case "authorization", "app_secret", "client_secret", "encrypt_key", "password", "token",
		"access_token", "app_access_token", "tenant_access_token", "user_access_token", "refresh_token",
		"verification_token", "app_ticket":
		return true
	case "code":
		_, isString := value.(string)
		return isString
	default:
		return false
	}
}
