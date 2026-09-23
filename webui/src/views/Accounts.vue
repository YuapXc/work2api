<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { message } from 'ant-design-vue'
import { CheckCircleOutlined } from '@ant-design/icons-vue'
import { api } from '@/api/client'
import type { AccountInfo, Settings } from '@/types'
import { accountLabel as accountLabelOf, buildLabelMap } from '@/utils/accountLabel'
import dayjs from 'dayjs'

const REFRESH_MS = 20000 // 每 20s 自动刷新
let timer: ReturnType<typeof setInterval> | null = null
let nowTimer: ReturnType<typeof setInterval> | null = null

const accounts = ref<AccountInfo[]>([])
const loading = ref(false)

// uid → 显示名（含重名消歧）。账号列表变化时自动重算。
const labelMap = computed(() => buildLabelMap(accounts.value))

// 当前时间（秒，每秒刷新），用于冷却倒计时与状态实时判定
const now = ref(Date.now() / 1000)

// 账号状态细分：已禁用 / 冷却中 / 余额不足 / 健康
function statusOf(record: AccountInfo): { label: string; color: string; countdown?: string } {
  if (!record.enabled) return { label: '已禁用', color: 'default' }
  if (record.cooldown_until && record.cooldown_until > now.value) {
    const sec = Math.ceil(record.cooldown_until - now.value)
    const countdown = sec < 60 ? `${sec} 秒` : `${Math.ceil(sec / 60)} 分钟`
    return { label: '冷却中', color: 'orange', countdown }
  }
  if (record.credits_remaining !== null && record.credits_remaining !== undefined && record.credits_remaining <= 0) {
    return { label: '余额不足', color: 'red' }
  }
  return { label: '健康', color: 'green' }
}

// 上传 auth 文件
const fileInput = ref<HTMLInputElement | null>(null)
const uploading = ref(false)

// 扫码/授权登录（对齐 work2api Go 后端：begin / poll，直接给授权链接，不渲染二维码）
const qrOpen = ref(false)
const qrLoading = ref(false)
const qrSite = ref('cn')
const oauthSites = ref<string[]>(['cn', 'intl', 'intl-codebuddy'])
const qrAuthUrl = ref('')
let pollTimer: ReturnType<typeof setInterval> | null = null

// 站点标识 → 中文label
const SITE_LABELS: Record<string, string> = {
  cn: '国内 CodeBuddy',
  intl: '国际 WorkBuddy',
  'intl-codebuddy': '国际 CodeBuddy',
}
function siteLabel(s: string): string {
  return SITE_LABELS[s] || s
}

// 自动签到设置
const settingsOpen = ref(false)
const settingsSaving = ref(false)
const settings = ref<Settings>({ checkin_hours: '9,21', credit_refresh_min: '30', model_refresh_hour: '6', model_ttl_min: '60', aa_refresh_hour: '7', keepalive_hour: '22' })
const creditMinutes = ref(30)
const modelRefreshHour = ref(6)
const modelTtlMin = ref(60)
const aaRefreshHour = ref(7)
const keepaliveHour = ref(22)
const aaKey = ref('')
const aaKeyMasked = ref('')
const aaEnabled = ref(false)
const aaClear = ref(false)
const keepaliveEnabled = ref(true)
// 模型别名与积分预警
const modelAliases = ref('')
const alertEnabled = ref(false)
const alertWebhook = ref('')
const alertThreshold = ref(20)
const alertExpiryDays = ref(7)

// 国际站 WorkBuddy 无签到活动，无需签到（后端也会跳过），不计入待签到提示
const anyUnchecked = computed(() => accounts.value.some((a) =>
  a.auth_mode !== 'pat' && a.site !== 'international' && !a.checkin_today))

async function load() {
  loading.value = true
  try {
    const res = await api.accounts()
    accounts.value = res.accounts ?? []
  } finally {
    loading.value = false
  }
}

async function toggle(acc: AccountInfo) {
  try {
    await api.setEnabled(acc.uid, !acc.enabled)
    message.success(`${acc.enabled ? '已停用' : '已启用'}`)
    load()
  } catch { /* 错误已在拦截器提示 */ }
}

