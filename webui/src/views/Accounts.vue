<script setup lang="ts">
import { useLiveRead } from '@/lib/liveRead'
import { ref, computed, nextTick, onMounted, onUnmounted, watch } from 'vue'
import { api } from '@/api/client'
import type { ProviderSummary, OAuthOption, ResourceSummary } from '@/types'
import { toast } from '@/lib/toast'
import { confirm } from '@/lib/confirm'
import { credits as fmtCredits, dt, cooldown } from '@/lib/format'
import { providerMeta, siteLabel } from '@/lib/providers'
import WPage from '@/components/ui/WPage.vue'
import WCard from '@/components/ui/WCard.vue'
import WTable, { type Column } from '@/components/ui/WTable.vue'
import WButton from '@/components/ui/WButton.vue'
import WInput from '@/components/ui/WInput.vue'
import WTextarea from '@/components/ui/WTextarea.vue'
import WSelect from '@/components/ui/WSelect.vue'
import WModal from '@/components/ui/WModal.vue'
import WTag from '@/components/ui/WTag.vue'
import WToggle from '@/components/ui/WToggle.vue'
import ResourceActions from '@/components/accounts/ResourceActions.vue'
import WLed from '@/components/ui/WLed.vue'
import WEmpty from '@/components/ui/WEmpty.vue'
import WSpinner from '@/components/ui/WSpinner.vue'
import WIcon from '@/components/ui/WIcon.vue'

interface Row {
  key: string
  provider: string
  id: string
  raw: Record<string, any>
  resource?: ResourceSummary
}

const providers = ref<ProviderSummary[]>([])
const rows = ref<Row[]>([])
const loading = computed(() => accountRead.loading.value)
const busy = ref(false)
const editingPriority = ref('')
const rowBusy = ref(new Set<string>())
async function renewAccount(r: Record<string, any>) { try { await mutateRow(r, () => api.providerRenewAccount(r.provider, r.id)); toast.success("授权已续期") } catch(e: any) { toast.error(e?.response?.data?.error?.message || "续期失败") } }
async function mutateRow(r: Record<string, any>, operation: () => Promise<unknown>) {
  const key = r.provider + '/' + r.id
  if (rowBusy.value.has(key)) return
  rowBusy.value.add(key)
  try { await operation(); await load() } finally { rowBusy.value.delete(key) }
}

const checkOpen = ref(false)
const checkTarget = ref<Row | null>(null)
const checkResult = ref<Record<string, any> | null>(null)
const modelTestResult = ref<Record<string, any> | null>(null)
const checkModels = ref<{ value: string; label: string }[]>([])
const checkModel = ref('')
const checking = ref(false)
const testing = ref(false)
const modelsLoading = ref(false)
const identityResult = ref<{ profile: string; endpoint: string; headers: Record<string,string>; present: Record<string,boolean> } | null>(null)
const identityBusy = ref(false)
const identityError = ref('')
async function inspectIdentity() {
  const row = checkTarget.value
  if (!row || identityBusy.value) return
  identityBusy.value = true; identityError.value = ''
  try {
    const result = await api.accountIdentity(row.provider, row.id)
    if (checkOpen.value && checkTarget.value?.key === row.key) identityResult.value = result
  } catch (e: any) {
    if (checkOpen.value && checkTarget.value?.key === row.key) identityError.value = e.message || '诊断读取失败'
  } finally { identityBusy.value = false }
}
async function openAccountCheck(source: Record<string, any>) {
  const row = source as Row
  dismissPopovers()
  checkTarget.value = row
  identityResult.value = null; identityError.value = ''
  checkResult.value = row.raw.account_check || null
  modelTestResult.value = row.raw.account_test || null
  checkModels.value = []; checkModel.value = ''; checkOpen.value = true; modelsLoading.value = true
  try {
    const detail = await api.getProvider(row.provider)
    if (checkTarget.value?.key !== row.key || !checkOpen.value) return
    checkModels.value = (detail.models || []).map((m: any) => ({ value: m.id, label: m.name ? `${m.name} · ${m.id}` : m.id }))
  } finally { modelsLoading.value = false }
}
async function runAccountCheck() {
  const row = checkTarget.value; if (!row || checking.value || testing.value) return
  checking.value = true
  try { checkResult.value = await api.checkAccount(row.provider, row.id); await load() }
  finally { checking.value = false }
}
let confirmingAccountTest = false
async function runAccountTest() {
  const row = checkTarget.value; const model = checkModel.value
  if (!row || !model || confirmingAccountTest || testing.value || checking.value) return
  confirmingAccountTest = true
  try {
    if (!await confirm({ title: '真实调用测试', body: '会使用选定账号实际调用该模型，可能消耗额度。只发送最小测试内容，不会换号；不会加入用户用量和续航估算。', okText: '开始测试' })) return
    if (!checkOpen.value || checkTarget.value?.key !== row.key || checkModel.value !== model) return
    testing.value = true
    modelTestResult.value = await api.testAccount(row.provider, row.id, model); await load()
  } finally { confirmingAccountTest = false; testing.value = false }
}
async function checkAccountsBatch() {
  if (batchBusy.value) return
  const targets = (selectedRows.value.length ? selectedRows.value : filtered.value).filter(r => r.raw.account_check_supported)
  if (!targets.length) return
  batchBusy.value = true; batchResults.value = []
  try {
    // Only two background checks at a time; normal model requests are unaffected.
    let next = 0
    await Promise.all([0, 1].map(async () => {
      while (next < targets.length) {
        const row = targets[next++]
        try { const result = await api.checkAccount(row.provider, row.id); batchResults.value.push({ key: row.key, label: label(row), ok: result.status === 'valid' || result.status === 'unknown', message: result.message }) }
        catch { batchResults.value.push({ key: row.key, label: label(row), ok: false, message: '检查失败，其余账号继续处理' }) }
      }
    }))
    await load()
  } finally { batchBusy.value = false }
}

const providerFilter = ref('')
const statusFilter = ref('')
const search = ref('')

// 供应商能力查询
function caps(name: string): string[] {
  return providers.value.find((p) => p.name === name)?.capabilities || []
}

const readErrors = ref<Record<string, string>>({})
const accountRead = useLiveRead(async signal => {
  let provs: ProviderSummary[]
  try { provs = (await api.getProviders(signal)).providers || [] }
  catch (e) { if (!signal.aborted) readErrors.value = { ...readErrors.value, providers: '渠道列表读取失败，保留上次成功快照' }; throw e }
  if (signal.aborted || editingPriority.value) return
  providers.value = provs
  const targets = provs.filter(p => p.capabilities?.includes('accounts') || p.capabilities?.includes('config'))
  const names = new Set(targets.map(p => p.name))
  rows.value = rows.value.filter(r => names.has(r.provider))
  readErrors.value = Object.fromEntries(Object.entries(readErrors.value).filter(([name]) => names.has(name)))
  const results = await Promise.allSettled(targets.map(async p => {
    const name = p.name
    try {
      const detail = await api.getProvider(name, signal)
      if (signal.aborted || editingPriority.value) return
      const metadata = new Map((detail.resources || []).map(r => [r.id, r]))
      const accounts: Row[] = []
      for (const raw of detail.accounts || []) {
        const id = String(raw.id ?? raw.uid)
        const resource = metadata.get(id)
        const quota = resource?.quota
        const normalized = quota ? { ...raw, credits_remaining: quota.remaining, credits_total: quota.total, credits_expire_at: quota.expires_at, quota_stale: quota.stale, quota_refresh_failed: quota.refresh_failed } : raw
        accounts.push({ key: name + '/' + id, provider: name, id, raw: normalized, resource })
      }
      const grouped = new Map<string, Row[]>()
      for (const row of rows.value) { const list=grouped.get(row.provider)||[];list.push(row);grouped.set(row.provider,list) }
      grouped.set(name, accounts)
      rows.value = [...grouped.values()].flat()
      selectedKeys.value = new Set([...selectedKeys.value].filter(key => rows.value.some(r => r.key === key)))
      const errors = { ...readErrors.value }; delete errors[name]; readErrors.value = errors
    } catch (e) {
      if (!signal.aborted) readErrors.value = { ...readErrors.value, [name]: '读取失败，保留上次成功快照' }
      throw e
    }
  }))
  const failure = results.find(r => r.status === 'rejected')
  if (failure?.status === 'rejected') throw failure.reason
}, () => {}, { interval: 30000, enabled: () => !checking.value && !testing.value && !batchBusy.value && rowBusy.value.size === 0 && !editingPriority.value })
const load = () => accountRead.refresh()

function action(r: Record<string, any>, name: string) { return (r.resource as ResourceSummary | undefined)?.actions[name] }
function can(r: Record<string, any>, name: string) { return action(r, name)?.enabled === true }
const selectedKeys = ref(new Set<string>())
const batchBusy = ref(false)
const batchResults = ref<{ key: string; label: string; ok: boolean; message?: string }[]>([])
const selectedRows = computed(() => rows.value.filter(r => selectedKeys.value.has(r.key)))
const batchEnabled = computed(() => selectedRows.value.length > 0 && selectedRows.value.every(r => can(r, 'enabled') && !rowBusy.value.has(r.key) && !readErrors.value[r.provider]))
function selectRow(r: Record<string, any>, checked: boolean) {
  if (checked) selectedKeys.value.add(r.key)
  else selectedKeys.value.delete(r.key)
}
async function batchToggle(enabled: boolean) {
  if (!batchEnabled.value || batchBusy.value) return
  const targets = [...selectedRows.value]
  batchBusy.value = true
  try {
    if (!enabled && !await confirm({ title: '停用所选账号', body: `停用 ${targets.length} 个账号将停止其后续调用，正在执行的请求继续完成。`, tone: 'danger', okText: '确认停用' })) return
    batchResults.value = []
    for (const r of targets) {
      if (rowBusy.value.has(r.key)) { batchResults.value.push({ key: r.key, label: label(r), ok: false }); continue }
      rowBusy.value.add(r.key)
      try { await api.providerAccountSettings(r.provider, r.id, { enabled }); batchResults.value.push({ key: r.key, label: label(r), ok: true }) }
      catch { batchResults.value.push({ key: r.key, label: label(r), ok: false }) }
      finally { rowBusy.value.delete(r.key) }
    }
    await load()
  } finally { batchBusy.value = false }
}

