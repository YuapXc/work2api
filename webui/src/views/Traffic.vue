<script setup lang="ts">
import { ref, computed, onMounted, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '@/api/client'
import type { UsageSummary, UsagePoint, UsageRecord, RecordsResponse } from '@/types'
import { toast } from '@/lib/toast'
import { int, abbr, credits as fmtCredits, dt, latency, pct } from '@/lib/format'
import WPage from '@/components/ui/WPage.vue'
import WCard from '@/components/ui/WCard.vue'
import WTabs from '@/components/ui/WTabs.vue'
import WStat from '@/components/ui/WStat.vue'
import WTable, { type Column } from '@/components/ui/WTable.vue'
import WButton from '@/components/ui/WButton.vue'
import WInput from '@/components/ui/WInput.vue'
import WSelect from '@/components/ui/WSelect.vue'
import WLed from '@/components/ui/WLed.vue'
import WModal from '@/components/ui/WModal.vue'
import WEmpty from '@/components/ui/WEmpty.vue'
import WSpinner from '@/components/ui/WSpinner.vue'
import WIcon from '@/components/ui/WIcon.vue'
import TrendChart from '@/components/TrendChart.vue'
import BarList from '@/components/BarList.vue'
import SessionsPanel from '@/components/SessionsPanel.vue'
import { buildLabelMap } from '@/utils/accountLabel'

const route = useRoute()
const router = useRouter()
const tab = ref(['logs', 'sessions'].includes(String(route.query.tab)) ? String(route.query.tab) : 'analytics')
watch(tab, (t) => { router.replace({ query: t !== 'analytics' ? { tab: t } : {} }); loadTab(t) })
watch(() => route.query.tab, (value) => { tab.value = ['logs', 'sessions'].includes(String(value)) ? String(value) : 'analytics' })

// ================= 分析 =================
const summary = ref<UsageSummary | null>(null)
const points = ref<UsagePoint[]>([])
const granularity = ref('hour')
const metric = ref<string>('count')
const anaLoading = ref(true)

async function loadAnalytics() {
  anaLoading.value = true
  try {
    const [s, ts] = await Promise.all([
      api.usageSummary(),
      api.usageTimeseries(granularity.value, granularity.value === 'hour' ? 24 : 30),
    ])
    summary.value = s
    points.value = ts.data || []
  } finally {
    anaLoading.value = false
  }
}
watch(granularity, loadAnalytics)

const protocolBars = computed(() =>
  (summary.value?.by_protocol || []).map((p) => ({ label: p.protocol, value: p.count, sub: abbr(p.tokens) + ' tok' })),
)
const modelBars = computed(() =>
  (summary.value?.by_model || []).slice(0, 10).map((m) => ({ label: m.model, value: m.count, sub: abbr(m.tokens) + ' tok' })),
)
const appBars = computed(() =>
  (summary.value?.by_app || []).map((a) => ({ label: a.app || '(未命名)', value: a.count, sub: abbr(a.tokens) + ' tok' })),
)
const accountBars = computed(() =>
  (summary.value?.by_account || []).map((a) => ({
    label: (a.label || a.account_uid.slice(0, 8)) + (a.removed ? ' (已删除)' : ''),
    value: a.count,
    sub: fmtCredits(a.credits) + ' 额度',
  })),
)

// 缓存命中：仅上游上报过 cached_tokens 的请求参与统计。
// cached / (cached + 未命中输入)；输入里包含缓存的增量部分才是"可命中"的总量。
const cacheStats = computed(() => {
  const c = summary.value?.cache
  if (!c || !c.known_rows || c.hit_rate == null) return null
  return { rows: c.known_rows, hitRate: pct(c.hit_rate), cached: c.cached_tokens, uncached: c.uncached_tokens }
})

// ================= 日志 =================
const records = ref<UsageRecord[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = 20
const logLoading = ref(false)
const filters = ref<{ protocol?: string; model?: string; app_name?: string; status?: string; search?: string }>({})
const filterOpts = ref<{ protocols: string[]; models: string[]; apps: string[]; apps_history: string[]; statuses: string[] }>({
  protocols: [], models: [], apps: [], apps_history: [], statuses: [],
})
const searchInput = ref('')

// account_uid → 显示名（含 workbuddy 别名）。日志表原本只显示 uid 短码，别名看不到。
// 仅覆盖 workbuddy 池账号；其它供应商/已删除账号回退 uid 短码（不误标「已移除」）。
const labelMap = ref<Record<string, string>>({})
async function loadAccountLabels() {
  try {
    const res = await api.accounts()
    labelMap.value = buildLabelMap(res.accounts || [])
  } catch {
    labelMap.value = {}
  }
}
function acctLabel(uid?: string | null): string {
  if (!uid) return '—'
  return labelMap.value[uid] || uid.slice(0, 8)
}

async function loadFilters() {
  const f = await api.usageFilters()
  filterOpts.value = { protocols: f.protocols || [], models: f.models || [], apps: f.apps || [], apps_history: f.apps_history || [], statuses: f.statuses || [] }
}
async function loadLogs() {
  logLoading.value = true
  try {
    const res: RecordsResponse = await api.usageRecent(page.value, pageSize, filters.value)
    records.value = res.records || []
    total.value = res.total || 0
  } finally {
    logLoading.value = false
  }
}
function applyFilter() {
  filters.value.search = searchInput.value.trim() || undefined
  // page 变化会触发 watch 加载；已在第 1 页时手动加载一次，避免重复请求
  if (page.value === 1) loadLogs()
  else page.value = 1
}
function resetFilter() {
  filters.value = {}
  searchInput.value = ''
  if (page.value === 1) loadLogs()
  else page.value = 1
}
watch(page, loadLogs)
const totalPages = computed(() => Math.max(1, Math.ceil(total.value / pageSize)))

const logCols: Column[] = [
  { key: 'ts', label: '时间', mono: true },
  { key: 'model', label: '模型' },
  { key: 'protocol', label: '协议' },
  { key: 'account_uid', label: '账号', mono: true },
  { key: 'tokens', label: 'Tokens（入/出）', align: 'right', mono: true },
  { key: 'credits', label: '额度', align: 'right', mono: true },
  { key: 'latency_ms', label: '延迟', align: 'right', mono: true },
  { key: 'status', label: '状态' },
  { key: 'actions', label: '', align: 'right' },
]

// 详情
const detailOpen = ref(false)
function completionLabel(row: { status?: string; diagnostics?: UsageRecord['diagnostics'] }): string {
  if (row.status !== 'incomplete') return row.status || '未知'
  const reason = row.diagnostics?.finish_reason
  if (reason === 'length') return '达到输出或上下文上限'
  if (reason === 'content_filter') return '上游内容过滤'
  return '输出未完成（原因未记录）'
}
function budgetLabel(limits?: Record<string, number>): string {
  return limits && Object.keys(limits).length ? Object.entries(limits).map(([k, v]) => `${k}: ${v}`).join(' · ') : '未显式指定'
}
function compatibilityLabel(value?: string): string {
  return ({ original: '原文', identity: '固定身份兼容', client_metadata: '客户端身份与归因精简' } as Record<string, string>)[value ?? ''] ?? '未记录'
}
const detail = ref<UsageRecord | null>(null)
const detailLoading = ref(false)
async function openDetail(id: number) {
  detailOpen.value = true
  detailLoading.value = true
  detail.value = null
  try {
    detail.value = (await api.usageDetail(id)).record
  } finally {
    detailLoading.value = false
  }
}

// 导出 CSV（遍历全部分页）
const exporting = ref(false)
async function exportCSV() {
  exporting.value = true
  try {
    const all: UsageRecord[] = []
    let p = 1
    for (;;) {
      const res = await api.usageRecent(p, 200, filters.value)
      all.push(...(res.records || []))
      if (all.length >= (res.total || 0) || !res.records?.length) break
      p++
    }
    const head = ['时间', '模型', '协议', '账号', '输入', '输出', '额度', '延迟ms', '状态']
    const lines = all.map((r) =>
      [dt(r.ts, 'YYYY-MM-DD HH:mm:ss'), r.model, r.protocol, r.account_uid, r.input_tokens, r.output_tokens, r.credits ?? '', r.latency_ms, r.status]
        .map((v) => `"${String(v ?? '').replace(/"/g, '""')}"`)
        .join(','),
    )
    const blob = new Blob(['﻿' + [head.join(','), ...lines].join('\n')], { type: 'text/csv;charset=utf-8' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `usage-${dt(Date.now() / 1000, 'YYYYMMDD-HHmm')}.csv`
    a.click()
    URL.revokeObjectURL(url)
    toast.success(`已导出 ${all.length} 条`)
  } finally {
    exporting.value = false
  }
}

function loadTab(value: string) {
  if (value === 'analytics') void loadAnalytics().catch(() => {})
  if (value === 'logs') {
    void loadFilters().catch(() => {})
    void loadAccountLabels()
    void loadLogs().catch(() => {})
  }
}
onMounted(() => loadTab(tab.value))
</script>

<template>
  <WPage title="流量" sub="调用量分析、调用日志与活跃会话账号调整。">
    <template #actions>
      <WTabs v-model="tab" :tabs="[{ key: 'analytics', label: '分析' }, { key: 'logs', label: '日志' }, { key: 'sessions', label: '会话' }]" />
    </template>

    <!-- ============ 分析 ============ -->
    <template v-if="tab === 'analytics'">
      <WSpinner v-if="anaLoading && !summary" center label="加载中" />
      <template v-else>
        <div class="mb-4 grid grid-cols-2 gap-3 lg:grid-cols-4">
          <WStat label="累计请求" :value="int(summary?.total_requests)" tone="brand" />
          <WStat label="累计 Tokens" :value="abbr(summary?.total_tokens)" tone="route" />
          <WStat label="今日请求" :value="int(summary?.today_requests)" tone="live" />
          <WStat label="今日 Tokens" :value="abbr(summary?.today_tokens)" />
        </div>

        <!-- 缓存命中：有可统计样本才展示（qoder 等不上报的上游自动隐藏） -->
        <WCard v-if="cacheStats" title="提示词缓存" class="mb-4">
          <template #actions>
            <span class="text-micro text-faint">基于 {{ int(cacheStats.rows) }} 条已知样本（仅统计缓存数据有效的成功请求）</span>
          </template>
          <div class="flex flex-wrap items-center gap-6">
            <div class="min-w-24">
              <div class="text-micro text-faint">命中率</div>
              <div class="mono text-2xl font-semibold text-live">{{ cacheStats.hitRate }}</div>
            </div>
            <div class="min-w-24">
              <div class="text-micro text-faint">命中 Tokens</div>
              <div class="mono text-small text-ink">{{ int(cacheStats.cached) }}</div>
            </div>
            <div class="min-w-24">
              <div class="text-micro text-faint">未命中 Tokens</div>
              <div class="mono text-small text-muted">{{ int(cacheStats.uncached) }}</div>
            </div>
            <div class="h-2 min-w-40 flex-1 overflow-hidden rounded-full bg-bg">
              <div class="h-full rounded-full bg-live/70" :style="{ width: cacheStats.hitRate || '0%' }" />
            </div>
          </div>
        </WCard>

        <WCard title="调用趋势" class="mb-4">
          <template #actions>
            <WTabs v-model="metric" :tabs="[{ key: 'count', label: '请求数' }, { key: 'tokens', label: 'Tokens' }]" />
            <WTabs v-model="granularity" :tabs="[{ key: 'hour', label: '24 小时' }, { key: 'day', label: '30 天' }]" />
          </template>
          <TrendChart :data="points" :metric="metric" />
        </WCard>

        <div class="grid gap-4 lg:grid-cols-2">
          <WCard title="按协议"><BarList :items="protocolBars" tone="brand" /></WCard>
          <WCard title="按账号"><BarList :items="accountBars" tone="live" /></WCard>
          <WCard title="按模型（Top 10）"><BarList :items="modelBars" tone="route" /></WCard>
          <WCard title="按应用"><BarList :items="appBars" tone="warn" /></WCard>
        </div>
      </template>
    </template>

    <!-- ============ 日志 ============ -->
    <template v-else-if="tab === 'logs'">
      <div class="mb-4 flex flex-wrap items-center gap-2">
        <WInput v-model="searchInput" placeholder="搜索输入/输出/思考内容" class="w-full sm:w-64" @enter="applyFilter">
          <template #prefix><WIcon name="search" :size="16" class="text-faint" /></template>
        </WInput>
        <WSelect v-model="filters.protocol" :options="filterOpts.protocols.map((p) => ({ value: p, label: p }))" placeholder="全部协议" class="w-32" @update:model-value="applyFilter" />
        <WSelect v-model="filters.model" :options="filterOpts.models.map((m) => ({ value: m, label: m }))" placeholder="全部模型" class="w-44" @update:model-value="applyFilter" />
        <WSelect v-model="filters.status" :options="filterOpts.statuses.map((s) => ({ value: s, label: s }))" placeholder="全部状态" class="w-28" @update:model-value="applyFilter" />
        <WButton variant="subtle" size="sm" @click="resetFilter">重置</WButton>
        <div class="ml-auto flex items-center gap-2">
          <WButton variant="ghost" size="sm" :loading="exporting" @click="exportCSV"><WIcon name="download" :size="15" /> 导出</WButton>
          <WButton variant="ghost" size="sm" @click="loadLogs"><WIcon name="refresh" :size="15" /> 刷新</WButton>
        </div>
      </div>

      <WCard flush>
        <WSpinner v-if="logLoading && !records.length" center label="加载中" />
        <WTable v-else :columns="logCols" :rows="records" row-key="id" min-width="960px">
          <template #cell-ts="{ value }">{{ dt(value, 'MM-DD HH:mm:ss') }}</template>
          <template #cell-account_uid="{ value }">{{ acctLabel(value) }}</template>
          <template #cell-tokens="{ row }">
            <div class="text-right leading-tight">
              <div><span class="text-ink">{{ int(row.input_tokens) }}</span><span class="text-faint"> / {{ int(row.output_tokens) }}</span></div>
              <!-- 缓存明细：未命中在前（异常醒目，琥珀色），命中灰色小字；
                   全量未命中整行升级警示；上游未上报则不显示 -->
              <div v-if="row.cached_tokens != null && (row.cached_tokens < 0 || row.cached_tokens > row.input_tokens)" class="text-micro text-warn" title="上游缓存计数超出输入范围，已排除命中率统计">缓存数据异常 · {{ int(row.cached_tokens) }}</div>
              <div v-else-if="row.cached_tokens === 0" class="text-micro text-warn" title="上游上报了缓存数据，但本次完全未命中（换号/前缀被打散？）">
                全量未命中 {{ int(row.input_tokens) }}
              </div>
              <div v-else-if="row.cached_tokens != null && row.input_tokens > row.cached_tokens" class="text-micro">
                <span class="text-warn" title="未命中输入 Tokens">未命中 {{ int(row.input_tokens - row.cached_tokens) }}</span>
                <span class="text-faint" title="缓存命中 Tokens"> · ⚡命中 {{ int(row.cached_tokens) }}</span>
              </div>
              <div v-else-if="row.cached_tokens != null" class="text-micro text-faint" title="全部输入命中缓存">⚡全命中 {{ int(row.cached_tokens) }}</div>
            </div>
          </template>
          <template #cell-credits="{ row }">
            <span v-if="row.credit_known" class="text-ink">{{ fmtCredits(row.credits) }}</span>
            <span v-else class="text-faint" title="上游未提供额度数据">—</span>
          </template>
          <template #cell-latency_ms="{ value }">{{ latency(value) }}</template>
          <template #cell-status="{ row }">
            <span class="inline-flex items-center gap-1.5">
              <WLed :tone="row.status === 'ok' ? 'live' : row.status === 'error' ? 'fault' : 'muted'" />
              <span class="text-small">{{ completionLabel(row) }}</span>
            </span>
          </template>
          <template #cell-actions="{ row }">
            <WButton size="sm" variant="subtle" @click="openDetail(row.id)">详情</WButton>
          </template>
          <template #empty><WEmpty title="没有调用记录" hint="调整筛选条件，或等待新的请求进来。" /></template>
        </WTable>
      </WCard>

      <!-- 分页 -->
      <div v-if="total > pageSize" class="mt-4 flex items-center justify-between text-small text-muted">
        <span>共 <span class="mono text-ink">{{ int(total) }}</span> 条</span>
        <div class="flex items-center gap-2">
          <WButton size="sm" variant="subtle" :disabled="page <= 1" @click="page--">上一页</WButton>
          <span class="mono">{{ page }} / {{ totalPages }}</span>
          <WButton size="sm" variant="subtle" :disabled="page >= totalPages" @click="page++">下一页</WButton>
        </div>
      </div>
    </template>

    <!-- 记录详情 -->
    <SessionsPanel v-if="tab === 'sessions'" />

    <WModal v-model:open="detailOpen" size="lg" title="调用详情">
      <WSpinner v-if="detailLoading" label="加载中" />
      <div v-else-if="detail" class="space-y-4">
        <div class="grid grid-cols-2 gap-3 sm:grid-cols-4">
          <div><div class="text-micro text-faint">模型</div><div class="text-small text-ink">{{ detail.model }}</div></div>
          <div><div class="text-micro text-faint">协议</div><div class="text-small text-ink">{{ detail.protocol }}</div></div>
          <div><div class="text-micro text-faint">延迟</div><div class="mono text-small text-ink">{{ latency(detail.latency_ms) }}</div></div>
          <div><div class="text-micro text-faint">状态</div><div class="text-small text-ink">{{ detail.status }}</div></div>
          <div><div class="text-micro text-faint">输入 Tokens</div><div class="mono text-small text-ink">{{ int(detail.input_tokens) }}</div></div>
          <div><div class="text-micro text-faint">输出 Tokens</div><div class="mono text-small text-ink">{{ int(detail.output_tokens) }}</div></div>
          <div v-if="detail.cached_tokens != null">
            <div class="text-micro text-faint">缓存命中</div>
            <div class="mono text-small">
              <span v-if="detail.cached_tokens < 0 || detail.cached_tokens > detail.input_tokens" class="text-warn">缓存数据异常 · 原始值 {{ int(detail.cached_tokens) }}（不计入命中率）</span>
              <span v-else-if="detail.cached_tokens === 0" class="text-warn">全量未命中 {{ int(detail.input_tokens) }}</span>
              <span v-else>
                <span class="text-ink">⚡命中 {{ int(detail.cached_tokens) }}</span>
                <span v-if="detail.input_tokens > detail.cached_tokens" class="text-warn"> · 未命中 {{ int(detail.input_tokens - detail.cached_tokens) }}</span>
              </span>
            </div>
          </div>
          <div v-if="detail.reasoning_effort"><div class="text-micro text-faint">推理强度</div><div class="text-small text-ink">{{ detail.reasoning_effort }}</div></div>
          <div><div class="text-micro text-faint">时间</div><div class="mono text-small text-ink">{{ dt(detail.ts, 'MM-DD HH:mm:ss') }}</div></div>
        </div>
        <div v-if="detail.diagnostics" class="rounded-lg border border-line p-3 text-small space-y-1">
          <div>结束原因：{{ detail.diagnostics.finish_reason || '未收到结束原因' }}</div>
          <div>客户端输出预算：{{ budgetLabel(detail.diagnostics.requested_output_limits) }}</div>
          <div>实际发送预算：{{ budgetLabel(detail.diagnostics.effective_output_limits) }}</div>
          <div>输入处理：{{ compatibilityLabel(detail.diagnostics.compatibility) }}</div>
          <div v-if="detail.diagnostics.error_kind">故障分类：{{ detail.diagnostics.error_kind === 'local_network' ? '本地网络或 DNS 不可达（未处罚账号）' : detail.diagnostics.error_kind }}</div>
      <div v-if="detail.diagnostics.performance" class="border-t border-line pt-2 space-y-1">
        <div class="mono break-all">请求标识：{{ detail.diagnostics.performance.request_id }}</div>
      <div>整次请求：{{ detail.diagnostics.performance.total_ms }} ms · 上游尝试 {{ detail.diagnostics.performance.attempts }} 次</div>
      <div>排队 / 账号等待：{{ detail.diagnostics.performance.queue_ms }} / {{ detail.diagnostics.performance.account_wait_ms }} ms</div>
      <div>首响应：{{ detail.diagnostics.performance.first_byte_ms == null ? '未写出响应' : detail.diagnostics.performance.first_byte_ms + ' ms' }} · 执行：{{ detail.diagnostics.performance.execution_ms == null ? '未发起调用' : detail.diagnostics.performance.execution_ms + ' ms' }}</div>
      <div v-for="stage in detail.diagnostics.performance.attempt_stages" :key="stage.number">第 {{ stage.number }} 次：{{ stage.headers_finished ? '响应头等待 ' + stage.header_wait_ms + ' ms · 状态 ' + (stage.http_status || '未收到 HTTP 响应') : '未收到响应头' }}</div>
      <div class="text-micro text-faint">首响应为网关首字节，非模型首个 token。重试记录共享整次请求计时。</div>
        <div v-if="detail.diagnostics.performance.attempts > (detail.diagnostics.performance.attempt_stages?.length || 0)" class="text-micro text-faint">仅保留前 16 次尝试的阶段记录。</div>
      </div>
          <div v-if="detail.status === 'incomplete'" class="text-warn">{{ completionLabel(detail) }}</div>
        </div>
        <div v-if="detail.error" class="rounded-lg border border-fault/40 bg-fault/5 p-3">
          <div class="mb-1 text-micro font-medium text-fault">错误</div>
          <pre class="mono whitespace-pre-wrap break-all text-small text-fault">{{ detail.error }}</pre>
        </div>
        <details v-if="detail.reasoning_content" open>
          <summary class="cursor-pointer text-small font-medium text-brand">思考链</summary>
          <pre class="mono mt-2 max-h-60 overflow-auto whitespace-pre-wrap break-all rounded-lg bg-bg/50 p-3 text-micro text-muted">{{ detail.reasoning_content }}</pre>
        </details>
        <details v-if="detail.input_content">
          <summary class="cursor-pointer text-small font-medium text-brand">输入</summary>
          <pre class="mono mt-2 max-h-60 overflow-auto whitespace-pre-wrap break-all rounded-lg bg-bg/50 p-3 text-micro text-muted">{{ detail.input_content }}</pre>
        </details>
        <details v-if="detail.output_content" open>
          <summary class="cursor-pointer text-small font-medium text-brand">输出</summary>
          <pre class="mono mt-2 max-h-60 overflow-auto whitespace-pre-wrap break-all rounded-lg bg-bg/50 p-3 text-micro text-muted">{{ detail.output_content }}</pre>
        </details>
      </div>
      <template #footer><WButton variant="primary" @click="detailOpen = false">关闭</WButton></template>
    </WModal>
  </WPage>
</template>
