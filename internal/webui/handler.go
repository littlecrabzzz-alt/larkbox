package webui

import (
	"bytes"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
)

// ServeHTTP serves the inspector's documents and Vite-generated assets.
func ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Path
	if (name != "/" && path.Clean(name) != strings.TrimSuffix(name, "/")) || strings.Contains(name, "\\") {
		http.NotFound(w, r)
		return
	}

	cache := "no-cache"
	switch strings.TrimSuffix(name, "/") {
	case "", "/index.html", "/chats", "/cards", "/traces", "/faults":
		name = "/index.html"
	default:
		asset, ok := strings.CutPrefix(name, "/assets/")
		if !ok || !fs.ValidPath(asset) || strings.Contains(asset, "/") || strings.HasPrefix(asset, ".") {
			http.NotFound(w, r)
			return
		}
		// Vite names every file in the assets directory with a content hash.
		cache = "public, max-age=31536000, immutable"
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := bundle.ReadFile("dist" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", cache)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	if mediaType := mime.TypeByExtension(path.Ext(name)); mediaType != "" {
		w.Header().Set("Content-Type", mediaType)
	}
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(body))
}