// 状态归一
function statusOf(r: any): { tone: 'live' | 'warn' | 'fault' | 'muted'; text: string } {
  const a = r.raw
  if (a.enabled === false) return { tone: a.auth_invalid ? 'fault' : Array.isArray(a.maintenance) && a.maintenance.length ? 'warn' : 'muted', text: '已停用' }
  if (a.auth_invalid) return { tone: 'fault', text: '授权已失效' }
  if (a.auth_partial || Array.isArray(a.maintenance) && a.maintenance.length) return { tone: 'warn', text: a.auth_partial ? '部分可用' : '维护异常' }
  const probe = a.account_check
  if (probe?.status === 'invalid') return { tone: 'fault', text: '授权已失效' }
  if (probe?.status === 'warning' && probe.expires_at > Date.now() / 1000) return { tone: 'warn', text: '检查异常' }
  if (a.quota_login_expired) return { tone: 'warn', text: '额度授权异常' }
  if (a.quota_reauthorize) return { tone: 'warn', text: '额度授权异常' }
  const cd = a.cooldown_until && a.cooldown_until > Date.now() / 1000
  if (cd) return { tone: 'warn', text: '冷却 ' + cooldown(a.cooldown_until) }
  const recentCall = a.health?.last_success > Date.now() / 1000 - 1800
  if (a.account_test?.status === 'valid' && a.account_test.expires_at > Date.now() / 1000 && a.healthy) return { tone: 'live', text: '测试通过' }
  if (probe?.status === 'unknown' && !recentCall) return { tone: 'muted', text: '待调用验证' }
  if (probe?.status === 'valid' && probe.expires_at > Date.now() / 1000 && !recentCall && a.healthy && !a.quota_exceeded) return { tone: 'live', text: '检查通过' }
  if (a.account_check_supported && !recentCall && (!probe || probe.expires_at <= Date.now() / 1000)) return { tone: 'muted', text: '待验证' }
  if (a.health_basis === 'credential' && !a.health?.last_success && a.healthy && !a.quota_exceeded) return { tone: 'muted', text: '待验证' }
  const states: Record<string, { tone: 'live' | 'warn' | 'fault' | 'muted'; text: string }> = {
    disabled: { tone: 'muted', text: '已停用' }, configured: { tone: 'live', text: '已配置' }, unconfigured: { tone: 'muted', text: '未配置' },
    quota_exceeded: { tone: 'warn', text: '额度受限' }, paid_exhausted: { tone: 'warn', text: '付费额度不足' }, healthy: { tone: 'live', text: '可调度' }, unauthorized: { tone: 'warn', text: '未授权' }, unhealthy: { tone: 'warn', text: '暂不可用' }, unknown: { tone: 'muted', text: '待验证' },
    cooling: { tone: 'warn', text: '冷却中' }, unavailable: { tone: 'warn', text: '暂不可用' }, degraded: { tone: 'warn', text: '部分可用' },
  }
  if (r.resource?.status && states[r.resource.status]) return states[r.resource.status]
  if (a.tier) return { tone: a.key_count > 0 ? 'live' : 'muted', text: a.key_count > 0 ? '已配置' : '未配置' }
  if (a.quota_exceeded) return { tone: 'warn', text: '额度不足' }
  if (a.healthy) return { tone: 'live', text: '可调度' }
  return { tone: 'warn', text: a.has_secret === false ? '未授权' : '异常' }
}

const filtered = computed(() =>
  rows.value.filter((r) => {
    if (providerFilter.value && r.provider !== providerFilter.value) return false
    if (statusFilter.value) {
      const st = statusOf(r)
      if (statusFilter.value === 'healthy' && st.tone !== 'live') return false
      if (statusFilter.value === 'unhealthy' && st.tone !== 'warn' && st.tone !== 'fault') return false
      if (statusFilter.value === 'pending' && (st.tone !== 'muted' || r.raw.enabled === false)) return false
      if (statusFilter.value === 'disabled' && r.raw.enabled !== false) return false
    }
    const q = search.value.trim().toLowerCase()
    if (q) {
      const a = r.raw
      const hay = `${a.alias || ''} ${a.label || ''} ${a.nickname || ''} ${r.id}`.toLowerCase()
      if (!hay.includes(q)) return false
    }
    return true
  }),
)

function kindLabel(r: Record<string, any>) { return r.resource?.kind === 'anonymous' ? '匿名入口' : r.resource?.kind === 'key_tier' ? '密钥层级' : r.resource?.source === 'local' ? '桌面账号' : '账号' }
function expiryOf(r: Record<string, any>) {
  const dates = pkgsOf(r).filter(p => p.remain != null && p.remain > 0 && p.expire_at).map(p => p.expire_at!)
  if (!pkgsOf(r).length && r.raw.credits_expire_at > 0 && r.raw.credits_remaining !== 0) dates.push(r.raw.credits_expire_at)
  return dates.length ? Math.min(...dates) : undefined
}
function expiryNotice(r: Record<string, any>) {
  const expiry = expiryOf(r)
  if (!expiry) return null
  const days = (expiry * 1000 - Date.now()) / 86400_000
  if (days <= 0) return { tone: 'text-fault', text: '已到期' }
  if (days <= 1) return { tone: 'text-fault', text: '不足 1 天到期' }
  if (days <= 7) return { tone: 'text-warn', text: `${Math.ceil(days)} 天内到期` }
  return null
}

const summary = computed(() => {
  const total = rows.value.length
  const healthy = rows.value.filter((r) => statusOf(r).tone === 'live').length
  return { total, healthy }
})

function label(r: any): string {
  const a = r.raw
  return r.resource?.label || a.label || a.alias || a.nickname || r.id.slice(0, 8)
}

const cols = computed<Column[]>(() => [
  { key: 'select', label: '选择', width: '3rem' },
  { key: 'provider', label: '渠道', minWidth: '6.5rem', nowrap: true },
  { key: 'account', label: '账号', minWidth: '11rem' },
  { key: 'site', label: '站点', minWidth: '4rem', nowrap: true },
  { key: 'status', label: '状态', minWidth: '5rem', nowrap: true },
  { key: 'credits', label: providerFilter.value && caps(providerFilter.value).includes('config') && !caps(providerFilter.value).includes('credits') ? '密钥 / 入口' : '额度（剩余/总）', align: 'right', minWidth: '10rem', nowrap: true },
  { key: 'expire', label: '到期', minWidth: '7.5rem', nowrap: true },
  { key: 'checkin', label: '今日签到', minWidth: '7rem', nowrap: true },
  { key: 'ctrl', label: providerFilter.value ? (rows.value.some(r => r.provider === providerFilter.value && can(r, 'priority')) ? '优先级' : '激活') : '优先级/激活', align: 'center', minWidth: '6rem', nowrap: true },
  { key: 'actions', label: '操作', align: 'right', minWidth: '10rem', nowrap: true },
].filter(c => {
  if (!providerFilter.value) return true
  const channelRows = rows.value.filter(r => r.provider === providerFilter.value)
  if (c.key === 'site') return channelRows.some(r => r.resource?.region || r.raw.site || r.raw.region)
  if (c.key === 'expire') return caps(providerFilter.value).includes('credits')
  if (c.key === 'checkin') return caps(providerFilter.value).includes('checkin')
  if (c.key === 'ctrl') return channelRows.some(r => r.resource?.actions.priority || r.resource?.actions.activate || r.raw.active)
  return true
}) as Column[])

// 状态文字颜色（避免动态拼接 class 被 Tailwind purge）
function statusTextClass(r: any): string {
  return { live: 'text-live', warn: 'text-warn', fault: 'text-fault', muted: 'text-faint' }[statusOf(r).tone]
}

// ---------- 积分构成悬浮层（对标官方控制台"积分明细"口径） ----------
// 渠道返回的 credit_packages 逐资源包展示；未知与无限额度不伪装成零。
// 弹层 Teleport 到 body——表格容器 overflow-x-auto 会裁剪行内绝对定位。
interface Pkg {
  name: string
  description?: string
  unit?: string
  remain: number | null
  used: number | null
  total: number | null
  unlimited?: boolean
  expire_at?: number | null
  reset_time?: string
 display_only?: boolean
}

const pkgOpenFor = ref<string | null>(null) // `${provider}:${id}`
const healthOpenFor = ref<string | null>(null)
const healthStyle = ref<Record<string, string>>({})
const healthAnchor = ref<HTMLElement | null>(null)
const healthPinned = ref(false)
const pkgStyle = ref<Record<string, string>>({})
const pkgAnchor = ref<HTMLElement | null>(null)
// v-for 内的模板 ref：Vue 3 会收集为数组，这里恒只有 0/1 个面板
const panelEl = ref<any>(null)
// 延迟关闭：锚点 → 面板之间移动会先触发锚点 mouseleave，留出间隙让面板
// mouseenter 接住（holdPkgs），避免弹层闪烁消失。
let closeTimer: ReturnType<typeof setTimeout> | null = null

function pkgKey(r: any): string {
  return `${r.provider}:${r.id}`
}

function pkgsOf(r: any): Pkg[] {
  return (r.raw?.credit_packages as Pkg[] | undefined) || []
}

function cancelClose() {
  if (closeTimer) {
    clearTimeout(closeTimer)
    closeTimer = null
  }
}

// 包剩余百分比（总量未知时不给条）
function pkgPct(p: Pkg): number | null {
  if (!p.total || p.total <= 0 || p.remain == null) return null
  return Math.max(0, Math.min(100, (p.remain / p.total) * 100))
}

// 有余额且 7 天内到期的包：临期（与选号"临期优先烧"同窗口语义）
function pkgExpiring(p: Pkg): boolean {
  if (!p.expire_at || p.remain == null || p.remain <= 0) return false
  return p.expire_at * 1000 - Date.now() < 7 * 86400_000
}

