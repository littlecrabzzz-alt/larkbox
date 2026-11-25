package sandbox_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

func TestPreviewValidatesCardWithoutCreatingEntity(t *testing.T) {
	srv := sandboxServer(t, config.Default())
	const path = "/_sandbox/cards/validate"
	valid := call(t, srv, http.MethodPost, path, `{"card":{"elements":[{"tag":"div","text":{"tag":"plain_text","content":"Card 1"}}]}}`, http.StatusOK)
	assertContains(t, valid, `"valid":true`)
	invalid := call(t, srv, http.MethodPost, path, `{"card":{"schema":"2.0","body":{"elements":[{"tag":"table","rows":"invalid"}]}}}`, http.StatusBadRequest)
	assertContains(t, invalid, "body.elements[0]")
	duplicate := call(t, srv, http.MethodPost, path, `{"card":{"schema":"2.0","body":{"elements":[
		{"tag":"markdown","element_id":"duplicate","content":"first"},
		{"tag":"markdown","element_id":"duplicate","content":"second"}
	]}}}`, http.StatusBadRequest)
	assertContains(t, duplicate, "Duplicate element_id")
	assertContains(t, call(t, srv, http.MethodGet, "/_sandbox/cards", "", http.StatusOK), `"cards":[]`)
}

func TestCardImagePreviewSniffsUploadedBytes(t *testing.T) {
	srv := sandboxServer(t, config.Default())
	srv.Store().Media().Put(store.Media{Key: "img_safe", Kind: "image", ContentType: "text/html", Bytes: []byte("\x89PNG\r\n\x1a\ncontent")})
	srv.Store().Media().Put(store.Media{Key: "img_html", Kind: "image", ContentType: "image/png", Bytes: []byte("<html>content</html>")})

	response := httptest.NewRecorder()
	srv.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/_sandbox/images/img_safe", nil))
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "image/png" ||
		response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("unexpected preview response: %d %v", response.Code, response.Header())
	}
	call(t, srv, http.MethodGet, "/_sandbox/images/img_html", "", http.StatusUnsupportedMediaType)
	call(t, srv, http.MethodGet, "/_sandbox/images/missing", "", http.StatusNotFound)
}
