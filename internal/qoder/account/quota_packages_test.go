package account

import (
	"encoding/json"
	"testing"
)

// 专属资源包解析（buddy-proxy #51）：available/status 双保险判活、多语言标签
// 回退、额度并入总额度。此前整条节点丢弃会让面板显示比实际少一截。
func TestExtractDedicatedPackages(t *testing.T) {
	raw := `{
	  "dedicatedResourcePackages": [
	    {"available": true, "status": "QUOTA_DETAIL_STATUS_ACTIVE", "used": 10, "total": 2000, "remaining": 1990,
	     "expiresAt": 1790000000000,
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
