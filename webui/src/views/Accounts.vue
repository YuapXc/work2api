<script setup lang="ts">
import { ref, computed, nextTick, onMounted, onUnmounted, watch } from 'vue'
import { api } from '@/api/client'
import type { ProviderSummary, OAuthOption } from '@/types'
import { toast } from '@/lib/toast'
import { confirm } from '@/lib/confirm'
import { credits as fmtCredits, dt, rel, cooldown, int as fmtInt } from '@/lib/format'
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
import WLed from '@/components/ui/WLed.vue'
import WEmpty from '@/components/ui/WEmpty.vue'
import WSpinner from '@/components/ui/WSpinner.vue'
import WIcon from '@/components/ui/WIcon.vue'

interface Row {
  provider: string
  id: string
  raw: Record<string, any>
}

const providers = ref<ProviderSummary[]>([])
const rows = ref<Row[]>([])
const opencodeTiers = ref<{ provider: string; raw: Record<string, any> }[]>([])
const loading = ref(true)
const busy = ref(false)

const providerFilter = ref('')
const statusFilter = ref('')
const search = ref('')

// 供应商能力查询
function caps(name: string): string[] {
  return providers.value.find((p) => p.name === name)?.capabilities || []
}

async function load() {
  loading.value = true
  const acc: Row[] = []
  const tiers: { provider: string; raw: Record<string, any> }[] = []
  try {
    const provs = (await api.getProviders()).providers || []
    providers.value = provs
    // workbuddy：默认账号池
    const wb = (await api.accounts()).accounts || []
    for (const a of wb) acc.push({ provider: 'workbuddy', id: a.uid, raw: a })
    // 其它供应商
    const others = provs.filter((p) => !p.default && (p.capabilities?.includes('accounts') || p.capabilities?.includes('config')))
    const details = await Promise.all(others.map((p) => api.getProvider(p.name).catch(() => null)))
    details.forEach((d, i) => {
      if (!d) return
      const name = others[i].name
      for (const a of (d.accounts as Record<string, any>[]) || []) {
        if (a.tier) tiers.push({ provider: name, raw: a })
        else acc.push({ provider: name, id: String(a.id ?? a.uid), raw: a })
      }
    })
    rows.value = acc
    opencodeTiers.value = tiers
  } finally {
    loading.value = false
  }
}

// 状态归一
function statusOf(r: any): { tone: 'live' | 'warn' | 'fault' | 'muted'; text: string } {
  const a = r.raw
  if (a.enabled === false) return { tone: 'muted', text: a.disabled_reason || '已停用' }
  const cd = a.cooldown_until && a.cooldown_until > Date.now() / 1000
  if (cd) return { tone: 'fault', text: '冷却 ' + cooldown(a.cooldown_until) }
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

const providerOptions = computed(() =>
  [...new Set(rows.value.map((r) => r.provider))].map((p) => ({ value: p, label: providerMeta(p).label })),
)
const summary = computed(() => {
  const total = rows.value.length
  const healthy = rows.value.filter((r) => statusOf(r).tone === 'live').length
  return { total, healthy }
})

function label(r: any): string {
  const a = r.raw
  return a.label || a.alias || a.nickname || r.id.slice(0, 8)
}

// 本机 Qoder 桌面端自动探测出的账号：非持久、由桌面端登录态决定，不能在此改名/删除/设为激活。
function isLocalQoder(r: any): boolean {
  return r.provider === 'qoder' && r.raw.source === 'local'
}

const cols: Column[] = [
  { key: 'provider', label: '供应商' },
  { key: 'account', label: '账号' },
  { key: 'site', label: '站点' },
  { key: 'status', label: '状态' },
  { key: 'credits', label: '额度（剩余/总）', align: 'right' },
  { key: 'expire', label: '到期' },
  { key: 'checkin', label: '今日签到' },
  { key: 'ctrl', label: '优先级/激活', align: 'center' },
  { key: 'actions', label: '操作', align: 'right' },
]

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
  if (r.provider === 'workbuddy') return r.raw?.site !== 'international'
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
  if (r.provider !== 'workbuddy') return
  await api.setEnabled(r.id, !r.raw.enabled)
  await load()
}
async function savePriority(r: any, v: number) {
  if (r.provider !== 'workbuddy' || isNaN(v)) return
  await api.setPriority(r.id, v)
  r.raw.priority = v
}
async function activate(r: any) {
  await api.providerActivateAccount(r.provider, r.id)
  toast.success('已设为激活账号')
  await load()
}
async function removeAccount(r: any) {
  const ok = await confirm({
    title: `删除账号「${label(r)}」？`,
    body: '删除后该账号不再参与请求分发，历史用量记录保留；桌面端原始凭据保留并持续隐藏，重新导入或授权可恢复。',
    tone: 'danger',
    okText: '删除',
  })
  if (!ok) return
  if (r.provider === 'workbuddy') await api.deleteAccount(r.id)
  else await api.providerDeleteAccount(r.provider, r.id)
  toast.success('已删除')
  await load()
}

