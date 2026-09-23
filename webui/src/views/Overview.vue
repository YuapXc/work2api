<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { useRouter } from 'vue-router'
import { api } from '@/api/client'
import type { Overview, UsagePoint } from '@/types'
import { buildLabelMap, labelOf as labelOfMap } from '@/utils/accountLabel'
import dayjs from 'dayjs'
import * as echarts from 'echarts/core'
import { LineChart } from 'echarts/charts'
import { GridComponent, TooltipComponent } from 'echarts/components'
import { CanvasRenderer } from 'echarts/renderers'

echarts.use([LineChart, GridComponent, TooltipComponent, CanvasRenderer])

const router = useRouter()
const REFRESH_MS = 20000 // 每 20s 自动刷新
let timer: ReturnType<typeof setInterval> | null = null

const data = ref<Overview>({
  accounts: [],
  alerts: [],
  models: [],
  usage: {
    total_requests: 0, total_tokens: 0, today_requests: 0, today_tokens: 0,
    by_protocol: [], by_model: [],
  },
  recent: [],
})
const loading = ref(false)
// 上游只返回 model_count（不返回完整 models 数组），单独存，供瓦片读数用
const modelCount = ref(0)

// 空态默认：/admin/overview 在无数据时可能省略部分字段，统一兜底避免读取 undefined 崩页
const EMPTY_USAGE = {
  total_requests: 0, total_tokens: 0, today_requests: 0, today_tokens: 0,
  by_protocol: [] as any[], by_model: [] as any[], by_account: [] as any[],
}

const healthyCount = computed(() => data.value.accounts.filter((a) => a.healthy).length)
const gatewayServing = computed(() => healthyCount.value > 0)

// uid → 显示名（含重名消歧）。账号列表来自 overview 的 accounts 字段。
const labelMap = computed(() => buildLabelMap(data.value.accounts || []))
const labelOf = (uid?: string | null) => labelOfMap(labelMap.value, uid)

// 账号池节点：按健康 / 冷却 / 禁用染色，供状态总线渲染
const now = ref(Date.now() / 1000)
function accountState(a: { enabled: boolean; healthy: boolean; cooldown_until: number }): 'live' | 'charge' | 'fault' {
  if (!a.enabled) return 'fault'
  if (a.cooldown_until && a.cooldown_until > now.value) return 'charge'
  return a.healthy ? 'live' : 'fault'
}
const poolNodes = computed(() =>
  (data.value.accounts || []).map((a) => ({
    uid: a.uid,
    label: labelOf(a.uid),
    state: accountState(a),
  })),
)

// 上游站点 chip：按账号 site 聚合，任一健康则 chip 为 live
const upstreamChips = computed(() => {
  const groups: Record<string, { label: string; total: number; healthy: number }> = {}
  for (const a of data.value.accounts || []) {
    const key = a.site === 'domestic' ? 'cn' : a.site === 'international' ? 'intl' : (a.site || 'other')
    const label = a.site === 'domestic' ? '国内' : a.site === 'international' ? '国际' : (a.site || '其他')
    if (!groups[key]) groups[key] = { label, total: 0, healthy: 0 }
    groups[key].total++
    if (a.healthy && a.enabled) groups[key].healthy++
  }
  return Object.entries(groups).map(([key, v]) => ({ key, ...v }))
})

// 读数瓦片：账号 / 模型 / 额度 / 今日请求 / 今日 tokens
const totalCredits = computed(() =>
  (data.value.accounts || []).reduce((s, a) => s + (a.credits_remaining ?? 0), 0),
)
const tiles = computed(() => [
  { label: '健康账号', value: `${healthyCount.value}/${data.value.accounts.length}`, tone: gatewayServing.value ? 'live' : 'fault' },
  { label: '可用模型', value: fmt(modelCount.value), tone: 'route' },
  { label: '剩余额度', value: fmtCredits(totalCredits.value), tone: 'charge' },
  { label: '今日请求', value: fmt(data.value.usage.today_requests), tone: 'route' },
  { label: '今日 Tokens', value: fmtK(data.value.usage.today_tokens), tone: 'route' },
])