function openPkgs(r: any, e: MouseEvent | FocusEvent) {
  if (!pkgsOf(r).length) return
  dismissPopovers()
  pkgAnchor.value = e.currentTarget as HTMLElement
  pkgOpenFor.value = pkgKey(r)
  nextTick(positionPkgs)
}

function positionPkgs() {
  const a = pkgAnchor.value
  if (!a) return
  const rect = a.getBoundingClientRect()
  // 宽度自适应：实测渲染宽度（w-fit），上限 440px、下限 300px
  const el = Array.isArray(panelEl.value) ? panelEl.value[0] : panelEl.value
  const panelW = Math.min(Math.max(el?.offsetWidth || 300, 300), 440)
  const left = Math.min(Math.max(8, rect.right - panelW), window.innerWidth - panelW - 8)
  const top = Math.min(rect.bottom + 6, window.innerHeight - 260)
  pkgStyle.value = { left: left + 'px', top: top + 'px' }
}

function closePkgs() {
  if (!pkgOpenFor.value) return
  scheduleClose()
}

function dismissPopovers() {
  cancelClose()
  pkgOpenFor.value = calOpenFor.value = healthOpenFor.value = null
  pkgAnchor.value = calAnchor.value = healthAnchor.value = null
  healthPinned.value = false
}

function openHealth(r: Record<string, any>, e: MouseEvent | FocusEvent) {
  if (healthOpenFor.value === pkgKey(r)) { cancelClose(); return }
  dismissPopovers()
  healthAnchor.value = e.currentTarget as HTMLElement
  healthOpenFor.value = pkgKey(r)
  nextTick(() => {
    const rect = healthAnchor.value?.getBoundingClientRect()
    const panel = Array.isArray(panelEl.value) ? panelEl.value[0] : panelEl.value
    if (!rect) return
    const width = panel?.offsetWidth || 320
    const height = panel?.offsetHeight || 250
    healthStyle.value = {
      left: Math.max(8, Math.min(rect.left, window.innerWidth - width - 8)) + 'px',
      top: Math.max(8, rect.bottom + height + 14 > window.innerHeight ? rect.top - height - 6 : rect.bottom + 6) + 'px',
    }
  })
}
function pinHealth(r: Record<string, any>, e: MouseEvent) {
  if (healthOpenFor.value === pkgKey(r) && healthPinned.value) { dismissPopovers(); return }
  openHealth(r, e)
  healthPinned.value = true
}
function closeHealth() { if (!healthPinned.value) scheduleClose() }
const healthReasons: Record<string, string> = {
  plan_limit: '上游权益额度不足', hard_credit: '上游积分不足',
  soft_rate: '上游暂时限流', session_dead: '授权已失效', auth_error: '授权校验失败',
  server: '上游服务暂时异常', not_found: '上游资源暂不可用',
  account_fault: '上游账号受限', channel_denied: '上游请求校验拒绝',
  content_policy: '上游内容校验拒绝', content_blocked: '上游内容校验拒绝',
  transport: '网络连接异常', upstream_error: '上游请求失败', timeout: '等待上游超时',
  local_network: '本地网络异常', response_limit: '响应超过大小保护阈值',
  bad_params: '上游请求参数校验失败', model_blocked: '该账号暂不支持此模型',
  waf_block: '上游网络防护拒绝', prompt_too_long: '输入超过模型上下文',
  gateway: '网关请求校验失败', incomplete: '输出未完整结束', request_failed: '请求失败',
}
function healthDetails(r: Record<string, any>): { label: string; value: string }[] {
  const a = r.raw
  const details = [{ label: '当前状态', value: statusOf(r).text }]
  for (const [field, title] of [['account_check', '账号检查'], ['account_test', '模型测试']] as const) {
    const result = a[field]
    if (result) details.push({ label: title, value: `${result.scope}：${result.message}${result.model ? `（${result.model}）` : ''}；${dt(result.checked_at)}${result.expires_at <= Date.now() / 1000 ? '，结果已过期' : ''}` })
  }
  if (a.auth_invalid) details.push({ label: '登录授权', value: '已确认失效，暂停模型调度与相关维护，请重新授权' })
  else if (a.auth_partial) details.push({ label: '登录授权', value: '部分产品身份失效，其他有效身份仍可调用' })
  if (a.quota_reauthorize) details.push({ label: '额度授权', value: a.quota_login_expired ? '额度接口报告授权失效，可重新授权后核实；模型调用凭据单独判断' : '额度接口鉴权失败，请重新授权或在官方客户端核实' })
  else if (a.quota_refresh_failed && a.quota_error_kind) details.push({ label: '额度查询', value: ({ network: '网络连接异常，请稍后重试', upstream: '上游额度服务异常', forbidden: '上游访问权限或防护拒绝', invalid_response: '额度响应格式异常', credential: '暂时无法刷新凭据', incomplete: '额度明细读取不完整，保留上次额度', unavailable: '未能获取完整额度明细' } as Record<string, string>)[a.quota_error_kind] || '刷新失败' })
  if (Array.isArray(a.maintenance) && a.maintenance.length) details.push({ label: '维护状态', value: a.maintenance.map((m: any) => `${m.scope}${m.kind === 'authorization_unverified' ? '（接口鉴权异常，未判定登录失效）' : ''}：${m.needs_login ? '请重新授权' : m.retry_at ? `暂停，${dt(m.retry_at)} 后先验证授权` : '等待授权验证'}`).join('；') })
  if (a.disabled_reason) details.push({ label: '停用原因', value: String(a.disabled_reason) })
  if (a.cooldown_until > Date.now() / 1000) {
    if (a.cooldown_kind) details.push({ label: '冷却原因', value: healthReasons[a.cooldown_kind] || '上游暂不可用' })
    details.push({ label: '预计恢复', value: dt(a.cooldown_until) })
  }
  if (a.available_count != null && a.total_count != null) details.push({ label: a.tier === 'anonymous' ? '可用出口' : '可用密钥', value: `${a.available_count} / ${a.total_count}` })
  if (a.health?.last_success) details.push({ label: '最近调用成功', value: dt(a.health.last_success) })
  else if (!a.tier) details.push({ label: '调用验证', value: '本次运行尚无成功记录' })
  if (a.health?.last_failure) {
    const code = a.health.business_code
    const failure = code === '11140' ? '上游账号限制，请在官方客户端核实' : code === '11128' ? '上游请求校验拒绝' : code === '11135' ? '上游图片无法识别' : healthReasons[a.health.failure_kind] || '请求失败'
    details.push({ label: '最近失败', value: `${failure}${code ? `（${code}）` : ''}` })
    details.push({ label: '失败时间', value: dt(a.health.last_failure) })
  }
  if (a.quota_updated_at) details.push({ label: '额度更新', value: `${dt(a.quota_updated_at)}${a.quota_refresh_failed ? '，最近刷新失败' : a.quota_stale ? '，待刷新' : ''}` })
  else if (a.quota_refresh_failed) details.push({ label: '额度刷新', value: '刷新失败，调用状态单独判断' })
  if (a.credits_reset_at || a.quota_reset_at) details.push({ label: '额度重置', value: dt(a.credits_reset_at || a.quota_reset_at) })
  if (a.catalog_stale) details.push({ label: '模型目录', value: '使用上次成功快照，等待刷新' })
  return details
}

function scheduleClose() {
  cancelClose()
  closeTimer = setTimeout(dismissPopovers, 150)
}

function dismissOutside(e: Event) {
  const target = e.target as Node | null
  if (!target || (!pkgOpenFor.value && !calOpenFor.value && !healthOpenFor.value)) return
  const panels: HTMLElement[] = Array.isArray(panelEl.value) ? panelEl.value : [panelEl.value]
  if (pkgAnchor.value?.contains(target) || calAnchor.value?.contains(target) || healthAnchor.value?.contains(target) || panels.some(p => p?.contains(target))) return
  dismissPopovers()
}

function dismissOnEscape(e: KeyboardEvent) {
  if (e.key === 'Escape') dismissPopovers()
}
function dismissOnScroll(e: Event) {
  const target = e.target as Node | null
  const panels: HTMLElement[] = Array.isArray(panelEl.value) ? panelEl.value : [panelEl.value]
  if (target && panels.some(panel => panel?.contains(target))) return
  dismissPopovers()
}

watch([providerFilter, search, statusFilter], dismissPopovers)

// ---------- 签到日历 ----------
// 悬停/点击「今日签到」列弹出 35 天打卡点阵。数据源 /admin/checkin/history，
// 一次拉全量按完整 uid 索引（workbuddy uid 形如 ea3293e0-…，qoder 为账号 id）。
const calOpenFor = ref<string | null>(null)
const calStyle = ref<Record<string, string>>({})
const calData = ref<Record<string, string[]>>({})
const calendars = ref<Record<string, { history: Record<string, string[]>; today: string; window_date: string; timezone: string }>>({})
const calDays = ref(35)
const calAnchor = ref<HTMLElement | null>(null)

// 历史接口按完整 uid 索引（checkin_history.account_uid 原样返回），这里必须用
// 完整 uid 查表——之前用 8 位短码查导致永远查不到、日历全灰。
function shortUid(r: any): string {
  return String(r.id || r.raw?.id || r.raw?.uid || '')
}

// 该账号是否属于有签到活动的 provider（workbuddy 国内 / qoder 有；国际站无）
function hasCheckin(r: any): boolean {
  if (typeof r.raw?.supports_checkin === 'boolean') return r.raw.supports_checkin
  return r.provider === 'qoder'
}

function calDatesOf(r: any): string[] {
  return calendars.value[r.provider]?.history[shortUid(r)] || (r.provider === 'workbuddy' ? calData.value[shortUid(r)] : []) || []
}

const calendarRead=useLiveRead(signal=>api.checkinHistory(calDays.value,signal),res=>{
  calData.value=res.history||{};calendars.value=res.calendars||{};calDays.value=res.days||35
}, { interval:60000 })
const loadCalendar=()=>calendarRead.refresh()

function openCalendar(r: any, e: MouseEvent | FocusEvent) {
  dismissPopovers()
  calAnchor.value = e.currentTarget as HTMLElement
  calOpenFor.value = pkgKey(r)
  void loadCalendar()
  nextTick(positionCal)
}

