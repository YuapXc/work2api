package app

import (
	"context"
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
	id, err := o.db.LogUsageWithID(params)
	if err == nil {
		trackUsageTiming(ctx, id)
	}
}
