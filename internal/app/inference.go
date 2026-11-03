package app

import (
	"context"
	"log"
	"maps"
	"time"
	"work2api/internal/jsonutil"
	"work2api/internal/store"
	wbruntime "work2api/internal/workbuddy/runtime"
)

type logArgs = wbruntime.LogArgs

var clipContent = wbruntime.ClipContent
var usageTokens = wbruntime.UsageTokens
var usageCountsKnown = wbruntime.UsageCountsKnown
var usageCachedTokens = wbruntime.UsageCachedTokens
var firstInt = wbruntime.FirstInt
var usageReasoningEffort = wbruntime.UsageReasoningEffort
var mergeUsageFromLine = wbruntime.MergeUsageFromLine
var deltaParts = wbruntime.DeltaParts
var sanitizeChatSSE = wbruntime.SanitizeChatSSE
var sanitizeChatSSEWithRole = wbruntime.SanitizeChatSSEWithRole
var isEmptyVal = wbruntime.IsEmptyVal

func (o *Orchestrator) logUsage(a logArgs) { o.wb.LogUsage(a) }
func (o *Orchestrator) extractInputText(body map[string]any) string {
	return o.wb.ExtractInputText(body)
}
func (o *Orchestrator) recordUsage(ctx context.Context, params store.UsageParams) {
	if ctx == nil || ctx.Err() == nil {
		name := "workbuddy"
		if rt, ok := o.runtimes.Resolve(params.Model); ok {
			name = rt.Name()
		}
		o.accountHealth.observe(name, params)
	}
	if ctx != nil {
		if rows, ok := ctx.Value(usageTimingKey{}).(*usageTimingRows); ok && rows.deferWrites {
			if params.Timestamp <= 0 {
				params.Timestamp = float64(time.Now().UnixNano()) / 1e9
			}
			if params.Diagnostics != nil {
				diag := *params.Diagnostics
				diag.RequestedLimits = maps.Clone(diag.RequestedLimits)
				diag.EffectiveLimits = maps.Clone(diag.EffectiveLimits)
				diag.Media = jsonutil.CloneMap(diag.Media)
				params.Diagnostics = &diag
			}
			rows.mu.Lock()
			rows.pending = append(rows.pending, params)
			rows.mu.Unlock()
			return
		}
	}
	o.persistUsage(ctx, params)
}

func (o *Orchestrator) persistUsage(ctx context.Context, params store.UsageParams) {
	id, err := o.db.LogUsageWithID(params)
	if err == nil {
		trackUsageTiming(ctx, id)
	} else {
		log.Printf("保存调用日志失败: %v", err)
	}
}