function positionCal() {
  const a = calAnchor.value
  if (!a) return
  const rect = a.getBoundingClientRect()
  const el = Array.isArray(panelEl.value) ? panelEl.value[0] : panelEl.value
  const panelW = Math.min(Math.max(el?.offsetWidth || 300, 300), 460)
  const left = Math.min(Math.max(8, rect.right - panelW), window.innerWidth - panelW - 8)
  const top = Math.min(rect.bottom + 6, window.innerHeight - 300)
  calStyle.value = { left: left + 'px', top: top + 'px' }
}

function closeCalendar() {
  if (!calOpenFor.value) return
  scheduleClose()
}

// 35 天点阵的格子状态：''（无记录）/ ok（已签）/ future
function calGrid(dates: string[], row: Row) {
  const set = new Set(dates)
  const cells: { date: string; state: '' | 'ok' | 'future' }[] = []
  const calendar = calendars.value[row.provider]
  if (!calendar?.today) return cells
  const now = new Date(calendar.today + 'T00:00:00Z')
  for (let i = calDays.value - 1; i >= 0; i--) {
    const d = new Date(now)
    d.setUTCDate(now.getUTCDate() - i)
    const ds = d.toISOString().slice(0, 10)
    cells.push({ date: ds, state: set.has(ds) ? 'ok' : ds > calendar.window_date ? 'future' : '' })
  }
  return cells
}

// ---------- 行内动作 ----------
async function toggleEnabled(r: any) {
  if (!can(r, 'enabled')) return
  await mutateRow(r, () => api.providerAccountSettings(r.provider, r.id, { enabled: !r.raw.enabled }))
}
async function savePriority(r: any, v: number) {
  if (!can(r, 'priority') || !Number.isSafeInteger(v)) return
  await mutateRow(r, () => api.providerAccountSettings(r.provider, r.id, { priority: v }))
}
async function activate(r: any) {
  if (!can(r, 'activate')) return
  await mutateRow(r, () => api.providerActivateAccount(r.provider, r.id))
  toast.success('已设为激活账号')
}
async function removeAccount(r: any) {
  const ok = await confirm({
    title: `${action(r, 'delete')?.label || '删除'}账号「${label(r)}」？`,
    body: action(r, 'delete')?.confirmation || '移出网关分发，历史用量记录保留。',
    tone: 'danger',
    okText: action(r, 'delete')?.label || '删除',
  })
  if (!ok) return
  if (!can(r, 'delete')) return
  await mutateRow(r, () => api.providerDeleteAccount(r.provider, r.id))
  toast.success('已' + (action(r, 'delete')?.label || '删除'))
}

// 重命名 / 别名（workbuddy=别名，其它=rename）
const renameOpen = ref(false)
const renameTarget = ref<Row | null>(null)
const renameValue = ref('')
function openRename(r: any) {
  renameTarget.value = r
  renameValue.value = action(r, 'rename')?.value || ''
  renameOpen.value = true
}
async function submitRename() {
  const r = renameTarget.value
  if (!r || rowBusy.value.has(r.provider + '/' + r.id)) return
  if (!can(r, 'rename')) return
  await mutateRow(r, () => api.providerRenameAccount(r.provider, r.id, renameValue.value.trim()))
  renameOpen.value = false
  toast.success('已保存')
  await load()
}

// ---------- 供应商级动作：签到 / 刷新额度 ----------
const taskResults = ref<{ provider: string; ok: boolean; message: string }[]>([])
async function runMaintenance(kind: 'checkin' | 'credits') {
  if (busy.value) return
  busy.value = true
  taskResults.value = []
  try {
    const targets = providers.value.filter(p => p.capabilities?.includes(kind) && (!providerFilter.value || p.name === providerFilter.value))
    for (const p of targets) {
      try {
        const result = kind === 'checkin' ? await api.providerCheckin(p.name) : await api.providerCredits(p.name)
        const items = Array.isArray(result.results) ? result.results as Record<string, unknown>[] : []
        const failed = items.filter(item => item.ok === false && !item.already && item.status !== 'unsupported' || item.status === 'error' || item.status === 'failed').length
        const pending = items.filter(item => item.status === 'pending').length
        const notApplicable = items.filter(item => item.status === 'unsupported').length
        const receipts = items.filter(item => item.ok === true && typeof item.credit === 'number' && Number.isFinite(item.credit))
        const reward = receipts.reduce((sum, item) => sum + Number(item.credit), 0)
        const unsupported = result.unsupported === true
        const ok = (result.ok !== false || unsupported) && failed === 0
        const summary = unsupported ? '此账号暂不支持该操作' : pending ? `领取已发起，${pending} 个账号待上游确认` : ok ? '任务已完成' : `任务存在失败${failed ? '（' + failed + ' 项）' : ''}，请查看账号状态`
        taskResults.value.push({ provider: p.name, ok, message: summary + (receipts.length ? ` · 获得 ${fmtCredits(reward)} 积分` : '') + (notApplicable ? ` · ${notApplicable} 个账号不适用` : '') })
      } catch { taskResults.value.push({ provider: p.name, ok: false, message: '执行失败，其余渠道继续处理' }) }
    }
    if (!targets.length) toast.info('所选渠道不支持此操作')
    else if (taskResults.value.some(r => !r.ok)) toast.info('部分任务失败，请查看执行结果')
    else toast.success('任务已完成')
    await load()
    if (kind === 'checkin') await loadCalendar()
  } finally { busy.value = false }
}
async function checkinAll() { await runMaintenance('checkin') }
async function refreshCreditsAll() { await runMaintenance('credits') }

// ---------- 添加账号 ----------
const addOpen = ref(false)
const reauthorizationLabel = ref('')
const addProvider = ref('')
const addMethod = ref<'upload' | 'oauth' | 'token'>('oauth')
const oauthOptions = ref<OAuthOption[]>([])
const oauthOptionsLoading = ref(false)
const oauthOptionsFailed = ref(false)
const oauthChoice = ref<Record<string, string>>({})
const oauthUrl = ref('')
const oauthLoginID = ref('')
const oauthManual = ref(false)
const oauthCallback = ref('')
const callbackBusy = ref(false)
const localImportBusy = ref(false)
async function importLocal() {
  if (localImportBusy.value) return
  localImportBusy.value = true
  try {
    const res = await api.providerImportLocal(addProvider.value)
    if (!res.results.length) { toast.info('服务所在电脑未检测到 Trae 登录态'); return }
    const errors = res.results.filter(r => r.status !== 'ok')
    toast.info(errors.length ? errors.map(r => r.message || '导入失败').join('；') : '本机账号已导入，持续刷新建议网页登录')
    await load()
  } catch (e: any) {
    toast.error(e?.response?.data?.error?.message || '本机导入失败')
  } finally {
    localImportBusy.value = false
  }
}
	async function submitOAuthCallback() {
		if (callbackBusy.value || !oauthPolling.value || !oauthCallback.value.trim() || !oauthLoginID.value) return
		const generation = pollGeneration
		const provider = addProvider.value
		const loginID = oauthLoginID.value
		callbackBusy.value=true
		try {await api.providerOAuthComplete(provider,loginID,oauthCallback.value.trim());if(generation!==pollGeneration)return;oauthCallback.value='';toast.success('回调已提交，正在验证账号')}
		catch(e:any){if(generation===pollGeneration)toast.error(e?.response?.data?.error?.message || '回调提交失败')}
		finally{callbackBusy.value=false}
	}
const oauthPolling = ref(false)
const uploadEl = ref<HTMLInputElement | null>(null)
let pollTimer: ReturnType<typeof setTimeout> | null = null
let pollGeneration = 0
let pollController: AbortController | null = null
const oauthMessage = ref('')

const addProviderOptions = computed(() =>
  providers.value
    .filter((p) => p.capabilities?.includes('oauth') || p.capabilities?.includes('upload') || p.capabilities?.includes('add_account'))
    .map((p) => ({ value: p.name, label: p.display_name || providerMeta(p.name).label })),
)

async function openAdd() {
	 reauthorizationLabel.value = ''
  addProvider.value = addProviderOptions.value.find(p => p.value === providerFilter.value)?.value || addProviderOptions.value[0]?.value || ''
  addOpen.value = true
  await onAddProviderChange()
}
async function reauthorizeAccount(r: Record<string, any>) {
	 reauthorizationLabel.value = label(r)
  dismissPopovers()
  addProvider.value = r.provider
  addOpen.value = true
  await onAddProviderChange()
  if (r.provider === 'workbuddy') oauthChoice.value.site = r.raw.profile === 'intl-cli' ? 'intl-codebuddy' : r.raw.site === 'international' ? 'intl' : 'cn'
}

async function onAddProviderChange() {
  patToken.value = ''
  oauthMessage.value = ''
  stopPoll()
  const generation = pollGeneration
  const name = addProvider.value
  oauthUrl.value = ''
  oauthLoginID.value = ''
  oauthManual.value = false
  oauthCallback.value = ''
  oauthOptions.value = []
  oauthChoice.value = {}
  oauthOptionsLoading.value = false
  oauthOptionsFailed.value = false
  const c = caps(addProvider.value)
  addMethod.value = c.includes('oauth') ? 'oauth' : c.includes('upload') ? 'upload' : c.includes('add_account') ? 'token' : 'oauth'
  if (c.includes('oauth')) {
    oauthOptionsLoading.value = true
    try {
      const res = await api.providerOAuthOptions(name)
      if (generation !== pollGeneration) return
      oauthOptions.value = res.options || []
      for (const o of oauthOptions.value) oauthChoice.value[o.key] = o.default || o.values[0]?.value || ''
    } catch (e: any) {
      if (generation === pollGeneration) {
        oauthOptionsFailed.value = true
        oauthMessage.value = e?.response?.data?.error?.message || '登录选项加载失败，请重新选择渠道'
      }
    } finally {
      if (generation === pollGeneration) oauthOptionsLoading.value = false
    }
  }
}

