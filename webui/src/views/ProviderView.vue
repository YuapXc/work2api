<script setup lang="ts">
// 供应商钻取页：一个页面按能力（capabilities）自适应渲染。
// workbuddy 复用既有的「账号」「模型」富交互视图（放进标签页，功能零丢失）；
// qoder / opencode 用通用能力驱动表格 + 签到/额度动作，未就绪时给出配置指引。
import { ref, computed, watch, onMounted, onUnmounted } from 'vue'
import { useRoute } from 'vue-router'
import { message } from 'ant-design-vue'
import { ReloadOutlined } from '@ant-design/icons-vue'
import { api } from '@/api/client'
import type { ProviderDetail } from '@/types'
import Accounts from '@/views/Accounts.vue'
import Models from '@/views/Models.vue'

const route = useRoute()
const name = computed(() => String(route.params.name || ''))

const detail = ref<ProviderDetail | null>(null)
const loading = ref(false)
const acting = ref<'' | 'credits' | 'checkin' | 'models'>('')

const caps = computed(() => detail.value?.capabilities ?? [])
function has(c: string) { return caps.value.includes(c) }

const accounts = computed(() => detail.value?.accounts ?? [])
const models = computed(() => detail.value?.models ?? [])
const ready = computed(() => !!detail.value?.ready)
const displayName = computed(() => detail.value?.display_name || name.value)
const isWorkbuddy = computed(() => name.value === 'workbuddy')
const tab = ref('accounts')

// 签到 / 额度结果内联展示（动作后就地反馈，不只靠 toast）
const checkinResults = ref<Record<string, any>[]>([])
const quota = ref<Record<string, any> | null>(null)
const quotaMsg = ref('')

// ---------- 扫码登录（OAuth）：qoder 等支持 oauth 能力的供应商 ----------
const oauthOpen = ref(false)
const oauthBeginning = ref(false)          // 「开始登录」请求进行中
const oauthOptions = ref<Record<string, any>[]>([])
const oauthRegion = ref('cn')
const oauthLoginUrl = ref('')
const oauthStatus = ref<'idle' | 'pending' | 'ready' | 'error'>('idle')
const oauthMsg = ref('')
let oauthTimer: ReturnType<typeof setInterval> | null = null
let oauthDeadline = 0

const regionValues = computed(() => {
  const opt = (oauthOptions.value ?? []).find((o) => o?.key === 'region')
  return (opt?.values ?? []) as { value: string; label: string }[]
})

const ledClass = computed(() => {
  if (!ready.value) return 'fog'
  if (name.value === 'workbuddy') {
    return Number((detail.value?.status as any)?.healthy_count ?? 0) > 0 ? 'live' : 'fault'
  }
  return 'live'
})

let timer: ReturnType<typeof setInterval> | null = null

async function load() {
  if (!name.value) return
  loading.value = true
  try {
    const res = await api.getProvider(name.value)
    detail.value = {
      ...res,
      capabilities: res.capabilities ?? [],
      accounts: res.accounts ?? [],
      models: res.models ?? [],
      status: res.status ?? {},
    }
  } catch { /* 拦截器已提示 */ } finally {
    loading.value = false
  }
}

async function doCredits() {
  acting.value = 'credits'
  quota.value = null
  quotaMsg.value = ''
  try {
    const res = (await api.providerCredits(name.value)) as Record<string, any>
    if (res?.unsupported || res?.ok === false) {
      quotaMsg.value = String(res?.message || '该供应商暂不支持额度查询')
      message.info(quotaMsg.value)
    } else if (res?.quota) {
      quota.value = res.quota as Record<string, any>
      message.success('额度已刷新')
    } else {
      message.success('额度已刷新')
    }
    await load()
  } catch { /* 拦截器已提示 */ } finally {
    acting.value = ''
  }
}

