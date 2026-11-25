package sandbox

import (
	"encoding/json"
	"net/http"

	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/cardkit"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

func validateCard(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Card json.RawMessage `json:"card"`
	}
	if !read(w, r, &input, false) {
		return
	}
	if err := cardkit.ValidatePreviewJSON(string(input.Card)); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	reply(w, http.StatusOK, data{"valid": true})
}

func (c *control) cardImage(w http.ResponseWriter, r *http.Request) {
	image, found := c.state.Media().Get(r.PathValue("image_key"))
	if !found || image.Kind != "image" {
		stateError(w, store.ErrNotFound)
		return
	}
	// Sniff stored bytes. Uploaded multipart types are supplied by the client.
	mime := http.DetectContentType(image.Bytes)
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/bmp", "image/x-icon":
		w.Header().Set("Content-Type", mime)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
		_, _ = w.Write(image.Bytes)
	default:
		fail(w, http.StatusUnsupportedMediaType, "preview requires a raster image")
	}
}
