package app

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// webuiFS holds the embedded Vue build. The `all:` prefix is required because
// Vite emits chunks like `_plugin-vue_export-helper-*.js` whose leading
// underscore a bare //go:embed would silently skip, breaking the app.
//
//go:embed all:webui
var webuiFS embed.FS

// mountWebUI serves the embedded WebUI at / (falling back to a status page).
// Static assets get conservative cache headers; admin pages are never cached by
// shared proxies (the HTML shell only, API responses are dynamic JSON anyway).
func (s *Server) mountWebUI(mux *http.ServeMux) {
	sub, err := fs.Sub(webuiFS, "webui")
	if err != nil {
		return
	}
	assetHandler := http.FileServer(http.FS(sub))
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Baseline hardening for the browser surface; harmless for API clients.
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		// Never let an intermediary cache a page that may carry session state.
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			h.Set("Cache-Control", "no-store")
		} else if strings.HasSuffix(r.URL.Path, ".html") {
			h.Set("Cache-Control", "no-store")
		}
		assetHandler.ServeHTTP(w, r)
	}))
}
