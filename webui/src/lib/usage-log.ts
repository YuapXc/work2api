import type { UsageRecord } from '../types'

// Older databases have stored counts but no observation marker. Display those
// consistent positive totals as historical records, never promote explicit unknowns.
export function legacyTokens(r: Partial<UsageRecord>) {
 return r.tokens_known == null && observed(r.input_tokens)!=null && observed(r.output_tokens)!=null
  && observed(r.total_tokens)!=null && (r.total_tokens ?? 0)>0
  && r.total_tokens===(r.input_tokens ?? 0)+(r.output_tokens ?? 0)
}
export function tokensKnown(r: Partial<UsageRecord>) { return r.tokens_known === true || r.tokens_known === 1 || legacyTokens(r) }
export function observed(n?: number | null): number | null {
  return n != null && Number.isSafeInteger(n) && n >= 0 ? n : null
}
export function reasoningTokens(r: Partial<UsageRecord>) {
  const n = observed(r.diagnostics?.token_details?.reasoning_tokens)
  return tokensKnown(r) && n != null && n <= (r.output_tokens ?? -1) ? n : null
}
export function nonReasoningTokens(r: Partial<UsageRecord>) {
  const n = reasoningTokens(r)
  return n == null ? null : (r.output_tokens ?? 0) - n
}
export function channelName(r: Partial<UsageRecord>) {
  const model = r.diagnostics?.physical_model || r.model || ''
  const id = model.includes('/') ? model.split('/')[0] : 'workbuddy'
  return ({workbuddy:'WorkBuddy',qoder:'Qoder',trae:'Trae',opencode:'OpenCode',unified:'统一路由'} as Record<string,string>)[id] || id
}
export function resultLabel(r: Partial<UsageRecord>) {
  const reason = r.diagnostics?.finish_reason
  if (r.diagnostics?.media_task_id && r.status === 'incomplete') return ({submitting:'提交中',queued:'排队中',in_progress:'生成中',unknown:'结果待确认'} as Record<string,string>)[reason || ''] || '处理中'
  if (reason === 'content_filter') return '审核拒绝'
  if (r.status === 'incomplete') return reason === 'length' ? '输出上限' : '输出未完成'
  if (r.status === 'ok') return reason === 'tool_calls' ? '工具调用' : '成功'
  return ({cancelled:'已取消',timeout:'超时',response_limit:'响应过大',local_network:'网络异常'} as Record<string,string>)[r.diagnostics?.error_kind || ''] || '失败'
}
export function resultClass(r: Partial<UsageRecord>) {
  return r.status === 'ok' ? 'border-live/25 bg-live/10 text-live' : r.status === 'incomplete' ? 'border-warn/25 bg-warn/10 text-warn' : 'border-fault/25 bg-fault/10 text-fault'
}
export function costUnit(r: Partial<UsageRecord>) {
  return channelName(r) === 'OpenCode' ? '上游消耗（单位未确认）' : channelName(r) + ' 积分'
}

// Observed generation window, including reasoning/text/tool argument deltas.
// Historical rows and single-frame replies have no reliable generation interval.
export function outputRate(r: Partial<UsageRecord>): string {
 return formatOutputRate(r,r.diagnostics?.performance?.generation_ms)
}

export function hasGenerationWindow(r: Partial<UsageRecord>): boolean {
 const duration=r.diagnostics?.performance?.generation_ms
 return typeof duration==='number' && Number.isFinite(duration) && duration>0
}

// Keep existing observable metrics visible without relabelling them as new
// generation metrics. This also covers upstream single-document replies.
export function displayedOutputRate(r: Partial<UsageRecord>): string {
 return hasGenerationWindow(r)?outputRate(r):averageOutputRate(r)
}
// Whole-request average, kept in details for comparison with older records.
export function averageOutputRate(r: Partial<UsageRecord>): string {
 const duration=r.diagnostics?.performance?.total_ms ?? r.latency_ms
 return formatOutputRate(r,duration)
}
function formatOutputRate(r: Partial<UsageRecord>,duration?:number):string {
 if(!tokensKnown(r) || (r.status!=='ok' && r.status!=='incomplete') || observed(r.output_tokens)==null || (r.output_tokens ?? 0)<=0 || duration==null || !Number.isFinite(duration) || duration<=0)return '—'
 const rate=(r.output_tokens ?? 0)*1000/duration
 return Number.isFinite(rate)?rate.toFixed(1)+' tok/s':'—'
}
export function outputBudget(v?:Record<string,number>):number|null {
 for(const key of ['max_output_tokens','max_completion_tokens','max_tokens']) {const n=observed(v?.[key]);if(n!=null && n>0)return n}
 return null
}

