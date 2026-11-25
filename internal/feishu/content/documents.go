package content

import (
	"bytes"
	"cmp"
	"encoding/json"
	"encoding/xml"
	"net/http"
	"strings"

	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

func documentData(doc store.Document) fields {
	return fields{"document_id": doc.ID, "title": doc.Title, "revision_id": doc.Revision}
}

func (h handler) createDocument(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Format         string          `json:"format"`
		Content        string          `json:"content"`
		ParentToken    string          `json:"parent_token"`
		ParentPosition string          `json:"parent_position"`
		Extra          json.RawMessage `json:"extra_param"`
		Scene          json.RawMessage `json:"scene"`
		ReferenceMap   json.RawMessage `json:"reference_map"`
	}
	if !read(w, r, &input) {
		return
	}
	if strings.TrimSpace(input.Content) == "" || (input.Format != "markdown" && input.Format != "xml") || (input.ParentToken != "" && input.ParentPosition != "") {
		fail(w, protocol.InvalidParameter)
		return
	}
	if input.ParentToken != "" && input.ParentToken != store.DriveRoot {
		fail(w, protocol.ResourceNotFound)
		return
	}
	if input.ParentPosition != "" && input.ParentPosition != "my_library" {
		fail(w, protocol.InvalidParameter)
		return
	}
	title, body := splitTitle(input.Content)
	h.saveDocument(w, r, cmp.Or(title, "Untitled document"), body, input.Format)
}

func splitTitle(content string) (string, string) {
	start, found := strings.CutPrefix(strings.TrimLeft(content, " \t\r\n"), "<title>")
	if found {
		candidate, body, closed := strings.Cut(start, "</title>")
		var title string
		if closed && xml.Unmarshal([]byte("<title>"+candidate+"</title>"), &title) == nil {
			return title, strings.TrimPrefix(body, "\n")
		}
	}
	return "", content
}

func (h handler) createDocx(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Title       string `json:"title"`
		FolderToken string `json:"folder_token"`
	}
	if !read(w, r, &input) {
		return
	}
	if input.FolderToken != "" && input.FolderToken != store.DriveRoot {
		fail(w, protocol.ResourceNotFound)
		return
	}
	h.saveDocument(w, r, cmp.Or(strings.TrimSpace(input.Title), "Untitled document"), "", "markdown")
}

func (h handler) saveDocument(w http.ResponseWriter, r *http.Request, title, body, format string) {
	created, err := store.Commit(r.Context(), h.state, func() (store.Document, error) {
		return h.state.CreateDocument(h.owner(r), title, body, format), nil
	})
	if err != nil {
		stateError(w, err)
		return
	}
	data := documentData(created)
	data["url"] = resourceURL(r, "/open-apis/docx/v1/documents/"+created.ID)
	reply(w, fields{"document": data})
}

func (h handler) document(w http.ResponseWriter, r *http.Request) (store.Document, bool) {
	doc, ok := h.state.Document(h.owner(r), r.PathValue("document_id"))
	if !ok {
		fail(w, protocol.ResourceNotFound)
	}
	return doc, ok
}

func (h handler) readDocx(w http.ResponseWriter, r *http.Request) {
	if doc, ok := h.document(w, r); ok {
		reply(w, fields{"document": documentData(doc)})
	}
}

func (h handler) rawDocx(w http.ResponseWriter, r *http.Request) {
	if doc, ok := h.document(w, r); ok {
		reply(w, fields{"content": renderDocument(doc, "markdown")})
	}
}

func (h handler) fetchDocument(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Format       string          `json:"format"`
		Lang         string          `json:"lang"`
		Revision     int64           `json:"revision_id"`
		Extra        json.RawMessage `json:"extra_param"`
		ExportOption json.RawMessage `json:"export_option"`
		ReadOption   struct {
			Mode string `json:"read_mode"`
		} `json:"read_option"`
		Scene json.RawMessage `json:"scene"`
	}
	if !read(w, r, &input) {
		return
	}
	if (input.Format != "markdown" && input.Format != "xml") || (input.ReadOption.Mode != "" && input.ReadOption.Mode != "full") {
		fail(w, protocol.InvalidParameter)
		return
	}
	doc, ok := h.document(w, r)
	if !ok {
		return
	}
	if input.Revision > 0 && input.Revision != doc.Revision {
		fail(w, protocol.ResourceNotFound)
		return
	}
	data := documentData(doc)
	data["content"] = renderDocument(doc, input.Format)
	data["format"] = input.Format
	reply(w, fields{"document": data})
}

func renderDocument(doc store.Document, format string) string {
	if format == doc.Format || doc.Content == "" {
		return doc.Content
	}
	if format == "xml" {
		var content bytes.Buffer
		content.WriteString("<p>")
		_ = xml.EscapeText(&content, []byte(doc.Content))
		content.WriteString("</p>")
		return content.String()
	}
	var content strings.Builder
	parser := xml.NewDecoder(strings.NewReader(doc.Content))
	for {
		token, err := parser.Token()
		if err != nil {
			break
		}
		switch item := token.(type) {
		case xml.CharData:
			content.Write(item)
		case xml.EndElement:
			if item.Name.Local == "p" {
				content.WriteByte('\n')
			}
		}
	}
	return strings.TrimRight(content.String(), "\n")
}