function stopPoll() {
  pollGeneration++
  pollController?.abort()
  pollController = null
  if (pollTimer) clearTimeout(pollTimer)
  pollTimer = null
  oauthPolling.value = false
}

async function beginOAuth() {
  if (oauthOptionsLoading.value || oauthOptionsFailed.value) return
  stopPoll()
  oauthUrl.value = ''
  oauthLoginID.value = ''
  oauthManual.value = false
  oauthCallback.value = ''
  const generation = pollGeneration
  const controller = new AbortController()
  pollController = controller
  const provider = addProvider.value
  oauthMessage.value = ''
  oauthPolling.value = true
  try {
    let poll: () => Promise<{ status: string; message?: string; models_verified?: boolean }>
    let deadline = Date.now() + 10 * 60_000
    const res = await api.providerOAuthBegin(provider, { ...oauthChoice.value }, controller.signal)
    if (generation !== pollGeneration) return
    oauthUrl.value = res.login_url
		oauthLoginID.value=res.login_id
		oauthManual.value=!!res.manual_callback
		oauthCallback.value=''
    if (res.expires_at) deadline = Math.min(deadline, res.expires_at * 1000)
    poll = () => api.providerOAuthPoll(provider, res.login_id, controller.signal)
    window.open(oauthUrl.value, '_blank', 'noopener,noreferrer')
    let failures = 0
    const tick = async () => {
      if (generation !== pollGeneration) return
      if (Date.now() >= deadline) { stopPoll(); oauthMessage.value = '授权已过期，请重新开始'; return }
      let delay = 2500
      try {
        const p = await poll()
        if (generation !== pollGeneration) return
        if (p.status === 'ready') { stopPoll(); if (p.models_verified === false) toast.info(p.message || '账号已保存，请刷新模型目录'); else toast.success('已添加账号'); addOpen.value = false; await load(); return }
        if (p.status === 'expired' || p.status === 'error') { stopPoll(); oauthMessage.value = p.message || '授权已过期或失败，请重新开始'; return }
        failures = 0
        oauthMessage.value = '等待浏览器授权…'
      } catch (err: any) {
        if (generation !== pollGeneration || controller.signal.aborted) return
        const status = err?.response?.status
        failures++
        if (status === 400 || status === 401 || status === 403 || failures >= 5) {
          stopPoll(); oauthMessage.value = '授权轮询失败，请重新开始'; return
        }
        delay = Math.min(15000, 2500 * 2 ** failures)
        oauthMessage.value = '网络暂时异常，正在重试…'
      }
      if (generation === pollGeneration) pollTimer = setTimeout(tick, Math.min(delay, Math.max(0, deadline-Date.now())))
    }
    pollTimer = setTimeout(tick, 2500)
  } catch (e: any) {
    if (generation === pollGeneration) {
      stopPoll()
      oauthMessage.value = e?.response?.data?.error?.message || '发起授权失败，请重试'
    }
  }
}

watch(addMethod, () => { if (oauthPolling.value) stopPoll() })
watch(oauthChoice, () => { if (oauthPolling.value) stopPoll() }, { deep: true })

async function onUpload(e: Event) {
  const file = (e.target as HTMLInputElement).files?.[0]
  if (!file) return
  try {
    const result = await api.uploadAuth(file)
    if (result.models_verified === false) toast.info(result.message || '账号已保存，请刷新模型目录')
    else toast.success('已上传账号凭据')
    addOpen.value = false
    await load()
  } finally {
    if (uploadEl.value) uploadEl.value.value = ''
  }
}

const patToken = ref('')
const patSaving = ref(false)
watch(addOpen, (open) => { if (!open) { patToken.value = ''; oauthCallback.value = ''; oauthMessage.value = ''; stopPoll() } })
async function onAddByToken() {
  const token = patToken.value.trim()
  if (!token || patSaving.value) return
  const generation = pollGeneration
  patSaving.value = true
  oauthMessage.value = ''
  try {
    const result = await api.providerImportAccount(addProvider.value, token, { ...oauthChoice.value })
    if (generation !== pollGeneration) { await load(); return }
    if (result.models_verified === false) toast.info(result.message || '账号已保存，请刷新模型目录')
    else toast.success('已添加账号')
    patToken.value = ''
    addOpen.value = false
    await load()
  } catch (err: any) {
    if (generation === pollGeneration) oauthMessage.value = err?.response?.data?.error?.message || '添加失败，请检查凭据后重试'
  } finally {
    patSaving.value = false
  }
}

function refreshVisibleCalendar() {
  if (document.hidden) dismissPopovers()
}
onMounted(() => {
  document.addEventListener('visibilitychange', refreshVisibleCalendar)
  document.addEventListener('pointerdown', dismissOutside, true)
  document.addEventListener('focusin', dismissOutside, true)
  document.addEventListener('keydown', dismissOnEscape)
  window.addEventListener('blur', dismissPopovers)
  window.addEventListener('resize', dismissPopovers)
  window.addEventListener('scroll', dismissOnScroll, true)
})
// 组件卸载时停掉可能残留的扫码轮询/悬浮层关闭定时器，避免离开页面后仍在轮询
onUnmounted(() => {
  checkOpen.value = false
  stopPoll()
  cancelClose()
  document.removeEventListener('visibilitychange', refreshVisibleCalendar)
  document.removeEventListener('pointerdown', dismissOutside, true)
  document.removeEventListener('focusin', dismissOutside, true)
  document.removeEventListener('keydown', dismissOnEscape)
  window.removeEventListener('blur', dismissPopovers)
  window.removeEventListener('resize', dismissPopovers)
  window.removeEventListener('scroll', dismissOnScroll, true)
})

// ---------- 配置型供应商（opencode 密钥层级） ----------
const configProviders = computed(() => providers.value.filter((p) => p.capabilities?.includes('config')))
const cfgOpen = ref(false)
const cfgProvider = ref('')
const cfgLoading = ref(false)
const cfgSaving = ref(false)
const cfgPath = ref('')
const cfgReady = ref(false)
const zenText = ref('')
const goText = ref('')
const cfgAnon = ref(false)
const cfgPrefer = ref('go')

async function openConfig(name: string) {
  if (providers.value.find(p => p.name === name)?.config_editor !== 'opencode-tiers') { toast.info('此渠道尚未提供兼容的配置编辑器'); return }
  cfgProvider.value = name
  cfgOpen.value = true
  cfgLoading.value = true
  try {
    const d = (await api.providerGetConfig(name)) as any
    cfgPath.value = d.config_path || ''
    cfgReady.value = !!d.ready
    zenText.value = (d.zen_keys || []).join('\n')
    goText.value = (d.go_keys || []).join('\n')
    cfgAnon.value = !!d.anonymous
    cfgPrefer.value = d.prefer || 'go'
  } finally {
    cfgLoading.value = false
  }
}

function linesToArr(s: string): string[] {
  return s.split('\n').map((l) => l.trim()).filter(Boolean)
}

async function saveConfig() {
  cfgSaving.value = true
  try {
    await api.providerSaveConfig(cfgProvider.value, {
      zen_keys: linesToArr(zenText.value),
      go_keys: linesToArr(goText.value),
      anonymous: cfgAnon.value,
      prefer: cfgPrefer.value,
    })
    toast.success('已保存并重载 OpenCode 配置')
    cfgOpen.value = false
    await load()
  } finally {
    cfgSaving.value = false
  }
}
</script>

