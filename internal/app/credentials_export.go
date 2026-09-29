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
	// workbuddy: from the live managers so refreshed tokens are included.
	wbAccounts := []map[string]any{}
	for _, acc := range s.o.pool.Accounts() {
		mgr := s.o.managers[acc.UID]
		if mgr == nil {
			continue
		}
		session, err := mgr.RawSession()
		if err != nil {
			continue
		}
		entry := map[string]any{"uid": acc.UID, "session": map[string]any{
			"auth": session.Auth, "account": session.Account,
		}}
		wbAccounts = append(wbAccounts, entry)
	}
	exported["workbuddy"] = map[string]any{
		"accounts": wbAccounts,
		"note":     "每个 entry 存为 auths/workbuddy-<uid>.info（session 原样落盘）即可在目标项目导入",
	}

	// Optional runtimes implementing provider.CredentialExporter.
	for _, rt := range provider.Runtimes() {
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