// 按账号调用分布
const accountStats = computed(() => (data.value.usage?.by_account || []))

// 最近记录限条数
const RECENT_LIMIT = 6
const recentLimited = computed(() => (data.value.recent ?? []).slice(0, RECENT_LIMIT))

// 积分 → 预测 token
const prediction = computed(() => data.value.prediction)
function fmtPredict(n: number | null | undefined) {
  if (n == null || n === 0) return '-'
  if (n >= 1000000) return `${(n / 1000000).toFixed(1)}M`
  if (n >= 1000) return `${(n / 1000).toFixed(1)}K`
  return n.toLocaleString()
}

// 趋势折线图
const chartEl = ref<HTMLDivElement | null>(null)
let chart: echarts.ECharts | null = null
const tsData = ref<UsagePoint[]>([])

function fmt(n: number) { return (n || 0).toLocaleString() }
function fmtK(v: number) {
  if (v >= 1000000) return `${(v / 1000000).toFixed(1)}M`
  if (v >= 1000) return `${(v / 1000).toFixed(1)}K`
  return String(v || 0)
}
function fmtCredits(v: number) {
  if (!v) return '0'
  return v >= 1000 ? `${(v / 1000).toFixed(1)}K` : v.toLocaleString(undefined, { maximumFractionDigits: 0 })
}
function fmtTime(ts?: number) {
  if (!ts) return '-'
  const d = dayjs(ts * 1000)
  const nowD = dayjs()
  if (d.isSame(nowD, 'day')) return `今天 ${d.format('HH:mm:ss')}`
  if (d.isSame(nowD.subtract(1, 'day'), 'day')) return `昨天 ${d.format('HH:mm:ss')}`
  return d.format('MM-DD HH:mm')
}

/** 成功率文本（无记录时显示 —）。 */
function okRateText(record: { count: number; ok_count?: number }) {
  if (!record.count) return '—'
  const ok = record.ok_count ?? 0
  return `${((ok / record.count) * 100).toFixed(0)}%`
}
/** 成功率着色：<90% charge、<70% fault。 */
function okRateColor(record: { count: number; ok_count?: number }) {
  if (!record.count) return 'var(--fog)'
  const r = (record.ok_count ?? 0) / record.count
  if (r >= 0.9) return 'var(--live)'
  if (r >= 0.7) return 'var(--charge)'
  return 'var(--fault)'
}

async function load() {
  loading.value = true
  const safety = setTimeout(() => { loading.value = false }, 8000)
  try {
    const r: any = await api.overview()
    // 归一化：缺字段一律兜底，模型数量取 model_count（上游不回 models 数组）
    modelCount.value = r.model_count ?? (Array.isArray(r.models) ? r.models.length : 0)
    data.value = {
      accounts: r.accounts ?? [],
      alerts: r.alerts ?? [],
      models: r.models ?? [],
      usage: { ...EMPTY_USAGE, ...(r.usage ?? {}) },
      recent: r.recent ?? [],
      prediction: r.prediction,
    }
  } catch { /* 拦截器已提示 */ } finally {
    clearTimeout(safety)
    loading.value = false
  }
}

async function loadTs() {
  const res: any = await api.usageTimeseries('hour', 24)
  tsData.value = res.data ?? []
  renderChart()
}

function renderChart() {
  if (!chartEl.value) return
  if (!chart) chart = echarts.init(chartEl.value)
  chart.setOption({
    tooltip: { trigger: 'axis', backgroundColor: '#06121A', borderColor: '#24454F', textStyle: { color: '#E6F0EE' } },
    grid: { left: 44, right: 16, top: 18, bottom: 26 },
    xAxis: {
      type: 'category', data: tsData.value.map((d) => d.bucket),
      axisLine: { lineStyle: { color: '#24454F' } },
      axisLabel: { color: '#7E9BA3', interval: 2, rotate: 45, fontFamily: 'JetBrains Mono' },
    },
    yAxis: {
      type: 'value',
      splitLine: { lineStyle: { color: '#1B363E' } },
      axisLabel: { color: '#7E9BA3', fontFamily: 'JetBrains Mono' },
    },
    series: [{
      name: '调用次数', type: 'line', data: tsData.value.map((d) => d.count), smooth: true,
      symbol: 'circle', symbolSize: 4,
      lineStyle: { width: 2, color: '#5AA9E6' },
      itemStyle: { color: '#5AA9E6' },
      areaStyle: { color: new echarts.graphic.LinearGradient(0, 0, 0, 1, [
        { offset: 0, color: 'rgba(90,169,230,0.28)' },
        { offset: 1, color: 'rgba(90,169,230,0.01)' },
      ]) },
    }],
  })
}

