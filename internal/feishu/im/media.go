package im

import (
	"cmp"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

const (
	maxImageSize    = 10 << 20
	maxFileSize     = 30 << 20
	maxFilenameSize = 255
)

func (h handler) uploadImage(w http.ResponseWriter, r *http.Request) {
	h.upload(w, r, "image", protocol.ImageKey, maxImageSize, []string{"message", "avatar"})
}

func (h handler) uploadFile(w http.ResponseWriter, r *http.Request) {
	h.upload(w, r, "file", protocol.FileKey, maxFileSize, []string{"opus", "mp4", "pdf", "doc", "xls", "ppt", "stream"})
}

func (h handler) upload(w http.ResponseWriter, r *http.Request, kind string, prefix protocol.IDPrefix, maxSize int64, allowed []string) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSize+(1<<20))
	reader, err := r.MultipartReader()
	if err != nil {
		badField(w, "body", "Expected multipart/form-data.")
		return
	}
	form, err := reader.ReadForm(maxSize)
	if err != nil {
		badField(w, "body", "Invalid multipart upload or size limit exceeded.")
		return
	}
	defer func() { _ = form.RemoveAll() }()

	typeField := kind + "_type"
	types := form.Value[typeField]
	if len(types) != 1 || !slices.Contains(allowed, types[0]) {
		badField(w, typeField, "Expected one of: "+strings.Join(allowed, ", ")+".")
		return
	}
	parts := form.File[kind]
	if len(parts) != 1 || parts[0].Size == 0 || parts[0].Size > maxSize {
		badField(w, kind, "Expected one non-empty file within the upload size limit.")
		return
	}
	part := parts[0]
	media := store.Media{
		Key: protocol.NewID(prefix), Kind: kind, UploadType: types[0], AppID: caller(r).AppID,
		Filename: safeFilename(part.Filename),
	}
	if len(media.Filename) > maxFilenameSize || !utf8.ValidString(media.Filename) {
		badField(w, kind, "The attachment filename must be valid UTF-8 and at most 255 bytes.")
		return
	}
	if kind == "file" {
		names := form.Value["file_name"]
		if len(names) != 1 || len(names[0]) > maxFilenameSize || !utf8.ValidString(names[0]) || safeFilename(names[0]) == "" {
			badField(w, "file_name", "Expected a valid UTF-8 filename of at most 255 bytes.")
			return
		}
		media.Filename = safeFilename(names[0])
		if durations, present := form.Value["duration"]; present {
			if len(durations) != 1 {
				badField(w, "duration", "Expected one nonnegative duration in milliseconds.")
				return
			}
			duration, err := strconv.ParseInt(durations[0], 10, 64)
			if err != nil || duration < 0 {
				badField(w, "duration", "Expected a nonnegative duration in milliseconds.")
				return
			}
			media.Duration = duration
		}
	}
	file, err := part.Open()
	if err != nil {
		badField(w, kind, "Could not read the file.")
		return
	}
	defer func() { _ = file.Close() }()
	media.Bytes, err = io.ReadAll(io.LimitReader(file, maxSize+1))
	if err != nil || len(media.Bytes) == 0 || int64(len(media.Bytes)) > maxSize {
		badField(w, kind, "Could not read the file within the upload size limit.")
		return
	}
	media.ContentType = part.Header.Get("Content-Type")
	supplied, _, err := mime.ParseMediaType(media.ContentType)
	if err != nil || supplied == "application/octet-stream" {
		media.ContentType = http.DetectContentType(media.Bytes)
	}
	h.state.Media().Put(media)
	success(w, fields{kind + "_key": media.Key})
}

func safeFilename(name string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name))
	if name == "." || name == ".." || name == "/" {
		return ""
	}
	return name
}

func (h handler) downloadImage(w http.ResponseWriter, r *http.Request) {
	h.download(w, r, "image", r.PathValue("image_key"))
}

func (h handler) downloadFile(w http.ResponseWriter, r *http.Request) {
	h.download(w, r, "file", r.PathValue("file_key"))
}

func (h handler) download(w http.ResponseWriter, r *http.Request, kind, key string) {
	media, found := h.state.Media().Get(key)
	if !found || media.Kind != kind || (kind == "image" && media.UploadType == "avatar") {
		fail(w, 234001, "Invalid request param.")
		return
	}
	if media.AppID != caller(r).AppID {
		_ = protocol.WriteError(w, protocol.APIError{Code: 234002, Message: "Unauthorized.", Status: http.StatusUnauthorized})
		return
	}
	writeMedia(w, media)
}

func writeMedia(w http.ResponseWriter, media store.Media) {
	filename := cmp.Or(media.Filename, media.Key)
	w.Header().Set("Content-Type", media.ContentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(media.Bytes)))
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(media.Bytes)
}

func (h handler) downloadResource(w http.ResponseWriter, r *http.Request) {
	kind, ok := queryChoice(w, r, "type", "", "image", "file")
	if !ok {
		return
	}
	m, err := h.state.IMResourceMessage(caller(r), r.PathValue("message_id"))
	switch {
	case errors.Is(err, store.ErrOutsideChat):
		fail(w, 234004, "App not in chat.")
		return
	case err != nil:
		fail(w, 234001, "Invalid request param.")
		return
	case m.Deleted:
		fail(w, 230110, "Action unavailable as the message has been deleted.")
		return
	case m.MsgType == "interactive" || m.MsgType == "merge_forward":
		fail(w, 234043, "Unsupported message type.")
		return
	}
	key := r.PathValue("file_key")
	if referencesResource(m, kind, key) {
		media, found := h.state.Media().Get(key)
		if found && media.Kind == kind && media.AppID == m.AppID && media.UploadType != "avatar" {
			writeMedia(w, media)
			return
		}
	}
	fail(w, 234003, "File not in message.")
}

func referencesResource(m store.Message, kind, key string) bool {
	var content map[string]any
	if json.Unmarshal([]byte(m.Content), &content) != nil {
		return false
	}
	switch {
	case kind == "image" && (m.MsgType == "image" || m.MsgType == "media"):
		return content["image_key"] == key
	case kind == "file" && (m.MsgType == "file" || m.MsgType == "audio" || m.MsgType == "media"):
		return content["file_key"] == key
	case m.MsgType == "post":
		elements, ok := parsePost(content)
		if !ok {
			return false
		}
		for _, element := range elements {
			if (element["tag"] == "media" || (kind == "image" && element["tag"] == "img")) &&
				element[kind+"_key"] == key {
				return true
			}
		}
	}
	return false
}