async function doCheckin() {
  acting.value = 'checkin'
  try {
    const res = (await api.providerCheckin(name.value)) as Record<string, any>
    const rows = (res?.results ?? []) as Record<string, any>[]
    checkinResults.value = rows
    const okN = rows.filter((r) => r.ok).length
    message.success(`签到完成：成功 ${okN}/${rows.length}`)
    await load()
  } catch { /* 拦截器已提示 */ } finally {
    acting.value = ''
  }
}

async function doRefreshModels() {
  acting.value = 'models'
  await load()
  acting.value = ''
  message.success('模型已刷新')
}

// ---------- 扫码登录（OAuth） ----------
async function openOAuth() {
  stopOAuthPoll()
  oauthOpen.value = true
  oauthStatus.value = 'idle'
  oauthMsg.value = ''
  oauthLoginUrl.value = ''
  try {
    const res = await api.providerOAuthOptions(name.value)
    oauthOptions.value = res?.options ?? []
    const region = oauthOptions.value.find((o) => o?.key === 'region')
    oauthRegion.value = String(region?.default || regionValues.value[0]?.value || 'cn')
  } catch { /* 拦截器已提示 */ }
}

async function beginOAuth() {
  oauthBeginning.value = true
  oauthMsg.value = ''
  oauthLoginUrl.value = ''
  stopOAuthPoll()
  try {
    const res = await api.providerOAuthBegin(name.value, { region: oauthRegion.value })
    oauthLoginUrl.value = String(res?.login_url || '')
    const loginId = String(res?.login_id || '')
    if (!loginId) {
      oauthStatus.value = 'error'
      oauthMsg.value = '未获取到登录会话，请重试'
      return
    }
    if (oauthLoginUrl.value) window.open(oauthLoginUrl.value, '_blank', 'noopener,noreferrer')
    oauthStatus.value = 'pending'
    oauthDeadline = Date.now() + 5 * 60 * 1000 // 最多轮询 5 分钟
    oauthTimer = setInterval(() => pollOAuth(loginId), 2000)
  } catch {
    oauthStatus.value = 'error'
    oauthMsg.value = '发起登录失败，请重试'
  } finally {
    oauthBeginning.value = false
  }
}

async function pollOAuth(loginId: string) {
  if (Date.now() > oauthDeadline) {
    stopOAuthPoll()
    oauthStatus.value = 'error'
    oauthMsg.value = '登录超时，请重新发起'
    return
  }
  try {
    const res = await api.providerOAuthPoll(name.value, loginId)
    const status = String(res?.status || 'pending')
    if (status === 'ready') {
      stopOAuthPoll()
      oauthStatus.value = 'ready'
      message.success('登录成功，账号已添加')
      closeOAuth()
      await load()
    } else if (status === 'error') {
      stopOAuthPoll()
      oauthStatus.value = 'error'
      oauthMsg.value = String(res?.message || '授权失败，请重试')
      message.error(oauthMsg.value)
    } else {
      oauthStatus.value = 'pending'
    }
  } catch {
    stopOAuthPoll()
    oauthStatus.value = 'error'
    oauthMsg.value = '轮询登录状态失败，请重试'
  }
}

function stopOAuthPoll() {
  if (oauthTimer) {
    clearInterval(oauthTimer)
    oauthTimer = null
  }
}

function closeOAuth() {
  stopOAuthPoll()
  oauthOpen.value = false
}

function fmtTokens(v: any) {
  const n = Number(v || 0)
  if (!n || n <= 0) return '-'
  if (n >= 1000000) return `${(n / 1000000).toFixed(1)}M`
  if (n >= 1000) return `${Math.round(n / 1000)}K`
  return String(n)
}

// 模型 flag → 信号色标签（各供应商 flag 字段名不同，统一映射）
const FLAG_TAGS: { key: string; text: string; color: string }[] = [
  { key: 'is_reasoning', text: '思考', color: 'cyan' },
  { key: 'reasoning', text: '思考', color: 'cyan' },
  { key: 'tool_call', text: '工具', color: 'gold' },
  { key: 'vision', text: '视觉', color: 'purple' },
  { key: 'free', text: '免费', color: 'green' },
  { key: 'native_protocol', text: '原生协议', color: 'blue' },
  { key: 'anonymous', text: '匿名', color: 'default' },
  { key: 'is_default', text: '默认', color: 'blue' },
]
function modelFlags(row: Record<string, any>) {
  const out: { text: string; color: string }[] = []
  const seen = new Set<string>()
  for (const f of FLAG_TAGS) {
    if (row[f.key] === true && !seen.has(f.text)) {
      out.push({ text: f.text, color: f.color })
      seen.add(f.text)
    }
  }
  return out
}

