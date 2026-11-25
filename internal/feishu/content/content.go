// Package content implements sandbox content and storage APIs.
package content

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

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
	register := func(path string, action http.HandlerFunc, scopes ...string) {
		mux.Handle(path, middleware.OwnedResource(state, middleware.TenantOrUser, cfg.StrictMode, action, scopes...))
	}
	register("POST /open-apis/docs_ai/v1/documents", h.createDocument, "docx:document:create", "docx:document")
	register("POST /open-apis/docs_ai/v1/documents/{document_id}/fetch", h.fetchDocument, "docx:document:readonly", "docx:document")
	register("POST /open-apis/docx/v1/documents", h.createDocx, "docx:document:create", "docx:document")
	register("GET /open-apis/docx/v1/documents/{document_id}", h.readDocx, "docx:document:readonly", "docx:document")
	register("GET /open-apis/docx/v1/documents/{document_id}/raw_content", h.rawDocx, "docx:document:readonly", "docx:document")
	register("POST /open-apis/wiki/v2/spaces", h.createWikiSpace, "wiki:space:write_only", "wiki:wiki")
	register("GET /open-apis/wiki/v2/spaces/{space_id}", h.readWikiSpace, "wiki:space:read", "wiki:wiki:readonly", "wiki:wiki")
	register("POST /open-apis/wiki/v2/spaces/{space_id}/nodes", h.createWikiNode, "wiki:node:create", "wiki:wiki")
	register("GET /open-apis/wiki/v2/spaces/node_by_token", h.readWikiNode, "wiki:node:retrieve", "wiki:node:read", "wiki:wiki:readonly", "wiki:wiki")
	register("GET /open-apis/wiki/v2/spaces/get_node", h.readWikiNode, "wiki:node:retrieve", "wiki:node:read", "wiki:wiki:readonly", "wiki:wiki")
	register("GET /open-apis/drive/explorer/v2/root_folder/meta", h.driveRoot, "drive:drive.metadata:readonly", "drive:drive:readonly", "drive:drive")
	register("POST /open-apis/drive/v1/files/upload_all", h.uploadDrive, "drive:file:upload", "drive:drive")
	register("GET /open-apis/drive/v1/files/{file_token}/download", h.downloadDrive, "drive:file:download", "drive:drive:readonly", "drive:drive")
	register("POST /open-apis/drive/v1/metas/batch_query", h.driveMetadata, "drive:drive.metadata:readonly", "drive:drive:readonly", "drive:drive")
	register("GET /open-apis/drive/v2/files/query_by_token", h.queryDriveToken, "drive:drive.metadata:readonly", "drive:drive:readonly", "drive:drive")
	register("GET /open-apis/drive/v1/permissions/{file_token}/members/auth", h.driveExportPermission, "docs:permission.member:auth")
	register("POST /open-apis/sheets/v3/spreadsheets", h.createSpreadsheet, "sheets:spreadsheet:create", "sheets:spreadsheet")
	register("GET /open-apis/sheets/v3/spreadsheets/{spreadsheet_token}", h.readSpreadsheet, "sheets:spreadsheet:read", "sheets:spreadsheet:readonly", "sheets:spreadsheet")
	register("GET /open-apis/sheets/v3/spreadsheets/{spreadsheet_token}/sheets/query", h.querySheets, "sheets:spreadsheet:read", "sheets:spreadsheet:readonly", "sheets:spreadsheet")
	register("POST /open-apis/sheet_ai/v2/spreadsheets/{spreadsheet_token}/tools/invoke_read", h.readSheetTool, "sheets:spreadsheet:read")
	register("POST /open-apis/sheet_ai/v2/spreadsheets/{spreadsheet_token}/tools/invoke_write", h.writeSheetTool, "sheets:spreadsheet:write_only")
	register("GET /open-apis/sheets/v2/spreadsheets/{spreadsheet_token}/values/{range}", h.readSheetValues, "sheets:spreadsheet:read", "sheets:spreadsheet:readonly", "sheets:spreadsheet")
	register("PUT /open-apis/sheets/v2/spreadsheets/{spreadsheet_token}/values", h.writeSheetValues, "sheets:spreadsheet:write_only", "sheets:spreadsheet")
	for _, prefix := range []string{"/open-apis/base/v3/bases", "/open-apis/bitable/v1/apps"} {
		base := prefix + "/{base_token}"
		table := base + "/tables/{table_id}"
		records := table + "/records"
		readScopes := func(scope string) []string { return []string{scope, "bitable:app:readonly", "bitable:app"} }
		writeScopes := func(scope string) []string { return []string{scope, "bitable:app"} }
		if strings.HasPrefix(prefix, "/open-apis/base/") {
			readScopes = func(scope string) []string { return []string{scope} }
			writeScopes = readScopes
		}
		register("POST "+prefix, h.createBase, writeScopes("base:app:create")...)
		register("GET "+base, h.readBase, readScopes("base:app:read")...)
		register("GET "+base+"/tables", h.listBaseTables, readScopes("base:table:read")...)
		register("GET "+table+"/fields", h.listBaseFields, readScopes("base:field:read")...)
		register("POST "+records, h.createBaseRecord, writeScopes("base:record:create")...)
		register("POST "+records+"/batch_create", h.batchCreateBaseRecords, writeScopes("base:record:create")...)
		register("GET "+records, h.listBaseRecords, readScopes("base:record:read")...)
		register("POST "+records+"/search", h.searchBaseRecords, readScopes("base:record:read")...)
		register("PATCH "+records+"/{record_id}", h.updateBaseRecord, writeScopes("base:record:update")...)
	}
	register("PUT /open-apis/bitable/v1/apps/{base_token}/tables/{table_id}/records/{record_id}", h.updateBaseRecord, "base:record:update", "bitable:app")
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