<template>
  <WPage title="账号与资源" sub="统一管理各渠道账号、密钥层级与匿名入口。">
    <template #actions>
      <WButton variant="ghost" :loading="batchBusy" :disabled="!rows.some(r => r.raw.account_check_supported)" @click="checkAccountsBatch">{{ selectedRows.length ? '检查所选账号' : '检查筛选账号' }}</WButton>
      <WButton v-if="providers.some(p => p.capabilities.includes('credits') && (!providerFilter || p.name === providerFilter))" variant="ghost" :loading="busy" @click="refreshCreditsAll"><WIcon name="refresh" :size="15" /> 刷新额度</WButton>
      <WButton v-if="providers.some(p => p.capabilities.includes('checkin') && (!providerFilter || p.name === providerFilter))" variant="ghost" :loading="busy" @click="checkinAll"><WIcon name="check" :size="15" /> 一键签到</WButton>
      <WButton v-for="p in configProviders.filter(p => !providerFilter || p.name === providerFilter)" :key="p.name" variant="ghost" @click="openConfig(p.name)">配置 {{ p.display_name }}</WButton>
      <WButton v-if="addProviderOptions.length" variant="primary" @click="openAdd"><WIcon name="plus" :size="16" /> 添加账号</WButton>
    </template>

    <div v-if="Object.keys(readErrors).length" class="mb-4 rounded-lg border border-line px-3 py-2 text-small text-muted" role="status">
      <div v-for="(message, name) in readErrors" :key="name">{{ name === 'providers' ? '渠道列表' : providerMeta(String(name)).label }}：{{ message }}</div>
    </div>
    <div v-if="taskResults.length" class="mb-4 rounded-lg border border-line px-3 py-2 text-small" role="status">
      <div v-for="result in taskResults" :key="result.provider" :class="result.ok ? 'text-muted' : 'text-warn'">{{ providerMeta(result.provider).label }}：{{ result.message }}</div>
    </div>
    <nav class="mb-4 flex flex-wrap gap-2" aria-label="渠道筛选">
      <WButton :variant="!providerFilter ? 'primary' : 'ghost'" @click="providerFilter = ''">全部资源 · {{ rows.length }}</WButton>
      <WButton v-for="p in providers" :key="p.name" :variant="providerFilter === p.name ? 'primary' : 'ghost'" @click="providerFilter = p.name">{{ p.display_name }} · {{ rows.filter(r => r.provider === p.name).length }}</WButton>
    </nav>
    <WSpinner v-if="loading && !rows.length" center label="加载中" />
    <template v-else>
      <!-- 筛选条 -->
      <div class="mb-4 flex flex-wrap items-center gap-2">
        <WInput v-model="search" placeholder="搜索别名 / 昵称 / ID" class="w-full sm:w-64">
          <template #prefix><WIcon name="search" :size="16" class="text-faint" /></template>
        </WInput>
        <WSelect
          v-model="statusFilter"
          :options="[{ value: 'healthy', label: '可调度 / 已配置' }, { value: 'unhealthy', label: '需关注' }, { value: 'pending', label: '待配置 / 验证' }, { value: 'disabled', label: '已停用' }]"
          placeholder="全部状态"
          class="w-32"
        />
        <span class="ml-auto text-small text-faint">可调度 / 已配置 <span class="mono text-live">{{ summary.healthy }}</span> / {{ summary.total }}</span>
      </div>

      <div v-if="selectedRows.length" class="mb-3 flex flex-wrap items-center gap-2 rounded-lg border border-line p-3">
        <span class="text-small text-muted">已选 {{ selectedRows.length }} 项（含筛选隐藏项）</span>
        <WButton :disabled="!batchEnabled || batchBusy" :loading="batchBusy" @click="batchToggle(true)">批量启用</WButton>
        <WButton :disabled="!batchEnabled || batchBusy" @click="batchToggle(false)">批量停用</WButton>
        <WButton variant="subtle" :disabled="batchBusy" @click="selectedKeys.clear()">清除选择</WButton>
        <span v-if="!batchEnabled" class="text-micro text-faint">所选资源须全部支持启停且状态读取成功。</span>
      </div>
      <div v-if="batchResults.length" role="status" class="mb-3 text-micro"><div v-for="result in batchResults" :key="result.key" :class="result.ok ? 'text-muted' : 'text-warn'">{{ result.label }}：{{ result.message || (result.ok ? '已更新' : '更新失败，其余账号继续处理') }}</div></div>
      <WCard flush>
        <WTable :columns="cols" :rows="filtered" row-key="key">
          <template #cell-select="{ row }"><input type="checkbox" :aria-label="'选择 ' + label(row)" :checked="selectedKeys.has(row.key)" :disabled="batchBusy || rowBusy.has(row.key)" @change="selectRow(row, ($event.target as HTMLInputElement).checked)"></template>
          <template #cell-provider="{ row }">
            <WTag :tone="providerMeta(row.provider).tone">{{ providerMeta(row.provider).label }}</WTag>
          </template>
          <template #cell-account="{ row }">
            <div class="w-44">
              <div class="truncate font-medium text-ink" :title="[label(row), kindLabel(row), row.id, row.raw.source ? ({local:'桌面',project:'导入',native:'网关'} as Record<string,string>)[row.raw.source] || row.raw.source : ''].filter(Boolean).join(' · ')">{{ label(row) }}</div>
			  <div v-if="row.raw.owner_username" class="mt-1 truncate text-micro text-muted" :title="'归属用户：' + row.raw.owner_username">{{ row.raw.owner_username }} · {{ row.raw.contribution_status === 'active' ? '已贡献' : row.raw.contribution_status === 'private' ? '仅本人' : '待验证' }}</div>
              <div v-if="row.provider === 'trae'" class="mt-1 whitespace-nowrap text-micro text-faint">{{ (row.raw.products || [row.raw.product]).map((p: string) => p === 'solo' ? 'Work/SOLO' : 'IDE').join(' / ') }}</div>
            </div>
          </template>
          <template #cell-site="{ row }">
            <span class="text-muted">{{ siteLabel(row.resource?.region || row.raw.site || row.raw.region) || '—' }}</span>
          </template>
          <template #cell-status="{ row }">
            <button type="button" class="inline-flex items-center gap-1.5 rounded text-left focus-visible:outline focus-visible:outline-2 focus-visible:outline-brand" :aria-label="label(row) + '，查看健康状态'" :aria-expanded="healthOpenFor === pkgKey(row)" aria-controls="account-health-details" @mouseenter="openHealth(row, $event)" @mouseleave="closeHealth" @focus="openHealth(row, $event)" @click="pinHealth(row, $event)">
              <WLed :tone="statusOf(row).tone" :pulse="statusOf(row).tone === 'live'" />
              <span class="text-small" :class="statusTextClass(row)">{{ statusOf(row).text }}</span>
            </button>
            <div v-if="row.raw.catalog_stale" class="text-micro text-warn" title="使用上次成功发现的模型目录；刷新成功后自动更新">模型目录待刷新</div>
          </template>
          <template #cell-credits="{ row }">
            <template v-if="row.raw.credits_remaining != null">
              <span class="mono text-ink">{{ fmtCredits(row.raw.credits_remaining) }}</span>
              <span v-if="row.raw.credits_total != null" class="mono text-faint"> / {{ fmtCredits(row.raw.credits_total) }}</span>
              <span v-if="row.raw.quota_unit && row.raw.quota_unit !== 'credits'" class="ml-1 text-micro text-faint">{{ row.raw.quota_unit }}</span>
            </template>
            <span v-else-if="row.raw.quota_unlimited" class="text-muted">上游额度不限</span>
 <span v-else-if="row.raw.credits_total != null" class="text-faint">未知 / {{fmtCredits(row.raw.credits_total)}} <span v-if="row.raw.quota_unit">{{row.raw.quota_unit}}</span></span>
 <span v-else-if="row.raw.quota_mixed_units" class="text-muted" title="资源包的计量单位不同，明细按资源包列出">分项额度</span>
            <span v-else-if="row.raw.tier" class="text-muted">{{ row.raw.key_count }} {{ row.raw.tier === 'anonymous' ? '个入口' : '个密钥' }}</span>
            <span v-else class="text-faint">未上报</span>
            <div v-if="row.raw.quota_refresh_failed || row.raw.quota_stale || row.raw.plan" class="mt-1 flex justify-end gap-2 text-micro text-faint"><button v-if="row.raw.quota_reauthorize" type="button" class="text-warn hover:underline" @click="reauthorizeAccount(row)">需重新授权</button><span v-else-if="row.raw.quota_refresh_failed" class="text-warn" :title="row.raw.quota_updated_at ? '最后成功：' + dt(row.raw.quota_updated_at) : '尚未成功获取额度'">刷新失败</span><span v-else-if="row.raw.quota_stale">缓存过期</span><span v-if="row.raw.plan" :title="`上游账号套餐：${row.raw.plan}；不代表模型调用免费`">套餐：{{ String(row.raw.plan).toLowerCase() === 'free' ? '免费' : row.raw.plan }}</span></div>
          </template>
          <template #cell-expire="{ row }">
            <template v-if="expiryOf(row) || pkgsOf(row).length">
              <button
                type="button"
                class="inline-flex items-center gap-1 whitespace-nowrap rounded text-muted hover:text-ink"
                :class="{ 'cursor-help': pkgsOf(row).length, 'cursor-default': !pkgsOf(row).length }"
                :title="pkgsOf(row).length ? '查看到期、重置与积分构成'  : dt(expiryOf(row), 'YYYY-MM-DD HH:mm')"
                @mouseenter="openPkgs(row, $event)"
                @focus="openPkgs(row, $event)"
                @click="openPkgs(row, $event)"
                @mouseleave="closePkgs"
                @blur="closePkgs"
              >
                <span class="inline-flex flex-col items-start gap-0.5" :class="expiryNotice(row)?.tone || 'text-muted'">
                  <span>{{ expiryOf(row) ? dt(expiryOf(row), 'YYYY-MM-DD') : '查看资源包' }}</span>
                  <span v-if="expiryNotice(row)" class="text-micro font-medium">{{ expiryNotice(row)?.text }}</span>
                </span>
                <WIcon v-if="pkgsOf(row).length" name="clock" :size="13" class="text-faint" />
              </button>
            </template>
            <span v-else class="text-faint">—</span>
          </template>
          <template #cell-checkin="{ row }">
            <button
              type="button"
              class="inline-flex flex-col items-start gap-1 whitespace-nowrap rounded focus:outline-none"
              @mouseenter="openCalendar(row, $event)"
              @focus="openCalendar(row, $event)"
              @click="openCalendar(row, $event)"
              @mouseleave="closeCalendar"
              @blur="closeCalendar"
              :title="row.raw.checkin_pending ? '今日已发起领取，上游尚未确认签到；后续仅刷新状态' : '悬停查看 35 天签到记录'"
            >
              <WTag v-if="row.raw.checkin_today" class="whitespace-nowrap" tone="live" dot>今日已签</WTag>
              <WTag v-else-if="row.raw.checkin_pending" class="whitespace-nowrap" tone="warn" dot>待确认</WTag>
              <WTag v-else-if="hasCheckin(row)" class="whitespace-nowrap" tone="warn" dot>未签</WTag>
              <span v-else class="text-faint">—</span>
              <div v-if="row.raw.streak_days" class="text-micro text-faint">连签 {{ row.raw.streak_days }} 天</div>
            </button>
          </template>
          <template #cell-ctrl="{ row }">
            <input
              v-if="can(row, 'priority')"
              type="number"
              :value="row.raw.priority ?? 0"
              @focus="editingPriority = row.key" @blur="editingPriority = ''"
              :disabled="rowBusy.has(row.provider + '/' + row.id)"
              class="mono h-8 w-16 rounded-lg border border-line bg-bg/40 px-2 text-center text-small text-ink focus:border-brand focus:outline-none"
              @change="savePriority(row, +($event.target as HTMLInputElement).value)"
              title="优先级：越大越优先被选中"
            />
            <WButton v-else-if="row.raw.active" size="sm" variant="subtle" disabled>已激活</WButton>
            <WButton v-else-if="can(row, 'activate')" size="sm" variant="ghost" :disabled="rowBusy.has(row.provider + '/' + row.id)" @click="activate(row)">设为激活</WButton>
            <span v-else class="text-faint">—</span>
          </template>
          <template #cell-actions="{ row }">
            <div class="flex flex-wrap items-center justify-end gap-1.5">
              <WButton v-if="row.raw.account_check_supported" size="sm" variant="subtle" :disabled="batchBusy" @click="openAccountCheck(row)">测活</WButton>
            <WButton v-if="can(row, 'config')" size="sm" @click="openConfig(row.provider)">编辑配置</WButton>
            <ResourceActions v-else :resource="row.resource" :enabled="row.raw.enabled === true" :busy="rowBusy.has(row.provider + '/' + row.id)" @renew="renewAccount(row)" @rename="openRename(row)" @remove="removeAccount(row)" @enabled="toggleEnabled(row)" />
            </div>
          </template>
          <template #empty>
            <WEmpty title="还没有账号" hint="添加一个上游账号，网关才能开始转发请求。">
              <WButton variant="primary" @click="openAdd"><WIcon name="plus" :size="16" /> 添加账号</WButton>
            </WEmpty>
          </template>
        </WTable>
      </WCard>

    </template>

    <!-- 重命名 -->
    <WModal v-model:open="checkOpen" :dismissible="!checking && !testing" :title="checkTarget ? `账号测活 · ${label(checkTarget)}` : '账号测活'">
      <div class="space-y-5">
        <section class="space-y-3">
          <div class="flex items-center justify-between gap-3"><h3 class="text-small font-medium text-ink">只读检查</h3><WButton :loading="checking" :disabled="testing" @click="runAccountCheck">检查账号</WButton></div>
          <p class="text-small text-muted">检查对应授权或目录接口，复用维护恢复机制；不生成内容、不改变启停状态。</p>
          <div v-if="checkResult" class="rounded-lg border border-line bg-bg/70 p-3 text-small" :class="checkResult.status === 'valid' ? 'text-live' : checkResult.status === 'invalid' ? 'text-fault' : 'text-muted'">
            <p>{{ checkResult.message }}</p><p class="mt-1 text-micro text-faint">{{ checkResult.scope }} · {{ dt(checkResult.checked_at) }}</p>
          </div>
        </section>
        <section v-if="checkTarget?.provider === 'workbuddy'" class="border-t border-line pt-4 space-y-3">
          <div class="flex items-center justify-between gap-3"><h3 class="text-small font-medium text-ink">出站身份诊断</h3><WButton variant="ghost" :loading="identityBusy" @click="inspectIdentity">查看诊断</WButton></div>
          <p class="text-micro text-faint">仅在本地构造并脱敏，不发送请求、不刷新凭据；字段存在不代表上游鉴权通过。</p>
          <p v-if="identityError" class="text-small text-warn">{{ identityError }}</p>
          <div v-if="identityResult" class="min-w-0 rounded-lg border border-line bg-bg/70 p-3 text-small space-y-3">
            <dl class="grid grid-cols-[4rem_minmax(0,1fr)] gap-x-3 gap-y-2"><dt class="text-muted">身份</dt><dd class="mono break-all">{{ identityResult.profile }}</dd><dt class="text-muted">端点</dt><dd class="mono break-all text-micro">{{ identityResult.endpoint }}</dd></dl>
            <details><summary class="cursor-pointer text-muted">协议字段</summary><dl class="mt-2 space-y-2"><div v-for="(value,name) in identityResult.headers" :key="name" class="min-w-0"><dt class="text-micro text-faint break-all">{{ name }}</dt><dd class="mono text-micro break-all">{{ value }}</dd></div></dl></details>
            <details><summary class="cursor-pointer text-muted">凭据与请求标识</summary><dl class="mt-2 space-y-1"><div v-for="(value,name) in identityResult.present" :key="name" class="flex flex-wrap justify-between gap-x-3"><dt class="mono text-micro text-faint break-all">{{ name }}</dt><dd class="text-micro whitespace-nowrap" :class="value ? 'text-muted' : 'text-faint'">{{ value ? '已提供' : '未提供' }}</dd></div></dl></details>
          </div>
        </section>
        <section class="border-t border-line pt-4 space-y-3">
          <h3 class="text-small font-medium text-ink">指定模型真实测试</h3>
          <p class="text-small text-muted">仅测试这个账号，不换号。可能消耗额度；通过只代表所选模型本次调用成功。</p>
          <WSelect v-model="checkModel" :options="[{ value: '', label: modelsLoading ? '读取模型目录…' : '请选择模型' }, ...checkModels]" :disabled="testing || checking || modelsLoading" />
          <WButton :loading="testing" :disabled="checking || !checkModel || checkTarget?.raw.enabled === false" @click="runAccountTest">测试调用</WButton>
          <p v-if="checkTarget?.raw.enabled === false" class="text-micro text-faint">已手动停用的账号可做只读检查；实际调用需先由你启用。</p>
          <div v-if="modelTestResult" class="rounded-lg border border-line bg-bg/70 p-3 text-small" :class="modelTestResult.status === 'valid' ? 'text-live' : 'text-warn'">
            <p>{{ modelTestResult.message }}</p><p class="mt-1 text-micro text-faint">{{ modelTestResult.model }} · {{ modelTestResult.latency_ms }} ms · {{ dt(modelTestResult.checked_at) }}</p>
          </div>
        </section>
      </div>
    </WModal>

    <WModal v-model:open="renameOpen" size="sm" :title="renameTarget ? action(renameTarget, 'rename')?.label || '修改名称' : '修改名称'">
      <p class="mb-2 text-micro text-faint">留空则恢复默认显示名（昵称 / ID 短码）。</p>
      <WInput v-model="renameValue" placeholder="输入显示名" @enter="submitRename" />
      <template #footer>
        <WButton variant="subtle" @click="renameOpen = false">取消</WButton>
        <WButton variant="primary" :loading="!!renameTarget && rowBusy.has(renameTarget.provider + '/' + renameTarget.id)" @click="submitRename">保存</WButton>
      </template>
    </WModal>

    <!-- 添加账号 -->
    <WModal v-model:open="addOpen" :title="reauthorizationLabel ? '重新授权' : '添加账号'" @update:open="(v) => !v && stopPoll()">
      <p v-if="reauthorizationLabel" class="mb-3 text-small text-muted">请登录 {{ reauthorizationLabel }} 对应的同一上游账号；登录其他账号会新增账号，不会替换原账号。</p>
      <label class="mb-1.5 block text-small text-muted">供应商</label>
      <WSelect v-model="addProvider" :options="addProviderOptions" :disabled="patSaving" class="mb-4" @update:model-value="onAddProviderChange" />

      <!-- 方式切换（供应商支持多种添加方式时展示） -->
		<div v-if="caps(addProvider).includes('local_import')" class="mb-4 space-y-2"><WButton :loading="localImportBusy" @click="importLocal">导入服务所在电脑的 Trae 登录态</WButton><p class="text-micro text-faint">只读取桌面凭据并验证副本，不修改桌面客户端。远程服务器无法读取你电脑上的登录态。</p></div>
      <div v-if="[caps(addProvider).includes('oauth'), caps(addProvider).includes('upload'), caps(addProvider).includes('add_account')].filter(Boolean).length > 1" class="mb-4 flex gap-2">
        <WButton v-if="caps(addProvider).includes('oauth')" size="sm" :disabled="patSaving" :variant="addMethod === 'oauth' ? 'primary' : 'subtle'" @click="addMethod = 'oauth'">扫码登录</WButton>
        <WButton v-if="caps(addProvider).includes('upload')" size="sm" :disabled="patSaving" :variant="addMethod === 'upload' ? 'primary' : 'subtle'" @click="addMethod = 'upload'">上传凭据</WButton>
        <WButton v-if="caps(addProvider).includes('add_account')" size="sm" :disabled="patSaving" :variant="addMethod === 'token' ? 'primary' : 'subtle'" @click="addMethod = 'token'">{{ addProvider === 'trae' ? 'JSON 导入' : 'PAT 添加' }}</WButton>
      </div>

      <!-- 扫码登录 -->
      <template v-if="addMethod === 'oauth' && caps(addProvider).includes('oauth')">
        <p v-if="addProvider === 'trae'" class="mb-3 text-small text-muted">通过隧道管理时，授权后的本机回调可能无法连接。开始授权后，本窗口会显示回调输入框，请保留此窗口。</p>
        <WSpinner v-if="oauthOptionsLoading" label="加载登录选项" />
        <div v-for="o in oauthOptions" :key="o.key" class="mb-3">
          <label class="mb-1.5 block text-small text-muted">{{ o.label }}</label>
          <WSelect v-model="oauthChoice[o.key]" :options="o.values" :disabled="patSaving" />
        </div>
        <div v-if="oauthUrl" class="rounded-lg border border-line bg-bg/40 p-3 text-small text-muted">
          已在新标签打开授权页，请完成登录后等待自动确认。
          <a :href="oauthUrl" target="_blank" class="ml-1 text-route hover:underline">重新打开</a>
        </div>
		<div v-if="oauthManual && oauthPolling" class="mt-3 space-y-2">
			<p class="text-small text-muted">远程部署时回调会打开你电脑的 localhost 页面。若无法连接，请将该页面完整地址粘贴到这里；不要发送给他人。</p>
			<WTextarea v-model="oauthCallback" :rows="2" placeholder="完整授权回调地址" />
			<WButton :loading="callbackBusy" :disabled="!oauthCallback.trim()" @click="submitOAuthCallback">提交回调</WButton>
		</div>
      </template>

      <!-- 上传凭据 -->
      <template v-else-if="addMethod === 'upload'">
        <input ref="uploadEl" type="file" accept=".info,.json" class="hidden" @change="onUpload" />
        <button
          class="flex w-full flex-col items-center gap-2 rounded-lg border border-dashed border-line py-8 text-muted transition-colors hover:border-brand hover:text-brand"
          @click="uploadEl?.click()"
        >
          <WIcon name="upload" :size="24" />
          <span class="text-small">点击选择 auth 凭据文件（.json）</span>
        </button>
      </template>

      <!-- PAT 添加（qoder：无浏览器/扫码的服务器路径） -->
      <template v-else-if="addMethod === 'token'">
        <div v-for="o in oauthOptions" :key="o.key" class="mb-3">
          <label class="mb-1.5 block text-small text-muted">{{ o.label }}</label>
          <WSelect v-model="oauthChoice[o.key]" :options="o.values" :disabled="patSaving" />
        </div>
        <label class="mb-1.5 block text-small text-muted">{{ addProvider === 'trae' ? 'Trae JSON 凭据' : 'Personal Access Token' }}</label>
        <WTextarea v-model="patToken" :rows="3" :placeholder="addProvider === 'trae' ? '粘贴导出的 JSON 凭据' : '在 Qoder 控制台生成的 PAT'" />
        <p class="mt-2 text-micro text-faint">凭据会先向上游验证再保存，验证失败不保存。</p>
      </template>

      <p v-if="oauthMessage" class="mt-3 text-small text-muted">{{ oauthMessage }}</p>
      <template #footer>
        <WButton variant="subtle" @click="addOpen = false">关闭</WButton>
        <WButton
          v-if="addMethod === 'oauth' && caps(addProvider).includes('oauth')"
          variant="primary"
          :loading="oauthPolling"
          :disabled="oauthOptionsLoading || oauthOptionsFailed"
          @click="beginOAuth"
        >
          {{ oauthPolling ? '等待授权…' : '开始扫码登录' }}
        </WButton>
        <WButton
          v-if="addMethod === 'token'"
          variant="primary"
          :loading="patSaving"
          :disabled="oauthOptionsLoading || oauthOptionsFailed || !patToken.trim()"
          @click="onAddByToken"
        >
          {{ patSaving ? '验证中…' : '验证并添加' }}
        </WButton>
      </template>
    </WModal>
    <!-- OpenCode 配置编辑 -->
    <WModal v-model:open="cfgOpen" title="编辑 OpenCode 配置">
      <WSpinner v-if="cfgLoading" label="加载中" />
      <template v-else>
        <div class="mb-3 rounded-lg border border-line bg-bg/40 p-3 text-micro text-faint">
          凭证是 OpenCode Zen 的 API Key（在 opencode.ai 控制台生成，或本地 <code class="mono text-muted">opencode auth login</code> 后取用）。
          写入 <code class="mono text-muted">{{ cfgPath }}</code>，保存即热重载，无需重启。
        </div>
        <label class="mb-1.5 block text-small text-muted">Zen 层密钥（每行一个）</label>
        <WTextarea v-model="zenText" :rows="3" placeholder="sk-..." class="mb-3" />
        <label class="mb-1.5 block text-small text-muted">Go 层密钥（每行一个）</label>
        <WTextarea v-model="goText" :rows="3" placeholder="sk-..." class="mb-3" />
        <div class="flex items-center justify-between">
          <div>
            <div class="text-small text-muted">启用匿名层</div>
            <div class="text-micro text-faint">无需密钥即可调用免费模型（public 通道）</div>
          </div>
          <WToggle v-model="cfgAnon" />
        </div>
        <div class="mt-3 grid grid-cols-[1fr_9rem] items-center gap-3">
          <label class="text-small text-muted">优先层级</label>
          <WSelect v-model="cfgPrefer" :options="[{ value: 'go', label: 'Go 优先' }, { value: 'zen', label: 'Zen 优先' }]" />
        </div>
      </template>
      <template #footer>
        <WButton variant="subtle" @click="cfgOpen = false">取消</WButton>
        <WButton variant="primary" :loading="cfgSaving" @click="saveConfig">保存并重载</WButton>
      </template>
    </WModal>

    <!-- 积分构成悬浮层：逐资源包明细（对齐官方控制台"积分明细"口径） -->
    <Teleport to="body">
      <div v-for="r in rows.filter(x => pkgKey(x) === healthOpenFor)" id="account-health-details" :key="'health-' + pkgKey(r)" ref="panelEl" role="region" aria-label="账号健康详情" class="fixed z-50 max-h-[calc(100vh-16px)] w-80 max-w-[calc(100vw-16px)] overflow-y-auto rounded-xl border border-line bg-elevated p-4 shadow-glass" :style="healthStyle" @mouseenter="cancelClose" @mouseleave="closeHealth">
        <div class="mb-3 flex items-center justify-between gap-3"><span class="font-medium text-ink">健康状态</span><span class="truncate text-micro text-muted">{{ label(r) }}</span></div>
        <dl class="space-y-2 text-small"><div v-for="item in healthDetails(r)" :key="item.label" class="grid grid-cols-[6rem_minmax(0,1fr)] gap-3"><dt class="text-faint">{{ item.label }}</dt><dd class="min-w-0 break-words text-ink">{{ item.value }}</dd></div></dl>
      </div>
      <div
        v-for="r in rows.filter((x) => pkgKey(x) === pkgOpenFor)"
        :key="pkgKey(r)"
        ref="panelEl"
        class="fixed z-50 w-fit min-w-[300px] max-w-[440px] rounded-xl border border-line bg-elevated/95 p-4 shadow-glass backdrop-blur-sm"
        :style="pkgStyle"
        @mouseenter="cancelClose"
        @mouseleave="closePkgs"
      >
        <div class="mb-3 flex items-start justify-between gap-3">
          <span class="shrink-0 font-medium text-ink">额度构成</span>
          <span class="min-w-0 break-words text-right text-micro leading-snug text-faint">{{ label(r) }}</span>
        </div>
        <div v-if="r.raw.credits_granted != null || r.raw.credits_used != null" class="mb-3 flex flex-wrap gap-3 text-micro text-muted"><span v-if="r.raw.credits_granted != null">累计发放 {{ fmtCredits(r.raw.credits_granted) }}</span><span v-if="r.raw.credits_used != null">累计消耗 {{ fmtCredits(r.raw.credits_used) }}</span></div>
        <div class="max-h-[60vh] overflow-y-auto space-y-3">
          <div v-for="(p, i) in pkgsOf(r)" :key="i">
            <div class="flex items-start justify-between gap-3">
              <span class="min-w-0 break-words text-small leading-snug text-muted">{{ p.name }}</span>
              <span v-if="pkgPct(p) != null" class="mono shrink-0 text-small leading-snug" :class="pkgExpiring(p) ? 'text-warn' : 'text-ink'">
                {{ Math.round(pkgPct(p)!) }}%
              </span>
            </div>
            <div v-if="p.description" class="mt-1 break-words text-micro leading-snug text-faint">{{ p.description }}</div>
            <div v-if="pkgPct(p) != null" class="mt-1.5 h-1.5 overflow-hidden rounded-full bg-line/60">
              <div
                v-if="pkgPct(p) != null"
                class="h-full rounded-full"
                :class="pkgExpiring(p) ? 'bg-warn' : 'bg-brand'"
                :style="{ width: pkgPct(p)! + '%' }"
              />
            </div>
            <!-- flex-wrap：一行放不下时整段折到下一行，数字串自身不折断 -->
            <div class="mt-1.5 flex flex-wrap items-baseline gap-x-3 gap-y-0.5 text-micro text-faint">
              <span class="mono whitespace-nowrap">{{ p.unlimited ? '上游额度不限' : '剩 ' + (p.remain == null ? '未知' : fmtCredits(p.remain)) + ' / ' + (p.total == null ? '未知' : fmtCredits(p.total)) }}</span>
              <span class="mono whitespace-nowrap">已用 {{ p.used == null ? '未知' : fmtCredits(p.used) }}</span>
              <span v-if="p.unit" class="whitespace-nowrap">{{ p.unit }}</span><span v-if="p.display_only" class="text-faint">仅供参考</span>
              <!-- 临期包：过期时间精确到分钟并标黄，一眼看出紧迫度 -->
              <span v-if="p.expire_at" class="ml-auto mono whitespace-nowrap" :class="pkgExpiring(p) ? 'text-warn' : ''">
                到期：{{ dt(p.expire_at, 'YYYY-MM-DD HH:mm') }}
              </span>
              <span v-if="p.reset_time" class="ml-auto mono whitespace-nowrap text-faint">重置：{{ p.reset_time }}</span>
            </div>
          </div>
        </div>
        <div class="mt-3 border-t border-line pt-2 text-micro text-faint">到期表示资源作废；重置表示周期补充。未上报的时间不会推断，已用完的包不计入最早到期。</div>
      </div>

      <!-- 签到日历：35 天打卡点阵 -->
      <div
        v-for="r in rows.filter((x) => pkgKey(x) === calOpenFor)"
        :key="'cal-' + pkgKey(r)"
        ref="panelEl"
        class="fixed z-50 w-fit min-w-[300px] max-w-[460px] rounded-xl border border-line bg-elevated/95 p-4 shadow-glass backdrop-blur-sm"
        :style="calStyle"
        @mouseenter="cancelClose"
        @mouseleave="closeCalendar"
      >
        <div class="mb-3 flex items-start justify-between gap-3">
          <span class="shrink-0 font-medium text-ink">签到记录 · 近 {{ calDays }} 天</span>
          <span class="min-w-0 break-words text-right text-micro leading-snug text-faint">{{ label(r) }}</span>
        </div>
        <p v-if="r.raw.checkin_pending" class="mb-3 text-small text-warn">今日已发起领取，上游尚未确认签到；后续仅刷新状态。</p>
        <div class="grid grid-cols-7 gap-1">
          <div
            v-for="c in calGrid(calDatesOf(r), r)"
            :key="c.date"
            class="h-4 w-4 rounded-sm"
            :class="{
              'bg-live/70': c.state === 'ok',
              'bg-line/50': c.state === '',
              'bg-transparent ring-1 ring-line/30': c.state === 'future',
            }"
            :title="c.date + (c.state === 'ok' ? ' · 已签到' : c.state === '' ? ' · 未签到' : ' · 签到窗口尚未开放')"
          />
        </div>
        <div class="mt-3 flex items-center justify-between gap-3 text-micro text-faint">
          <span class="flex items-center gap-2">
            <span class="inline-block h-2.5 w-2.5 rounded-sm bg-live/70" /> 已签
            <span class="ml-1 inline-block h-2.5 w-2.5 rounded-sm bg-line/50" /> 未签
          </span>
          <span class="mono whitespace-nowrap">{{ calDatesOf(r).length }} / {{ calDays }} 天</span>
        </div>
        <div class="mt-2 border-t border-line pt-2 text-micro text-faint">
          按渠道本地记录展示 · {{ calendars[r.provider]?.timezone || '时区未知' }}
          <span v-if="calendars[r.provider]?.window_date"> · 当前窗口 {{ calendars[r.provider]?.window_date }}</span>
        </div>
      </div>
    </Teleport>
  </WPage>
</template>