// 数字千分位（额度剩余/总量），缺失时给 "—"
function fmtNum(v: any): string {
  if (v === null || v === undefined || v === '') return '—'
  const n = Number(v)
  if (!Number.isFinite(n)) return '—'
  return n.toLocaleString('zh-CN', { maximumFractionDigits: 1 })
}

// 区域标识 → 中文
function regionText(r: any): string {
  const s = String(r || '')
  if (s === 'cn') return '国内'
  if (s === 'global' || s === 'international' || s === 'intl') return '国际'
  return s || '—'
}

// qoder 账号状态：使用中 / 备用 / 不可用（对齐 workbuddy 的 LED 语义）
function acctStatus(record: Record<string, any>): { led: string; text: string } {
  if (record.active) return { led: 'live', text: '使用中' }
  if (record.has_secret || record.healthy) return { led: 'fog', text: '备用' }
  return { led: 'fault', text: '不可用' }
}
function isNative(record: Record<string, any>) { return record.source === 'native' }

// qoder 模型排序：默认 → 启用 → 名称，让常用模型置顶（其它供应商保持原序）
const displayModels = computed(() => {
  if (name.value !== 'qoder') return models.value
  return [...models.value].sort((a, b) => {
    const da = a.is_default ? 1 : 0, db = b.is_default ? 1 : 0
    if (da !== db) return db - da
    const ea = a.enable === false ? 0 : 1, eb = b.enable === false ? 0 : 1
    if (ea !== eb) return eb - ea
    return String(a.name || a.id).localeCompare(String(b.name || b.id))
  })
})

// ---------- 逐账号动作：设为激活 / 重命名 / 删除（qoder 原生账号） ----------
const rowBusy = ref<string>('')             // 正在执行动作的账号 id
const editingRenameId = ref<string | null>(null)
const renameDraft = ref<Record<string, string>>({})

async function doActivate(record: Record<string, any>) {
  rowBusy.value = record.id
  try {
    await api.providerActivateAccount(name.value, record.id)
    message.success('已设为激活账号')
    await load()
  } catch { /* 拦截器已提示 */ } finally {
    rowBusy.value = ''
  }
}

function startRename(record: Record<string, any>) {
  editingRenameId.value = record.id
  renameDraft.value[record.id] = record.label || ''
}

async function saveRename(record: Record<string, any>) {
  const val = (renameDraft.value[record.id] ?? '').trim()
  if (!val) { message.warning('名称不能为空'); return }
  rowBusy.value = record.id
  try {
    await api.providerRenameAccount(name.value, record.id, val)
    message.success(`已重命名为「${val}」`)
    editingRenameId.value = null
    await load()
  } catch { /* 拦截器已提示 */ } finally {
    rowBusy.value = ''
  }
}

async function doDelete(record: Record<string, any>) {
  rowBusy.value = record.id
  try {
    await api.providerDeleteAccount(name.value, record.id)
    message.success('账号已删除')
    await load()
  } catch { /* 拦截器已提示 */ } finally {
    rowBusy.value = ''
  }
}

watch(name, () => {
  detail.value = null
  checkinResults.value = []
  quota.value = null
  quotaMsg.value = ''
  tab.value = 'accounts'
  closeOAuth()
  load()
})

onMounted(() => {
  load()
  // 通用供应商每 30s 轻量刷新详情；workbuddy 标签内嵌视图各自轮询，无需在此重复拉取
  timer = setInterval(() => { if (!isWorkbuddy.value) load() }, 30000)
})
onUnmounted(() => {
  if (timer) clearInterval(timer)
  stopOAuthPoll()
})
</script>

