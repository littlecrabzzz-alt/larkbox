// Package content implements sandbox content and storage APIs.
package content

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/middleware"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

type fields = map[string]any

type handler struct{ state *store.Store }

// RegisterRoutes mounts the content APIs on the Open API listener.
func RegisterRoutes(mux *http.ServeMux, cfg config.Config, state *store.Store) {
	h := handler{state}
	bearer := middleware.Bearer(state, middleware.TenantOrUser, cfg.StrictMode)
	register := func(path string, action http.HandlerFunc) { mux.Handle(path, bearer(action)) }
	register("POST /open-apis/docs_ai/v1/documents", h.createDocument)
	register("POST /open-apis/docs_ai/v1/documents/{document_id}/fetch", h.fetchDocument)
	register("POST /open-apis/docx/v1/documents", h.createDocx)
	register("GET /open-apis/docx/v1/documents/{document_id}", h.readDocx)
	register("GET /open-apis/docx/v1/documents/{document_id}/raw_content", h.rawDocx)
	register("POST /open-apis/wiki/v2/spaces", h.createWikiSpace)
	register("GET /open-apis/wiki/v2/spaces/{space_id}", h.readWikiSpace)
	register("POST /open-apis/wiki/v2/spaces/{space_id}/nodes", h.createWikiNode)
	register("GET /open-apis/wiki/v2/spaces/node_by_token", h.readWikiNode)
	register("GET /open-apis/wiki/v2/spaces/get_node", h.readWikiNode)
	register("GET /open-apis/drive/explorer/v2/root_folder/meta", h.driveRoot)
	register("POST /open-apis/drive/v1/files/upload_all", h.uploadDrive)
	register("GET /open-apis/drive/v1/files/{file_token}/download", h.downloadDrive)
	register("POST /open-apis/drive/v1/metas/batch_query", h.driveMetadata)
	register("POST /open-apis/sheets/v3/spreadsheets", h.createSpreadsheet)
	register("GET /open-apis/sheets/v3/spreadsheets/{spreadsheet_token}", h.readSpreadsheet)
	register("GET /open-apis/sheets/v3/spreadsheets/{spreadsheet_token}/sheets/query", h.querySheets)
	register("POST /open-apis/sheet_ai/v2/spreadsheets/{spreadsheet_token}/tools/invoke_read", h.readSheetTool)
	register("POST /open-apis/sheet_ai/v2/spreadsheets/{spreadsheet_token}/tools/invoke_write", h.writeSheetTool)
	register("GET /open-apis/sheets/v2/spreadsheets/{spreadsheet_token}/values/{range}", h.readSheetValues)
	register("PUT /open-apis/sheets/v2/spreadsheets/{spreadsheet_token}/values", h.writeSheetValues)
	for _, prefix := range []string{"/open-apis/base/v3/bases", "/open-apis/bitable/v1/apps"} {
		base := prefix + "/{base_token}"
		table := base + "/tables/{table_id}"
		records := table + "/records"
		register("POST "+prefix, h.createBase)
		register("GET "+base, h.readBase)
		register("GET "+base+"/tables", h.listBaseTables)
		register("GET "+table+"/fields", h.listBaseFields)
		register("POST "+records, h.createBaseRecord)
		register("POST "+records+"/batch_create", h.batchCreateBaseRecords)
		register("GET "+records, h.listBaseRecords)
		register("POST "+records+"/search", h.searchBaseRecords)
		register("PATCH "+records+"/{record_id}", h.updateBaseRecord)
	}
	register("PUT /open-apis/bitable/v1/apps/{base_token}/tables/{table_id}/records/{record_id}", h.updateBaseRecord)
}

func (h handler) owner(r *http.Request) store.ResourceOwner {
	if actor, ok := middleware.AuthFromContext(r.Context()); ok {
		return store.ResourceOwner{AppID: actor.AppID, UserOpenID: actor.UserOpenID}
	}
	if apps := h.state.Apps().List(); len(apps) > 0 {
		return store.ResourceOwner{AppID: apps[0].AppID}
	}
	return store.ResourceOwner{}
}

func reply(w http.ResponseWriter, data any) {
	_ = protocol.WriteJSON(w, http.StatusOK, protocol.Envelope{Code: protocol.Success, Msg: "success", Data: data})
}

func fail(w http.ResponseWriter, code protocol.Code) {
	_ = protocol.WriteError(w, protocol.NewError(code, nil))
}

func stateError(w http.ResponseWriter, err error) {
	if middleware.WriteResetConflict(w, err) {
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		fail(w, protocol.ResourceNotFound)
		return
	}
	fail(w, protocol.InvalidParameter)
}

func read(w http.ResponseWriter, r *http.Request, into any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if dec.Decode(into) != nil || !errors.Is(dec.Decode(new(json.RawMessage)), io.EOF) {
		fail(w, protocol.MalformedRequest)
		return false
	}
	return true
}

func resourceURL(r *http.Request, path string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return (&url.URL{Scheme: scheme, Host: r.Host, Path: path}).String()
}