function onResize() { chart?.resize() }

onMounted(async () => {
  await Promise.allSettled([load(), loadTs()])
  timer = setInterval(() => { now.value = Date.now() / 1000; Promise.allSettled([load(), loadTs()]) }, REFRESH_MS)
  window.addEventListener('resize', onResize)
})

onUnmounted(() => {
  if (timer) clearInterval(timer)
  window.removeEventListener('resize', onResize)
  chart?.dispose()
  chart = null
})
</script>

<template>
  <div class="overview">
    <div class="view-header">
      <div>
        <div class="vh-title">概览</div>
        <div class="vh-meta">
          <span class="led" :class="gatewayServing ? 'live' : 'fault'"></span>
          {{ gatewayServing ? '网关正在服务' : '网关降级：无健康账号' }}
        </div>
      </div>
    </div>

    <!-- 预警横幅（放在 spin 之外，避免轮询时闪烁） -->
    <a-alert
      v-for="(al, i) in (data.alerts || [])"
      :key="i"
      :type="al.level === 'warning' ? 'warning' : 'info'"
      :message="al.message"
      show-icon
      style="margin-bottom: 12px"
    />

    <!-- ========== 状态总线：唯一的大动作 ========== -->
    <section class="bus" :class="{ degraded: !gatewayServing }">
      <div class="bus-track">
        <!-- client 节点 -->
        <div class="bus-node client">
          <div class="node-box"><span class="node-glyph">◇</span></div>
          <div class="node-cap">客户端</div>
        </div>

        <span class="wire"><span class="pulse"></span></span>

        <!-- 网关核心 -->
        <div class="bus-node core">
          <div class="node-box core-box">
            <span class="led" :class="gatewayServing ? 'live' : 'fault'"></span>
            <span class="core-name mono">work2api</span>
          </div>
          <div class="node-cap" :style="{ color: gatewayServing ? 'var(--live)' : 'var(--fault)' }">
            {{ gatewayServing ? '正在服务' : '降级' }}
          </div>
        </div>

        <span class="wire"><span class="pulse"></span></span>

        <!-- 账号池：一排小节点，按健康染色 -->
        <div class="bus-node pool">
          <div class="pool-grid">
            <template v-if="poolNodes.length">
              <a-tooltip v-for="n in poolNodes" :key="n.uid" :title="n.label">
                <span class="pool-dot" :class="n.state"></span>
              </a-tooltip>
            </template>
            <span v-else class="pool-empty">无账号</span>
          </div>
          <div class="node-cap"><span class="mono">{{ healthyCount }}</span>/<span class="mono">{{ data.accounts.length }}</span> 账号池</div>
        </div>

        <span class="wire"><span class="pulse"></span></span>

        <!-- 上游站点 chip -->
        <div class="bus-node upstream">
          <div class="chip-col">
            <template v-if="upstreamChips.length">
              <span
                v-for="c in upstreamChips"
                :key="c.key"
                class="site-chip"
                :class="c.healthy > 0 ? 'up' : 'down'"
              >
                <span class="led" :class="c.healthy > 0 ? 'live' : 'fault'"></span>
                {{ c.label }} <span class="mono">{{ c.healthy }}/{{ c.total }}</span>
              </span>
            </template>
            <span v-else class="pool-empty">无上游</span>
          </div>
          <div class="node-cap">上游站点</div>
        </div>
      </div>
    </section>

    <!-- ========== 读数瓦片行（扁平 hairline，非渐变卡片） ========== -->
    <div class="tiles">
      <div v-for="t in tiles" :key="t.label" class="tile">
        <div class="tile-num mono">{{ t.value }}</div>
        <div class="tile-label">{{ t.label }}</div>
        <div class="tile-underline" :class="t.tone"></div>
      </div>
    </div>

    <a-spin :spinning="loading">
      <!-- 积分 → 预测 token -->
      <section class="panel" v-if="prediction">
        <div class="panel-title">积分额度预测</div>
        <p class="predict-line" v-if="prediction?.predicted_tokens">
          当前剩余约 <span class="mono hl">{{ prediction.remaining_credits }}</span> 积分，预计可兑换约
          <span class="mono hl route">{{ fmtPredict(prediction.predicted_tokens) }}</span> Token
          <span class="predict-note" v-if="prediction?.tokens_per_credit">（按模型加权平均每 1 积分约 {{ fmt(prediction.tokens_per_credit) }} Token）</span>
        </p>
        <p class="predict-line" v-else>
          当前剩余约 <span class="mono hl">{{ prediction?.remaining_credits ?? '-' }}</span> 积分，历史消耗数据不足，暂无法预测可兑换 Token
        </p>
        <div v-if="prediction?.models?.length" class="predict-models">
          <div v-for="m in prediction.models.slice(0, 5)" :key="m.model" class="predict-model">
            <span class="pm-name mono">{{ m.model }}</span>
            <span class="pm-meta">权重 {{ (m.weight * 100).toFixed(0) }}% · 每 1 积分约 {{ fmt(m.tokens_per_credit) }} Token</span>
            <span class="pm-val mono">{{ fmtPredict(m.predicted_tokens) }}</span>
          </div>
        </div>
      </section>

      <!-- 请求趋势 -->
      <section class="panel">
        <div class="panel-title">请求趋势（近 24 小时）</div>
        <div ref="chartEl" style="width: 100%; height: 180px"></div>
      </section>

      <!-- 按账号调用分布 -->
      <section v-if="accountStats.length" class="panel">
        <div class="panel-title">账号调用分布</div>
        <a-table
          :data-source="accountStats"
          :pagination="false"
          size="small"
          row-key="account_uid"
          :locale="{ emptyText: '暂无记录' }"
        >
          <a-table-column title="账号" key="label" align="left">
            <template #default="{ record }">
              <a-tooltip :title="record.account_uid || ''">
                <span :style="{ color: record.removed ? 'var(--fog)' : 'var(--paper)' }">
                  {{ labelOf(record.account_uid) }}
                </span>
              </a-tooltip>
              <a-tag v-if="record.removed" color="default" style="margin-left: 6px">已移除</a-tag>
              <a-tag v-else-if="record.enabled === false" color="default" style="margin-left: 6px">已停用</a-tag>
            </template>
          </a-table-column>
          <a-table-column title="站点" key="site" align="left" :width="80">
            <template #default="{ record }">
              <span style="color: var(--fog)">{{ record.site_label || record.site || '—' }}</span>
            </template>
          </a-table-column>
          <a-table-column title="调用" key="count" align="right" :width="80">
            <template #default="{ record }"><span class="mono">{{ fmt(record.count) }}</span></template>
          </a-table-column>
          <a-table-column title="成功率" key="ok" align="right" :width="90">
            <template #default="{ record }">
              <span class="mono" :style="{ color: okRateColor(record) }">{{ okRateText(record) }}</span>
            </template>
          </a-table-column>
          <a-table-column title="Tokens" key="tokens" align="right" :width="110">
            <template #default="{ record }"><span class="mono">{{ fmtK(record.tokens) }}</span></template>
          </a-table-column>
          <a-table-column title="积分" key="credits" align="right" :width="90">
            <template #default="{ record }">
              <span v-if="record.credits > 0" class="mono" style="color: var(--charge)">{{ record.credits.toFixed(2) }}</span>
              <a-tooltip v-else-if="record.unknown_count === 0 && record.ok_count && record.ok_count > 0" title="该账号的成功调用均明确返回 0 积分">
                <span style="color: var(--live)">免费</span>
              </a-tooltip>
              <a-tooltip v-else title="存在未记录积分的调用，无法判断是否收费">
                <span style="color: var(--fog)">未知</span>
              </a-tooltip>
            </template>
          </a-table-column>
        </a-table>
      </section>

      <!-- 最近使用记录 -->
      <section class="panel">
        <div class="panel-head">
          <div class="panel-title" style="margin-bottom: 0">最近使用记录</div>
          <a class="panel-more" @click="router.push('/records')">查看全部</a>
        </div>
        <a-table
          :data-source="recentLimited"
          :pagination="false"
          size="small"
          row-key="id"
          :locale="{ emptyText: '暂无记录' }"
          :scroll="{ y: 260 }"
        >
          <a-table-column title="时间" data-index="ts" key="ts" :width="130">
            <template #default="{ record }"><span class="mono">{{ fmtTime(record.ts) }}</span></template>
          </a-table-column>
          <a-table-column title="协议" data-index="protocol" key="protocol" align="left" :width="90" />
          <a-table-column title="模型" data-index="model" key="model" align="left">
            <template #default="{ record }"><span class="mono">{{ record.model }}</span></template>
          </a-table-column>
          <a-table-column title="账号" key="account" align="left" :width="130" ellipsis>
            <template #default="{ record }">
              <a-tooltip :title="record.account_uid || ''">
                <span style="color: var(--fog)">{{ labelOf(record.account_uid) }}</span>
              </a-tooltip>
            </template>
          </a-table-column>
          <a-table-column title="Tokens" data-index="total_tokens" key="total_tokens" align="right" :width="100">
            <template #default="{ record }"><span class="mono">{{ fmt(record.total_tokens) }}</span></template>
          </a-table-column>
          <a-table-column title="积分" data-index="credits" key="credits" align="right" :width="70">
            <template #default="{ record }">
              <span v-if="record.credits" class="mono" style="color: var(--charge)">{{ record.credits.toFixed(2) }}</span>
              <a-tooltip v-else-if="record.status === 'ok' && record.credit_known === true && record.credits === 0" title="上游明确返回 0 积分">
                <span style="color: var(--live)">免费</span>
              </a-tooltip>
              <a-tooltip v-else-if="record.status === 'ok'" title="上游积分未返回或旧版未保存，无法判断是否收费">
                <span style="color: var(--fog)">未知</span>
              </a-tooltip>
              <span v-else style="color: var(--fog)">-</span>
            </template>
          </a-table-column>
          <a-table-column title="状态" data-index="status" key="status" align="left" :width="90">
            <template #default="{ record }">
              <span class="cell-status">
                <span class="led" :class="record.status === 'ok' ? 'live' : 'fault'"></span>
                {{ record.status === 'ok' ? '成功' : record.status }}
              </span>
            </template>
          </a-table-column>
        </a-table>
      </section>
    </a-spin>
  </div>
