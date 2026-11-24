package store

import (
	"net/http"
	"strings"
	"testing"
)

func TestOutboundTraceHeaderLimits(t *testing.T) {
	s := New(nil, 0)
	request := make(http.Header)
	for range TraceHeaderEntries + 10 {
		request.Add("Accept", "*/*")
	}
	response := make(http.Header)
	for range TraceHeaderEntries {
		response.Add("X-Request-Id", strings.Repeat("ø", TraceHeaderValueLimit))
	}
	response.Set("Set-Cookie", "private")
	s.AppendTrace(Trace{Direction: Outbound, RequestHeaders: request, ResponseHeaders: response})
	trace := s.Traces()[0]
	if got := len(trace.RequestHeaders.Values("Accept")); got != TraceHeaderEntries {
		t.Fatalf("captured %d header entries, want %d", got, TraceHeaderEntries)
	}
	if len(request.Values("Accept")) != TraceHeaderEntries+10 || response.Get("Set-Cookie") != "private" {
		t.Fatal("audit changed the original headers")
	}
	total := 0
	for name, values := range trace.ResponseHeaders {
		for _, value := range values {
			total += len(name) + len(value)
			if len(value) > TraceHeaderValueLimit {
				t.Fatalf("captured oversized header %s", name)
			}
		}
	}
	if total == 0 || total > TraceHeaderLimit || trace.ResponseHeaders.Get("Set-Cookie") != "" {
		t.Fatalf("captured %d bytes or retained a sensitive header", total)
	}
}

func TestOutboundAuditRedactsBeforeTruncation(t *testing.T) {
	s := New(nil, 0)
	wire := []byte(`{"header":{"token":"private-verification"},"event":{"token":"private-action","context":{"preview_token":"private-preview"}},"token":"private-root","z":"` +
		strings.Repeat("x", TraceBodyLimit) + `"}`)
	response := []byte(`{"token":"private-incomplete","remaining":`)
	s.AppendTrace(Trace{Direction: Outbound, RequestBody: wire, ResponseBody: response})
	trace := s.Traces()[0]
	if !trace.RequestBodyTruncated || len(trace.RequestBody) != 0 ||
		strings.Contains(string(trace.RequestBody), "private-") ||
		strings.Contains(string(trace.ResponseBody), "private-") || strings.Count(string(wire), "private-") != 4 {
		t.Fatal("audit redaction changed wire data or retained a private field")
	}
}

func TestTraceScrubbingCanonicalizesNestedCredentials(t *testing.T) {
	for _, direction := range []TraceDirection{Inbound, Outbound} {
		t.Run(string(direction), func(t *testing.T) {
			state := New(nil, 0)
			body := []byte(`{ "keep":{"token":"hidden-duplicate"}, "keep":"safe", "items":[{"token":"first","ToKeN":"second","preview_token":"third","challenge":"readable"}], "content":"{\"access_token\":\"encoded-secret\",\"text\":\"visible\"}" }`)
			state.AppendTrace(Trace{Direction: direction, RequestBody: body, ResponseBody: []byte("bearer opaque-secret")})
			trace := state.Traces()[0]
			want := `{"content":"{\"access_token\":\"[REDACTED]\",\"text\":\"visible\"}","items":[{"ToKeN":"[REDACTED]","challenge":"readable","preview_token":"[REDACTED]","token":"[REDACTED]"}],"keep":"safe"}`
			if string(trace.RequestBody) != want || trace.ResponseBody != nil {
				t.Fatalf("unsafe or incorrectly canonicalized trace: %s / %s", trace.RequestBody, trace.ResponseBody)
			}
			for _, malformed := range []string{`{"token":"private"} unexpected`, `{"token":"private"`, `"private"`} {
				if got := (Redactor{}).JSON([]byte(malformed)); got != nil {
					t.Fatalf("ambiguous body was retained: %s", got)
				}
			}
		})
	}
}

func TestRedactorPreservesCollidingObjectEntries(t *testing.T) {
	redactor := NewRedactor("first-secret", "second-secret")
	input := []byte(`{"nested":{"prefix-first-secret":"First result first-secret","prefix-second-secret":"Second result second-secret","prefix-[REDACTED]":"already safe","prefix-[REDACTED]#2":"also safe"}}`)
	const want = `{"nested":{"prefix-[REDACTED]":"already safe","prefix-[REDACTED]#2":"also safe","prefix-[REDACTED]#3":"First result [REDACTED]","prefix-[REDACTED]#4":"Second result [REDACTED]"}}`
	for range 10 {
		if got := string(redactor.JSON(input)); got != want {
			t.Fatalf("redacted collision yielded %s, want %s", got, want)
		}
	}
	localized := map[string]string{"first-secret": "first", "second-secret": "second"}
	scrubbed := redactor.TextMap(localized)
	if len(scrubbed) != 2 || scrubbed["[REDACTED]"] != "first" || scrubbed["[REDACTED]#2"] != "second" ||
		localized["first-secret"] != "first" || localized["second-secret"] != "second" {
		t.Fatalf("localization collision changed the source or lost entries: %v", scrubbed)
	}
}