async function refreshCredits() {
  try {
    const res = await api.refreshCredits()
    accounts.value = res.accounts ?? []
    message.success('额度已刷新')
  } catch { /* 拦截器已提示 */ }
}

async function checkin() {
  try {
    const res = await api.checkin()
    const parts = (res.results || []).map((r) => {
      const state = r.already ? '已签到' : r.ok ? '成功' : '失败'
      // 用显示名而非 8 位 uid 短码——多账号下十六进制串根本认不出是谁
      const name = labelMap.value[r.uid] || r.uid.slice(0, 8)
      return `${name} ${state}`
    })
    const skippedCount = (res.skipped || []).length
    const skipMsg = skippedCount ? `（跳过 ${skippedCount} 个已签到账号）` : ''
    message.success(`签到结果：${parts.join(', ') || '无账号'}${skipMsg}`)
    load()
  } catch { /* 拦截器已提示 */ }
}

// ---------- 上传 auth 文件 ----------
function pickFile() {
  fileInput.value?.click()
}

async function onFileChange(e: Event) {
  const input = e.target as HTMLInputElement
  const file = input.files?.[0]
  if (!file) return
  uploading.value = true
  try {
    const res = await api.uploadAuth(file)
    message.success(`已导入账号 ${res.account.uid.slice(0, 8)}…`)
    load()
  } catch { /* 拦截器已提示 */ } finally {
    uploading.value = false
    input.value = ''
  }
}

// ---------- 授权登录 ----------
async function openQr() {
  const site = qrSite.value
  qrOpen.value = true
  qrLoading.value = true
  qrAuthUrl.value = ''
  stopPolling()
  try {
    const res = await api.oauthBegin(site)
    qrAuthUrl.value = res.authUrl
    // 直接在浏览器新标签打开授权页（无二维码），同时页面上保留可点击链接
    window.open(res.authUrl, '_blank', 'noopener,noreferrer')
    startPolling(res.state, site)
  } catch {
    qrOpen.value = false
  } finally {
    qrLoading.value = false
  }
}

function startPolling(state: string, site: string) {
  stopPolling()
  pollTimer = setInterval(async () => {
    try {
      const res = await api.oauthPoll(state, site)
      if (res.status === 'ready') {
        stopPolling()
        qrOpen.value = false
        message.success('授权登录成功')
        load()
      }
    } catch {
      // 轮询出错（如 state 失效）：停止轮询，避免无限打后端
      stopPolling()
    }
  }, 2000)
}

function stopPolling() {
  if (pollTimer) {
    clearInterval(pollTimer)
    pollTimer = null
  }
}

function closeQr() {
  stopPolling()
  qrOpen.value = false
}

function onAAClearChange() {
  if (aaClear.value) aaKey.value = ''
}

// ---------- 自动签到设置 ----------
const checkinErr = ref('')

function validateCheckinHours(s: string): boolean {
  const parts = s.split(',').map((p) => p.trim()).filter((p) => p !== '')
  if (parts.length === 0) return false
  for (const p of parts) {
    const n = Number(p)
    if (!/^\d{1,2}$/.test(p) || n < 0 || n > 23) return false
  }
  return true
}

async function openSettings() {
  settingsOpen.value = true
  checkinErr.value = ''
  try {
    const res = await api.getSettings()
    settings.value = res
    creditMinutes.value = parseInt(res.credit_refresh_min, 10) || 30
    modelRefreshHour.value = parseInt(res.model_refresh_hour, 10) || 6
    modelTtlMin.value = parseInt(res.model_ttl_min as any, 10) || 60
    aaRefreshHour.value = parseInt(res.aa_refresh_hour, 10) || 7
    keepaliveHour.value = parseInt(res.keepalive_hour, 10) || 22
    aaKeyMasked.value = res.aa_api_key_masked || ''
    aaEnabled.value = !!res.aa_enabled
    aaKey.value = ''
    keepaliveEnabled.value = res.keepalive_enabled !== '0'
    modelAliases.value = res.model_aliases || ''
    alertEnabled.value = res.alert_enabled === '1'
    alertWebhook.value = res.alert_webhook || ''
    alertThreshold.value = parseFloat(res.alert_threshold_percent || '20') || 20
    alertExpiryDays.value = parseFloat(res.alert_expiry_days || '7') || 7
  } catch { /* 拦截器已提示 */ }
}