</template>

<style scoped>
.overview { width: 100%; }

/* ========== 状态总线 ========== */
.bus {
  background: var(--panel);
  border: 1px solid var(--line);
  border-radius: var(--radius);
  padding: 22px 24px;
  margin-bottom: 20px;
  overflow-x: auto;
}
.bus-track { display: flex; align-items: stretch; gap: 8px; min-width: 640px; }
.bus-node { display: flex; flex-direction: column; align-items: center; gap: 8px; justify-content: center; }
.node-box {
  min-height: 44px; display: flex; align-items: center; justify-content: center; gap: 8px;
  padding: 0 14px; border: 1px solid var(--line); border-radius: var(--radius);
  background: var(--panel-2);
}
.node-glyph { color: var(--fog); font-size: 18px; }
.core-box { border-color: var(--route); background: rgba(90,169,230,0.08); }
.bus.degraded .core-box { border-color: var(--fault); background: rgba(242,121,94,0.08); }
.core-name { color: var(--paper); font-weight: 500; font-size: 14px; }
.node-cap { font-size: 12px; color: var(--fog); white-space: nowrap; }

/* 布线 + 信号脉冲 */
.wire {
  flex: 1; min-width: 40px; align-self: center; height: 2px; position: relative;
  background: var(--line); border-radius: 2px; overflow: hidden;
}
.pulse {
  position: absolute; top: 0; left: -30%; width: 30%; height: 100%;
  background: linear-gradient(90deg, transparent, var(--route), transparent);
}

