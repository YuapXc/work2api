package account

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOAuthMetadataRejectsStatusMalformedAndOversizedResponses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"upstream failure", 503, `{"userId":"wrong"}`},
		{"invalid JSON", 200, `<html>error</html>`},
		{"null", 200, `null`},
		{"oversized", 200, `{"padding":"` + strings.Repeat("x", 1<<20) + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer s.Close()
			if _, err := oauthJSON(context.Background(), s.URL, ""); err == nil {
				t.Fatal("invalid upstream metadata accepted")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := oauthJSON(ctx, "https://example.invalid", ""); err == nil {
		t.Fatal("cancelled request was not rejected")
	}
}

// 专属资源包解析（buddy-proxy #51）：available/status 双保险判活、多语言标签
// 回退、额度并入总额度。此前整条节点丢弃会让面板显示比实际少一截。
func TestExtractDedicatedPackages(t *testing.T) {
	raw := `{
	  "dedicatedResourcePackages": [
	    {"available": true, "status": "QUOTA_DETAIL_STATUS_ACTIVE", "used": 10, "total": 2000, "remaining": 1990,
	     "expiresAt": 1790000000000, "description": "Only selected models", "unit": "Credits",
	     "displayLabels": [{"dimension": "title", "valueI18n": {"zh-CN": "Qwen 专属积分", "en-US": "Qwen credits"}, "value": "fallback"}]},
	    {"available": false, "used": 1, "total": 100, "remaining": 99},
	    {"available": true, "status": "QUOTA_DETAIL_STATUS_EXPIRED", "used": 1, "total": 100, "remaining": 99},
	    {"available": true, "status": "SOMETHING_NEW_UNKNOWN", "used": 5, "total": 500, "remaining": 495,
	     "name": "act-20260901-170"},
	    {"available": true, "used": 0, "total": 0, "remaining": 0}
	  ]
	}`
	var result map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	pkgs := extractDedicatedPackages(result)
	if len(pkgs) != 2 {
		t.Fatalf("got %d packages, want 2 (active + unknown-status): %+v", len(pkgs), pkgs)
	}
	if pkgs[0].Label != "Qwen 专属积分" {
		t.Errorf("label fallback zh-CN: %q", pkgs[0].Label)
	}
	if pkgs[0].Description != "Only selected models" || pkgs[0].Unit != "Credits" {
		t.Fatal("package usage metadata lost", pkgs[0])
	}
	if pkgs[0].Remaining != 1990 || pkgs[0].Total != 2000 {
		t.Errorf("bucket values: %+v", pkgs[0])
	}
	if pkgs[0].ExpireAt != 1790000000000 {
		t.Errorf("expire ms: %d", pkgs[0].ExpireAt)
	}
	// name 兜底：无 displayLabels 时用 name（活动代号也比「专属积分」信息量大）
	if pkgs[1].Label != "act-20260901-170" {
		t.Errorf("name fallback: %q", pkgs[1].Label)
	}
}

func TestExtractDedicatedPackagesAbsent(t *testing.T) {
	if got := extractDedicatedPackages(map[string]interface{}{}); got != nil {
		t.Errorf("absent node should be nil, got %+v", got)
	}
}

func TestPkgLabelFallbackChain(t *testing.T) {
	// zh-CN 缺失 → en-US
	pkg := map[string]interface{}{
		"displayLabels": []interface{}{map[string]interface{}{
			"dimension": "title", "valueI18n": map[string]interface{}{"en-US": "English"},
		}},
	}
	if l := pkgLabel(pkg); l != "English" {
		t.Errorf("en-US fallback: %q", l)
	}
	// 全缺 → 通用名
	if l := pkgLabel(map[string]interface{}{}); l != "专属积分" {
		t.Errorf("generic fallback: %q", l)
	}
}

func TestReauthorizationPreservesSameAccountDeviceAndPreferences(t *testing.T) {
	oldRoot := DataRoot()
	SetDataRoot(t.TempDir())
	t.Cleanup(func() { SetDataRoot(oldRoot) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/userinfo" {
			w.Write([]byte(`{"userId":"same","email":"same@example.test","name":"upstream"}`))
		} else {
			w.Write([]byte(`{"plan_tier_name":"pro"}`))
		}
	}))
	defer server.Close()
	ep := Endpoints{UserinfoBase: server.URL + "/userinfo", PlanEndpoint: server.URL + "/plan"}
	original, err := buildAccountFromToken(context.Background(), "first", "refresh", RegionCN, ep)
	if err != nil {
		t.Fatal(err)
	}
	original.CheckinFingerprint = &CheckinFingerprint{MachineID: "stable", MachineToken: "token", MachineType: "desktop"}
	original.CreatedAt = time.Unix(100, 0)
	original.Tags = []string{"keep"}
	original.APIMode = "anthropic"
	if err := Save(original); err != nil {
		t.Fatal(err)
	}
	next, err := buildAccountFromToken(context.Background(), "second", "refresh-2", RegionCN, ep)
	if err != nil {
		t.Fatal(err)
	}
	if next.CheckinFingerprint == nil || next.CheckinFingerprint.MachineID != "stable" || !next.CreatedAt.Equal(original.CreatedAt) || next.APIMode != "anthropic" || len(next.Tags) != 1 {
		t.Fatal("reauthorization lost device or preferences", next)
	}
	if err := Save(next); err != nil {
		t.Fatal(err)
	}
	stored, err := Get(next.ID)
	if err != nil || stored.CheckinFingerprint == nil {
		t.Fatal("device not saved", err)
	}
	other, err := buildAccountFromToken(context.Background(), "other", "refresh-3", RegionGlobal, ep)
	if err != nil || other.CheckinFingerprint != nil || other.ID == next.ID {
		t.Fatal("cross-region fingerprint reused", err)
	}
}
