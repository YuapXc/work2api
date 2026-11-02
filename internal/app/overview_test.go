package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"work2api/internal/store"
	"work2api/internal/workbuddy/models"
	"work2api/internal/workbuddy/pool"
)

func TestOverviewCompactPreservesCountersAndProviderCards(t *testing.T) {
	o, _ := newDNSFailoverOrch(t, &failingUpstream{})
	acc := o.wb.Pool.Accounts()[0]
	o.wb.Pool = pool.New(map[string]pool.Credential{acc.UID: catalogOffline{acc.Mgr}}, "")
	o.wb.Catalog = models.NewWithCatalogClient(o.wb.Pool, o.db, &http.Client{Transport: catalogTransport{}})
	o.wb.Catalog.Refresh()
	if err := o.db.LogUsage(store.UsageParams{Model: "test", Status: "ok", InputTokens: 2, OutputTokens: 3}); err != nil {
		t.Fatal(err)
	}
	s := NewServer(o)
	for _, compact := range []bool{false, true} {
		path := "/admin/overview"
		if compact {
			path += "?compact=1"
		}
		w := httptest.NewRecorder()
		s.adminOverview(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
		var result struct {
			Usage     map[string]any   `json:"usage"`
			Providers []map[string]any `json:"providers"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Usage["today_tokens"] != float64(5) || result.Usage["today_requests"] != float64(1) {
			t.Fatalf("lost counters: %v", result.Usage)
		}
		if len(result.Providers) == 0 {
			t.Fatal("provider cards missing")
		}
		_, full := result.Usage["by_model"]
		if full == compact {
			t.Fatalf("legacy/compact contract changed: %v", result.Usage)
		}
	}
}
