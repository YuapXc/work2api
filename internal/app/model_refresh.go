package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"work2api/internal/core/provider"
)

type modelRefreshResult struct {
	Provider string `json:"provider"`
	Status   string `json:"status"`
	Count    int    `json:"count"`
	Added    int    `json:"added"`
	Removed  int    `json:"removed"`
	Source   string `json:"source"`
	Stale    bool   `json:"stale"`
	Message  string `json:"message,omitempty"`
}

// A complete management round fits inside the client's 30-second deadline.
// Provider gates merge overlapping explicit and scheduled tasks independently.
func (o *Orchestrator) refreshModelCatalogs(ctx context.Context, selected string) []modelRefreshResult {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	runtimes := o.runtimes.Runtimes()
	if selected != "" {
		rt, ok := o.runtimes.ByName(selected)
		if !ok {
			return nil
		}
		runtimes = []provider.Runtime{rt}
	}
	results := make([]modelRefreshResult, len(runtimes))
	var wg sync.WaitGroup
	slots := make(chan struct{}, 3)
	for i, rt := range runtimes {
		wg.Add(1)
		go func(i int, rt provider.Runtime) {
			defer wg.Done()
			result := modelRefreshResult{Provider: rt.Name(), Status: "skipped"}
			defer func() { results[i] = result }()
			if !rt.Ready() {
				result.Message = "渠道尚未配置可用账号"
				return
			}
			refresh, ok := rt.(provider.ModelRefresher)
			if !ok {
				result.Message = "渠道不支持手动刷新"
				return
			}
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				result.Status = "error"
				result.Message = "目录刷新超时或已取消"
				result.Stale = true
				return
			}
			// A free slot and cancellation can both be ready. Do not dispatch
			// discovery merely because select happened to choose the slot.
			if ctx.Err() != nil {
				result.Status = "error"
				result.Message = "目录刷新超时或已取消"
				result.Stale = true
				return
			}
			before := modelIDSet(rt.Models(ctx))
			err := refresh.RefreshModels(ctx)
			models := rt.Models(ctx)
			after := modelIDSet(models)
			result.Count = len(after)
			for id := range after {
				if !before[id] {
					result.Added++
				}
			}
			for id := range before {
				if !after[id] {
					result.Removed++
				}
			}
			for _, m := range models {
				if source, _ := m.Extra["catalog_source"].(string); source != "" {
					result.Source = source
				}
				stale, _ := m.Extra["catalog_stale"].(bool)
				result.Stale = result.Stale || stale
			}
			result.Status = "ok"
			if err != nil {
				result.Status = "error"
				result.Stale = true
				result.Message = "目录刷新失败，保留可用快照"
				if errors.Is(err, provider.ErrPartialRefresh) {
					result.Status = "partial"
					result.Message = "部分来源刷新失败，保留可用快照"
				}
				if ctx.Err() != nil {
					result.Message = "目录刷新超时或已取消，保留可用快照"
				}
			}
			if !o.runtimes.IsCurrent(rt) {
				result.Status = "error"
				result.Message = "渠道配置已变更，请重新刷新"
				result.Stale = true
				result.Added = 0
				result.Removed = 0
			}
		}(i, rt)
	}
	wg.Wait()
	return results
}

func modelIDSet(models []provider.CatalogModel) map[string]bool {
	ids := make(map[string]bool, len(models))
	for _, m := range models {
		if m.ID != "" {
			ids[m.ID] = true
		}
	}
	return ids
}

func modelRefreshWarnings(results []modelRefreshResult) []string {
	warnings := []string{}
	for _, result := range results {
		if result.Status == "error" || result.Status == "partial" {
			warnings = append(warnings, result.Provider+": "+result.Message)
		}
	}
	return warnings
}
