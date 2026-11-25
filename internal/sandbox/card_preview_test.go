package sandbox_test

import (
	"fmt"
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
	multiOutsideForm := call(t, srv, http.MethodPost, path, `{"card":{"schema":"2.0","body":{"elements":[
		{"tag":"select_img","multi_select":true,"options":[{"img_key":"img_test","value":"photo"}]}
	]}}}`, http.StatusBadRequest)
	assertContains(t, multiOutsideForm, "Multiple image selection requires a form.")
	assertContains(t, call(t, srv, http.MethodGet, "/_sandbox/cards", "", http.StatusOK), `"cards":[]`)
}

func TestPreviewRequiresTablesAndFormsAtCardRoot(t *testing.T) {
	srv := sandboxServer(t, config.Default())
	const (
		path  = "/_sandbox/cards/validate"
		card  = `{"card":{"schema":"2.0","body":{"elements":[%s]}}}`
		panel = `{"tag":"collapsible_panel","header":{"title":{"tag":"plain_text","content":"Details"}},"elements":[%s]}`
		table = `{"tag":"table","columns":[{"name":"state","data_type":"text"}],"rows":[{"state":"Ready"}]}`
		form  = `{"tag":"form","name":"review","elements":[{"tag":"button","name":"save","form_action_type":"submit","text":{"tag":"plain_text","content":"Save"}}]}`
	)
	for tag, component := range map[string]string{"table": table, "form": form} {
		t.Run(tag, func(t *testing.T) {
			call(t, srv, http.MethodPost, path, fmt.Sprintf(card, component), http.StatusOK)
			body := call(t, srv, http.MethodPost, path, fmt.Sprintf(card, fmt.Sprintf(panel, component)), http.StatusBadRequest)
			assertContains(t, body, "check the component at body.elements[0] and its nested fields")
		})
	}
	call(t, srv, http.MethodPost, path, `{"card":{"elements":[
		{"tag":"markdown","content":"Review","elements":[{"tag":"form"}]},
		{"tag":"select_static","options":[{"value":"ready","tag":"table"}]}
	]}}`, http.StatusOK)
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