async function saveSettings() {
  const hrs = settings.value.checkin_hours
  if (!validateCheckinHours(hrs)) {
    checkinErr.value = '格式无效：请输入 0-23 之间的整数，逗号分隔（如 9,21）'
    return
  }
  checkinErr.value = ''
  settingsSaving.value = true
  const payload: Partial<Settings> = {
    checkin_hours: settings.value.checkin_hours,
    credit_refresh_min: String(creditMinutes.value),
    model_refresh_hour: String(modelRefreshHour.value),
    model_ttl_min: String(modelTtlMin.value),
    aa_refresh_hour: String(aaRefreshHour.value),
    keepalive_hour: String(keepaliveHour.value),
    keepalive_enabled: keepaliveEnabled.value ? '1' : '0',
    model_aliases: modelAliases.value,
    alert_enabled: alertEnabled.value ? '1' : '0',
    alert_webhook: alertWebhook.value.trim(),
    alert_threshold_percent: String(alertThreshold.value),
    alert_expiry_days: String(alertExpiryDays.value),
  }
  if (aaKey.value) {
    payload.aa_api_key = aaKey.value.trim()
  } else if (aaClear.value) {
    payload.aa_api_key = ''
    payload.clear_aa_api_key = true
  }
  try {
    await api.saveSettings(payload)
    message.success('设置已保存，将按新配置生效')
    settingsOpen.value = false
  } catch { /* 拦截器已提示 */ } finally {
    settingsSaving.value = false
  }
}

// ---------- 删除账号 ----------
const deletingUid = ref('')
async function removeAccount(acc: AccountInfo) {
  deletingUid.value = acc.uid
  try {
    await api.deleteAccount(acc.uid)
    message.success(`已删除账号 ${short(acc.uid)}`)
    load()
  } catch { /* 拦截器已提示 */ } finally {
    deletingUid.value = ''
  }
}

function short(s: string | null | undefined, n = 8) {
  const str = s ? String(s) : ''
  return str.length > n ? str.slice(0, n) + '…' : (str || '-')
}

// 积分到期倒计时（秒时间戳 → "x 天后 / 今天到期 / 已于 MM-DD 到期 / -"）
function expiryText(ts: number | null | undefined): string {
  if (!ts) return '-'
  const diffDays = (ts - Date.now() / 1000) / 86400
  if (diffDays <= 0) return `已于 ${dayjs(ts * 1000).format('MM-DD')} 到期`
  if (diffDays < 1) return '今天到期'
  if (diffDays < 30) return `${Math.ceil(diffDays)} 天后`
  return `${Math.round(diffDays / 30)} 个月后`
}
function expiryColor(ts: number | null | undefined): string {
  if (!ts) return ''
  const diffDays = (ts - Date.now() / 1000) / 86400
  if (diffDays <= 3) return '#ef4444'
  if (diffDays <= 7) return '#f59e0b'
  return '#4ade80'
}

// 积分构成明细（悬浮弹层用）：与官方控制台"积分明细"同口径
function fmtNum(n: number | null | undefined): string {
  return Number(n ?? 0).toLocaleString('zh-CN', { maximumFractionDigits: 1 })
}
function remainPct(remain: number, total: number): number {
  if (!total) return 0
  return Math.max(0, Math.min(100, Math.round((remain / total) * 100)))
}
function fmtDateTime(ts: number | null | undefined): string {
  if (!ts) return '-'
  return dayjs(ts * 1000).format('YYYY/MM/DD HH:mm:ss')
}

// 优先级编辑
const editingPriority = ref<Record<string, number>>({})
async function savePriority(acc: AccountInfo) {
  const val = Math.max(0, Math.min(100, Math.round(editingPriority.value[acc.uid] ?? acc.priority ?? 0)))
  editingPriority.value[acc.uid] = val
  try {
    await api.setPriority(acc.uid, val)
    message.success(`已设置优先级 ${val}`)
    load()
  } catch { /* 拦截器已提示 */ }
}