<template>
  <div class="provider-view">
    <div class="view-header">
      <div>
        <div class="vh-title">{{ displayName }}</div>
        <div class="vh-meta">
          <span class="led" :class="ledClass"></span>
          {{ ready ? '就绪' : '未配置' }}
          <template v-if="ready">
            · <span class="mono">{{ accounts.length }}</span> {{ name === 'opencode' ? '密钥层' : '账号' }}
            · <span class="mono">{{ models.length }}</span> 模型
          </template>
        </div>
      </div>
    </div>

    <!-- ===== workbuddy：复用既有富交互「账号 / 模型」视图，功能零丢失 ===== -->
    <a-tabs v-if="isWorkbuddy" v-model:activeKey="tab" class="prov-tabs">
      <a-tab-pane key="accounts" tab="账号"><Accounts :embedded="true" /></a-tab-pane>
      <a-tab-pane key="models" tab="模型"><Models :embedded="true" /></a-tab-pane>
    </a-tabs>

    <!-- ===== qoder / opencode：能力驱动的通用结构，与 workbuddy 同构（账号 / 模型 标签页） ===== -->
    <template v-else>
      <a-alert
        v-if="!ready && detail?.notes"
        type="info"
        show-icon
        :message="`${displayName} 未就绪`"
        :description="detail.notes"
        style="margin-bottom: 16px"
      />

      <a-tabs v-model:activeKey="tab" class="prov-tabs">
        <a-tab-pane key="accounts" tab="账号">
          <!-- 账号页工具栏：能力门控的动作 -->
          <div v-if="has('oauth') || has('checkin') || has('credits')" class="tab-toolbar">
            <a-button v-if="has('oauth')" type="primary" @click="openOAuth">添加账号</a-button>
            <a-button v-if="has('checkin')" :loading="acting === 'checkin'" @click="doCheckin">签到</a-button>
            <a-button v-if="has('credits')" :loading="acting === 'credits'" @click="doCredits">刷新额度</a-button>
          </div>

      <!-- 额度查询结果（qoder quota） -->
      <section v-if="quota" class="panel">
        <div class="panel-title">额度</div>
        <div class="quota-grid">
          <div class="q-item"><span class="q-k">套餐</span><span class="q-v mono">{{ quota.plan ?? '—' }}</span></div>
          <div class="q-item"><span class="q-k">用户额度</span><span class="q-v mono">{{ quota.user_quota ?? '—' }}</span></div>
          <div class="q-item"><span class="q-k">加购额度</span><span class="q-v mono">{{ quota.addon_quota ?? '—' }}</span></div>
          <div class="q-item">
            <span class="q-k">额度状态</span>
            <span class="q-v" :style="{ color: quota.is_quota_exceeded ? 'var(--fault)' : 'var(--live)' }">
              {{ quota.is_quota_exceeded ? '已超额' : '可用' }}
            </span>
          </div>
          <div v-if="quota.expires_at" class="q-item"><span class="q-k">到期</span><span class="q-v mono">{{ quota.expires_at }}</span></div>
        </div>
      </section>
      <a-alert v-else-if="quotaMsg" type="warning" show-icon :message="quotaMsg" style="margin-bottom: 16px" />

      <!-- 签到结果（逐账号） -->
      <section v-if="checkinResults.length" class="panel">
        <div class="panel-title">签到结果</div>
        <a-table :data-source="checkinResults" :pagination="false" size="small" row-key="account_id" :locale="{ emptyText: '无结果' }">
          <a-table-column title="账号" data-index="account" key="account">
            <template #default="{ record }">{{ record.account || record.account_id || '—' }}</template>
          </a-table-column>
          <a-table-column title="状态" key="ok" :width="90">
            <template #default="{ record }">
              <span class="cell-status"><span class="led" :class="record.ok ? 'live' : 'fault'"></span>{{ record.ok ? '成功' : (record.status || '失败') }}</span>
            </template>
          </a-table-column>
          <a-table-column title="获得" key="amount" align="right" :width="90">
            <template #default="{ record }"><span class="mono">{{ record.amount ?? '—' }}</span></template>
          </a-table-column>
          <a-table-column title="连续签到" key="streak" align="right" :width="100">
            <template #default="{ record }"><span class="mono">{{ record.streak_days ?? '—' }}</span></template>
          </a-table-column>
          <a-table-column title="说明" data-index="message" key="message">
            <template #default="{ record }"><span style="color: var(--fog)">{{ record.message || '' }}</span></template>
          </a-table-column>
        </a-table>
      </section>

          <!-- 账号 / 密钥层表（列随供应商自适应，裸表贴合仪表台，与 workbuddy 同一视觉语言） -->
          <a-table v-if="accounts.length" :data-source="accounts" :loading="loading" :pagination="false" row-key="id" :scroll="{ x: 'max-content' }">
          <!-- qoder 账号列（对齐 workbuddy 账号表的视觉与信息密度） -->
          <template v-if="name === 'qoder'">
            <a-table-column title="账号 / 别名" key="label" :width="220">
              <template #default="{ record }">
                <template v-if="editingRenameId === record.id">
                  <a-input
                    :value="renameDraft[record.id]"
                    size="small" :maxlength="40" style="width: 150px"
                    placeholder="账号名称"
                    @change="(e: any) => { renameDraft[record.id] = e.target.value }"
                    @press-enter="saveRename(record)"
                  />
                  <a-button size="small" type="link" style="padding: 0 4px" :loading="rowBusy === record.id" @click="saveRename(record)">保存</a-button>
                </template>
                <template v-else>
                  <a-tooltip :title="`ID：${record.id}`">
                    <span class="acct-name">{{ record.label || record.id }}</span>
                  </a-tooltip>
                  <a-button v-if="isNative(record)" size="small" type="link" style="padding: 0 4px" @click="startRename(record)">改名</a-button>
                </template>
              </template>
            </a-table-column>
            <a-table-column title="区域" key="region" :width="90">
              <template #default="{ record }">
                <a-tag :color="regionText(record.region) === '国内' ? 'geekblue' : 'cyan'">{{ regionText(record.region) }}</a-tag>
              </template>
            </a-table-column>
            <a-table-column title="来源" key="source" :width="110">
              <template #default="{ record }">
                <a-tag v-if="isNative(record)" color="purple">原生</a-tag>
                <a-tooltip v-else title="本机 Qoder 桌面端自动探测的登录凭据，不支持改名/删除">
                  <a-tag color="geekblue" style="cursor: help">本地探测</a-tag>
                </a-tooltip>
              </template>
            </a-table-column>
            <a-table-column title="状态" key="active" :width="100">
              <template #default="{ record }">
                <span class="cell-status"><span class="led" :class="acctStatus(record).led"></span>{{ acctStatus(record).text }}</span>
              </template>
            </a-table-column>
            <a-table-column title="额度剩余" key="credits_remaining" align="right" :width="110">
              <template #default="{ record }">
                <div class="mono">{{ fmtNum(record.credits_remaining) }}</div>
                <div v-if="record.plan" style="font-size: 11px; color: var(--fog)">{{ record.plan }}</div>
              </template>
            </a-table-column>
            <a-table-column title="额度总量" key="credits_total" align="right" :width="100">
              <template #default="{ record }"><span class="mono">{{ fmtNum(record.credits_total) }}</span></template>
            </a-table-column>
            <a-table-column title="今日签到" key="checkin" :width="110">
              <template #default="{ record }">
                <a-tag :color="record.checkin_today ? 'green' : 'default'">{{ record.checkin_today ? '已签到' : '未签到' }}</a-tag>
                <div v-if="record.streak_days" style="font-size: 11px; color: var(--fog); margin-top: 2px">连续 {{ record.streak_days }} 天</div>
              </template>
            </a-table-column>
            <a-table-column title="操作" key="action" :width="170" fixed="right">
              <template #default="{ record }">
                <template v-if="isNative(record)">
                  <a-space>
                    <a-button v-if="!record.active" size="small" :loading="rowBusy === record.id" @click="doActivate(record)">设为激活</a-button>
                    <a-popconfirm title="确认删除该账号？" ok-text="删除" cancel-text="取消" @confirm="doDelete(record)">
                      <a-button size="small" danger :loading="rowBusy === record.id">删除</a-button>
                    </a-popconfirm>
                  </a-space>
                </template>
                <a-tooltip v-else title="本地探测账号由桌面端自动管理，无法在此改名或删除">
                  <span style="color: var(--fog); cursor: help">自动管理</span>
                </a-tooltip>
              </template>
            </a-table-column>
          </template>
          <!-- opencode 密钥层列 -->
          <template v-else-if="name === 'opencode'">
            <a-table-column title="层" key="label">
              <template #default="{ record }">{{ record.label || record.id }}</template>
            </a-table-column>
            <a-table-column title="档位" key="tier" :width="120">
              <template #default="{ record }"><a-tag color="blue">{{ record.tier || '—' }}</a-tag></template>
            </a-table-column>
            <a-table-column title="密钥数" key="key_count" align="right" :width="100">
              <template #default="{ record }"><span class="mono">{{ record.key_count ?? 0 }}</span></template>
            </a-table-column>
          </template>
          <!-- 其它供应商：兜底展示 label/id -->
          <template v-else>
            <a-table-column title="账号" key="label">
              <template #default="{ record }">{{ record.label || record.id || '—' }}</template>
            </a-table-column>
          </template>
          </a-table>
          <div v-if="ready && !accounts.length && !loading" class="empty-hint">
            {{ displayName }} 暂无{{ name === 'opencode' ? '密钥层' : '账号' }}数据。
          </div>
        </a-tab-pane>

        <a-tab-pane key="models" tab="模型">
          <!-- 模型页工具栏 -->
          <div class="tab-toolbar">
            <a-button :loading="acting === 'models'" @click="doRefreshModels"><ReloadOutlined />刷新模型</a-button>
          </div>
          <a-table v-if="models.length" :data-source="displayModels" :loading="loading" :pagination="false" row-key="id" :scroll="{ x: 'max-content' }">
          <a-table-column title="模型" key="name" :width="300">
            <template #default="{ record }">
              <div class="model-name">{{ record.name || record.id }}</div>
              <code class="model-id">{{ record.id }}</code>
            </template>
          </a-table-column>
          <a-table-column title="上下文" key="context" align="right" :width="100">
            <template #default="{ record }"><span class="mono">{{ fmtTokens(record.context) }}</span></template>
          </a-table-column>
          <a-table-column title="最大输出" key="max_output" align="right" :width="100">
            <template #default="{ record }"><span class="mono">{{ fmtTokens(record.max_output) }}</span></template>
          </a-table-column>
          <a-table-column v-if="name === 'qoder'" title="倍率" key="price_factor" align="right" :width="90">
            <template #default="{ record }">
              <span v-if="record.price_factor === 0" class="mono" style="color: var(--live)">免费</span>
              <span v-else-if="record.price_factor != null" class="mono" style="color: var(--charge)">x{{ record.price_factor }}</span>
              <span v-else class="mono" style="color: var(--fog)">—</span>
            </template>
          </a-table-column>
          <a-table-column v-if="name === 'opencode'" title="档位" key="tier" :width="90">
            <template #default="{ record }"><span v-if="record.tier">{{ record.tier }}</span><span v-else style="color: var(--fog)">—</span></template>
          </a-table-column>
          <a-table-column title="标记" key="flags">
            <template #default="{ record }">
              <a-tag v-for="f in modelFlags(record)" :key="f.text" :color="f.color">{{ f.text }}</a-tag>
              <a-tag v-if="name === 'qoder' && record.enable === false" color="default">停用</a-tag>
              <span v-if="!modelFlags(record).length && !(name === 'qoder' && record.enable === false)" style="color: var(--fog)">—</span>
            </template>
          </a-table-column>
          </a-table>
          <div v-if="ready && !models.length && !loading" class="empty-hint">
            {{ displayName }} 暂无模型数据。
          </div>
        </a-tab-pane>
      </a-tabs>
    </template>

    <!-- 扫码登录弹窗（能力含 oauth 时通过「添加账号」打开） -->
    <a-modal :open="oauthOpen" title="添加账号 · 扫码登录" :footer="null" :closable="true" @cancel="closeOAuth">
      <div class="oauth-body">
        <div class="oauth-row">
          <span class="oauth-label">区域</span>
          <a-select v-model:value="oauthRegion" style="width: 220px" :disabled="oauthStatus === 'pending'">
            <a-select-option v-for="v in regionValues" :key="v.value" :value="v.value">{{ v.label }}</a-select-option>
          </a-select>
        </div>

        <a-button type="primary" :loading="oauthBeginning" :disabled="oauthStatus === 'pending'" @click="beginOAuth">
          {{ oauthStatus === 'error' ? '重新开始登录' : '开始登录' }}
        </a-button>

        <div v-if="oauthLoginUrl && oauthStatus === 'pending'" class="oauth-pending">
          <a :href="oauthLoginUrl" target="_blank" rel="noopener">在浏览器中打开登录页</a>
          <div class="oauth-hint">
            <a-spin size="small" />
            <span>请在打开的页面完成授权，完成后本页会自动检测…</span>
          </div>
        </div>

        <a-alert v-if="oauthStatus === 'error' && oauthMsg" type="error" show-icon :message="oauthMsg" />
      </div>
    </a-modal>
  </div>
