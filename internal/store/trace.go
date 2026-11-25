package store

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
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
func TraceHeaders(headers http.Header) http.Header { return Redactor{}.Headers(headers) }

// Headers copies the allowlisted headers and scrubs reflected credential values.
func (r Redactor) Headers(headers http.Header) http.Header {
	result := make(http.Header)
	remaining, entries := TraceHeaderLimit, TraceHeaderEntries
	for _, name := range []string{
		"Accept", "Content-Type", "Content-Length", "Content-Disposition", "Content-Encoding", "User-Agent",
		"Retry-After", "X-Ogw-Ratelimit-Limit", "X-Ogw-Ratelimit-Reset",
		"X-Request-Id", "X-Tt-Logid", "X-Lark-Request-Timestamp",
	} {
		for _, value := range headers.Values(name) {
			value = r.Replace(value)
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

const redactedValue = "[REDACTED]"

// Redactor protects protocol fields and known credentials in diagnostic copies.
// Its zero value redacts credential fields without replacing specific values.
type Redactor struct {
	values *strings.Replacer
}

// NewRedactor also redacts occurrences of the given nonempty credentials.
func NewRedactor(secrets ...string) Redactor {
	if len(secrets) == 0 {
		return Redactor{}
	}
	values := slices.Clone(secrets)
	slices.SortFunc(values, func(a, b string) int { return len(b) - len(a) })
	pairs := make([]string, 0, len(values)*2)
	for _, value := range values {
		if value != "" {
			pairs = append(pairs, value, redactedValue)
		}
	}
	if len(pairs) == 0 {
		return Redactor{}
	}
	return Redactor{values: strings.NewReplacer(pairs...)}
}

// AppRedactor protects the app's credentials in addition to delivery credentials.
func AppRedactor(app App, deliverySecrets ...string) Redactor {
	secrets := append([]string{app.VerificationToken, app.AppSecret, app.EncryptKey}, deliverySecrets...)
	return NewRedactor(secrets...)
}

// DiagnosticRedactor uses the callback's captured credentials when available.
// Cards created through the Open API use the app's configured credentials.
func (s *Store) DiagnosticRedactor(appID string, callback Redactor) Redactor {
	if callback.values != nil {
		return callback
	}
	app, _ := s.Apps().Get(appID)
	return AppRedactor(app)
}

// Replace removes known credentials from a string while preserving its format.
func (r Redactor) Replace(text string) string {
	if r.values == nil {
		return text
	}
	return r.values.Replace(text)
}

// JSON canonicalizes structured JSON and redacts credentials. Opaque, partial,
// or ambiguous bodies are omitted.
func (r Redactor) JSON(body []byte) []byte {
	root, ok := structuredJSON(bytes.TrimSpace(body))
	if !ok {
		return nil
	}
	redacted, _ := json.Marshal(r.scrub(root, "", 0))
	return redacted
}

// Text preserves display text and scrubs any structured JSON encoded inside it.
func (r Redactor) Text(text string) string {
	return r.scrub(text, "", 0).(string)
}

// TextMap scrubs diagnostic keys and text without modifying the source.
func (r Redactor) TextMap(entries map[string]string) map[string]string {
	if entries == nil {
		return nil
	}
	return redactObject(r, entries, func(_ string, text string) string { return r.Text(text) })
}

func redactObject[T any](r Redactor, entries map[string]T, scrub func(string, T) T) map[string]T {
	out := make(map[string]T, len(entries))
	var reflected []string
	for key, value := range entries {
		if r.Replace(key) == key {
			out[key] = scrub(key, value)
		} else {
			reflected = append(reflected, key)
		}
	}
	// Keep unmodified keys stable. For collisions, assign suffixes in
	// original key order so every diagnostic value stays accessible.
	slices.Sort(reflected)
	next := make(map[string]int)
	for _, key := range reflected {
		base := r.Replace(key)
		safe := base
		for {
			if _, exists := out[safe]; !exists {
				break
			}
			n := max(2, next[base])
			next[base] = n + 1
			safe = base + "#" + strconv.Itoa(n)
		}
		out[safe] = scrub(key, entries[key])
	}
	return out
}

func structuredJSON(body []byte) (any, bool) {
	if len(body) == 0 || (body[0] != '{' && body[0] != '[') || !json.Valid(body) {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var root any
	err := decoder.Decode(&root)
	return root, err == nil
}

const maxEmbeddedJSONDepth = 4

func (r Redactor) scrub(value any, parent string, embeddedDepth int) any {
	switch value := value.(type) {
	case map[string]any:
		tag, _ := value["tag"].(string)
		icon := tag == "standard_icon" || parent == "icon" && tag == ""
		return redactObject(r, value, func(key string, child any) any {
			_, iconToken := child.(string)
			iconToken = iconToken && icon && key == "token"
			if !iconToken && credentialField(key, child) {
				return redactedValue
			}
			return r.scrub(child, key, embeddedDepth)
		})
	case []any:
		for i, child := range value {
			value[i] = r.scrub(child, "", embeddedDepth)
		}
	case string:
		// IM content and CardKit data can contain JSON-encoded objects.
		raw := strings.TrimSpace(value)
		if len(raw) == 0 || (raw[0] != '{' && raw[0] != '[') {
			return r.Replace(value)
		}
		if embeddedDepth == maxEmbeddedJSONDepth {
			return "[OMITTED]"
		}
		if nested, ok := structuredJSON([]byte(raw)); ok {
			body, _ := json.Marshal(r.scrub(nested, "", embeddedDepth+1))
			return string(body)
		}
		return r.Replace(value)
	}
	return value
}

func credentialField(key string, value any) bool {
	switch strings.ToLower(key) {
	case "authorization", "password", "app_secret", "client_secret", "encrypt_key", "verification_token",
		"access_token", "tenant_access_token", "app_access_token", "user_access_token", "refresh_token",
		"app_ticket", "code_verifier", "token", "preview_token", "device_code", "user_code", "verification_uri_complete":
		return true
	case "code":
		_, isString := value.(string)
		return isString
	default:
		return false
	}
}

func (r Redactor) captureTraceBody(body []byte, truncated bool) ([]byte, bool) {
	if truncated || len(body) > TraceBodyLimit {
		return nil, true
	}
	redacted := r.JSON(body)
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
