<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { useRouter } from 'vue-router'
import { api } from '@/api/client'
import type { Overview, ProviderSummary } from '@/types'
import { int, abbr, dt } from '@/lib/format'
import { providerMeta } from '@/lib/providers'
import { buildLabelMap } from '@/utils/accountLabel'
import WPage from '@/components/ui/WPage.vue'
import WCard from '@/components/ui/WCard.vue'
import WStat from '@/components/ui/WStat.vue'
import WLed from '@/components/ui/WLed.vue'
import WTable, { type Column } from '@/components/ui/WTable.vue'
import WSpinner from '@/components/ui/WSpinner.vue'
import TrendChart from '@/components/TrendChart.vue'
import BudgetDetails from '@/components/BudgetDetails.vue'

const router = useRouter()
const ov = ref<Overview | null>(null)
const providers = ref<ProviderSummary[]>([])
const points = ref<{ bucket: string; bucket_ts: number; count: number; tokens: number }[]>([])
const loading = ref(true)
let timer: ReturnType<typeof setInterval> | null = null

const refreshFailed = ref(false)
let refreshing = false
async function load() {
  if (refreshing || document.hidden) return
  refreshing = true
  try {
    const [o, ts] = await Promise.all([api.overview(), api.usageTimeseries('hour', 24)])
    ov.value = o
    providers.value = o.providers || []
    points.value = ts.data || []
    refreshFailed.value = false
  } catch {
    refreshFailed.value = true
  } finally {
    refreshing = false
    loading.value = false
  }
}

function memorySize(value: number | null | undefined): string {
  if (value == null) return '—'
  return `${(value / 1048576).toFixed(1)} MiB`
}

const healthy = computed(() => (ov.value?.accounts || []).filter((a) => a.healthy).length)
const serving = computed(() => providers.value.some((p) => p.name === 'workbuddy' ? healthy.value > 0 : p.ready))
const pred = computed(() => ov.value?.prediction)

// 供应商健康灯
function provLed(p: ProviderSummary): 'live' | 'fault' | 'muted' {
  if (!p.ready) return 'muted'
  if (p.name === 'workbuddy') return Number((p.status as any)?.healthy_count ?? 0) > 0 ? 'live' : 'fault'
  return 'live'
}
function provStat(p: ProviderSummary): string {
  const s = (p.status || {}) as any
  if (p.name === 'workbuddy') return `${s.healthy_count ?? 0}/${s.account_count ?? 0} 账号 · ${s.model_count ?? 0} 模型`
  if (p.name === 'qoder') return `${s.account_count ?? 0} 账号 · ${s.model_count ?? 0} 模型`
  return `${s.model_count ?? 0} 模型`
}

const recentCols: Column[] = [
  { nowrap: true, key: 'ts', label: '时间', mono: true },
  { nowrap: true, key: 'model', label: '模型' },
  { nowrap: true, key: 'account_uid', label: '账号', mono: true },
  { nowrap: true, key: 'total_tokens', label: 'Tokens', align: 'right', mono: true },
  { nowrap: true, key: 'status', label: '状态' },
]
const recent = computed(() => (ov.value?.recent || []).slice(0, 8))

// 用各渠道的安全名称投影映射调用账号；兼容旧服务只返回 accounts。
const labelMap = computed(() => buildLabelMap(ov.value?.display_accounts || ov.value?.accounts || []))
const acctLabel = (uid?: string | null) => (uid ? labelMap.value[uid] || uid.slice(0, 8) : '—')

function onVisibilityChange() {
  if (!document.hidden) void load()
}

onMounted(() => {
  document.addEventListener('visibilitychange', onVisibilityChange)
  load()
  timer = setInterval(load, 20000)
})
onUnmounted(() => {
  if (timer) clearInterval(timer)
  document.removeEventListener('visibilitychange', onVisibilityChange)
})
</script>

