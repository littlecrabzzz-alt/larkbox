package store

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"
)

const (
	// TraceHeaderLimit bounds the combined captured header bytes.
	TraceHeaderLimit = 8 * 1024
	// TraceHeaderValueLimit bounds each captured field value.
	TraceHeaderValueLimit = 1024
	// TraceHeaderEntries bounds the number of captured field values.
	TraceHeaderEntries = 64
)

// TraceHeaders snapshots diagnostic headers within entry and byte limits.
// Credential and arbitrary application headers are omitted.
func TraceHeaders(headers http.Header) http.Header {
	result := make(http.Header)
	remaining, entries := TraceHeaderLimit, TraceHeaderEntries
	for _, name := range []string{
		"Accept", "Content-Type", "Content-Length", "Content-Disposition", "Content-Encoding", "User-Agent",
		"Retry-After", "X-Ogw-Ratelimit-Limit", "X-Ogw-Ratelimit-Reset",
		"X-Request-Id", "X-Tt-Logid", "X-Lark-Request-Timestamp",
	} {
		for _, value := range headers.Values(name) {
			// Charge each entry for its key and slice/string bookkeeping.
			overhead := len(name) + 16
			if entries == 0 || remaining <= overhead {
				return result
			}
			n := min(len(value), TraceHeaderValueLimit, remaining-overhead)
			for n > 0 && n < len(value) && !utf8.RuneStart(value[n]) {
				n--
			}
			result.Add(name, strings.Clone(value[:n]))
			remaining -= overhead + n
			entries--
		}
	}
	return result
}

// RedactTraceJSON removes credential fields from an audit copy of a payload.
// It preserves unrelated fields, including application data and card icon tokens.
func RedactTraceJSON(body []byte) []byte {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return body
	}
	if (trimmed[0] == '{' || trimmed[0] == '[') && !json.Valid(trimmed) {
		// A partial JSON response may contain a complete credential field.
		return []byte("[invalid JSON omitted]")
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(trimmed, &root) != nil {
		return body
	}
	changed := false
	for _, key := range []string{"token", "access_token", "tenant_access_token", "app_access_token", "user_access_token", "refresh_token", "app_secret"} {
		changed = redactTracePath(root, key) || changed
	}
	changed = redactTracePath(root, "header", "token") || changed
	changed = redactTracePath(root, "event", "token") || changed
	changed = redactTracePath(root, "event", "context", "preview_token") || changed
	if !changed {
		return body
	}
	redacted, _ := json.Marshal(root)
	return redacted
}

func redactTracePath(root map[string]json.RawMessage, key string, rest ...string) bool {
	value, exists := root[key]
	if !exists {
		return false
	}
	if len(rest) == 0 {
		root[key] = json.RawMessage(`"[redacted]"`)
		return true
	}
	var child map[string]json.RawMessage
	if json.Unmarshal(value, &child) != nil || !redactTracePath(child, rest[0], rest[1:]...) {
		return false
	}
	root[key], _ = json.Marshal(child)
	return true
}