// ---------------- 别名编辑 ----------------
// 同一时刻只允许一行处于编辑态，避免多行 input 同时展开显得杂乱
const editingAliasUid = ref<string | null>(null)
const editingAlias = ref<Record<string, string>>({})

function startEditAlias(acc: AccountInfo) {
  editingAliasUid.value = acc.uid
  editingAlias.value[acc.uid] = acc.alias || ''
}

async function saveAlias(acc: AccountInfo) {
  const val = (editingAlias.value[acc.uid] ?? '').trim()
  try {
    await api.setAccountAlias(acc.uid, val)
    message.success(val ? `别名已设为「${val}」` : '别名已清除，恢复默认显示')
    editingAliasUid.value = null
    load()
  } catch { /* 拦截器已提示 */ }
}

/** 悬浮提示：说清当前显示名从哪来，以及原始 uid/昵称是什么。 */
function aliasTooltip(acc: AccountInfo) {
  const lines: string[] = []
  if (acc.alias) lines.push(`别名：${acc.alias}`)
  if (acc.nickname) lines.push(`上游昵称：${acc.nickname}`)
  lines.push(`UID：${acc.uid}`)
  if (!acc.alias) lines.push('（点击可设置别名）')
  return lines.join('\n')
}

/** 账号的显示名（含重名消歧）——多账号场景下统一称呼方式。 */
function accountLabel(acc: AccountInfo): string {
  return labelMap.value[acc.uid] || accountLabelOf(acc)
}

onMounted(() => {
  load()
  timer = setInterval(load, REFRESH_MS)
  // 每秒刷新 now，驱动冷却倒计时与状态实时变化
  nowTimer = setInterval(() => { now.value = Date.now() / 1000 }, 1000)
  // 拉取后端支持的授权站点（失败则用默认列表兜底）
  api.oauthSites().then((res) => {
    if (res?.sites?.length) {
      oauthSites.value = res.sites
      if (!res.sites.includes(qrSite.value)) qrSite.value = res.sites[0]
    }
  }).catch(() => { /* 用默认列表兜底 */ })
})

onUnmounted(() => {
  if (timer) clearInterval(timer)
  if (nowTimer) clearInterval(nowTimer)
  // 授权轮询也要停：否则弹窗开着切页后每 2s 继续打后端
  stopPolling()
})
</script>

