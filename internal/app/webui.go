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
		// 门户 SPA：/portal 与 /portal/<任意路径> 都回 portal.html（hash 路由）。
		// API 前缀 /portal/api/* 不经过这里（在 mountPortal 已注册，Go mux 优先
		// 匹配更具体的模式）。未知 API 路径也不会被 SPA 吞掉。
		if strings.HasPrefix(r.URL.Path, "/portal/api/") || strings.HasPrefix(r.URL.Path, "/admin/") || strings.HasPrefix(r.URL.Path, "/v1/") {
			h.Set("Cache-Control", "no-store")
			writeJSON(w, 404, errBody(404, "接口不存在", "not_found").Body)
			return
		}
		if r.URL.Path == "/" || r.URL.Path == "/index.html" || r.URL.Path == "/portal" || strings.HasPrefix(r.URL.Path, "/portal/") {
			h.Set("Cache-Control", "no-store")
			data, err := fs.ReadFile(sub, "portal.html")
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(data)
			return
		}
		if r.URL.Path == "/admin-ui" || strings.HasPrefix(r.URL.Path, "/admin-ui/") {
			h.Set("Cache-Control", "no-store")
			data, err := fs.ReadFile(sub, "index.html")
			if err != nil {
				http.NotFound(w, r)
				return
			}
			h.Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(data)
			return
		}
		// Never let an intermediary cache a page that may carry session state.
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			h.Set("Cache-Control", "no-store")
		} else if strings.HasSuffix(r.URL.Path, ".html") {
			h.Set("Cache-Control", "no-store")
		}
		assetHandler.ServeHTTP(w, r)
	}))
}
