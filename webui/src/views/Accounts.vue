<script setup lang="ts">
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
const loading = ref(true)
const busy = ref(false)
const rowBusy = ref(new Set<string>())
async function mutateRow(r: Record<string, any>, operation: () => Promise<unknown>) {
  const key = r.provider + '/' + r.id
  if (rowBusy.value.has(key)) return
  rowBusy.value.add(key)
  try { await operation(); await load() } finally { rowBusy.value.delete(key) }
}

const providerFilter = ref('')
const statusFilter = ref('')
const search = ref('')

// 供应商能力查询
function caps(name: string): string[] {
  return providers.value.find((p) => p.name === name)?.capabilities || []
}

const readErrors = ref<Record<string, string>>({})
let loadVersion = 0
async function load() {
  const version = ++loadVersion
  loading.value = true
  const nextRows = new Map<string, Row[]>()
  for (const p of providers.value) {
    nextRows.set(p.name, rows.value.filter(r => r.provider === p.name))
  }
  const errors: Record<string, string> = {}
  try {
    let provs: ProviderSummary[]
    try { provs = (await api.getProviders()).providers || [] }
    catch { if (version === loadVersion) readErrors.value = { providers: '渠道列表读取失败，保留上次成功快照' }; return }
    if (version !== loadVersion) return
    providers.value = provs
    const names = new Set(provs.filter(p => p.capabilities?.includes('accounts') || p.capabilities?.includes('config')).map(p => p.name))
    for (const name of nextRows.keys()) if (!names.has(name)) nextRows.delete(name)
    const targets = provs.filter(p => p.capabilities?.includes('accounts') || p.capabilities?.includes('config'))
    const results = await Promise.allSettled(targets.map(p => api.getProvider(p.name)))
    if (version !== loadVersion) return
    results.forEach((result, i) => {
      const name = targets[i].name
      if (result.status === 'rejected') { errors[name] = '读取失败，保留上次成功快照'; return }
      const detail = result.value
      const metadata = new Map((detail.resources || []).map(r => [r.id, r]))
      const accounts: Row[] = []
      for (const raw of detail.accounts || []) {
        const id = String(raw.id ?? raw.uid)
        const resource = metadata.get(id)
        const quota = resource?.quota
        const normalized = quota ? { ...raw, credits_remaining: quota.remaining, credits_total: quota.total, credits_expire_at: quota.expires_at, quota_stale: quota.stale, quota_refresh_failed: quota.refresh_failed } : raw
        accounts.push({ key: name + '/' + id, provider: name, id, raw: normalized, resource })
      }
      nextRows.set(name, accounts)
    })
    rows.value = [...nextRows.values()].flat()
    selectedKeys.value = new Set([...selectedKeys.value].filter(key => rows.value.some(r => r.key === key)))
    readErrors.value = errors
  } finally { if (version === loadVersion) loading.value = false }
}
function action(r: Record<string, any>, name: string) { return (r.resource as ResourceSummary | undefined)?.actions[name] }
function can(r: Record<string, any>, name: string) { return action(r, name)?.enabled === true }
const selectedKeys = ref(new Set<string>())
const batchBusy = ref(false)
const batchResults = ref<{ key: string; label: string; ok: boolean }[]>([])
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
  if (a.enabled === false) return { tone: 'muted', text: a.disabled_reason || '已停用' }
  const cd = a.cooldown_until && a.cooldown_until > Date.now() / 1000
  if (cd) return { tone: 'fault', text: '冷却 ' + cooldown(a.cooldown_until) }
  const states: Record<string, { tone: 'live' | 'warn' | 'fault' | 'muted'; text: string }> = {
    disabled: { tone: 'muted', text: '已停用' }, configured: { tone: 'live', text: '已配置' }, unconfigured: { tone: 'muted', text: '未配置' },
    quota_exceeded: { tone: 'warn', text: '额度不足' }, healthy: { tone: 'live', text: '正常' }, unauthorized: { tone: 'warn', text: '未授权' }, unhealthy: { tone: 'warn', text: '异常' }, unknown: { tone: 'muted', text: '未上报' },
  }
  if (r.resource?.status && states[r.resource.status]) return states[r.resource.status]
  if (a.tier) return { tone: a.key_count > 0 ? 'live' : 'muted', text: a.key_count > 0 ? '已配置' : '未配置' }
  if (a.quota_exceeded) return { tone: 'warn', text: '额度不足' }
  if (a.healthy) return { tone: 'live', text: '正常' }
  return { tone: 'warn', text: a.has_secret === false ? '未授权' : '异常' }
}

