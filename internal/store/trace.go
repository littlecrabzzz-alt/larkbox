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
	var root any
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	if decoder.Decode(&root) != nil {
		return body
	}
	changed := redactSensitiveTraceFields(root)
	if obj, ok := root.(map[string]any); ok {
		changed = redactTracePath(obj, "token") || changed
		changed = redactTracePath(obj, "header", "token") || changed
		changed = redactTracePath(obj, "event", "token") || changed
		changed = redactTracePath(obj, "event", "context", "preview_token") || changed
	}
	if !changed {
		return body
	}
	redacted, _ := json.Marshal(root)
	return redacted
}

// Preserve application tokens such as card icon tokens. These names identify
// protocol credentials wherever they occur in a JSON object.
func redactSensitiveTraceFields(value any) bool {
	changed := false
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			switch strings.ToLower(key) {
			case "access_token", "tenant_access_token", "app_access_token", "user_access_token", "refresh_token",
				"app_secret", "client_secret", "encrypt_key", "verification_token", "authorization":
				value[key] = "[redacted]"
				changed = true
			default:
				changed = redactSensitiveTraceFields(child) || changed
			}
		}
	case []any:
		for _, child := range value {
			changed = redactSensitiveTraceFields(child) || changed
		}
	}
	return changed
}

func redactTracePath(root map[string]any, key string, rest ...string) bool {
	value, exists := root[key]
	if !exists {
		return false
	}
	if len(rest) == 0 {
		root[key] = "[redacted]"
		return true
	}
	child, ok := value.(map[string]any)
	return ok && redactTracePath(child, rest[0], rest[1:]...)
}

// TraceEpoch identifies the current audit ring generation.
func (s *Store) TraceEpoch() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.traceEpoch
}

// ClearTraces clears both directions and drops exchanges admitted to the old ring.
func (s *Store) ClearTraces() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clearTraces()
}

func (s *Store) clearTraces() {
	clear(s.traces)
	s.traces = s.traces[:0]
	s.tracePos = 0
	s.traceEpoch++
}
