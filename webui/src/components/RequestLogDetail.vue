<script setup lang="ts">
import { computed, ref } from 'vue'
import type { UsageRecord } from '../types'
import { dt, int, latency } from '../lib/format'
import { tokensKnown, reasoningTokens, nonReasoningTokens, observed, channelName, resultLabel, resultClass, costUnit, outputRate, hasGenerationWindow, averageOutputRate, outputBudget } from '../lib/usage-log'
const props=defineProps<{record:UsageRecord;admin?:boolean;accountLabel?:(uid:string,model:string)=>string}>()
const d=computed(()=>props.record.diagnostics),p=computed(()=>d.value?.performance)
const budget=(v?:Record<string,number>)=>v && Object.keys(v).length?Object.entries(v).map(([k,n])=>k+': '+int(n)).join(' · '):'未显式指定'
const compatibility=computed(()=>({original:'原文',identity:'固定身份兼容',client_metadata:'客户端身份与归因精简'} as Record<string,string>)[d.value?.compatibility || ''] || '未记录')
const explanation=computed(()=>{
 if(d.value?.media_task_id) return props.record.status === 'ok' ? '媒体生成已完成。' : '媒体任务尚未完成或生成失败，请使用原任务 ID 查询，避免重复提交。'
 if(d.value?.finish_reason==='length')return '上游报告 length：达到生成预算或上下文边界。本次已产生的思考、回答或工具参数可能不完整。'
 if(d.value?.finish_reason==='content_filter')return '上游报告审核拒绝，HTTP 成功不代表模型输出完整。'
 if(props.record.status==='incomplete')return '响应未完整结束，请结合故障阶段与上游尝试记录定位。'
 if(props.record.status==='error')return '调用失败，请保留请求标识以便定位。'
 return d.value?.finish_reason==='tool_calls'?'模型已返回工具调用，后续由客户端执行。':'本次调用已正常结束。'
})
const copied=ref(''),copyError=ref('')
async function copy(value:string) {try{await navigator.clipboard.writeText(value);copied.value=value;copyError.value=''}catch{copyError.value='复制失败，请手动选择标识复制'}}
const sections=computed(()=>[{title:'输入（已保存片段）',text:props.record.input_content},{title:'思考内容（已保存片段）',text:props.record.reasoning_content},{title:'输出（已保存片段）',text:props.record.output_content}].filter(v=>v.text))
function stageLabel(stage:{headers_finished:boolean;http_status:number;header_wait_ms:number}) {return stage.headers_finished?'HTTP '+(stage.http_status || '无响应')+' · 响应头等待 '+latency(stage.header_wait_ms):'未收到响应头'}
</script>
<template>
 <div class="space-y-4 text-small">
  <p v-if="admin && d?.content_cleared" class="rounded-lg bg-elevated px-3 py-2 text-muted">正文已按保留策略清理；用量、成本与性能记录保留。</p>
  <div class="flex flex-wrap items-center justify-between gap-2"><span class="inline-flex items-center gap-2 rounded-md border px-2 py-1 text-micro font-medium" :class="resultClass(record)">{{resultLabel(record)}}<span v-if="p?.http_status">HTTP {{p.http_status}}</span></span><span class="text-micro text-muted">{{dt(record.ts,'YYYY-MM-DD HH:mm:ss')}}</span></div>
  <section v-if="d?.media_task_id" class="rounded-xl border border-line p-4 space-y-2 text-small"><div class="font-medium text-ink">媒体生成</div><div class="mono break-all text-micro text-muted">{{ d.media_task_id }}</div><div class="flex flex-wrap gap-x-4 gap-y-1 text-muted"><span v-if="d.media?.size">尺寸 {{ d.media.size }}</span><span v-if="d.media?.n">数量 {{ d.media.n }}</span><span v-if="d.media?.seconds">时长 {{ d.media.seconds }} 秒</span><span v-if="d.media?.output_video_duration">实际时长 {{ d.media.output_video_duration }} 秒</span><span v-if="d.media?.resolution">分辨率 {{ d.media.resolution }}</span><span v-if="d.media?.aspect_ratio">画幅 {{ d.media.aspect_ratio }}</span></div></section>
  <p v-if="record.status!=='ok' || d?.finish_reason==='tool_calls'" class="rounded-lg bg-elevated px-3 py-2 text-muted">{{explanation}}</p>
  <div v-if="record.error" class="rounded-lg border border-fault/25 bg-fault/5 p-3 whitespace-pre-wrap break-all text-fault">{{record.error}}</div>
  <section class="rounded-xl border border-line">
   <h4 class="border-b border-line bg-elevated/40 px-4 py-2.5 font-semibold text-ink">请求信息</h4>
   <dl class="grid gap-x-8 gap-y-3 p-4 sm:grid-cols-2">
    <div class="sm:col-span-2"><dt class="text-micro text-muted">调用模型</dt><dd class="mt-0.5 break-all font-medium text-ink">{{d?.requested_model || record.model}}<span v-if="d?.physical_model && d.physical_model!==(d.requested_model || record.model)" class="ml-2 text-muted">↳ {{d.physical_model}}</span></dd></div>
    <div class="flex gap-4"><dt class="w-20 flex-none text-muted">渠道</dt><dd class="text-ink">{{channelName(record)}}</dd></div>
    <div class="flex gap-4"><dt class="w-20 flex-none text-muted">协议</dt><dd class="text-ink">{{record.protocol}}<template v-if="p?.response_stream!=null"> · {{p.response_stream?'流式':'非流式'}}</template></dd></div>
    <div class="flex gap-4"><dt class="w-20 flex-none text-muted">Key</dt><dd class="min-w-0 break-all text-ink">{{record.app_name || '未命名 Key'}}</dd></div>
    <div class="flex gap-4"><dt class="w-20 flex-none text-muted">思考强度</dt><dd class="text-ink">{{record.reasoning_effort || '未记录'}}</dd></div>
    <div v-if="admin && record.account_uid" class="flex gap-4"><dt class="w-20 flex-none text-muted">账号</dt><dd class="min-w-0 break-all text-ink">{{accountLabel?.(record.account_uid,record.model) || record.account_uid}}</dd></div>
    <div v-if="admin" class="flex gap-4"><dt class="w-20 flex-none text-muted">用户</dt><dd class="text-ink">{{record.user_id?'用户 #'+record.user_id:'管理员 Key'}}</dd></div>
    <div class="flex gap-4"><dt class="w-20 flex-none text-muted">结束原因</dt><dd class="text-ink">{{d?.finish_reason || '未上报'}}</dd></div>
   </dl>
  </section>
  <section class="rounded-xl border border-line">
   <h4 class="border-b border-line bg-elevated/40 px-4 py-2.5 font-semibold text-ink">用量与性能</h4>
   <div class="grid gap-5 p-4 sm:grid-cols-2">
    <dl class="space-y-2.5"><div class="flex justify-between gap-3"><dt class="text-muted">输入 Tokens</dt><dd class="tabular-nums font-medium text-ink">{{tokensKnown(record)?int(record.input_tokens):'—'}}</dd></div><div class="flex justify-between gap-3"><dt class="text-muted">输出 Tokens</dt><dd class="tabular-nums font-medium text-ink">{{tokensKnown(record)?int(record.output_tokens):'—'}}</dd></div><div class="flex justify-between gap-3"><dt class="pl-3 text-muted">其中思考</dt><dd class="tabular-nums text-ink">{{int(reasoningTokens(record))}}</dd></div><div class="flex justify-between gap-3"><dt class="pl-3 text-muted" title="可能含工具参数，不等于纯正文">非思考输出</dt><dd class="tabular-nums text-ink">{{int(nonReasoningTokens(record))}}</dd></div><div class="flex justify-between gap-3"><dt class="text-muted">缓存读取</dt><dd class="tabular-nums text-ink">{{int(observed(record.cached_tokens))}}</dd></div><div class="flex justify-between gap-3"><dt class="text-muted">缓存写入</dt><dd class="tabular-nums text-ink">{{int(observed(d?.token_details?.cache_creation_tokens))}}</dd></div></dl>
    <dl class="space-y-2.5 border-t border-line pt-4 sm:border-l sm:border-t-0 sm:pl-5 sm:pt-0">
     <div v-if="p?.first_generation_ms != null" class="flex justify-between gap-3"><dt class="text-muted" title="首次有效思考、正文或工具参数增量，不计开始事件和心跳">首生成</dt><dd class="tabular-nums text-ink">{{latency(p.first_generation_ms)}}</dd></div>
     <div v-if="hasGenerationWindow(record)" class="flex justify-between gap-3"><dt class="text-muted" title="输出总 Tokens ÷ 首次至末次有效生成增量的时间，含思考与工具参数；受上游缓冲和网络影响">生成速度</dt><dd class="tabular-nums font-medium text-ink">{{outputRate(record)}}</dd></div>
     <div v-if="hasGenerationWindow(record)" class="flex justify-between gap-3"><dt class="text-muted">生成阶段耗时</dt><dd class="tabular-nums text-ink">{{latency(p?.generation_ms)}}</dd></div>
     <div class="flex justify-between gap-3"><dt class="text-muted">总耗时</dt><dd class="tabular-nums font-medium text-ink">{{latency(p?.total_ms ?? record.latency_ms)}}</dd></div>
     <div class="flex justify-between gap-3"><dt class="text-muted" title="输出总 Tokens ÷ 整次请求耗时，含思考、等待与重试">整次平均速度</dt><dd class="tabular-nums text-ink">{{averageOutputRate(record)}}</dd></div>
     <div class="flex justify-between gap-3"><dt class="text-muted" title="网关首字节，可能是协议事件，不等于模型首 Token">首响应</dt><dd class="tabular-nums text-ink">{{latency(p?.first_byte_ms)}}</dd></div>
     <div class="flex justify-between gap-3"><dt class="text-muted">上游执行</dt><dd class="tabular-nums text-ink">{{latency(p?.execution_ms)}}</dd></div>
     <div class="flex justify-between gap-3"><dt class="text-muted">排队 / 账号等待</dt><dd class="tabular-nums text-ink">{{p?latency(p.queue_ms)+' / '+latency(p.account_wait_ms):'—'}}</dd></div>
     <div v-if="admin" class="flex justify-between gap-3"><dt class="text-muted">实际消耗</dt><dd class="tabular-nums text-ink">{{record.credits ?? '—'}}<span class="ml-1 text-micro text-muted">{{costUnit(record)}}</span></dd></div>
    </dl>
   </div>
   <p v-if="d?.cache_reuse" class="border-t border-line px-4 py-2.5 text-micro text-muted" title="当前缓存读取 Tokens ÷ 同会话前次输入 Tokens；不代表完整前缀命中率，也不能据此认定跨账号共享缓存。">缓存读取 / 前次输入：{{(d.cache_reuse.read_ratio*100).toFixed(1)}}%<span v-if="d.cache_reuse.account_changed"> · 已换号，缓存复用可能变化</span></p>
   <p class="border-t border-line px-4 py-2.5 text-micro text-muted">输入含缓存，输出含思考。</p>
  </section>
  <details v-if="outputBudget(d?.requested_output_limits)!=null || outputBudget(d?.effective_output_limits)!=null || d?.finish_reason==='length'" :open="d?.finish_reason==='length'" class="rounded-xl border border-line"><summary class="cursor-pointer px-4 py-2.5 font-medium text-ink">输出预算</summary><div class="grid gap-3 border-t border-line p-4 sm:grid-cols-3"><div><div class="text-micro text-muted">客户端请求</div><div class="mt-1 tabular-nums font-semibold text-ink">{{int(outputBudget(d?.requested_output_limits))}}</div><div class="mt-1 break-all text-micro text-muted">{{budget(d?.requested_output_limits)}}</div></div><div><div class="text-micro text-muted">实际发送</div><div class="mt-1 tabular-nums font-semibold text-ink">{{int(outputBudget(d?.effective_output_limits))}}</div><div class="mt-1 break-all text-micro text-muted">{{budget(d?.effective_output_limits)}}</div></div><div><div class="text-micro text-muted">实际生成</div><div class="mt-1 tabular-nums font-semibold text-ink">{{tokensKnown(record)?int(record.output_tokens):'—'}}</div></div></div></details>
  <details v-if="d?.compatibility || d?.error_kind || d?.images_removed || d?.images_repaired" class="rounded-xl border border-line"><summary class="cursor-pointer px-4 py-2.5 text-small text-muted">调用诊断</summary><dl class="space-y-2 border-t border-line p-4"><div v-if="d?.compatibility" class="flex justify-between gap-3"><dt class="text-muted">输入兼容</dt><dd>{{compatibility}}</dd></div><div v-if="d?.images_removed || d?.images_repaired" class="flex flex-wrap justify-between gap-3"><dt class="text-muted">图片恢复</dt><dd class="text-right"><span v-if="d.images_repaired">修复 {{d.images_repaired}} 张 · </span><span v-if="d.images_removed">跳过 {{d.images_removed}} 张 · </span>{{d.image_recovery==='partial'?'局部恢复':'全部图片降级'}}，仅本次重试生效</dd></div><div v-if="d?.error_kind" class="flex justify-between gap-3"><dt class="text-muted">故障分类</dt><dd>{{d.error_kind}}</dd></div></dl></details>
  <section v-if="p?.request_id" class="rounded-xl border border-line p-4"><div class="flex flex-wrap items-center justify-between gap-2"><h4 class="font-medium text-ink">网关请求 ID</h4><button class="rounded border border-line px-2 py-1 text-micro text-muted hover:text-brand" @click="copy(p.request_id)">{{copied===p.request_id?'已复制':'复制'}}</button></div><div class="mono mt-2 select-all break-all text-micro text-muted">{{p.request_id}}</div><p v-if="copyError" class="mt-1 text-micro text-fault">{{copyError}}</p></section>
  <section v-if="p?.attempt_stages?.length" class="rounded-xl border border-line"><h4 class="border-b border-line bg-elevated/40 px-4 py-2.5 font-medium text-ink">上游尝试 · {{p.attempts}} 次</h4><ol class="divide-y divide-line px-4"><li v-for="stage in p.attempt_stages" :key="stage.number" class="flex gap-3 py-3"><span class="flex-none text-micro text-muted">#{{stage.number}}</span><div class="min-w-0 flex-1"><div class="text-ink">{{stageLabel(stage)}}</div><div v-if="stage.upstream_request_id" class="mt-1 flex items-start gap-2"><span class="min-w-0 break-all text-micro text-muted">上游请求 ID：<span class="mono select-all">{{stage.upstream_request_id}}</span></span><button class="flex-none text-micro text-muted hover:text-brand" @click="copy(stage.upstream_request_id)">{{copied===stage.upstream_request_id?'已复制':'复制'}}</button></div></div></li></ol><p v-if="p.attempts>p.attempt_stages.length" class="px-4 pb-3 text-micro text-muted">阶段信息最多保留前 16 次。</p></section>
  <section v-if="record.attempt_records && record.attempt_records.length>1" class="rounded-xl border border-line"><h4 class="border-b border-line bg-elevated/40 px-4 py-2.5 font-medium text-ink">各次命中与消耗</h4><div v-for="item in record.attempt_records" :key="item.id" class="border-b border-line p-4 last:border-b-0"><div class="flex flex-wrap items-center gap-2"><span class="break-all text-ink">{{item.diagnostics?.physical_model || item.model}}</span><span class="rounded border px-1.5 py-0.5 text-micro" :class="resultClass(item)">{{resultLabel(item)}}</span><span v-if="admin && item.account_uid" class="text-micro text-muted">{{accountLabel?.(item.account_uid,item.model) || item.account_uid}}</span><span v-if="admin && item.credit_known && item.credits!=null" class="text-micro">{{item.credits}} {{costUnit(item)}}</span></div><div v-if="item.error" class="mt-1 whitespace-pre-wrap break-all text-micro text-fault">{{item.error}}</div></div><p class="border-t border-line px-4 py-2.5 text-micro text-muted">各次消耗分别保留，跨渠道单位不直接相加。</p></section>
  <template v-if="admin"><details v-for="section in sections" :key="section.title" class="rounded-xl border border-line"><summary class="cursor-pointer px-4 py-2.5 font-medium text-ink">{{section.title}}</summary><pre class="mono max-h-80 overflow-auto border-t border-line p-4 whitespace-pre-wrap break-all text-micro text-muted">{{section.text}}</pre></details></template>
 </div>
</template>