/* 账号池小节点 */
.pool-grid {
  display: flex; flex-wrap: wrap; gap: 6px; max-width: 260px; justify-content: center;
  padding: 8px 10px; border: 1px solid var(--line); border-radius: var(--radius); background: var(--panel-2);
  min-height: 44px; align-items: center;
}
.pool-dot { width: 12px; height: 12px; border-radius: 3px; background: var(--fog); }
.pool-dot.live { background: var(--live); }
.pool-dot.charge { background: var(--charge); }
.pool-dot.fault { background: var(--fault); }
.pool-empty { color: var(--fog); font-size: 12px; }

/* 上游 chip */
.chip-col {
  display: flex; flex-direction: column; gap: 6px; justify-content: center;
  min-height: 44px;
}
.site-chip {
  display: inline-flex; align-items: center; gap: 6px;
  padding: 4px 10px; border: 1px solid var(--line); border-radius: var(--radius);
  font-size: 12px; color: var(--paper); background: var(--panel-2); white-space: nowrap;
}
.site-chip .mono { color: var(--fog); }

/* 信号沿总线扫一遍（仅入场一次） */
@keyframes sweep { from { left: -30%; } to { left: 100%; } }
.pulse { animation: sweep 1.1s ease-out 1 both; }
.bus-track .wire:nth-of-type(2) .pulse { animation-delay: 0.18s; }
.bus-track .wire:nth-of-type(3) .pulse { animation-delay: 0.36s; }
@media (prefers-reduced-motion: reduce) {
  .pulse { animation: none; opacity: 0; }
}

