<script setup lang="ts">
import { computed, ref, nextTick, onMounted, onUnmounted, useId } from 'vue'
import type { UsageRecord } from '../types'
import WTable, { type Column } from './ui/WTable.vue'
import WButton from './ui/WButton.vue'
import WEmpty from './ui/WEmpty.vue'
import { dt, int, latency } from '../lib/format'
import { tokensKnown, reasoningTokens, observed, channelName, resultLabel, resultClass, costUnit, hasGenerationWindow, displayedOutputRate } from '../lib/usage-log'
const props=defineProps<{ rows: UsageRecord[]; accountLabel?: (uid: string, model: string) => string; admin?: boolean }>()
defineEmits<{ detail: [id: number] }>()
const choices: Column[]=[{key:'effort',label:'思考强度',nowrap:true},{key:'cost',label:'实际消耗',nowrap:true,align:'right' as const}].filter(c=>props.admin || c.key!=='cost')
const preference='usage-log-columns-'+(props.admin?'admin':'portal')
const extra=ref<string[]>([])
const menuOpen=ref(false), menuButton=ref<HTMLButtonElement|null>(null), menuPanel=ref<HTMLElement|null>(null)
const menuID=useId()
const menuStyle=ref({left:'0px',top:'0px'})
function closeMenu(focus=false) {menuOpen.value=false;if(focus)menuButton.value?.focus()}
async function openMenu() {
 if(menuOpen.value){closeMenu(true);return}
 const rect=menuButton.value?.getBoundingClientRect();if(!rect)return
 const width=192,height=choices.length*36+16
 const left=Math.max(8,Math.min(rect.right-width,window.innerWidth-width-8))
 const top=rect.bottom+height+6<=window.innerHeight?rect.bottom+6:Math.max(8,rect.top-height-6)
 menuStyle.value={left:left+'px',top:top+'px'}
 menuOpen.value=true
 await nextTick();menuPanel.value?.focus()
}
function dismissOutside(event:PointerEvent) {const target=event.target as Node|null;if(target && !menuPanel.value?.contains(target) && !menuButton.value?.contains(target))closeMenu()}
function dismissKey(event:KeyboardEvent) {if(menuOpen.value && event.key==='Escape'){event.preventDefault();event.stopPropagation();closeMenu(true)}}
function dismissMove() {closeMenu()}
onMounted(()=>{document.addEventListener('pointerdown',dismissOutside);document.addEventListener('keydown',dismissKey);window.addEventListener('resize',dismissMove);window.addEventListener('scroll',dismissMove,true)})
onUnmounted(()=>{document.removeEventListener('pointerdown',dismissOutside);document.removeEventListener('keydown',dismissKey);window.removeEventListener('resize',dismissMove);window.removeEventListener('scroll',dismissMove,true)})

