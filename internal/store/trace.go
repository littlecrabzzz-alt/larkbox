package store

import (
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