/* ========== 读数瓦片行 ========== */
.tiles {
  display: grid; grid-template-columns: repeat(5, 1fr); gap: 0;
  border: 1px solid var(--line); border-radius: var(--radius);
  background: var(--panel); margin-bottom: 20px; overflow: hidden;
}
.tile { padding: 16px 18px; border-left: 1px solid var(--line-2); }
.tile:first-child { border-left: none; }
.tile-num { font-size: 25px; font-weight: 500; color: var(--paper); line-height: 1.1; }
.tile-label { font-size: 12px; color: var(--fog); margin-top: 4px; }
.tile-underline { height: 2px; width: 28px; margin-top: 10px; border-radius: 2px; background: var(--fog); }
.tile-underline.live { background: var(--live); }
.tile-underline.route { background: var(--route); }
.tile-underline.charge { background: var(--charge); }
.tile-underline.fault { background: var(--fault); }
@media (max-width: 900px) {
  .tiles { grid-template-columns: repeat(2, 1fr); }
  .tile:nth-child(odd) { border-left: none; }
  .tile { border-top: 1px solid var(--line-2); }
  .tile:nth-child(-n+2) { border-top: none; }
}

/* ========== 面板（扁平 hairline） ========== */
.panel {
  background: var(--panel); border: 1px solid var(--line); border-radius: var(--radius);
  padding: 16px 18px; margin-bottom: 16px;
}
.panel-head { display: flex; align-items: center; justify-content: space-between; margin-bottom: 12px; }
.panel-title { font-size: 15px; font-weight: 500; color: var(--paper); margin-bottom: 12px; }
.panel-more { color: var(--route); font-size: 13px; cursor: pointer; }
.panel-more:hover { color: #6FB6EC; }

/* 积分预测 */
.predict-line { font-size: 14px; color: var(--paper); margin: 0 0 4px; line-height: 1.6; }
.predict-line .hl { color: var(--paper); font-weight: 500; }
.predict-line .hl.route { color: var(--route); font-size: 16px; }
.predict-note { color: var(--fog); font-size: 12px; }
.predict-models { margin-top: 12px; display: flex; flex-direction: column; gap: 8px; }
.predict-model { display: flex; align-items: center; gap: 12px; font-size: 12.5px; padding: 6px 0; border-top: 1px solid var(--line-2); }
.pm-name { color: var(--paper); min-width: 140px; }
.pm-meta { color: var(--fog); flex: 1; }
.pm-val { color: var(--route); font-weight: 500; }

/* 表格状态单元 */
.cell-status { display: inline-flex; align-items: center; gap: 6px; color: var(--paper); }
</style>
