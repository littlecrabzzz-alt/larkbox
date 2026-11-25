package webui

import (
	"mime"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestDocuments(t *testing.T) {
	for _, path := range []string{"/", "/index.html", "/chats", "/cards", "/traces", "/faults", "/cards/"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") ||
				rec.Header().Get("Cache-Control") != "no-cache" || !strings.Contains(rec.Body.String(), "<title>larkbox Web Inspector</title>") {
				t.Fatalf("unexpected document: %d %v %s", rec.Code, rec.Header(), rec.Body)
			}
		})
	}
}

func TestAssets(t *testing.T) {
	index := httptest.NewRecorder()
	ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/", nil))
	assets := regexp.MustCompile(`(?:src|href)="(/assets/[^"]+\.(?:js|css))"`).FindAllStringSubmatch(index.Body.String(), -1)
	if len(assets) < 2 {
		t.Fatalf("could not find the production assets in %s", index.Body)
	}
	for _, asset := range assets {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			rec := httptest.NewRecorder()
			ServeHTTP(rec, httptest.NewRequest(method, asset[1], nil))
			kind, _, _ := mime.ParseMediaType(rec.Header().Get("Content-Type"))
			validType := strings.HasSuffix(asset[1], ".css") && kind == "text/css" ||
				strings.HasSuffix(asset[1], ".js") && (kind == "text/javascript" || kind == "application/javascript")
			if rec.Code != http.StatusOK || !validType ||
				rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" ||
				(rec.Body.Len() > 0) != (method == http.MethodGet) {
				t.Errorf("%s %s: unexpected asset response %d %v (length %d)", method, asset[1], rec.Code, rec.Header(), rec.Body.Len())
			}
		}
	}
}

func TestMethodsAndPaths(t *testing.T) {
	for _, tc := range []struct {
		method string
		path   string
		status int
	}{
		{http.MethodHead, "/", http.StatusOK},
		{http.MethodPost, "/", http.StatusMethodNotAllowed},
		{http.MethodPut, "/traces", http.StatusMethodNotAllowed},
		{http.MethodGet, "/../../etc/passwd", http.StatusNotFound},
		{http.MethodGet, "/assets/%2e%2e/index.html", http.StatusNotFound},
		{http.MethodGet, "/assets/%5c..%5cindex.html", http.StatusNotFound},
		{http.MethodGet, "/assets/", http.StatusNotFound},
		{http.MethodGet, "/assets/missing.js", http.StatusNotFound},
		{http.MethodGet, "/dist/index.html", http.StatusNotFound},
	} {
		rec := httptest.NewRecorder()
		ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != tc.status || (tc.method == http.MethodHead && rec.Body.Len() != 0) ||
			(tc.status == http.StatusMethodNotAllowed && rec.Header().Get("Allow") != "GET, HEAD") {
			t.Errorf("%s %s: got %d %v, want %d", tc.method, tc.path, rec.Code, rec.Header(), tc.status)
		}
	}
}