function statusHint(row:Partial<UsageRecord>) {
 const p=row.diagnostics?.performance
 return [resultLabel(row),p?.http_status?'HTTP '+p.http_status:'', (p?.attempts || row.logged_attempts || 0)>1?(p?.attempts || row.logged_attempts)+' 次上游尝试':''].filter(Boolean).join(' · ')
}
try {const saved=JSON.parse(localStorage.getItem(preference)||'[]');if(Array.isArray(saved))extra.value=saved.filter(v=>choices.some(c=>c.key===v))}catch{}
function toggle(key:string) {extra.value=extra.value.includes(key)?extra.value.filter(v=>v!==key):[...extra.value,key];try{localStorage.setItem(preference,JSON.stringify(extra.value))}catch{}}
const columns=computed<Column[]>(()=>[
 {key:'ts',label:'时间',nowrap:true,minWidth:'125px'},
 {key:'model',label:'模型 / 渠道',minWidth:'200px'},
 {key:'app_name',label:props.admin?'Key / 用户':'Key / 协议',nowrap:true,minWidth:'145px'},
 {key:'tokens',label:'用量',nowrap:true,minWidth:'160px',hint:'输入含缓存，输出含思考及工具调用；完整细分见详情'},
 {key:'timing',label:'耗时 / 速度',align:'right',nowrap:true,minWidth:'130px'},
 ...choices.filter(c=>extra.value.includes(c.key)),
 {key:'actions',label:'',align:'right',nowrap:true},
])
</script>
<template>
 <div class="space-y-2">
  <WTable :columns="columns" :rows="rows" row-key="id" min-width="800px">
   <template #head-actions><button ref="menuButton" type="button" class="inline-flex items-center gap-1.5 rounded-md px-2 py-1 text-micro font-normal normal-case tracking-normal text-muted hover:bg-line/40 hover:text-ink focus-visible:outline focus-visible:outline-2 focus-visible:outline-brand" :aria-expanded="menuOpen" :aria-controls="menuID" aria-haspopup="dialog" @click="openMenu"><svg aria-hidden="true" class="h-4 w-4" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.5"><rect x="3" y="4" width="14" height="12" rx="2"/><path d="M8 4v12M13 4v12"/></svg>显示列</button></template>
   <template #cell-ts="{row}"><div class="flex items-center gap-3"><span role="img" :aria-label="statusHint(row)" :title="statusHint(row)" class="flex h-5 w-5 flex-none items-center justify-center rounded-full text-xs font-semibold" :class="resultClass(row)">{{row.status==='ok'?'✓':row.status==='incomplete'?'!':'×'}}</span><div><div class="text-small text-ink">{{dt(row.ts,'MM-DD')}}</div><div class="mono text-micro text-faint">{{dt(row.ts,'HH:mm:ss')}}</div></div></div></template>
   <template #cell-model="{row}">
    <div class="max-w-64 truncate font-medium text-ink" :title="row.diagnostics?.requested_model || row.model">{{row.diagnostics?.requested_model || row.model}}</div>
    <div v-if="row.diagnostics?.physical_model && row.diagnostics.physical_model!==(row.diagnostics.requested_model || row.model)" class="max-w-64 truncate text-micro text-faint" :title="row.diagnostics.physical_model">↳ {{row.diagnostics.physical_model}}</div>
    <div class="mt-1 flex items-center gap-2"><span class="text-micro text-faint">{{channelName(row)}}</span><span v-if="admin && row.account_uid" class="max-w-40 truncate text-micro text-faint" :title="accountLabel?.(row.account_uid,row.diagnostics?.physical_model || row.model) || row.account_uid">{{accountLabel?.(row.account_uid,row.diagnostics?.physical_model || row.model) || row.account_uid.slice(0,8)}}</span></div>
   </template>
   <template #cell-app_name="{row}"><div class="max-w-40 truncate font-medium text-ink" :title="row.app_name">{{row.app_name || '未命名 Key'}}</div><div class="text-micro text-faint">{{row.protocol}}<template v-if="row.diagnostics?.performance?.response_stream!=null"> · {{row.diagnostics.performance.response_stream?'流式':'非流式'}}</template></div><div v-if="admin && row.user_id" class="text-micro text-faint">用户 #{{row.user_id}}</div></template>
   <template #cell-tokens="{row}">
    <div class="flex items-center gap-4 tabular-nums"><span class="inline-flex items-center gap-1.5" title="输入总 Tokens，含缓存"><span aria-hidden="true" class="text-live">↓</span><span class="sr-only">输入 Tokens：</span><span class="font-medium text-ink">{{tokensKnown(row)?int(row.input_tokens):'—'}}</span></span><span class="inline-flex items-center gap-1.5" title="输出总 Tokens，含思考及工具调用"><span aria-hidden="true" class="text-muted">↑</span><span class="sr-only">输出 Tokens：</span><span class="font-medium text-ink">{{tokensKnown(row)?int(row.output_tokens):'—'}}</span></span></div>
    <div v-if="(reasoningTokens(row) ?? 0)>0 || (observed(row.cached_tokens) ?? 0)>0 || (observed(row.diagnostics?.token_details?.cache_creation_tokens) ?? 0)>0" class="mt-1 flex flex-wrap gap-x-3 gap-y-0.5 text-micro tabular-nums text-muted">
     <span v-if="(reasoningTokens(row) ?? 0)>0">思考 {{int(reasoningTokens(row))}}</span><span v-if="(observed(row.cached_tokens) ?? 0)>0" title="缓存读取 Tokens">缓存读 {{int(row.cached_tokens)}}</span><span v-if="(observed(row.diagnostics?.token_details?.cache_creation_tokens) ?? 0)>0" title="缓存写入 Tokens">缓存写 {{int(row.diagnostics.token_details.cache_creation_tokens)}}</span>
    </div>
   </template>
   <template #cell-timing="{row}"><div class="inline-grid grid-cols-[auto_auto] gap-x-3 gap-y-0.5 text-micro tabular-nums"><span class="text-faint" :title="row.diagnostics?.performance?.first_generation_ms != null ? '首次有效思考、正文或工具参数增量，不计开始事件和心跳' : '首次下游写入，可能是协议事件，不等于首个生成增量'">{{row.diagnostics?.performance?.first_generation_ms != null ? '首生成' : '首响应'}}</span><span class="text-ink">{{latency(row.diagnostics?.performance?.first_generation_ms ?? row.diagnostics?.performance?.first_byte_ms)}}</span><span class="text-faint">总耗时</span><span class="font-medium text-ink">{{latency(row.diagnostics?.performance?.total_ms ?? row.latency_ms)}}</span><span class="text-faint" :title="hasGenerationWindow(row) ? '输出总 Tokens ÷ 有效生成增量时间，含思考与工具参数；受缓冲和网络影响' : '输出总 Tokens ÷ 整次请求耗时，含思考、等待与重试'">{{hasGenerationWindow(row) ? '生成速度' : '平均速度'}}</span><span class="text-ink">{{displayedOutputRate(row)}}</span></div></template>
   <template #cell-effort="{row}"><span class="text-micro text-muted">{{row.reasoning_effort || '未记录'}}</span></template>
   <template #cell-cost="{row}"><div class="tabular-nums text-ink">{{row.credits ?? '—'}}</div><div class="text-micro text-faint">{{costUnit(row)}}</div></template>
   <template #cell-actions="{row}"><WButton size="sm" variant="subtle" @click="$emit('detail',row.id)">详情</WButton></template>
   <template #empty><WEmpty title="没有调用记录" hint="调整筛选条件，或等待新的请求。" /></template>
  </WTable>
  <Teleport to="body"><div v-if="menuOpen" :id="menuID" ref="menuPanel" role="dialog" aria-label="显示列" tabindex="-1" :style="menuStyle" class="fixed z-[80] w-48 rounded-lg border border-line bg-surface p-2 text-small text-ink shadow-xl outline-none"><label v-for="choice in choices" :key="choice.key" class="flex min-h-9 cursor-pointer items-center gap-2 rounded-md px-2 hover:bg-elevated"><input type="checkbox" :checked="extra.includes(choice.key)" @change="toggle(choice.key)" />{{choice.label}}</label></div></Teleport>
 </div>
</template>
