<script setup lang="ts">
import { ref, computed, onMounted, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '@/api/client'
import type { UsageSummary, UsagePoint, UsageRecord, RecordsResponse } from '@/types'
import { toast } from '@/lib/toast'
import { int, abbr, credits as fmtCredits, dt, latency } from '@/lib/format'
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
import { buildLabelMap } from '@/utils/accountLabel'

const route = useRoute()
const router = useRouter()
const tab = ref((route.query.tab as string) === 'logs' ? 'logs' : 'analytics')
watch(tab, (t) => router.replace({ query: t === 'logs' ? { tab: 'logs' } : {} }))

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

onMounted(() => {
  loadAnalytics()
  loadFilters()
  loadAccountLabels()
  loadLogs()
})
</script>

<template>
  <WPage title="流量" sub="调用量分析与逐条调用日志。">
    <template #actions>
      <WTabs v-model="tab" :tabs="[{ key: 'analytics', label: '分析' }, { key: 'logs', label: '日志' }]" />
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
    <template v-else>
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
            <span class="text-ink">{{ int(row.input_tokens) }}</span><span class="text-faint"> / {{ int(row.output_tokens) }}</span>
          </template>
          <template #cell-credits="{ row }">
            <span v-if="row.credit_known" class="text-ink">{{ fmtCredits(row.credits) }}</span>
            <span v-else class="text-faint" title="上游未提供额度数据">—</span>
          </template>
          <template #cell-latency_ms="{ value }">{{ latency(value) }}</template>
          <template #cell-status="{ row }">
            <span class="inline-flex items-center gap-1.5">
              <WLed :tone="row.status === 'ok' ? 'live' : row.status === 'error' ? 'fault' : 'muted'" />
              <span class="text-small">{{ row.status }}</span>
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
          <div v-if="detail.reasoning_effort"><div class="text-micro text-faint">推理强度</div><div class="text-small text-ink">{{ detail.reasoning_effort }}</div></div>
          <div><div class="text-micro text-faint">时间</div><div class="mono text-small text-ink">{{ dt(detail.ts, 'MM-DD HH:mm:ss') }}</div></div>
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
