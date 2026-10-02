<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { useRouter } from 'vue-router'
import { api } from '@/api/client'
import type { Overview, ProviderSummary } from '@/types'
import { int, abbr, credits as fmtCredits, dt } from '@/lib/format'
import { providerMeta } from '@/lib/providers'
import { buildLabelMap } from '@/utils/accountLabel'
import WPage from '@/components/ui/WPage.vue'
import WCard from '@/components/ui/WCard.vue'
import WStat from '@/components/ui/WStat.vue'
import WLed from '@/components/ui/WLed.vue'
import WTable, { type Column } from '@/components/ui/WTable.vue'
import WSpinner from '@/components/ui/WSpinner.vue'
import TrendChart from '@/components/TrendChart.vue'

const router = useRouter()
const ov = ref<Overview | null>(null)
const providers = ref<ProviderSummary[]>([])
const points = ref<{ bucket: string; bucket_ts: number; count: number; tokens: number }[]>([])
const loading = ref(true)
let timer: ReturnType<typeof setInterval> | null = null

const refreshFailed = ref(false)
let refreshing = false
async function load() {
  if (refreshing) return
  refreshing = true
  try {
    const [o, p, ts] = await Promise.all([api.overview(), api.getProviders(), api.usageTimeseries('hour', 24)])
    ov.value = o
    providers.value = p.providers || []
    points.value = ts.data || []
    refreshFailed.value = false
  } catch {
    refreshFailed.value = true
  } finally {
    refreshing = false
    loading.value = false
  }
}

function rejectionLabel(reason: string): string {
  return ({ model_queue_full: '模型队列满', key_queue_full: '密钥队列满', model_queue_timeout: '等待超时', request_capacity: '普通请求繁忙', heavy_admin_capacity: '重管理请求繁忙', body_read_capacity: '请求体读取繁忙', body_budget_exhausted: '请求体内存不足' } as Record<string,string>)[reason] || reason
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
  const s = p.status as any
  if (p.name === 'workbuddy') return `${s.healthy_count ?? 0}/${s.account_count ?? 0} 账号 · ${s.model_count ?? 0} 模型`
  if (p.name === 'qoder') return `${s.account_count ?? 0} 账号 · ${s.model_count ?? 0} 模型`
  return `${s.model_count ?? 0} 模型`
}

const recentCols: Column[] = [
  { key: 'ts', label: '时间', mono: true },
  { key: 'model', label: '模型' },
  { key: 'account_uid', label: '账号', mono: true },
  { key: 'total_tokens', label: 'Tokens', align: 'right', mono: true },
  { key: 'status', label: '状态' },
]
const recent = computed(() => (ov.value?.recent || []).slice(0, 8))

// 最近调用的账号列同流量日志：uid → 显示名（含 workbuddy 别名）。
// 概览返回的 accounts 已带 alias/nickname，直接建映射；未命中回退 uid 短码。
const labelMap = computed(() => buildLabelMap(ov.value?.accounts || []))
const acctLabel = (uid?: string | null) => (uid ? labelMap.value[uid] || uid.slice(0, 8) : '—')

onMounted(() => {
  load()
  timer = setInterval(load, 20000)
})
onUnmounted(() => timer && clearInterval(timer))
</script>

<template>
  <WPage title="概览" sub="网关运行状态与额度续航一览。">
    <div v-if="refreshFailed" class="mb-4 text-small text-warn">刷新失败，当前显示上次获取的数据。</div>
    <WCard v-if="ov?.admission" class="mb-4">
      <div class="flex flex-wrap gap-4 text-small">
        <span>模型执行 {{ ov.admission.running }} / {{ ov.admission.capacity }}</span>
        <span>排队 {{ ov.admission.queued }} / {{ ov.admission.queue_capacity }}</span>
        <span>最长等待 {{ (ov.admission.oldest_wait_ms / 1000).toFixed(1) }} 秒</span>
        <span>请求体占用 {{ ((ov.request_body_bytes || 0) / 1048576).toFixed(1) }} MiB</span>
      </div>
      <div v-if="Object.keys(ov.admission.rejected).length" class="mt-2 text-small text-faint">
        本次启动以来的容量拒绝：{{ Object.entries(ov.admission.rejected).map(([reason, count]) => `${rejectionLabel(reason)} ${count}`).join(' · ') }}
      </div>
    </WCard>
    <WSpinner v-if="loading" center label="加载中" />
    <template v-else>
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
            <div class="text-micro uppercase tracking-wide text-faint">预计可服务 Tokens（按当前额度与用量结构估算）</div>
            <div class="mono mt-1 text-4xl font-semibold leading-none text-brand">
              {{ pred?.predicted_tokens != null ? abbr(pred.predicted_tokens, 2) : '—' }}
            </div>
            <div class="mt-2 flex flex-wrap gap-x-6 gap-y-1 text-small text-muted">
              <span>剩余额度 <span class="mono text-ink">{{ fmtCredits(pred?.remaining_credits) }}</span></span>
              <span v-if="pred?.tokens_per_credit != null">每额度产出 <span class="mono text-ink">{{ abbr(pred.tokens_per_credit) }}</span> tok</span>
              <span>已消耗 <span class="mono text-ink">{{ abbr(pred?.tokens_used) }}</span> tok</span>
            </div>
          </div>
        </WCard>

        <div class="grid grid-cols-2 gap-4">
          <WStat label="可用渠道" :value="`${providers.filter((p) => p.name === 'workbuddy' ? healthy > 0 : p.ready).length}/${providers.length}`" :tone="serving ? 'live' : 'fault'" />
          <WStat label="模型数" :value="int(ov?.model_count)" tone="route" :sub="ov?.model_source === 'dynamic' ? '上游实时' : '内置'" />
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