// 重命名 / 别名（workbuddy=别名，其它=rename）
const renameOpen = ref(false)
const renameTarget = ref<Row | null>(null)
const renameValue = ref('')
function openRename(r: any) {
  renameTarget.value = r
  renameValue.value = r.provider === 'workbuddy' ? r.raw.alias || '' : r.raw.label || ''
  renameOpen.value = true
}
async function submitRename() {
  const r = renameTarget.value
  if (!r) return
  if (r.provider === 'workbuddy') await api.setAccountAlias(r.id, renameValue.value.trim())
  else await api.providerRenameAccount(r.provider, r.id, renameValue.value.trim())
  renameOpen.value = false
  toast.success('已保存')
  await load()
}

// ---------- 供应商级动作：签到 / 刷新额度 ----------
async function checkinAll() {
  busy.value = true
  try {
    const targets = providers.value.filter((p) => p.capabilities?.includes('checkin'))
    for (const p of targets) await api.providerCheckin(p.name)
    toast.success('签到完成')
    await load()
    await loadCalendar()
  } finally {
    busy.value = false
  }
}
async function refreshCreditsAll() {
  busy.value = true
  try {
    const targets = providers.value.filter((p) => p.capabilities?.includes('credits'))
    for (const p of targets) await api.providerCredits(p.name)
    toast.success('额度已刷新')
    await load()
  } finally {
    busy.value = false
  }
}

// ---------- 添加账号 ----------
const addOpen = ref(false)
const addProvider = ref('')
const addMethod = ref<'upload' | 'oauth'>('oauth')
const oauthSites = ref<string[]>([])
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
  addProvider.value = providers.value.find((p) => p.default)?.name || providers.value[0]?.name || ''
  addOpen.value = true
  await onAddProviderChange()
}

