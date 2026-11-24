package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

// auditBodyLimit caps memory used to record each HTTP body.
const auditBodyLimit = store.TraceBodyLimit

// Audit records bounded request and response snapshots for Open API calls.
// It omits credential headers, redacts JSON credentials, and skips body
// formats that cannot be safely redacted, such as multipart forms.
func Audit(state *store.Store, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, epoch := state.Admit(r.Context())
		started := time.Now()
		trace := store.Trace{
			Direction: store.Inbound, StartedAt: started, Method: r.Method, Path: r.URL.EscapedPath(),
			RequestHeaders: store.TraceHeaders(r.Header),
		}
		r = r.WithContext(context.WithValue(ctx, traceAppKey{}, &trace.AppID))
		requestContentType := r.Header.Get("Content-Type")
		requestLength := r.ContentLength
		var request *auditReader
		if r.Body != nil && r.Body != http.NoBody {
			request = &auditReader{ReadCloser: r.Body}
			r.Body = request
		}

		response := &auditWriter{ResponseWriter: w}
		var wrapped http.ResponseWriter = response
		if _, ok := w.(http.Flusher); ok {
			wrapped = &flushingAuditWriter{response}
		}
		next.ServeHTTP(wrapped, r)
		response.commit(http.StatusOK) // A handler that writes nothing gets an implicit 200.
		trace.Latency = time.Since(started)
		if request != nil {
			trace.RequestBodyTruncated = request.truncated || request.failed ||
				(request.bytesRead > 0 && !request.eof && request.bytesRead != requestLength)
			trace.RequestBody = auditBody(request.body.Bytes(), requestContentType, trace.RequestBodyTruncated)
		}
		trace.Status = response.status
		trace.ResponseHeaders = response.headers
		trace.ResponseBodyTruncated = response.truncated
		if !response.attachment {
			trace.ResponseBody = auditBody(response.body.Bytes(), response.contentType, response.truncated)
		}
		trace.Code = response.code
		if trace.Code == nil && !response.truncated && !response.attachment {
			var envelope struct {
				Code *int `json:"code"`
			}
			if json.Unmarshal(response.body.Bytes(), &envelope) == nil {
				trace.Code = envelope.Code
			}
		}
		state.AppendTraceAt(epoch, trace)
	})
}

// SetTraceApp records the app identified by an Open API handler.
func SetTraceApp(r *http.Request, appID string) {
	if traceAppID, ok := r.Context().Value(traceAppKey{}).(*string); ok {
		*traceAppID = appID
	}
}

type bodyCapture struct {
	body      bytes.Buffer
	truncated bool
}

func (c *bodyCapture) record(data []byte) {
	remaining := auditBodyLimit - c.body.Len()
	_, _ = c.body.Write(data[:min(len(data), remaining)])
	c.truncated = c.truncated || len(data) > remaining
}

type auditReader struct {
	io.ReadCloser
	bodyCapture
	bytesRead int64
	eof       bool
	failed    bool
}

func (r *auditReader) Read(data []byte) (int, error) {
	n, err := r.ReadCloser.Read(data)
	r.record(data[:n])
	r.bytesRead += int64(n)
	switch {
	case errors.Is(err, io.EOF):
		r.eof = true
	case err != nil:
		r.failed = true
	}
	return n, err
}

type auditWriter struct {
	http.ResponseWriter
	bodyCapture
	status      int
	headers     http.Header
	contentType string
	attachment  bool
	code        *int
}

func (w *auditWriter) commit(status int) {
	if w.status == 0 {
		w.status = status
		w.headers = store.TraceHeaders(w.Header())
		w.contentType = w.Header().Get("Content-Type")
		w.attachment = w.Header().Get("Content-Disposition") != ""
	}
}

func (w *auditWriter) WriteHeader(status int) {
	if status == http.StatusSwitchingProtocols || status >= 200 {
		w.commit(status)
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *auditWriter) Write(body []byte) (int, error) {
	w.commit(http.StatusOK)
	n, err := w.ResponseWriter.Write(body)
	if !w.attachment {
		w.record(body[:n])
	}
	return n, err
}

func (w *auditWriter) SetFeishuCode(code protocol.Code) {
	if w.status == 0 {
		value := int(code)
		w.code = &value
	}
}

func (w *auditWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type flushingAuditWriter struct{ *auditWriter }

func (w *flushingAuditWriter) Flush() { _ = w.FlushError() }

func (w *auditWriter) FlushError() error {
	controller := http.NewResponseController(w.ResponseWriter)
	if w.status != 0 {
		return controller.Flush()
	}
	headers := store.TraceHeaders(w.Header())
	contentType := w.Header().Get("Content-Type")
	attachment := w.Header().Get("Content-Disposition") != ""
	err := controller.Flush()
	if !errors.Is(err, http.ErrNotSupported) {
		w.status, w.headers, w.contentType, w.attachment = http.StatusOK, headers, contentType, attachment
	}
	return err
}

func auditBody(body []byte, contentType string, truncated bool) []byte {
	format, _, _ := mime.ParseMediaType(contentType)
	if truncated || strings.HasPrefix(format, "multipart/") || format == "application/x-www-form-urlencoded" {
		return nil
	}
	return body
}
