package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"

	"work2api/internal/benchmarks"
)

func (o *Orchestrator) benchTargets(ctx context.Context) []benchmarks.Target {
	var out []benchmarks.Target
	for _, m := range o.runtimeModels(ctx) {
		id := str2(m["id"])
		provider := str2(m["provider"])
		switch str2(m["model_type"]) {
		case "image", "video", "audio":
			continue
		}
		if id == "" || provider == "workbuddy" && physicalModel(id) == "workbuddy/auto" ||
			strings.EqualFold(str2(m["native_protocol"]), "systemone") || m["catalog_source"] == "fallback" {
			continue
		}
		names, _ := m["benchmark_names"].([]string)
		target := benchmarks.Target{
			ID: physicalModel(id), Provider: provider, Name: str2(m["name"]), ReportedNames: names,
		}
		reasoning, _ := m["reasoning"].(map[string]any)
		target.DefaultEffort = str2(reasoning["defaultEffort"])
		if target.DefaultEffort == "" {
			target.DefaultEffort = str2(reasoning["effort"])
		}
		var efforts []string
		switch levels := reasoning["supportedEfforts"].(type) {
		case []string:
			efforts = append(efforts, levels...)
		case []any:
			for _, level := range levels {
				efforts = append(efforts, str2(level))
			}
		}
		efforts = append(efforts, target.DefaultEffort)
		if reasoning["canDisableThinking"] == true && reasoning["onlyReasoning"] != true {
			efforts = append(efforts, "off")
		}
		for _, level := range efforts {
			level = benchmarks.NormalizeEffort(level)
			if level != "" && (level != "off" || reasoning["onlyReasoning"] != true) && !slices.Contains(target.Efforts, level) {
				target.Efforts = append(target.Efforts, level)
			}
		}
		target.DefaultEffort = benchmarks.NormalizeEffort(target.DefaultEffort)
		if m["benchmark_default_conflict"] == true {
			target.DefaultEffort = ""
		}
		out = append(out, target)
	}
	return out
}

func (s *Server) benchmarkTarget(w http.ResponseWriter, r *http.Request, id string) (benchmarks.Target, bool) {
	for _, target := range s.o.benchTargets(r.Context()) {
		if target.ID == id {
			return target, true
		}
	}
	writeAPIErr(w, errBody(404, "当前目录没有可绑定评测的模型", "invalid_request_error"))
	return benchmarks.Target{}, false
}

func (s *Server) adminBenchmarkBinding(w http.ResponseWriter, r *http.Request) {
	target, ok := s.benchmarkTarget(w, r, r.URL.Query().Get("model"))
	if !ok {
		return
	}
	bindings, err := s.o.db.BenchmarkBindings(r.Context())
	if err != nil {
		writeAPIErr(w, errBody(500, "评测绑定读取失败", "server_error"))
		return
	}
	binding := bindings[target.ID]
	catalog := s.o.bench.Catalog()
	if catalog == nil || !s.o.bench.Configured() {
		writeJSON(w, 200, map[string]any{"ready": false, "binding": binding})
		return
	}
	query := r.URL.Query().Get("q")
	if len(query) > 200 {
		writeAPIErr(w, errBody(400, "评测搜索内容过长", "invalid_request_error"))
		return
	}
	writeJSON(w, 200, map[string]any{
		"ready": true, "binding": binding, "state": catalog.BindingState(target, binding),
		"efforts": target.Efforts, "families": catalog.Candidates(target, binding, query),
	})
}

func (s *Server) adminSaveBenchmarkBinding(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Model   string             `json:"model"`
		Binding benchmarks.Binding `json:"binding"`
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
	if err != nil || json.Unmarshal(raw, &input) != nil {
		writeAPIErr(w, errBody(400, "无效的评测绑定请求", "invalid_request_error"))
		return
	}
	target, ok := s.benchmarkTarget(w, r, input.Model)
	if !ok {
		return
	}
	if input.Binding.EntryID != "" {
		catalog := s.o.bench.Catalog()
		if catalog == nil || !s.o.bench.Configured() {
			writeAPIErr(w, errBody(409, "请先刷新 AA 评测目录", "configuration_conflict"))
			return
		}
		if err := catalog.ValidateBinding(target, input.Binding); err != nil {
			writeAPIErr(w, errBody(400, err.Error(), "invalid_request_error"))
			return
		}
		input.Binding.Identity = target.Identity()
	}
	if err := s.o.db.SaveBenchmarkBinding(r.Context(), target.ID, input.Binding); err != nil {
		writeAPIErr(w, errBody(500, "评测绑定保存失败", "server_error"))
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
