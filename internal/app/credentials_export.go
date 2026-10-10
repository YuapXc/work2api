package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"work2api/internal/core/provider"
)

// mountCredentialExport registers the admin credential-export endpoint.
func (s *Server) mountCredentialExport(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/credentials/export", s.adminExportCredentials)
}

// adminExportCredentials serves the current credentials of every provider as one
// JSON document for migration to another deployment. Workbuddy sessions come from
// the in-memory managers (post-refresh state), not the possibly stale .info files
// on disk. The response carries secrets — the adminGuard on /admin/* is the only
// gate, same as the config-edit endpoints that already expose keys.
func (s *Server) adminExportCredentials(w http.ResponseWriter, r *http.Request) {
	exported := map[string]any{}
	// Optional runtimes implementing provider.CredentialExporter.
	for _, rt := range s.o.runtimes.Runtimes() {
		ce, ok := rt.(provider.CredentialExporter)
		if !ok {
			continue
		}
		doc, err := ce.ExportCredentials()
		if err != nil {
			doc = map[string]any{"error": err.Error()}
		}
		exported[rt.Name()] = doc
	}

	doc := map[string]any{
		"exported_at": time.Now().UTC().Format(time.RFC3339),
		"providers":   exported,
	}
	name := fmt.Sprintf("work2api-credentials-%s.json", time.Now().UTC().Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	_ = json.NewEncoder(w).Encode(doc)
}