const filtered = computed(() =>
  rows.value.filter((r) => {
    if (providerFilter.value && r.provider !== providerFilter.value) return false
    if (statusFilter.value) {
      const st = statusOf(r)
      if (statusFilter.value === 'healthy' && st.tone !== 'live') return false
      if (statusFilter.value === 'unhealthy' && st.tone === 'live') return false
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
  const dates = pkgsOf(r).filter(p => p.remain > 0 && p.expire_at).map(p => p.expire_at!)
  if (r.raw.credits_expire_at > 0) dates.push(r.raw.credits_expire_at)
  return dates.length ? Math.min(...dates) : undefined
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
  { key: 'kind', label: '类型', minWidth: '5.5rem', nowrap: true },
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
// workbuddy 账号的 credit_packages 逐资源包展示：名称/剩余/已用/总量/到期。
// 弹层 Teleport 到 body——表格容器 overflow-x-auto 会裁剪行内绝对定位。
interface Pkg {
  name: string
  remain: number
  used: number
  total: number
  expire_at?: number | null
  reset_time?: string
}

const pkgOpenFor = ref<string | null>(null) // `${provider}:${id}`
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
  if (!p.total || p.total <= 0) return null
  return Math.max(0, Math.min(100, (p.remain / p.total) * 100))
}

// 有余额且 7 天内到期的包：临期（与选号"临期优先烧"同窗口语义）
function pkgExpiring(p: Pkg): boolean {
  if (!p.expire_at || p.remain <= 0) return false
  return p.expire_at * 1000 - Date.now() < 7 * 86400_000
}

function openPkgs(r: any, e: MouseEvent | FocusEvent) {
  if (!pkgsOf(r).length) return
  cancelClose()
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
  cancelClose()
  closeTimer = setTimeout(() => {
    pkgOpenFor.value = null
    pkgAnchor.value = null
  }, 150)
}

// ---------- 签到日历 ----------
// 悬停/点击「今日签到」列弹出 35 天打卡点阵。数据源 /admin/checkin/history，
// 一次拉全量按完整 uid 索引（workbuddy uid 形如 ea3293e0-…，qoder 为账号 id）。
const calOpenFor = ref<string | null>(null)
const calStyle = ref<Record<string, string>>({})
const calData = ref<Record<string, string[]>>({})
const calendars = ref<Record<string, { history: Record<string, string[]>; today: string; window_date: string; timezone: string }>>({})
let calendarTimer: ReturnType<typeof setInterval> | null = null
let calendarInflight: Promise<void> | null = null
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

async function loadCalendar() {
  if (calendarInflight) return calendarInflight
  calendarInflight = fetchCalendar()
  try { await calendarInflight } finally { calendarInflight = null }
}

async function fetchCalendar() {
  try {
    const res = await api.checkinHistory(calDays.value)
    calData.value = res.history || {}
    calendars.value = res.calendars || {}
    calDays.value = res.days || 35
  } catch {
    /* 静默：日历是增强信息，失败不打断账号列表 */
  }
}

function openCalendar(r: any, e: MouseEvent | FocusEvent) {
  cancelClose()
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
  cancelClose()
  closeTimer = setTimeout(() => {
    calOpenFor.value = null
    calAnchor.value = null
  }, 150)
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
        const unsupported = result.unsupported === true
        const ok = (result.ok !== false || unsupported) && failed === 0
        taskResults.value.push({ provider: p.name, ok, message: unsupported ? '此账号暂不支持该操作' : ok ? '任务已完成' : `任务存在失败${failed ? '（' + failed + ' 项）' : ''}，请查看账号状态` })
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
const addProvider = ref('')
const addMethod = ref<'upload' | 'oauth' | 'token'>('oauth')
const oauthOptions = ref<OAuthOption[]>([])
const oauthChoice = ref<Record<string, string>>({})
const oauthUrl = ref('')
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
  addProvider.value = addProviderOptions.value.find(p => p.value === providerFilter.value)?.value || addProviderOptions.value[0]?.value || ''
  addOpen.value = true
  await onAddProviderChange()
}

async function onAddProviderChange() {
  patToken.value = ''
  oauthMessage.value = ''
  stopPoll()
  const generation = pollGeneration
  const name = addProvider.value
  oauthUrl.value = ''
  oauthChoice.value = {}
  const c = caps(addProvider.value)
  addMethod.value = c.includes('oauth') ? 'oauth' : c.includes('upload') ? 'upload' : c.includes('add_account') ? 'token' : 'oauth'
  if (c.includes('oauth')) {
    const res = await api.providerOAuthOptions(name)
    if (generation !== pollGeneration) return
    oauthOptions.value = res.options || []
    for (const o of oauthOptions.value) oauthChoice.value[o.key] = o.default || o.values[0]?.value || ''
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
  stopPoll()
  const generation = pollGeneration
  const controller = new AbortController()
  pollController = controller
  const provider = addProvider.value
  oauthMessage.value = ''
  oauthPolling.value = true
  try {
    let poll: () => Promise<{ status: string; message?: string }>
    let deadline = Date.now() + 10 * 60_000
    const res = await api.providerOAuthBegin(provider, { ...oauthChoice.value }, controller.signal)
    if (generation !== pollGeneration) return
    oauthUrl.value = res.login_url
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
        if (p.status === 'ready') { stopPoll(); toast.success('已添加账号'); addOpen.value = false; await load(); return }
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
  } catch {
    if (generation === pollGeneration) stopPoll()
  }
}

watch(addMethod, () => { if (oauthPolling.value) stopPoll() })
watch(oauthChoice, () => { if (oauthPolling.value) stopPoll() }, { deep: true })

async function onUpload(e: Event) {
  const file = (e.target as HTMLInputElement).files?.[0]
  if (!file) return
  try {
    await api.uploadAuth(file)
    toast.success('已上传账号凭据')
    addOpen.value = false
    await load()
  } finally {
    if (uploadEl.value) uploadEl.value.value = ''
  }
}

const patToken = ref('')
const patSaving = ref(false)
watch(addOpen, (open) => { if (!open) { patToken.value = ''; oauthMessage.value = ''; stopPoll() } })
async function onAddByToken() {
  const token = patToken.value.trim()
  if (!token || patSaving.value) return
  const generation = pollGeneration
  patSaving.value = true
  oauthMessage.value = ''
  try {
    await api.providerImportAccount(addProvider.value, token, { ...oauthChoice.value })
    if (generation !== pollGeneration) { await load(); return }
    toast.success('已添加账号')
    patToken.value = ''
    addOpen.value = false
    await load()
  } catch (err: any) {
    if (generation === pollGeneration) oauthMessage.value = err?.response?.data?.error?.message || '添加失败，请检查 PAT 后重试'
  } finally {
    patSaving.value = false
  }
}

function refreshVisibleCalendar() { if (document.visibilityState === 'visible') void loadCalendar() }
onMounted(() => {
  void load()
  void loadCalendar()
  calendarTimer = setInterval(refreshVisibleCalendar, 60000)
  document.addEventListener('visibilitychange', refreshVisibleCalendar)
})
// 组件卸载时停掉可能残留的扫码轮询/悬浮层关闭定时器，避免离开页面后仍在轮询
onUnmounted(() => {
  loadVersion++
  stopPoll()
  cancelClose()
  if (calendarTimer) clearInterval(calendarTimer)
  document.removeEventListener('visibilitychange', refreshVisibleCalendar)
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
          :options="[{ value: 'healthy', label: '仅正常' }, { value: 'unhealthy', label: '仅异常' }]"
          placeholder="全部状态"
          class="w-32"
        />
        <span class="ml-auto text-small text-faint">正常 / 已配置 <span class="mono text-live">{{ summary.healthy }}</span> / {{ summary.total }}</span>
      </div>

      <div v-if="selectedRows.length" class="mb-3 flex flex-wrap items-center gap-2 rounded-lg border border-line p-3">
        <span class="text-small text-muted">已选 {{ selectedRows.length }} 项（含筛选隐藏项）</span>
        <WButton :disabled="!batchEnabled || batchBusy" :loading="batchBusy" @click="batchToggle(true)">批量启用</WButton>
        <WButton :disabled="!batchEnabled || batchBusy" @click="batchToggle(false)">批量停用</WButton>
        <WButton variant="subtle" :disabled="batchBusy" @click="selectedKeys.clear()">清除选择</WButton>
        <span v-if="!batchEnabled" class="text-micro text-faint">所选资源须全部支持启停且状态读取成功。</span>
      </div>
      <div v-if="batchResults.length" role="status" class="mb-3 text-micro"><div v-for="result in batchResults" :key="result.key" :class="result.ok ? 'text-muted' : 'text-warn'">{{ result.label }}：{{ result.ok ? '已更新' : '更新失败，其余账号继续处理' }}</div></div>
      <WCard flush>
        <WTable :columns="cols" :rows="filtered" row-key="key">
          <template #cell-select="{ row }"><input type="checkbox" :aria-label="'选择 ' + label(row)" :checked="selectedKeys.has(row.key)" :disabled="batchBusy || rowBusy.has(row.key)" @change="selectRow(row, ($event.target as HTMLInputElement).checked)"></template>
          <template #cell-provider="{ row }">
            <WTag :tone="providerMeta(row.provider).tone">{{ providerMeta(row.provider).label }}</WTag>
          </template>
          <template #cell-kind="{ row }"><WTag tone="muted">{{ kindLabel(row) }}</WTag></template>
          <template #cell-account="{ row }">
            <div class="w-44">
              <div class="truncate font-medium text-ink" :title="label(row)">{{ label(row) }}</div>
              <div class="mt-1 flex items-center gap-2 whitespace-nowrap text-micro text-faint"><span class="mono truncate" :title="row.id">{{ row.id.length > 14 ? row.id.slice(0, 12) + '…' : row.id }}</span><span v-if="row.raw.source" class="shrink-0">{{ ({local:'桌面',project:'导入',native:'网关'} as Record<string,string>)[row.raw.source] || row.raw.source }}</span></div>
            </div>
          </template>
          <template #cell-site="{ row }">
            <span class="text-muted">{{ siteLabel(row.resource?.region || row.raw.site || row.raw.region) || '—' }}</span>
          </template>
          <template #cell-status="{ row }">
            <span class="inline-flex items-center gap-1.5">
              <WLed :tone="statusOf(row).tone" :pulse="statusOf(row).tone === 'live'" />
              <span class="text-small" :class="statusTextClass(row)">{{ statusOf(row).text }}</span>
            </span>
            <div v-if="row.raw.failure_count" class="text-micro text-faint">失败 {{ row.raw.failure_count }} 次</div>
          </template>
          <template #cell-credits="{ row }">
            <template v-if="row.raw.credits_remaining != null">
              <span class="mono text-ink">{{ fmtCredits(row.raw.credits_remaining) }}</span>
              <span v-if="row.raw.credits_total != null" class="mono text-faint"> / {{ fmtCredits(row.raw.credits_total) }}</span>
            </template>
            <span v-else-if="row.raw.tier" class="text-muted">{{ row.raw.key_count }} {{ row.raw.tier === 'anonymous' ? '个入口' : '个密钥' }}</span>
            <span v-else class="text-faint">未上报</span>
            <div v-if="row.raw.quota_refresh_failed || row.raw.quota_stale || row.raw.plan" class="mt-1 flex justify-end gap-2 text-micro text-faint"><span v-if="row.raw.quota_refresh_failed" class="text-warn" :title="row.raw.quota_updated_at ? '最后成功：' + dt(row.raw.quota_updated_at) : '尚未成功获取额度'">刷新失败</span><span v-else-if="row.raw.quota_stale">缓存过期</span><span v-if="row.raw.plan" :title="`上游账号套餐：${row.raw.plan}；不代表模型调用免费`">套餐：{{ String(row.raw.plan).toLowerCase() === 'free' ? '免费' : row.raw.plan }}</span></div>
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
                <span class="text-muted">{{ expiryOf(row) ? dt(expiryOf(row), 'YYYY-MM-DD') : '查看资源包' }}</span>
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
              title="悬停查看 35 天签到记录"
            >
              <WTag v-if="row.raw.checkin_today" class="whitespace-nowrap" tone="live" dot>今日已签</WTag>
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
            <WButton v-if="can(row, 'config')" size="sm" @click="openConfig(row.provider)">编辑配置</WButton>
            <ResourceActions v-else :resource="row.resource" :enabled="row.raw.enabled === true" :busy="rowBusy.has(row.provider + '/' + row.id)" @rename="openRename(row)" @remove="removeAccount(row)" @enabled="toggleEnabled(row)" />
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
    <WModal v-model:open="renameOpen" size="sm" :title="renameTarget ? action(renameTarget, 'rename')?.label || '修改名称' : '修改名称'">
      <p class="mb-2 text-micro text-faint">留空则恢复默认显示名（昵称 / ID 短码）。</p>
      <WInput v-model="renameValue" placeholder="输入显示名" @enter="submitRename" />
      <template #footer>
        <WButton variant="subtle" @click="renameOpen = false">取消</WButton>
        <WButton variant="primary" :loading="!!renameTarget && rowBusy.has(renameTarget.provider + '/' + renameTarget.id)" @click="submitRename">保存</WButton>
      </template>
    </WModal>

    <!-- 添加账号 -->
    <WModal v-model:open="addOpen" title="添加账号" @update:open="(v) => !v && stopPoll()">
      <label class="mb-1.5 block text-small text-muted">供应商</label>
      <WSelect v-model="addProvider" :options="addProviderOptions" :disabled="patSaving" class="mb-4" @update:model-value="onAddProviderChange" />

      <!-- 方式切换（供应商支持多种添加方式时展示） -->
      <div v-if="[caps(addProvider).includes('oauth'), caps(addProvider).includes('upload'), caps(addProvider).includes('add_account')].filter(Boolean).length > 1" class="mb-4 flex gap-2">
        <WButton v-if="caps(addProvider).includes('oauth')" size="sm" :disabled="patSaving" :variant="addMethod === 'oauth' ? 'primary' : 'subtle'" @click="addMethod = 'oauth'">扫码登录</WButton>
        <WButton v-if="caps(addProvider).includes('upload')" size="sm" :disabled="patSaving" :variant="addMethod === 'upload' ? 'primary' : 'subtle'" @click="addMethod = 'upload'">上传凭据</WButton>
        <WButton v-if="caps(addProvider).includes('add_account')" size="sm" :disabled="patSaving" :variant="addMethod === 'token' ? 'primary' : 'subtle'" @click="addMethod = 'token'">PAT 添加</WButton>
      </div>

      <!-- 扫码登录 -->
      <template v-if="addMethod === 'oauth' && caps(addProvider).includes('oauth')">
        <div v-for="o in oauthOptions" :key="o.key" class="mb-3">
          <label class="mb-1.5 block text-small text-muted">{{ o.label }}</label>
          <WSelect v-model="oauthChoice[o.key]" :options="o.values" :disabled="patSaving" />
        </div>
        <div v-if="oauthUrl" class="rounded-lg border border-line bg-bg/40 p-3 text-small text-muted">
          已在新标签打开授权页，请完成登录后等待自动确认。
          <a :href="oauthUrl" target="_blank" class="ml-1 text-route hover:underline">重新打开</a>
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
        <label class="mb-1.5 block text-small text-muted">Personal Access Token</label>
        <WTextarea v-model="patToken" :rows="3" placeholder="在 Qoder 控制台生成的 PAT" />
        <p class="mt-2 text-micro text-faint">PAT 会先向上游验证再落库，验证失败不保存。</p>
      </template>

      <p v-if="oauthMessage" class="mt-3 text-small text-muted">{{ oauthMessage }}</p>
      <template #footer>
        <WButton variant="subtle" @click="addOpen = false">关闭</WButton>
        <WButton
          v-if="addMethod === 'oauth' && caps(addProvider).includes('oauth')"
          variant="primary"
          :loading="oauthPolling"
          @click="beginOAuth"
        >
          {{ oauthPolling ? '等待授权…' : '开始扫码登录' }}
        </WButton>
        <WButton
          v-if="addMethod === 'token'"
          variant="primary"
          :loading="patSaving"
          :disabled="!patToken.trim()"
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
          <span class="shrink-0 font-medium text-ink">积分构成</span>
          <span class="min-w-0 break-words text-right text-micro leading-snug text-faint">{{ label(r) }}</span>
        </div>
        <div class="space-y-3">
          <div v-for="(p, i) in pkgsOf(r)" :key="i">
            <div class="flex items-start justify-between gap-3">
              <span class="min-w-0 break-words text-small leading-snug text-muted">{{ p.name }}</span>
              <span v-if="pkgPct(p) != null" class="mono shrink-0 text-small leading-snug" :class="pkgExpiring(p) ? 'text-warn' : 'text-ink'">
                {{ Math.round(pkgPct(p)!) }}%
              </span>
            </div>
            <div class="mt-1.5 h-1.5 overflow-hidden rounded-full bg-line/60">
              <div
                v-if="pkgPct(p) != null"
                class="h-full rounded-full"
                :class="pkgExpiring(p) ? 'bg-warn' : 'bg-brand'"
                :style="{ width: pkgPct(p)! + '%' }"
              />
            </div>
            <!-- flex-wrap：一行放不下时整段折到下一行，数字串自身不折断 -->
            <div class="mt-1.5 flex flex-wrap items-baseline gap-x-3 gap-y-0.5 text-micro text-faint">
              <span class="mono whitespace-nowrap">剩 {{ fmtCredits(p.remain) }} / {{ fmtCredits(p.total) }}</span>
              <span class="mono whitespace-nowrap">已用 {{ fmtCredits(p.used) }}</span>
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
