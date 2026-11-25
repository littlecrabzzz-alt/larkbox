package sdk_test

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/event"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

func TestContentPermissionsAndLookup(t *testing.T) {
	const scopes = "docx:document:create docx:document:readonly wiki:space:write_only wiki:space:read drive:file:upload drive:file:download sheets:spreadsheet:create sheets:spreadsheet:read base:app:create base:app:read"
	const writer, otherUser, missingScope = "u-content-writer", "u-content-other", "u-content-missing"
	cfg := sdkTestConfig()
	srv, web := sdkEventServer(t, cfg, event.Options{})
	issueTokens := func() {
		expires := time.Now().Add(time.Minute)
		app := cfg.Apps[0].AppID
		requireSDKNetwork(t, srv.Store().PutTokens(
			store.Token{Value: writer, Kind: store.UserToken, AppID: app, UserOpenID: cfg.Users[0].OpenID, Scope: scopes, ExpiresAt: expires},
			store.Token{Value: otherUser, Kind: store.UserToken, AppID: app, UserOpenID: cfg.Users[1].OpenID, Scope: scopes, ExpiresAt: expires},
			store.Token{Value: missingScope, Kind: store.UserToken, AppID: app, UserOpenID: cfg.Users[0].OpenID, Scope: "im:message", ExpiresAt: expires},
		))
	}
	issueTokens()

	type asset struct {
		Name, ReadPath string
	}
	created := func(path, payload string) map[string]any {
		t.Helper()
		body := contentCall(t, web, http.MethodPost, path, writer, "application/json", strings.NewReader(payload), http.StatusOK)
		result := sandboxDecode[map[string]any](t, body)
		if result["code"] != float64(0) {
			t.Fatalf("cannot create resource at %s: %s", path, body)
		}
		return result["data"].(map[string]any)
	}
	doc := created("/open-apis/docx/v1/documents", `{"title":"Sandbox document"}`)["document"].(map[string]any)["document_id"].(string)
	space := created("/open-apis/wiki/v2/spaces", `{"name":"Sandbox space"}`)["space"].(map[string]any)["space_id"].(string)
	sheet := created("/open-apis/sheets/v3/spreadsheets", `{"title":"Sandbox workbook"}`)["spreadsheet"].(map[string]any)["spreadsheet_token"].(string)
	base := created("/open-apis/base/v3/bases", `{"name":"Sandbox base"}`)["base_token"].(string)
	var file bytes.Buffer
	form := multipart.NewWriter(&file)
	for _, field := range [][2]string{{"file_name", "fixture.txt"}, {"parent_type", "explorer"}, {"size", "7"}} {
		requireSDKNetwork(t, form.WriteField(field[0], field[1]))
	}
	part, err := form.CreateFormFile("file", "fixture.txt")
	requireSDKNetwork(t, err)
	_, err = io.WriteString(part, "fixture")
	requireSDKNetwork(t, err)
	requireSDKNetwork(t, form.Close())
	uploaded := sandboxDecode[struct {
		Data struct {
			FileToken string `json:"file_token"`
		} `json:"data"`
	}](t, contentCall(t, web, http.MethodPost, "/open-apis/drive/v1/files/upload_all", writer, form.FormDataContentType(), &file, http.StatusOK))
	if uploaded.Data.FileToken == "" {
		t.Fatal("upload did not return a file token")
	}
	assets := []asset{
		{"doc", "/open-apis/docx/v1/documents/" + doc},
		{"wiki", "/open-apis/wiki/v2/spaces/" + space},
		{"drive", "/open-apis/drive/v1/files/" + uploaded.Data.FileToken + "/download"},
		{"sheet", "/open-apis/sheets/v3/spreadsheets/" + sheet},
		{"base", "/open-apis/base/v3/bases/" + base},
	}
	for _, resource := range assets {
		t.Run(resource.Name, func(t *testing.T) {
			contentCall(t, web, http.MethodGet, resource.ReadPath, writer, "", nil, http.StatusOK)
			denied := sandboxDecode[protocol.Envelope](t, contentCall(t, web, http.MethodGet, resource.ReadPath, missingScope, "", nil, http.StatusBadRequest))
			if denied.Code != protocol.InsufficientScope || denied.Error == nil || len(denied.Error.PermissionViolations) == 0 {
				t.Fatalf("missing scope returned %+v", denied)
			}
			missing := sandboxDecode[protocol.Envelope](t, contentCall(t, web, http.MethodGet, resource.ReadPath, otherUser, "", nil, http.StatusNotFound))
			if missing.Code != protocol.ResourceNotFound {
				t.Fatalf("resource lookup disclosed another user's asset: %+v", missing)
			}
		})
	}
	sandboxRequest(t, web, http.MethodPost, "reset", nil, http.StatusOK)
	issueTokens()
	for _, resource := range assets {
		contentCall(t, web, http.MethodGet, resource.ReadPath, writer, "", nil, http.StatusNotFound)
	}
}

