<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, ref } from 'vue'
import { api } from '@/api/client'
import type { SessionRow, SessionAccountOption } from '@/types'
import { credits, dt } from '@/lib/format'
import { toast } from '@/lib/toast'
import WButton from '@/components/ui/WButton.vue'
import WInput from '@/components/ui/WInput.vue'
import WSelect from '@/components/ui/WSelect.vue'
import WTable from '@/components/ui/WTable.vue'

const rows = ref<SessionRow[]>([]), loading = ref(false), failed = ref(false)
const search = ref(''), model = ref(''), account = ref(''), status = ref(''), sort = ref('recent')
const showSingleInferred = ref(false)
const isWeak = (source: string) => ['fb', 'user', 'pck'].includes(source)
const selectedID = ref(''), options = ref<SessionAccountOption[]>([]), target = ref('')
const optionsLoading = ref(false), optionsVersion = ref(-1), saving = ref(false)
const drawer = ref<HTMLElement | null>(null)
let alive = true, timer: ReturnType<typeof setTimeout> | undefined
let optionsRequest = 0
let previousFocus: HTMLElement | null = null, previousOverflow = ''
const selected = computed(() => rows.value.find(r => r.session.id === selectedID.value))
const changed = computed(() => selected.value?.session.version !== optionsVersion.value)
const models = computed(() => [...new Set(rows.value.map(r => r.session.model))].sort().map(v => ({ value: v, label: v })))
const accounts = computed(() => [...new Map(rows.value.filter(r => r.session.account_uid).map(r => [r.session.account_uid, { value: r.session.account_uid, label: r.account_label || r.session.account_uid.slice(0, 8) }])).values()])
const filtered = computed(() => {
 const query = search.value.toLowerCase().trim()
 const result = rows.value.filter(r => {
  const s = r.session
  if (!showSingleInferred.value && isWeak(s.source) && s.requests === 1 && !s.running && !s.waiting && !s.pending_action && s.id !== selectedID.value) return false
  return (!query || `${s.id} ${s.app} ${s.user_id} ${s.model} ${r.account_label}`.toLowerCase().includes(query)) &&
   (!model.value || s.model === model.value) && (!account.value || s.account_uid === account.value) &&
   (!status.value || (status.value === 'running' ? s.running > 0 : status.value === 'waiting' ? s.waiting > 0 : status.value === 'pending' ? !!s.pending_action : !s.running && !s.waiting))
 })
 return sort.value === 'credits' ? result.sort((a, b) => (b.session.credits_known ? b.session.credits : -1) - (a.session.credits_known ? a.session.credits : -1)) : result
})
const targetOption = computed(() => options.value.find(o => o.uid === target.value))
const columns = [
 { key: 'identity', label: '会话 / 应用' }, { key: 'model', label: '模型' }, { key: 'account', label: '当前账号' },
 { key: 'state', label: '状态' }, { key: 'credits', label: '已观测积分', align: 'right' as const },
 { key: 'last', label: '最近调用' }, { key: 'action', label: '' },
]
function compatibilityLabel(value: string) { return ({ identity: '固定身份兼容', client_metadata: '客户端身份与归因精简' } as Record<string, string>)[value] || '' }
function sourceLabel(source: string) { return ({ sid: '显式会话标识', pck: '缓存分组标识（弱识别）', cid: '显式对话标识', user: '用户字段（弱识别）', fb: '首条消息推断' } as Record<string, string>)[source] || '会话标识' }
function accountLabel(row: SessionRow) { return row.account_label || (row.session.account_uid ? row.session.account_uid.slice(0, 8) : '尚未绑定') }
function stateLabel(row: SessionRow) { const s = row.session; return [s.running ? `执行中 ${s.running}` : '', s.waiting ? `排队中 ${s.waiting}` : ''].filter(Boolean).join(' / ') || '等待下一次调用' }
async function refresh() {
 if (loading.value || !alive) return
 loading.value = true
 try { const result = await api.sessions(); if (alive) { const incoming = result.sessions; if (selectedID.value) { const order = new Map(rows.value.map((r, i) => [r.id, i])); incoming.sort((a, b) => (order.get(a.id) ?? order.size) - (order.get(b.id) ?? order.size)) }; rows.value = incoming; failed.value = false } }
 catch { if (alive) failed.value = true }
 finally { if (alive) loading.value = false }
}
function schedule() {
 timer = setTimeout(async () => { if (alive && !document.hidden && !failed.value) await refresh(); if (alive) schedule() }, 5000)
}
async function loadOptions() {
 const id = selectedID.value
 if (!id || optionsLoading.value) return
 optionsLoading.value = true
 const request = ++optionsRequest
 try {
  const result = await api.sessionAccounts(id)
  if (alive && request === optionsRequest && id === selectedID.value) { options.value = result.accounts; optionsVersion.value = result.session.version; if (!result.accounts.some(o => o.uid === target.value && o.selectable)) target.value = '' }
 } finally { if (alive && request === optionsRequest) optionsLoading.value = false }
}
async function open(row: SessionRow) {
 previousFocus = document.activeElement as HTMLElement
 previousOverflow = document.body.style.overflow; document.body.style.overflow = 'hidden'
 selectedID.value = row.session.id; options.value = []; target.value = ''; optionsVersion.value = -1
 await nextTick(); drawer.value?.focus()
 try { await loadOptions() } catch { /* shared API error feedback */ }
}
function close() { if (saving.value) return; ++optionsRequest; optionsLoading.value = false; selectedID.value = ''; document.body.style.overflow = previousOverflow; previousFocus?.focus() }
async function apply(action: 'switch' | 'reselect' | 'cancel') {
 const row = selected.value
 if (!row || saving.value) return
 saving.value = true
 try {
  await api.sessionControl(row.session.id, action === 'switch' ? optionsVersion.value : row.session.version, action, target.value)
  toast.success(action === 'cancel' ? '待生效调整已取消' : '调整已保存，等待下一次调用')
  target.value = ''; await refresh(); await loadOptions()
 } catch { await refresh(); try { await loadOptions() } catch { /* retain drawer */ } }
 finally { if (alive) saving.value = false }
}
function keydown(event: KeyboardEvent) {
 if (!selectedID.value) return
 if (event.key === 'Escape') { event.preventDefault(); close() }
 if (event.key === 'Tab') {
  const elements = Array.from(drawer.value?.querySelectorAll<HTMLElement>('button:not(:disabled), input:not(:disabled), select:not(:disabled), [tabindex="0"]') || []).filter(el => el.getClientRects().length)
  const first = elements[0], last = elements[elements.length - 1]
  if (!first) { event.preventDefault(); drawer.value?.focus(); return }
  if (event.shiftKey && (document.activeElement === first || document.activeElement === drawer.value)) { event.preventDefault(); last.focus() }
  else if (!event.shiftKey && (document.activeElement === last || document.activeElement === drawer.value)) { event.preventDefault(); first.focus() }
 }
}
onMounted(() => { refresh(); schedule(); document.addEventListener('keydown', keydown) })
onUnmounted(() => { alive = false; clearTimeout(timer); document.removeEventListener('keydown', keydown); if (selectedID.value) document.body.style.overflow = previousOverflow })
</script>