async function onAddProviderChange() {
  stopPoll()
  const generation = pollGeneration
  const name = addProvider.value
  oauthUrl.value = ''
  oauthChoice.value = {}
  const c = caps(addProvider.value)
  addMethod.value = c.includes('oauth') ? 'oauth' : c.includes('upload') ? 'upload' : 'oauth'
  if (name === 'workbuddy') {
    const res = await api.oauthSites()
    if (generation !== pollGeneration) return
    oauthSites.value = res.sites || []
    oauthOptions.value = []
    if (oauthSites.value.length) oauthChoice.value.site = oauthSites.value[0]
  } else if (c.includes('oauth')) {
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
  const site = oauthChoice.value.site
  oauthMessage.value = ''
  oauthPolling.value = true
  try {
    let poll: () => Promise<{ status: string; message?: string }>
    let deadline = Date.now() + 10 * 60_000
    if (provider === 'workbuddy') {
      const res = await api.oauthBegin(site, controller.signal)
      if (generation !== pollGeneration) return
      oauthUrl.value = res.authUrl
      if (res.expires_at) deadline = Math.min(deadline, res.expires_at * 1000)
      poll = () => api.oauthPoll(res.state, site, controller.signal)
    } else {
      const res = await api.providerOAuthBegin(provider, { ...oauthChoice.value }, controller.signal)
      if (generation !== pollGeneration) return
      oauthUrl.value = res.login_url
      if (res.expires_at) deadline = Math.min(deadline, res.expires_at * 1000)
      poll = () => api.providerOAuthPoll(provider, res.login_id, controller.signal)
    }
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

function refreshVisibleCalendar() { if (document.visibilityState === 'visible') void loadCalendar() }
onMounted(() => {
  void load()
  void loadCalendar()
  calendarTimer = setInterval(refreshVisibleCalendar, 60000)
  document.addEventListener('visibilitychange', refreshVisibleCalendar)
})
// 组件卸载时停掉可能残留的扫码轮询/悬浮层关闭定时器，避免离开页面后仍在轮询
onUnmounted(() => {
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

function tiersOf(name: string) {
  return opencodeTiers.value.filter((t) => t.provider === name)
}

async function openConfig(name: string) {
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
  <WPage title="账号" sub="所有供应商的账号集中在一处；供应商作为筛选维度。">
    <template #actions>
      <WButton variant="ghost" :loading="busy" @click="refreshCreditsAll"><WIcon name="refresh" :size="15" /> 刷新额度</WButton>
      <WButton variant="ghost" :loading="busy" @click="checkinAll"><WIcon name="check" :size="15" /> 一键签到</WButton>
      <WButton variant="primary" @click="openAdd"><WIcon name="plus" :size="16" /> 添加账号</WButton>
    </template>

    <WSpinner v-if="loading" center label="加载中" />
    <template v-else>
      <!-- 筛选条 -->
      <div class="mb-4 flex flex-wrap items-center gap-2">
        <WInput v-model="search" placeholder="搜索别名 / 昵称 / ID" class="w-full sm:w-64">
          <template #prefix><WIcon name="search" :size="16" class="text-faint" /></template>
        </WInput>
        <WSelect v-model="providerFilter" :options="providerOptions" placeholder="全部供应商" class="w-40" />
        <WSelect
          v-model="statusFilter"
          :options="[{ value: 'healthy', label: '仅正常' }, { value: 'unhealthy', label: '仅异常' }]"
          placeholder="全部状态"
          class="w-32"
        />
        <span class="ml-auto text-small text-faint">健康 <span class="mono text-live">{{ summary.healthy }}</span> / {{ summary.total }}</span>
      </div>

      <WCard flush>
        <WTable :columns="cols" :rows="filtered" min-width="1040px">
          <template #cell-provider="{ row }">
            <WTag :tone="providerMeta(row.provider).tone">{{ providerMeta(row.provider).label }}</WTag>
          </template>
          <template #cell-account="{ row }">
            <div class="font-medium text-ink">{{ label(row) }}</div>
            <div class="mono text-micro text-faint">{{ row.id.slice(0, 16) }}<span v-if="row.raw.source"> · {{ row.raw.source }}</span></div>
          </template>
          <template #cell-site="{ row }">
            <span class="text-muted">{{ siteLabel(row.raw.site || row.raw.region) || '—' }}</span>
          </template>
          <template #cell-status="{ row }">
            <span class="inline-flex items-center gap-1.5">
              <WLed :tone="statusOf(row).tone" :pulse="statusOf(row).tone === 'live'" />
              <span class="text-small" :class="statusTextClass(row)">{{ statusOf(row).text }}</span>
            </span>
            <div v-if="row.raw.failure_count" class="text-micro text-faint">失败 {{ row.raw.failure_count }} 次</div>
          </template>
          <template #cell-credits="{ row }">
              <span v-if="row.raw.quota_refresh_failed" class="mr-1 text-micro text-warn" :title="row.raw.quota_updated_at ? '最后成功：' + dt(row.raw.quota_updated_at) : '尚未成功获取额度'">刷新失败</span>
              <span v-else-if="row.raw.quota_stale" class="mr-1 text-micro text-faint">缓存过期</span>
            <template v-if="row.raw.credits_remaining != null">
              <span class="mono text-ink">{{ fmtCredits(row.raw.credits_remaining) }}</span>
              <span v-if="row.raw.credits_total != null" class="mono text-faint"> / {{ fmtCredits(row.raw.credits_total) }}</span>
              <span v-if="row.raw.plan" class="ml-1 text-micro text-faint">{{ row.raw.plan }}</span>
            </template>
            <span v-else class="text-faint">—</span>
          </template>
          <template #cell-expire="{ row }">
            <template v-if="row.raw.credits_expire_at || pkgsOf(row).length">
              <button
                type="button"
                class="inline-flex items-center gap-1 rounded text-muted hover:text-ink"
                :class="{ 'cursor-help': pkgsOf(row).length, 'cursor-default': !pkgsOf(row).length }"
                :title="pkgsOf(row).length ? '查看积分构成明细' : dt(row.raw.credits_expire_at, 'YYYY-MM-DD HH:mm')"
                @mouseenter="openPkgs(row, $event)"
                @focus="openPkgs(row, $event)"
                @click="openPkgs(row, $event)"
                @mouseleave="closePkgs"
                @blur="closePkgs"
              >
                <span class="text-muted">{{ row.raw.credits_expire_at ? rel(row.raw.credits_expire_at) : fmtInt(row.raw.credits_remaining) + ' 积分' }}</span>
                <WIcon v-if="pkgsOf(row).length" name="clock" :size="13" class="text-faint" />
              </button>
            </template>
            <span v-else class="text-faint">—</span>
          </template>
          <template #cell-checkin="{ row }">
            <button
              type="button"
              class="inline-flex items-center gap-1.5 rounded focus:outline-none"
              @mouseenter="openCalendar(row, $event)"
              @focus="openCalendar(row, $event)"
              @click="openCalendar(row, $event)"
              @mouseleave="closeCalendar"
              @blur="closeCalendar"
              title="悬停查看 35 天签到记录"
            >
              <WTag v-if="row.raw.checkin_today" tone="live" dot>今日已签</WTag>
              <WTag v-else-if="hasCheckin(row)" tone="warn" dot>未签</WTag>
              <span v-else class="text-faint">—</span>
              <div v-if="row.raw.streak_days" class="text-micro text-faint">连签 {{ row.raw.streak_days }} 天</div>
            </button>
          </template>
          <template #cell-ctrl="{ row }">
            <input
              v-if="row.provider === 'workbuddy'"
              type="number"
              :value="row.raw.priority ?? 0"
              class="mono h-8 w-16 rounded-lg border border-line bg-bg/40 px-2 text-center text-small text-ink focus:border-brand focus:outline-none"
              @change="savePriority(row, +($event.target as HTMLInputElement).value)"
              title="优先级：越大越优先被选中"
            />
            <WButton v-else-if="row.raw.active" size="sm" variant="subtle" disabled>已激活</WButton>
            <WButton v-else-if="row.provider === 'qoder' && row.raw.source !== 'local'" size="sm" variant="ghost" @click="activate(row)">设为激活</WButton>
            <span v-else class="text-faint">—</span>
          </template>
          <template #cell-actions="{ row }">
            <div class="flex items-center justify-end gap-1.5">
              <template v-if="isLocalQoder(row)">
                <WTag tone="muted" title="本机自动探测；隐藏不会删除桌面端凭据">本机自动探测</WTag>
                <WButton size="sm" variant="danger" @click="removeAccount(row)">隐藏</WButton>
              </template>
              <template v-else>
                <WButton size="sm" variant="subtle" @click="openRename(row)">{{ row.provider === 'workbuddy' ? '别名' : '重命名' }}</WButton>
                <WToggle v-if="row.provider === 'workbuddy'" :model-value="row.raw.enabled" @update:model-value="toggleEnabled(row)" />
                <WButton size="sm" variant="danger" @click="removeAccount(row)">删除</WButton>
              </template>
            </div>
          </template>
          <template #empty>
            <WEmpty title="还没有账号" hint="添加一个上游账号，网关才能开始转发请求。">
              <WButton variant="primary" @click="openAdd"><WIcon name="plus" :size="16" /> 添加账号</WButton>
            </WEmpty>
          </template>
        </WTable>
      </WCard>

      <!-- 配置型供应商（OpenCode）：密钥按层级配置，此处可编辑并热重载 -->
      <WCard
        v-for="cp in configProviders"
        :key="cp.name"
        :title="`${cp.display_name || providerMeta(cp.name).label} 密钥层级`"
        sub="OpenCode 无账号概念，凭证按 Zen / Go 层级配置；也可启用匿名层调用免费模型。"
        class="mt-4"
      >
        <template #actions>
          <WButton size="sm" variant="primary" @click="openConfig(cp.name)"><WIcon name="keys" :size="15" /> 编辑配置</WButton>
        </template>
        <div v-if="tiersOf(cp.name).length" class="grid gap-3 sm:grid-cols-3">
          <div v-for="t in tiersOf(cp.name)" :key="t.raw.id" class="glass rounded-lg p-3">
            <div class="flex items-center justify-between">
              <span class="font-medium text-ink">{{ t.raw.label }}</span>
              <WTag tone="warn">{{ t.raw.tier }}</WTag>
            </div>
            <div class="mono mt-1.5 text-lg font-semibold text-ink">{{ t.raw.key_count }}<span class="ml-1 text-micro text-faint">个密钥</span></div>
          </div>
        </div>
        <p v-else class="text-small text-muted">
          {{ cp.ready ? '尚未配置任何密钥。' : (cp.notes || '未配置。点击右上角「编辑配置」添加 Zen / Go 密钥或启用匿名层。') }}
        </p>
      </WCard>
    </template>

    <!-- 重命名 -->
    <WModal v-model:open="renameOpen" size="sm" :title="renameTarget?.provider === 'workbuddy' ? '设置别名' : '重命名账号'">
      <p class="mb-2 text-micro text-faint">留空则恢复默认显示名（昵称 / ID 短码）。</p>
      <WInput v-model="renameValue" placeholder="输入显示名" @enter="submitRename" />
      <template #footer>
        <WButton variant="subtle" @click="renameOpen = false">取消</WButton>
        <WButton variant="primary" @click="submitRename">保存</WButton>
      </template>
    </WModal>

    <!-- 添加账号 -->
    <WModal v-model:open="addOpen" title="添加账号" @update:open="(v) => !v && stopPoll()">
      <label class="mb-1.5 block text-small text-muted">供应商</label>
      <WSelect v-model="addProvider" :options="addProviderOptions" class="mb-4" @update:model-value="onAddProviderChange" />

      <!-- 方式切换（仅 workbuddy 同时支持上传与扫码） -->
      <div v-if="caps(addProvider).includes('upload') && caps(addProvider).includes('oauth')" class="mb-4 flex gap-2">
        <WButton size="sm" :variant="addMethod === 'oauth' ? 'primary' : 'subtle'" @click="addMethod = 'oauth'">扫码登录</WButton>
        <WButton size="sm" :variant="addMethod === 'upload' ? 'primary' : 'subtle'" @click="addMethod = 'upload'">上传凭据</WButton>
      </div>

      <!-- 扫码登录 -->
      <template v-if="addMethod === 'oauth' && caps(addProvider).includes('oauth')">
        <div v-if="addProvider === 'workbuddy'" class="mb-3">
          <label class="mb-1.5 block text-small text-muted">站点</label>
          <WSelect v-model="oauthChoice.site" :options="oauthSites.map((s) => ({ value: s, label: s }))" />
        </div>
        <div v-for="o in oauthOptions" :key="o.key" class="mb-3">
          <label class="mb-1.5 block text-small text-muted">{{ o.label }}</label>
          <WSelect v-model="oauthChoice[o.key]" :options="o.values" />
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
                {{ dt(p.expire_at, pkgExpiring(p) ? 'YYYY-MM-DD HH:mm' : 'YYYY-MM-DD') }}
              </span>
              <span v-else-if="p.reset_time" class="ml-auto mono whitespace-nowrap text-faint">重置：{{ p.reset_time }}</span>
            </div>
          </div>
        </div>
        <div class="mt-3 border-t border-line pt-2 text-micro text-faint">已用完的批次不计入最早到期时间</div>
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