func TestSheetRangeUpdateIsAtomic(t *testing.T) {
	cfg := sdkTestConfig()
	srv, web := sdkEventServer(t, cfg, event.Options{})
	const credential = "t-sheets-range"
	requireSDKNetwork(t, srv.Store().PutToken(store.Token{Value: credential, Kind: store.TenantToken, AppID: cfg.Apps[0].AppID, ExpiresAt: time.Now().Add(time.Minute)}))
	request := func(method, path, payload string, status int) []byte {
		return contentCall(t, web, method, path, credential, "application/json", strings.NewReader(payload), status)
	}
	created := sandboxDecode[struct {
		Data struct {
			Spreadsheet struct {
				Token string `json:"spreadsheet_token"`
			} `json:"spreadsheet"`
		} `json:"data"`
	}](t, request(http.MethodPost, "/open-apis/sheets/v3/spreadsheets", `{"title":"Sheet"}`, http.StatusOK))
	id := created.Data.Spreadsheet.Token
	values := "/open-apis/sheets/v2/spreadsheets/" + id + "/values"
	tools := "/open-apis/sheet_ai/v2/spreadsheets/" + id + "/tools/invoke_write"
	request(http.MethodPut, values, `{"valueRange":{"range":"B2","values":[[23]]}}`, http.StatusOK)
	spec := `"input":"{\"excel_id\":\"` + id + `\",\"range\":\"A2:B2\",\"cells\":[[{\"value\":\"partial\"},{\"value\":24}]],\"allow_overwrite\":false}"`
	request(http.MethodPost, tools, `{"tool_name":"set_cell_range",`+spec+`}`, http.StatusBadRequest)
	answer := request(http.MethodGet, values+"/A2:B2", "", http.StatusOK)
	if !bytes.Contains(answer, []byte(`"values":[[null,23]]`)) {
		t.Fatalf("denied write changed some cells: %s", answer)
	}
	request(http.MethodPut, values, `{"valueRange":{"range":"A2:B2","values":[["committed",24]]}}`, http.StatusOK)
	answer = request(http.MethodGet, values+"/A2:B2", "", http.StatusOK)
	if !bytes.Contains(answer, []byte(`"values":[["committed",24]]`)) {
		t.Fatalf("valid write did not round-trip: %s", answer)
	}
}

func contentCall(t *testing.T, web *httptest.Server, method, path, bearer, mediaType string, payload io.Reader, status int) []byte {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, web.URL+path, payload)
	requireSDKNetwork(t, err)
	req.Header.Set("Authorization", "Bearer "+bearer)
	if mediaType != "" {
		req.Header.Set("Content-Type", mediaType)
	}
	resp, err := web.Client().Do(req)
	requireSDKNetwork(t, err)
	defer func() { _ = resp.Body.Close() }()
	answer, err := io.ReadAll(resp.Body)
	requireSDKNetwork(t, err)
	if resp.StatusCode != status {
		t.Fatalf("%s %s = %d, want %d: %s", method, path, resp.StatusCode, status, answer)
	}
	return answer
}