<template>
 <div>
  <div class="mb-3 flex flex-wrap gap-2">
   <WInput v-model="search" placeholder="搜索应用、用户、会话或账号" class="w-full sm:w-64" />
   <WSelect v-model="model" :options="models" placeholder="全部模型" class="min-w-36" />
   <WSelect v-model="account" :options="accounts" placeholder="全部账号" class="min-w-36" />
   <WSelect v-model="status" :options="[{value:'running',label:'正在执行'},{value:'waiting',label:'正在排队'},{value:'pending',label:'待切换'},{value:'idle',label:'近期空闲'}]" placeholder="全部状态" />
   <WSelect v-model="sort" :options="[{value:'recent',label:'最近调用'},{value:'credits',label:'积分消耗'}]" />
   <WButton :loading="loading" @click="refresh">刷新</WButton>
  </div>
  <p class="mb-3 text-micro text-faint">仅 WorkBuddy。保留最近 30 分钟的会话绑定，正在执行的会话继续保留；重启清空。积分仅累计完成调用的实际上报值。相同会话的子 Agent 合并归因。</p>
  <label class="mb-3 flex items-center gap-2 text-micro text-muted"><input v-model="showSingleInferred" type="checkbox">显示已结束的单次推断记录（无稳定标识时无法保证连续识别）</label>
  <p v-if="failed" role="alert" class="mb-3 text-small text-warn">刷新失败，显示上次数据；自动刷新已暂停，请点击刷新重试。</p>
  <div class="overflow-hidden rounded-xl border border-line bg-surface">
   <WTable :columns="columns" :rows="filtered" row-key="id" min-width="880px" :loading="loading">
    <template #cell-identity="{row}">
     <div class="text-ink">{{ row.session.app || '未命名应用' }}<span v-if="row.session.user_id" class="ml-2 text-micro text-faint">用户 {{ row.session.user_id }}</span></div>
     <div class="mono text-micro text-faint">{{ row.session.id.slice(0, 10) }} <span v-if="isWeak(row.session.source)" class="font-sans">· 弱识别</span></div>
    </template>
    <template #cell-model="{row}"><span class="mono">{{ row.session.model }}</span></template>
    <template #cell-account="{row}"><div class="text-ink">{{ accountLabel(row as SessionRow) }}</div><div v-if="row.session.pending_action" class="text-micro text-route">待切换：{{ row.session.pending_action === 'switch' ? row.target_label || row.session.target_uid.slice(0,8) : '按成本重新选择' }}</div></template>
    <template #cell-state="{row}"><span :class="row.session.running ? 'text-live' : row.session.waiting ? 'text-warn' : 'text-muted'">{{ stateLabel(row as SessionRow) }}</span><div v-if="row.session.route_status === 'failed'" class="text-micro text-fault">调整未生效</div></template>
    <template #cell-credits="{row}"><span class="mono">{{ row.session.credits_known ? credits(row.session.credits) : '未知' }}</span><div v-if="row.session.credits_unknown" class="text-micro text-faint">{{ row.session.credits_unknown }} 次未上报</div></template>
    <template #cell-last="{row}"><span class="text-micro">{{ dt(row.session.last_at) }}</span></template>
    <template #cell-action="{row}"><WButton size="sm" variant="subtle" @click="open(row as SessionRow)">调整账号</WButton></template>
    <template #empty><div class="px-5 py-10 text-center text-small text-faint">{{ loading ? '正在读取会话…' : rows.length ? '没有符合筛选条件的会话' : '暂无已观测会话。使用支持会话标识的客户端调用 WorkBuddy 后，会话会出现在这里。' }}</div></template>
   </WTable>
  </div>
  <Teleport to="body">
   <div v-if="selectedID" class="fixed inset-0 z-50 flex justify-end bg-black/55" @click.self="close">
    <section ref="drawer" role="dialog" aria-modal="true" aria-labelledby="session-drawer-title" tabindex="-1" class="flex h-full w-full max-w-xl flex-col border-l border-line bg-surface shadow-xl outline-none">
     <header class="flex items-center justify-between border-b border-line px-5 py-4"><h2 id="session-drawer-title" class="font-semibold text-ink">调整会话账号</h2><WButton variant="subtle" :disabled="saving" @click="close">关闭</WButton></header>
     <div v-if="selected" class="flex-1 space-y-5 overflow-y-auto p-5">
      <div><div class="text-ink">{{ selected.session.app || '未命名应用' }} / {{ selected.session.model }}</div><div class="mono mt-1 text-micro text-faint">{{ selected.session.id.slice(0,12) }}</div><p class="mt-2 text-micro text-faint">{{ sourceLabel(selected.session.source) }}{{ isWeak(selected.session.source) ? '：可能合并独立任务或拆分连续请求；建议客户端发送 X-Session-ID。' : '；同一对话的不同模型分别绑定。' }}</p><p v-if="selected.session.agent_requests" class="mt-1 text-micro text-faint">其中子 Agent 请求 {{ selected.session.agent_requests }} 次</p><p v-if="selected.last_success_label" class="mt-1 text-micro text-faint">最后成功：{{ selected.last_success_label }}</p><p v-if="selected.last_attempt_label" class="mt-1 text-micro text-faint">最后尝试：{{ selected.last_attempt_label }}</p></div>
      <div class="border-y border-line py-3 text-small"><div>当前账号：<span class="text-ink">{{ accountLabel(selected) }}</span></div><div class="mt-1">{{ stateLabel(selected) }}</div><div class="mt-1 text-micro text-faint">自 {{ dt(selected.session.started_at) }} 开始观测 · {{ selected.session.requests }} 次请求</div></div>
      <p v-if="compatibilityLabel(selected.session.compatibility || '')" class="text-micro text-faint">当前账号兼容方式：{{ compatibilityLabel(selected.session.compatibility || '') }}；技能、工具和权限指令保留。</p>
      <div v-if="selected.session.route_message" role="status" class="rounded-lg border border-line px-3 py-2 text-small" :class="selected.session.route_status === 'failed' ? 'text-warn' : 'text-route'">{{ selected.session.route_message }}<div v-if="selected.session.pending_action === 'switch'" class="mt-1">目标：{{ selected.target_label || selected.session.target_uid.slice(0,8) }}</div><WButton v-if="selected.session.pending_action" size="sm" variant="subtle" class="mt-2" :loading="saving" @click="apply('cancel')">取消待切换</WButton></div>
      <div><div class="mb-2 flex items-center justify-between"><h3 class="text-small font-semibold text-ink">可选账号</h3><WButton size="sm" variant="subtle" :loading="optionsLoading" :disabled="saving" @click="loadOptions().catch(() => {})">刷新账号</WButton></div>
       <p class="mb-2 text-micro text-faint">仅允许同成本或更低成本账号。显示成本系数，非本次积分账单；余额为最近一次获取值。</p>
       <div class="space-y-2">
        <button v-for="option in options" :key="option.uid" type="button" :disabled="!option.selectable || saving" :aria-pressed="target === option.uid" class="w-full rounded-lg border p-3 text-left transition-colors disabled:opacity-50" :class="target === option.uid ? 'border-route bg-route/10' : 'border-line hover:bg-elevated'" @click="target = option.uid">
         <div class="flex items-center justify-between text-small"><span class="text-ink">{{ option.label || option.uid.slice(0,8) }}</span><span class="text-micro text-faint">{{ option.site === 'domestic' ? '国内' : '国际' }}</span></div>
         <div class="mt-1 flex flex-wrap gap-3 text-micro text-muted"><span>余额 {{ credits(option.remaining) }}</span><span>成本 {{ option.cost == null ? '未知' : option.cost }} · {{ option.cost_relation }}</span><span>执行中 {{ option.running }}</span></div>
         <div v-if="option.reason" class="mt-1 text-micro text-faint">{{ option.reason }}</div>
        </button>
        <p v-if="!options.length" class="py-3 text-small text-faint">{{ optionsLoading ? '正在核实账号…' : '暂无可选账号，可刷新或按成本重新选择。' }}</p>
       </div>
      </div>
      <p v-if="changed" role="status" class="text-small text-warn">会话状态已变化，请刷新账号后再指定目标。</p>
      <div v-if="targetOption" class="rounded-lg border border-route/40 bg-route/5 p-3 text-small text-ink">{{ accountLabel(selected) }} → {{ targetOption.label || targetOption.uid.slice(0,8) }}<p class="mt-2 text-micro text-muted">下一次尚未开始执行的请求生效；正在执行的请求继续完成。换号可能降低缓存命中，同成本换号只分摊消耗。</p></div>
     </div>
     <div v-else class="flex-1 p-5 text-small text-warn">会话已过期或被移除，请关闭后重新选择。</div>
     <footer class="flex flex-wrap justify-end gap-2 border-t border-line p-4"><WButton :disabled="!selected || saving" @click="apply('reselect')">按成本更换账号</WButton><WButton variant="primary" :disabled="!selected || !target || changed || optionsLoading" :loading="saving" @click="apply('switch')">应用到下一次调用</WButton><p class="w-full text-right text-micro text-faint">仅更换至同成本或更低成本账号；成功后保持新绑定。</p></footer>
    </section>
   </div>
  </Teleport>
 </div>
</template>
