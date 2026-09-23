package app

import (
	"embed"
	"io/fs"
	"net/http"
)

// webuiFS holds the embedded Vue build. The `all:` prefix is required because
// Vite emits chunks like `_plugin-vue_export-helper-*.js` whose leading
// underscore a bare //go:embed would silently skip, breaking the app.
//
//go:embed all:webui
var webuiFS embed.FS

// mountWebUI serves the embedded WebUI at / (falling back to a status page).
func (s *Server) mountWebUI(mux *http.ServeMux) {
	sub, err := fs.Sub(webuiFS, "webui")
	if err != nil {
		return
	}
	mux.Handle("GET /", http.FileServer(http.FS(sub)))
}