<template>
  <div style="width: 100%">
    <div class="view-header">
      <div>
        <div class="vh-title">账号</div>
        <div class="vh-meta">
          <span class="led" :class="accounts.some((a) => a.healthy && a.enabled) ? 'live' : 'fault'"></span>
          <span class="mono">{{ accounts.filter((a) => a.healthy && a.enabled).length }}</span>/<span class="mono">{{ accounts.length }}</span> 健康 · 签到与设置
        </div>
      </div>
      <div class="vh-actions">
        <a-button type="primary" :loading="uploading" @click="pickFile">上传 auth 文件</a-button>
      </div>
    </div>

    <div style="margin-bottom: 14px; display: flex; gap: 8px; flex-wrap: wrap; align-items: center">
      <a-select v-model:value="qrSite" style="width: 150px">
        <a-select-option v-for="s in oauthSites" :key="s" :value="s">{{ siteLabel(s) }}</a-select-option>
      </a-select>
      <a-button :loading="qrLoading" @click="openQr">扫码登录</a-button>
      <a-button :disabled="!anyUnchecked" @click="checkin">手动签到</a-button>
      <span style="width: 1px; height: 24px; background: var(--line)"></span>
      <a-button @click="refreshCredits">刷新额度</a-button>
      <a-button @click="load">刷新列表</a-button>
      <a-button @click="openSettings" style="margin-left: 4px">自动签到设置</a-button>
      <span v-if="!anyUnchecked && accounts.length" style="margin-left: auto; display: inline-flex; align-items: center; gap: 5px; color: var(--live); font-size: 12px; background: rgba(53,208,165,0.1); padding: 3px 10px; border-radius: 6px; border: 1px solid rgba(53,208,165,0.3)">
        <CheckCircleOutlined />今日已全部签到
      </span>
    </div>
    <input
      ref="fileInput"
      type="file"
      accept=".info,.json"
      style="display: none"
      @change="onFileChange"
    />

    <a-alert
      v-if="!loading && accounts.length === 0"
      type="info"
      show-icon
      style="margin-bottom: 12px"
      message="尚未配置任何账号"
      description="WorkBuddy/CodeBuddy 可授权登录或导入 auth 文件。所有账号共用同一个 API 地址并按模型自动路由。"
    />

    <a-card title="账号列表">
      <!-- 列宽合计 ~1210px，窄窗口下容器放不下：开横向滚动并固定操作列，
           否则操作按钮会被卡片右缘裁掉（无滚动条可拉） -->
      <a-table
        :data-source="accounts"
        :loading="loading"
        row-key="uid"
        :pagination="false"
        :scroll="{ x: 1210 }"
        table-layout="fixed"
      >
        <a-table-column title="别名" key="alias" :width="230">
          <template #default="{ record }">
            <!-- inline 编辑：与「优先级」列同款范式，点开即改、无需弹窗 -->
            <a-input
              v-if="editingAliasUid === record.uid"
              :value="editingAlias[record.uid] ?? record.alias ?? ''"
              size="small"
              :maxlength="40"
              placeholder="留空则恢复默认显示"
              style="width: 140px"
              @change="(e: any) => { editingAlias[record.uid] = e.target.value }"
              @press-enter="saveAlias(record)"
            />
            <template v-else>
              <a-tooltip :title="aliasTooltip(record)">
                <span
                  style="cursor: pointer; border-bottom: 1px dashed rgba(255,255,255,0.25)"
                  :style="{ color: record.alias ? '#e6edf7' : '#8a94a6' }"
                  @click="startEditAlias(record)"
                >{{ accountLabel(record) }}</span>
              </a-tooltip>
            </template>
            <a-button
              v-if="editingAliasUid === record.uid"
              size="small" type="link" style="padding: 0 4px"
              @click="saveAlias(record)"
            >保存</a-button>
            <a-button
              v-else
              size="small" type="link" style="padding: 0 4px"
              @click="startEditAlias(record)"
            >改名</a-button>
          </template>
        </a-table-column>
        <a-table-column title="站点" key="site" :width="80">
          <template #default="{ record }">
            <!-- site 是内部标识（domestic/international），必须映射成中文再显示 -->
            <a-tag v-if="record.site === 'domestic'" color="geekblue">国内</a-tag>
            <a-tag v-else-if="record.site === 'international'" color="purple">国际</a-tag>
            <a-tag v-else-if="record.site === 'qoder-cn'" color="cyan">Qoder 国内</a-tag>
            <a-tag v-else-if="record.site === 'qoder-global'" color="blue">Qoder 国际</a-tag>
            <a-tag v-else color="default">{{ record.site || '未知' }}</a-tag>
          </template>
        </a-table-column>
        <a-table-column title="UID" key="uid" :width="150">
          <template #default="{ record }"><a-tooltip :title="record.uid"><code>{{ short(record.uid, 24) }}</code></a-tooltip></template>
        </a-table-column>
        <a-table-column title="来源" key="source" :width="130">
          <template #default="{ record }">
            <a-tag v-if="record.source === 'project'" color="purple">项目 auths</a-tag>
            <a-tag v-else-if="record.source === 'local'" color="geekblue">本机 CodeBuddy</a-tag>
            <a-tag v-else-if="record.source === 'qoder'" color="cyan">Qoder {{ record.auth_mode === 'pat' ? 'PAT' : 'OAuth' }}</a-tag>
            <a-tag v-else color="default">未知</a-tag>
          </template>
        </a-table-column>
        <a-table-column title="状态" key="healthy" :width="130">
          <template #default="{ record }">
            <template v-if="statusOf(record).label === '冷却中'">
              <a-tooltip :title="`${statusOf(record).countdown}后恢复`">
                <a-tag color="orange">{{ statusOf(record).label }}</a-tag>
              </a-tooltip>
              <span style="font-size: 12px; color: #d97706">{{ statusOf(record).countdown }}后恢复</span>
            </template>
            <!-- 已禁用时展示原因：用户需要知道是手动停用还是保活失败，后者要重新登录 -->
            <a-tooltip v-else-if="!record.enabled && record.disabled_reason" :title="record.disabled_reason">
              <a-tag :color="statusOf(record).color">{{ statusOf(record).label }}</a-tag>
            </a-tooltip>
            <a-tag v-else :color="statusOf(record).color">{{ statusOf(record).label }}</a-tag>
          </template>
        </a-table-column>
        <a-table-column title="今日签到" key="checkin" :width="90">
          <template #default="{ record }">
            <a-tag :color="record.auth_mode === 'pat' ? 'default' : record.site === 'international' ? 'default' : record.checkin_today ? 'green' : 'default'">
              {{ record.auth_mode === 'pat' ? '需 OAuth' : record.site === 'international' ? '无需签到' : record.checkin_today ? '已签到' : '未签到' }}
            </a-tag>
          </template>
        </a-table-column>
        <a-table-column title="额度剩余" data-index="credits_remaining" key="credits_remaining" :width="90">
          <template #default="{ record }">{{ record.credits_remaining ?? '-' }}</template>
        </a-table-column>
        <a-table-column title="额度总量" data-index="credits_total" key="credits_total" :width="90">
          <template #default="{ record }">{{ record.credits_total ?? '-' }}</template>
        </a-table-column>
        <a-table-column title="积分到期" key="expiry" :width="110">
          <template #default="{ record }">
            <!-- 有积分构成明细时悬浮展示（与官方控制台"积分明细"同口径） -->
            <a-popover
              v-if="record.credit_packages && record.credit_packages.length"
              trigger="hover"
              placement="left"
            >
              <template #content>
                <div style="min-width: 300px">
                  <div style="font-weight: 700; color: #e6edf7; margin-bottom: 10px">积分构成</div>
                  <div v-for="p in record.credit_packages" :key="p.name" style="margin-bottom: 12px">
                    <div style="display: flex; justify-content: space-between; gap: 12px; font-size: 12px; color: #cdd6e8">
                      <span>{{ p.name }}</span>
                      <span style="color: #8a94a6; white-space: nowrap">剩余 {{ remainPct(p.remain, p.total) }}%</span>
                    </div>
                    <a-progress
                      :percent="remainPct(p.remain, p.total)"
                      :show-info="false"
                      size="small"
                      stroke-color="#63b3ed"
                      trail-color="rgba(255,255,255,0.08)"
                      style="margin: 2px 0 4px"
                    />
                    <div style="font-size: 12px; color: #8a94a6">
                      已使用 {{ fmtNum(p.used) }} / {{ fmtNum(p.total) }}
                      <template v-if="p.expire_at"> · 最早到期 {{ fmtDateTime(p.expire_at) }}</template>
                    </div>
                  </div>
                  <div style="font-size: 11px; color: #5c6a8a">已用完的批次不计入最早到期时间</div>
                </div>
              </template>
              <span
                v-if="record.credits_expire_at"
                :style="{ color: expiryColor(record.credits_expire_at), fontWeight: 600, cursor: 'help', borderBottom: '1px dashed rgba(255,255,255,0.25)' }"
              >{{ expiryText(record.credits_expire_at) }}</span>
              <span v-else style="cursor: help; border-bottom: 1px dashed rgba(255,255,255,0.25)">
                {{ fmtNum(record.credits_remaining ?? 0) }} 积分
              </span>
            </a-popover>
            <a-tooltip v-else-if="record.credits_expire_at" :title="fmtDateTime(record.credits_expire_at)">
              <span :style="{ color: expiryColor(record.credits_expire_at), fontWeight: 600 }">{{ expiryText(record.credits_expire_at) }}</span>
            </a-tooltip>
            <span v-else>-</span>
          </template>
        </a-table-column>
        <a-table-column title="优先级" key="priority" :width="150">
          <template #default="{ record }">
            <a-input-number
              :value="editingPriority[record.uid] ?? record.priority ?? 0"
              :min="0"
              :max="100"
              :step="1"
              size="small"
              style="width: 90px"
              @change="(v: number | null) => { editingPriority[record.uid] = v ?? 0 }"
            />
            <a-button size="small" type="link" style="padding: 0 4px" @click="savePriority(record)">保存</a-button>
          </template>
        </a-table-column>
        <a-table-column title="失败数" data-index="failure_count" key="failure_count" :width="70" />
        <a-table-column title="操作" key="action" :width="150" fixed="right">
          <template #default="{ record }">
            <a-space>
              <a-button size="small" @click="toggle(record)">
                {{ record.enabled ? '停用' : '启用' }}
              </a-button>
              <a-popconfirm
                title="确认删除该账号？"
                ok-text="删除"
                cancel-text="取消"
                @confirm="removeAccount(record)"
              >
                <a-button size="small" danger :loading="deletingUid === record.uid">删除</a-button>
              </a-popconfirm>
            </a-space>
          </template>
        </a-table-column>
      </a-table>
    </a-card>

    <!-- 授权登录弹窗（链接式，不渲染二维码） -->
    <a-modal
      :open="qrOpen"
      title="授权登录"
      :footer="null"
      :closable="true"
      @cancel="closeQr"
    >
      <div style="text-align: center; padding: 12px 0">
        <a-spin :spinning="qrLoading">
          <div v-if="qrAuthUrl" style="padding: 8px 0">
            <a :href="qrAuthUrl" target="_blank" rel="noopener" style="font-size: 15px">
              在浏览器中打开登录
            </a>
            <p style="margin-top: 12px; color: #8a94a6; font-size: 12px">
              已尝试自动打开授权页；若未弹出，请点击上方链接。授权完成后本页会自动完成添加。
            </p>
          </div>
          <div v-else-if="!qrLoading" style="color: #999">正在获取授权链接…</div>
        </a-spin>
      </div>
    </a-modal>

    <!-- 自动签到设置弹窗 -->
    <a-modal
      :open="settingsOpen"
      title="自动签到设置"
      :confirm-loading="settingsSaving"
      ok-text="保存"
      cancel-text="取消"
      width="560px"
      @ok="saveSettings"
      @cancel="settingsOpen = false"
    >
      <!-- 表单项较多（签到/额度/模型/AA/保活/别名/预警），限高滚动，
           否则「保存」按钮会滚出视野，用户改完底部设置还得滚回顶部才能提交 -->
      <a-form layout="vertical" style="max-height: 62vh; overflow-y: auto; padding-right: 6px">
        <a-form-item
          label="每日自动签到时间（小时，逗号分隔，0-23）"
          :validate-status="checkinErr ? 'error' : ''"
          :help="checkinErr || undefined"
        >
          <a-input v-model:value="settings.checkin_hours" placeholder="例如 9,21" @change="checkinErr=''" />
          <div style="color: #999; font-size: 12px; margin-top: 4px">
            到达设定的小时且当日未签到即自动签到一次，例如 9,21 表示每天 9 点和 21 点各检查一次
          </div>
        </a-form-item>
        <a-form-item label="额度刷新间隔（分钟，1-1440）">
          <a-input-number v-model:value="creditMinutes" :min="1" :max="1440" style="width: 100%" />
        </a-form-item>
        <a-form-item label="每日模型目录刷新时间（小时，0-23）">
          <a-input-number v-model:value="modelRefreshHour" :min="0" :max="23" style="width: 100%" />
          <div style="color: #999; font-size: 12px; margin-top: 4px">
            每天该小时自动从上游拉取可用模型列表（供 /v1/models 与 WebUI 展示）
          </div>
        </a-form-item>
        <a-form-item label="模型缓存 TTL（分钟，1-1440）">
          <a-input-number v-model:value="modelTtlMin" :min="1" :max="1440" style="width: 100%" />
          <div style="color: #999; font-size: 12px; margin-top: 4px">
            推理端点 /v1/models 在缓存超过该时长后才会惰性刷新（默认 60 分钟）。值越大上游调用越少、但模型列表越旧；WebUI 展示不受此影响（由上方定时刷新控制）。
          </div>
        </a-form-item>
        <a-form-item label="每日 AA 评测刷新时间（小时，0-23）">
          <a-input-number v-model:value="aaRefreshHour" :min="0" :max="23" style="width: 100%" />
          <div style="color: #999; font-size: 12px; margin-top: 4px">
            每天该小时自动刷新 Artificial Analysis 评测数据（intelligence / coding / agentic 指数）。需先配置 AA API Key。
          </div>
        </a-form-item>
        <a-form-item label="每日 token 保活">
          <a-space direction="vertical" style="width: 100%">
            <a-switch v-model:checked="keepaliveEnabled" checked-children="开启" un-checked-children="关闭" />
            <div style="color: #999; font-size: 12px">
              开启后每天定时自动刷新账号 token（防止长期不用过期）。关闭则完全不保活。
            </div>
          </a-space>
        </a-form-item>
        <a-form-item label="每日 token 保活时间（小时，0-23）">
          <a-input-number v-model:value="keepaliveHour" :min="0" :max="23" style="width: 100%" :disabled="!keepaliveEnabled" />
          <div style="color: #999; font-size: 12px; margin-top: 4px">
            每天该小时自动刷新账号 token；连续多次失败（session 失效）的账号才会被自动停用
          </div>
        </a-form-item>
        <a-form-item label="Artificial Analysis API Key（可选）">
          <a-input-password
            v-model:value="aaKey"
            :placeholder="aaEnabled ? `已配置：${aaKeyMasked}（留空不修改）` : '填入 AA API Key 启用评测数据'"
            autocomplete="new-password"
            style="width: 100%"
            @change="aaClear = false"
          />
          <div style="color: #999; font-size: 12px; margin-top: 4px">
            用于获取各模型的权威评测（智能 intelligence / 编码 coding / agentic 指数）。在 artificialanalysis.ai 注册生成，免费档 1000 次/天。key 仅存于后端，不暴露给前端。
          </div>
          <a-checkbox
            v-if="aaEnabled"
            v-model:checked="aaClear"
            style="margin-top: 6px"
            @change="onAAClearChange"
          >清除已配置的 Key</a-checkbox>        </a-form-item>
        <a-form-item label="模型别名（可选）">
          <a-textarea
            v-model:value="modelAliases"
            :rows="3"
            placeholder="gpt-4o=hy3&#10;claude-sonnet=deepseek-v4-pro"
            style="width: 100%"
          />
          <div style="color: #999; font-size: 12px; margin-top: 4px">
            每行一个 <code>别名=真实模型</code>。客户端用别名调用时自动路由到真实模型，
            别名也会出现在 <code>/v1/models</code> 列表里。目标模型不存在时不生效。
          </div>
        </a-form-item>
        <a-form-item label="积分预警推送">
          <a-space direction="vertical" style="width: 100%">
            <a-switch v-model:checked="alertEnabled" checked-children="开启" un-checked-children="关闭" />
            <a-input
              v-model:value="alertWebhook"
              placeholder="webhook 地址（企微 / 飞书 / Bark）"
              :disabled="!alertEnabled"
              style="width: 100%"
            />
            <div style="display: flex; gap: 8px; align-items: center">
              <span style="color: #999; font-size: 12px">余额低于</span>
              <a-input-number v-model:value="alertThreshold" :min="1" :max="100" :disabled="!alertEnabled" style="width: 90px" addon-after="%" />
              <span style="color: #999; font-size: 12px">或积分在</span>
              <a-input-number v-model:value="alertExpiryDays" :min="1" :max="365" :disabled="!alertEnabled" style="width: 90px" addon-after="天" />
              <span style="color: #999; font-size: 12px">内过期时推送</span>
            </div>
            <div style="color: #999; font-size: 12px">
              按 URL 自动识别平台格式（含 qyapi.weixin.qq.com 为企微，open.feishu.cn 为飞书，
              其余按 Bark/通用 JSON 发送）。6 小时内不重复推送。
            </div>
          </a-space>
        </a-form-item>
      </a-form>
    </a-modal>
  </div>
</template>
