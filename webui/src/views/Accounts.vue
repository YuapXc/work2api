<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { api } from '@/api/client'
import type { ProviderSummary, OAuthOption } from '@/types'
import { toast } from '@/lib/toast'
import { confirm } from '@/lib/confirm'
import { credits as fmtCredits, dt, rel, cooldown } from '@/lib/format'
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
    body: '删除后该账号不再参与请求分发，历史用量记录保留。',
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
  renameValue.value = r.raw.alias || ''
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
let pollTimer: ReturnType<typeof setInterval> | null = null

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
  oauthUrl.value = ''
  oauthChoice.value = {}
  const c = caps(addProvider.value)
  addMethod.value = c.includes('oauth') ? 'oauth' : c.includes('upload') ? 'upload' : 'oauth'
  if (addProvider.value === 'workbuddy') {
    oauthSites.value = (await api.oauthSites()).sites || []
    oauthOptions.value = []
    if (oauthSites.value.length) oauthChoice.value.site = oauthSites.value[0]
  } else if (c.includes('oauth')) {
    const res = await api.providerOAuthOptions(addProvider.value)
    oauthOptions.value = res.options || []
    for (const o of oauthOptions.value) oauthChoice.value[o.key] = o.default || o.values[0]?.value || ''
  }
}

function stopPoll() {
  if (pollTimer) clearInterval(pollTimer)
  pollTimer = null
  oauthPolling.value = false
}

async function beginOAuth() {
  oauthPolling.value = true
  try {
    if (addProvider.value === 'workbuddy') {
      const site = oauthChoice.value.site
      const res = await api.oauthBegin(site)
      oauthUrl.value = res.authUrl
      window.open(res.authUrl, '_blank')
      pollTimer = setInterval(async () => {
        const p = await api.oauthPoll(res.state, site)
        if (p.status === 'ready') { stopPoll(); toast.success('已添加账号'); addOpen.value = false; await load() }
      }, 2500)
    } else {
      const res = await api.providerOAuthBegin(addProvider.value, { ...oauthChoice.value })
      oauthUrl.value = res.login_url
      window.open(res.login_url, '_blank')
      pollTimer = setInterval(async () => {
        const p = await api.providerOAuthPoll(addProvider.value, res.login_id)
        if (p.status === 'ready') { stopPoll(); toast.success('已添加账号'); addOpen.value = false; await load() }
        else if (p.status === 'error') { stopPoll(); toast.error(p.message || '登录失败') }
      }, 2500)
    }
  } catch {
    oauthPolling.value = false
  }
}

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

onMounted(load)
// 组件卸载时停掉可能残留的扫码轮询定时器，避免离开页面后仍在轮询
onUnmounted(stopPoll)

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
            <template v-if="row.raw.credits_remaining != null">
              <span class="mono text-ink">{{ fmtCredits(row.raw.credits_remaining) }}</span>
              <span v-if="row.raw.credits_total != null" class="mono text-faint"> / {{ fmtCredits(row.raw.credits_total) }}</span>
              <span v-if="row.raw.plan" class="ml-1 text-micro text-faint">{{ row.raw.plan }}</span>
            </template>
            <span v-else class="text-faint">—</span>
          </template>
          <template #cell-expire="{ row }">
            <span v-if="row.raw.credits_expire_at" :title="dt(row.raw.credits_expire_at, 'YYYY-MM-DD HH:mm')" class="text-muted">{{ rel(row.raw.credits_expire_at) }}</span>
            <span v-else class="text-faint">—</span>
          </template>
          <template #cell-checkin="{ row }">
            <WTag v-if="row.raw.checkin_today" tone="live" dot>今日已签</WTag>
            <span v-else class="text-faint">—</span>
            <div v-if="row.raw.streak_days" class="text-micro text-faint">连签 {{ row.raw.streak_days }} 天</div>
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
            <WButton v-else-if="row.provider === 'qoder'" size="sm" variant="ghost" @click="activate(row)">设为激活</WButton>
            <span v-else class="text-faint">—</span>
          </template>
          <template #cell-actions="{ row }">
            <div class="flex items-center justify-end gap-1.5">
              <WButton size="sm" variant="subtle" @click="openRename(row)">{{ row.provider === 'workbuddy' ? '别名' : '重命名' }}</WButton>
              <WToggle v-if="row.provider === 'workbuddy'" :model-value="row.raw.enabled" @update:model-value="toggleEnabled(row)" />
              <WButton size="sm" variant="danger" @click="removeAccount(row)">删除</WButton>
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
        <input ref="uploadEl" type="file" accept=".json" class="hidden" @change="onUpload" />
        <button
          class="flex w-full flex-col items-center gap-2 rounded-lg border border-dashed border-line py-8 text-muted transition-colors hover:border-brand hover:text-brand"
          @click="uploadEl?.click()"
        >
          <WIcon name="upload" :size="24" />
          <span class="text-small">点击选择 auth 凭据文件（.json）</span>
        </button>
      </template>

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
  </WPage>
</template>
