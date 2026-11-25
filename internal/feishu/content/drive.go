package content

import (
	"cmp"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/middleware"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

func (h handler) driveRoot(w http.ResponseWriter, _ *http.Request) {
	reply(w, fields{"token": store.DriveRoot})
}

func (h handler) uploadDrive(w http.ResponseWriter, r *http.Request) {
	const maxFile = 20 << 20
	r.Body = http.MaxBytesReader(w, r.Body, maxFile+(1<<20))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		fail(w, protocol.MalformedRequest)
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	name := r.FormValue("file_name")
	if strings.TrimSpace(name) == "" || path.Base(name) != name || strings.ContainsAny(name, "\\\r\n") || r.FormValue("parent_type") != "explorer" {
		fail(w, protocol.InvalidParameter)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		fail(w, protocol.InvalidParameter)
		return
	}
	defer func() { _ = file.Close() }()
	size, sizeErr := strconv.ParseInt(r.FormValue("size"), 10, 64)
	if sizeErr != nil || header.Size != size || size > maxFile {
		fail(w, protocol.InvalidParameter)
		return
	}
	body, err := io.ReadAll(io.LimitReader(file, maxFile+1))
	if err != nil || int64(len(body)) != size {
		fail(w, protocol.MalformedRequest)
		return
	}
	stored, err := store.Commit(r.Context(), h.state, func() (store.DriveFile, error) {
		return h.state.UploadDriveFile(store.DriveFile{
			ID: r.FormValue("file_token"), Owner: middleware.ResourceOwner(r), Name: name, Parent: cmp.Or(r.FormValue("parent_node"), store.DriveRoot),
			ContentType: http.DetectContentType(body), Data: body,
		})
	})
	if err != nil {
		stateError(w, err)
		return
	}
	reply(w, fields{"file_token": stored.ID})
}

func (h handler) downloadDrive(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.state.DriveFile(middleware.ResourceOwner(r), r.PathValue("file_token"), true)
	if !ok {
		fail(w, protocol.ResourceNotFound)
		return
	}
	w.Header().Set("Content-Type", entry.ContentType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": entry.Name}))
	w.Header().Set("Content-Length", strconv.Itoa(len(entry.Data)))
	_, _ = w.Write(entry.Data)
}

func (h handler) queryDriveToken(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	owner := middleware.ResourceOwner(r)
	if file, ok := h.state.DriveFile(owner, token, false); ok {
		reply(w, fields{"obj_token": file.ID, "obj_type": "file", "is_wiki_token": false})
		return
	}
	if node, ok := h.state.WikiNode(owner, token, "wiki"); ok {
		reply(w, fields{"obj_token": node.ObjectID, "obj_type": node.ObjectType, "is_wiki_token": true})
		return
	}
	if doc, ok := h.state.Document(owner, token); ok {
		reply(w, fields{"obj_token": doc.ID, "obj_type": "docx", "is_wiki_token": false})
		return
	}
	fail(w, protocol.ResourceNotFound)
}

func (h handler) driveExportPermission(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if query.Get("type") != "file" || query.Get("action") != "export" {
		fail(w, protocol.InvalidParameter)
		return
	}
	_, allowed := h.state.DriveFile(middleware.ResourceOwner(r), r.PathValue("file_token"), false)
	reply(w, fields{"auth_result": allowed})
}

func (h handler) driveMetadata(w http.ResponseWriter, r *http.Request) {
	type selector struct {
		Token string `json:"doc_token"`
		Kind  string `json:"doc_type"`
	}
	var input struct {
		Requests []selector `json:"request_docs"`
		WithURL  bool       `json:"with_url"`
	}
	if !read(w, r, &input) {
		return
	}
	if len(input.Requests) == 0 || len(input.Requests) > 200 {
		fail(w, protocol.InvalidParameter)
		return
	}
	owner := middleware.ResourceOwner(r)
	metas, misses := make([]fields, 0), make([]fields, 0)
	for _, request := range input.Requests {
		entry, found := h.state.DriveFile(owner, request.Token, false)
		if !found || request.Kind != "file" {
			misses = append(misses, fields{"token": request.Token, "type": request.Kind, "code": protocol.ResourceNotFound})
			continue
		}
		metadata := fields{"doc_token": entry.ID, "doc_type": "file", "title": entry.Name}
		if input.WithURL {
			metadata["url"] = resourceURL(r, "/open-apis/drive/v1/files/"+entry.ID+"/download")
		}
		metas = append(metas, metadata)
	}
	reply(w, fields{"metas": metas, "failed_list": misses})
}