</template>

<style scoped>
.provider-view { width: 100%; }

.panel {
  background: var(--panel); border: 1px solid var(--line); border-radius: var(--radius);
  padding: 16px 18px; margin-bottom: 16px;
}
.panel-title { font-size: 15px; font-weight: 500; color: var(--paper); margin-bottom: 12px; }

/* 额度读数格 */
.quota-grid {
  display: grid; grid-template-columns: repeat(auto-fill, minmax(150px, 1fr)); gap: 0;
  border: 1px solid var(--line-2); border-radius: var(--radius); overflow: hidden;
}
.q-item { padding: 10px 14px; border-left: 1px solid var(--line-2); display: flex; flex-direction: column; gap: 4px; }
.q-item:first-child { border-left: none; }
.q-k { font-size: 12px; color: var(--fog); }
.q-v { font-size: 16px; font-weight: 500; color: var(--paper); }

.cell-status { display: inline-flex; align-items: center; gap: 6px; color: var(--paper); }

/* 账号名 / 模型名单元格 */
.acct-name { color: var(--paper); }
.model-name { color: var(--paper); line-height: 1.4; }
.model-id { font-family: var(--mono); font-size: 12px; color: var(--fog); background: transparent; }

.empty-hint {
  padding: 40px 24px; text-align: center; color: var(--fog);
  background: var(--panel); border: 1px solid var(--line); border-radius: var(--radius);
}

/* 标签页内的动作工具栏（账号 / 模型 页顶部） */
.tab-toolbar { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; margin-bottom: 14px; }

/* 供应商标签页：贴合仪表台配色（三个供应商统一） */
.prov-tabs :deep(.ant-tabs-tab) { color: var(--fog); }
.prov-tabs :deep(.ant-tabs-tab-active .ant-tabs-tab-btn) { color: var(--route) !important; }
.prov-tabs :deep(.ant-tabs-ink-bar) { background: var(--route); }
.prov-tabs :deep(.ant-tabs-nav::before) { border-bottom-color: var(--line) !important; }

/* 扫码登录弹窗 */
.oauth-body { display: flex; flex-direction: column; gap: 16px; padding: 8px 0; }
.oauth-row { display: flex; align-items: center; gap: 12px; }
.oauth-label { color: var(--fog); font-size: 13px; }
.oauth-pending { display: flex; flex-direction: column; gap: 8px; }
.oauth-hint { display: flex; align-items: center; gap: 8px; color: var(--fog); font-size: 12px; }

</style>



