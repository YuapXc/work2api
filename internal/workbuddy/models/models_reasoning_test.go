package models

import "testing"

func TestExtractReasoningKeepsEffort(t *testing.T) {
	// 国内版目录用 reasoning.effort 代替 defaultEffort（实测 18 个模型），
	// 且 effort 从不与 supportedEfforts 同时出现。只记录、不参与裁剪。
	m := map[string]any{
		"id":                "glm-x",
		"supportsReasoning": true,
		"reasoning":         map[string]any{"effort": "medium"},
	}
	cfg := extractReasoning(m)
	if cfg["effort"] != "medium" {
		t.Fatalf("effort 应被提取, got %#v", cfg)
	}
	if _, ok := cfg["defaultEffort"]; ok {
		t.Fatal("有 effort 时不应再造 defaultEffort")
	}
	// defaultEffort 优先：effort 不覆盖
	m["reasoning"] = map[string]any{"effort": "medium", "defaultEffort": "high"}
	cfg = extractReasoning(m)
	if cfg["defaultEffort"] != "high" {
		t.Fatal("defaultEffort 应保留")
	}
	if _, ok := cfg["effort"]; ok {
		t.Fatal("有 defaultEffort 时 effort 不应重复记录")
	}
}

// A4 回归：onlyReasoning 模型即使目录写了 canDisableThinking:true 也不放开 off。
func TestReasoningEffortsOnlyReasoningNeverOffersOff(t *testing.T) {
	r := &Registry{reasoning: map[string]map[string]any{
		"locked": {"supportedEfforts": []string{"high", "xhigh"}, "canDisableThinking": true, "onlyReasoning": true},
		"free":   {"supportedEfforts": []string{"high"}, "canDisableThinking": true},
	}}
	if eff := r.ReasoningEfforts("locked"); contains(eff, "off") {
		t.Fatalf("onlyReasoning 模型不应提供 off, got %v", eff)
	}
	if eff := r.ReasoningEfforts("free"); !contains(eff, "off") {
		t.Fatalf("可关闭思考的模型应提供 off, got %v", eff)
	}
}

// effort（国内版代替 defaultEffort 的「默认档」标注）可作最低档来源，
// 使 onlyReasoning 模型的 bare off 能抬到该档 + thinking:disabled（11150 规避）。
func TestReasoningEffortsEffortAsDefaultRung(t *testing.T) {
	r := &Registry{reasoning: map[string]map[string]any{
		"deepseek-v4-pro": {"effort": "high", "onlyReasoning": true, "supportsReasoning": true},
	}}
	eff := r.ReasoningEfforts("deepseek-v4-pro")
	if len(eff) != 1 || eff[0] != "high" {
		t.Fatalf("effort 应作为唯一档位返回, got %v", eff)
	}
}
