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

// RedactTraceJSON canonicalizes structured JSON and removes protocol credentials.
// Opaque, partial, or ambiguous bodies are omitted.
func RedactTraceJSON(body []byte) []byte {
	body = bytes.TrimSpace(body)
	if len(body) == 0 || (body[0] != '{' && body[0] != '[') || !json.Valid(body) {
		return nil
	}
	var root any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if decoder.Decode(&root) != nil {
		return nil
	}
	redactCredentials(root, 0)
	redacted, _ := json.Marshal(root)
	return redacted
}

// RedactDiagnosticText preserves display text and scrubs JSON encoded inside it.
func RedactDiagnosticText(text string) string {
	return redactCredentials(text, 0).(string)
}

const maxEmbeddedJSONDepth = 4

func redactCredentials(value any, embeddedDepth int) any {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if credentialField(key, child) {
				value[key] = "[REDACTED]"
			} else {
				value[key] = redactCredentials(child, embeddedDepth)
			}
		}
	case []any:
		for i, child := range value {
			value[i] = redactCredentials(child, embeddedDepth)
		}
	case string:
		// IM content and CardKit data can contain JSON-encoded objects.
		raw := strings.TrimSpace(value)
		if len(raw) == 0 || (raw[0] != '{' && raw[0] != '[') {
			return value
		}
		if embeddedDepth == maxEmbeddedJSONDepth {
			return "[OMITTED]"
		}
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()
		var nested any
		if json.Valid([]byte(raw)) && decoder.Decode(&nested) == nil {
			body, _ := json.Marshal(redactCredentials(nested, embeddedDepth+1))
			return string(body)
		}
	}
	return value
}

func credentialField(key string, value any) bool {
	switch strings.ToLower(key) {
	case "authorization", "password", "app_secret", "client_secret", "encrypt_key", "verification_token",
		"access_token", "tenant_access_token", "app_access_token", "user_access_token", "refresh_token",
		"app_ticket", "code_verifier", "token", "preview_token":
		return true
	case "code":
		_, isString := value.(string)
		return isString
	default:
		return false
	}
}

func captureTraceBody(body []byte, truncated bool) ([]byte, bool) {
	if truncated || len(body) > TraceBodyLimit {
		return nil, true
	}
	redacted := RedactTraceJSON(body)
	if len(redacted) > TraceBodyLimit {
		return nil, true
	}
	return redacted, false
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