<template>
  <WPage title="概览" sub="网关运行状态与额度续航一览。">
    <div v-if="refreshFailed" class="mb-4 text-small text-warn">刷新失败，当前显示上次获取的数据。</div>
    <WCard v-if="ov?.admission" class="mb-4">
      <span class="text-small text-muted">正在执行 <span class="mono font-medium text-ink">{{ ov.admission.running }}</span> 个请求</span>
      <p v-if="!ov.admission.limits_disabled && ov.admission.memory_pressure" class="mt-2 text-small text-warn">内存压力保护中，请求等待恢复。</p>
    </WCard>
    <WSpinner v-if="loading" center label="加载中" />
    <WCard v-if="ov?.call_metrics" class="mb-4" title="最近一小时调用">
      <div class="flex flex-wrap gap-4 text-small"><span>请求 {{ ov.call_metrics.requests }}</span><span>失败 {{ ov.call_metrics.errors }}</span><span>输出未完成 {{ ov.call_metrics.incomplete }}</span></div>
      <details class="mt-3 text-small text-muted"><summary class="cursor-pointer">调用诊断</summary>
        <div class="mt-2 flex flex-wrap gap-x-5 gap-y-2">
          <span>上游尝试 {{ ov.call_metrics.attempts }}</span><span>上游限流 {{ ov.call_metrics.upstream_429 }}</span>
          <span>等待 P95 ≤ {{ ov.call_metrics.wait_p95_ms == null ? '—' : ov.call_metrics.wait_p95_ms + ' ms' }}</span>
          <span>上游执行 P95 ≤ {{ ov.call_metrics.execution_p95_ms == null ? '—' : ov.call_metrics.execution_p95_ms + ' ms' }}</span>
          <span title="首字节，可能是协议事件；不是模型首生成">首响应 P95 ≤ {{ ov.call_metrics.first_byte_p95_ms == null ? '—' : ov.call_metrics.first_byte_p95_ms + ' ms' }}</span>
          <span>总耗时 P95 ≤ {{ ov.call_metrics.total_p95_ms == null ? '—' : ov.call_metrics.total_p95_ms + ' ms' }}</span>
        </div><p class="mt-2 text-micro text-faint">P95 为区间估算，统计随服务重启重新开始。逐次尝试见调用日志详情。</p>
      </details>
    </WCard>
    <WCard v-if="ov?.resource_metrics" class="mb-4">
      <details class="text-small">
        <summary class="cursor-pointer text-muted">资源与日志维护</summary>
        <dl class="mt-3 grid gap-x-6 gap-y-3 sm:grid-cols-2 lg:grid-cols-3">
          <div><dt class="text-micro text-faint" title="Go 运行时内存，不等于进程或整机内存">Go 内存 / 堆对象</dt><dd class="mono mt-1">{{ memorySize(ov.resource_metrics.go_memory_in_use_bytes) }} / {{ memorySize(ov.resource_metrics.heap_objects_bytes) }}</dd></div>
          <div><dt class="text-micro text-faint">请求体内存 / Goroutines</dt><dd class="mono mt-1">{{ memorySize(ov.resource_metrics.request_body_bytes) }} / {{ int(ov.resource_metrics.goroutines) }}</dd></div>
          <div><dt class="text-micro text-faint">数据库 / WAL</dt><dd class="mono mt-1">{{ memorySize(ov.resource_metrics.database_bytes) }} / {{ memorySize(ov.resource_metrics.wal_bytes) }}</dd></div>
          <div><dt class="text-micro text-faint">日志写入平均耗时 / 失败</dt><dd class="mono mt-1">{{ ov.resource_metrics.usage.write_average_ms.toFixed(2) }} ms / {{ int(ov.resource_metrics.usage.write_errors) }}</dd></div>
          <div><dt class="text-micro text-faint">日志维护</dt><dd class="mt-1">{{ ov.resource_metrics.usage.maintenance_running ? '进行中' : '空闲' }} · {{ int(ov.resource_metrics.usage.maintenance_runs) }} 次 · 失败 {{ int(ov.resource_metrics.usage.maintenance_errors) }}</dd></div>
          <div><dt class="text-micro text-faint">上次维护耗时</dt><dd class="mono mt-1">{{ ov.resource_metrics.usage.maintenance_runs ? ov.resource_metrics.usage.maintenance_last_ms + ' ms' : '—' }}</dd></div>
        </dl>

      </details>
    </WCard>
    <template v-if="ov">
      <!-- 预警条 -->
      <div v-if="ov?.alerts?.length" class="mb-4 space-y-2">
        <div
          v-for="(a, i) in ov.alerts"
          :key="i"
          class="flex items-center gap-2 rounded-lg border px-3.5 py-2.5 text-small"
          :class="a.level === 'warning' ? 'border-warn/40 bg-warn/5 text-warn' : 'border-route/40 bg-route/5 text-route'"
        >
          <WLed :tone="a.level === 'warning' ? 'warn' : 'route'" />
          <span>{{ a.message }}</span>
        </div>
      </div>

      <!-- 英雄区：续航 + 网关状态 -->
      <div class="mb-4 grid gap-4 lg:grid-cols-[1.4fr_1fr]">
        <WCard hover>
          <div class="flex items-center gap-2.5">
            <WLed :tone="serving ? 'live' : 'fault'" :pulse="serving" />
            <span class="text-small font-medium" :class="serving ? 'text-live' : 'text-fault'">
              {{ serving ? '网关正在服务' : '暂无健康账号，网关降级' }}
            </span>
          </div>
          <div class="mt-4">
            <div class="text-micro uppercase tracking-wide text-faint">预计付费续航 · Tokens</div>
            <div class="mono mt-1 text-4xl font-semibold leading-none text-brand">
              {{ pred?.predicted_tokens != null ? abbr(pred.predicted_tokens, 2) : '—' }}
            </div>
            <div class="mt-2 flex flex-wrap gap-x-6 gap-y-1 text-small text-muted">
              <span v-if="pred?.partial" class="text-warn">部分余额已计入</span>
              <span v-else>按近期付费调用估算</span>
              <BudgetDetails :prediction="pred" />
            </div>
          </div>
        </WCard>

        <div class="grid grid-cols-2 gap-4">
          <WStat label="可用渠道" :value="`${providers.filter((p) => p.name === 'workbuddy' ? healthy > 0 : p.ready).length}/${providers.length}`" :tone="serving ? 'live' : 'fault'" />
          <WStat label="模型数" :value="int(ov?.model_count)" tone="route" />
          <WStat label="今日请求" :value="int(ov?.usage?.today_requests)" tone="brand" />
          <WStat label="今日 Tokens" :value="abbr(ov?.usage?.today_tokens)" />
        </div>
      </div>

      <!-- 供应商卡片 -->
      <div class="mb-4 grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
        <button
          v-for="p in providers"
          :key="p.name"
          class="glass flex items-center gap-3 rounded-xl p-4 text-left transition-colors hover:border-brand/50"
          @click="router.push('/accounts')"
        >
          <div class="flex h-9 w-9 items-center justify-center rounded-lg text-small font-semibold" :class="providerMeta(p.name).chip">
            {{ providerMeta(p.name).short }}
          </div>
          <div class="min-w-0 flex-1">
            <div class="flex items-center gap-2">
              <span class="font-medium text-ink">{{ p.display_name || providerMeta(p.name).label }}</span>
              <WLed :tone="provLed(p)" :pulse="provLed(p) === 'live'" />
            </div>
            <div class="truncate text-micro text-faint">{{ p.ready ? provStat(p) : (p.notes || '未就绪') }}</div>
          </div>
        </button>
      </div>

      <!-- 趋势 + 近期调用 -->
      <div class="grid gap-4 lg:grid-cols-[1fr_1fr]">
        <WCard title="近 24 小时请求"><TrendChart :data="points" metric="count" /></WCard>
        <WCard title="最近调用" flush>
          <WTable :columns="recentCols" :rows="recent" row-key="id" min-width="420px">
            <template #cell-ts="{ value }">{{ dt(value, 'HH:mm:ss') }}</template>
            <template #cell-account_uid="{ value }">{{ acctLabel(value) }}</template>
            <template #cell-total_tokens="{ value }">{{ int(value) }}</template>
            <template #cell-status="{ row }">
              <WLed :tone="row.status === 'ok' ? 'live' : row.status === 'error' ? 'fault' : 'muted'" />
            </template>
          </WTable>
        </WCard>
      </div>
    </template>
  </WPage>
</template>
