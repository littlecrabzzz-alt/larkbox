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
