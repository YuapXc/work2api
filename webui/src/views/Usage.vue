<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { api } from '@/api/client'
import type { UsageSummary, UsagePoint } from '@/types'
import { ThunderboltOutlined, FileTextOutlined, DatabaseOutlined, ApartmentOutlined } from '@ant-design/icons-vue'
import * as echarts from 'echarts/core'
import { LineChart } from 'echarts/charts'
import { GridComponent, TooltipComponent, LegendComponent } from 'echarts/components'
import { CanvasRenderer } from 'echarts/renderers'

echarts.use([LineChart, GridComponent, TooltipComponent, LegendComponent, CanvasRenderer])

const summary = ref<UsageSummary>({
  total_requests: 0, total_tokens: 0, today_requests: 0, today_tokens: 0,
  by_protocol: [], by_model: [],
})

const chartEl = ref<HTMLDivElement | null>(null)
let chart: echarts.ECharts | null = null
const granularity = ref<'hour' | 'day'>('hour')
const series = ref<'count' | 'tokens'>('count')
const tsData = ref<UsagePoint[]>([])

// KPI 概览（k3 建议：先结论后趋势）
const kpis = computed(() => [
  { title: '总调用', value: fmt(summary.value.total_requests), icon: ThunderboltOutlined, accent: '#0ea5e9' },
  { title: '总 Tokens', value: fmtK(summary.value.total_tokens), icon: FileTextOutlined, accent: '#8b5cf6' },
  { title: '涉及模型', value: fmt(summary.value.by_model.length), icon: DatabaseOutlined, accent: '#14b8a6' },
  { title: '协议数', value: fmt(summary.value.by_protocol.length), icon: ApartmentOutlined, accent: '#f59e0b' },
])

// 协议/模型统计占比
const protoTotal = computed(() => summary.value.by_protocol.reduce((s, p) => s + p.count, 0))
const modelTotal = computed(() => summary.value.by_model.reduce((s, m) => s + m.count, 0))
const protoMax = computed(() => Math.max(1, ...summary.value.by_protocol.map((p) => p.count)))
const modelMax = computed(() => Math.max(1, ...summary.value.by_model.map((m) => m.count)))
const apps = computed(() => summary.value.by_app ?? [])
const appTotal = computed(() => apps.value.reduce((s, a) => s + a.count, 0))
const appMax = computed(() => Math.max(1, ...apps.value.map((a) => a.count)))
// 按账号统计（后端 by_account 已带 label/site；账号删除时标 removed）
const accountStats = computed(() => summary.value.by_account ?? [])
const acctMax = computed(() => Math.max(1, ...accountStats.value.map((a) => a.count)))

// 模型列表渲染策略：Top N + 「其他」聚合（可展开），避免几十行把卡片拉得过长
const MODEL_TOP_N = 10
const showAllModels = ref(false)
const sortedModels = computed(() => [...summary.value.by_model].sort((a, b) => b.count - a.count))
interface ModelRow { model: string; count: number; tokens: number; dim?: boolean }
const modelRows = computed<ModelRow[]>(() => {
  const all = sortedModels.value
  if (showAllModels.value || all.length <= MODEL_TOP_N) return all
  const top: ModelRow[] = all.slice(0, MODEL_TOP_N)
  const rest = all.slice(MODEL_TOP_N)
  const agg = rest.reduce((s, m) => ({ count: s.count + m.count, tokens: s.tokens + m.tokens }), { count: 0, tokens: 0 })
  return [...top, { model: `其他 ${rest.length} 个模型`, count: agg.count, tokens: agg.tokens, dim: true }]
})

function fmt(n: number) {
  return (n || 0).toLocaleString()
}
function fmtK(v: number) {
  if (v >= 1000000) return `${(v / 1000000).toFixed(1)}M`
  if (v >= 1000) return `${(v / 1000).toFixed(1)}K`
  return String(v)
}

async function loadSummary() {
  const r: any = await api.usageSummary()
  // 兜底：空态下这些数组可能缺失，直接 .length / .reduce 会崩页
  summary.value = {
    total_requests: r.total_requests ?? 0,
    total_tokens: r.total_tokens ?? 0,
    today_requests: r.today_requests ?? 0,
    today_tokens: r.today_tokens ?? 0,
    by_protocol: r.by_protocol ?? [],
    by_model: r.by_model ?? [],
    by_app: r.by_app ?? [],
    by_account: r.by_account ?? [],
  }
}

async function loadTs() {
  const res: any = await api.usageTimeseries(granularity.value, granularity.value === 'hour' ? 24 : 14)
  tsData.value = res.data ?? []
  renderChart()
}

function renderChart() {
  if (!chartEl.value) return
  if (!chart) chart = echarts.init(chartEl.value)
  const labels = tsData.value.map((d) => d.bucket)
  const values = tsData.value.map((d) => (series.value === 'count' ? d.count : d.tokens))
  const name = series.value === 'count' ? '调用次数' : 'Tokens'
  chart.setOption({
    tooltip: {
      trigger: 'axis',
      backgroundColor: '#06121A', borderColor: '#24454F',
      textStyle: { color: '#E6F0EE' },
      valueFormatter: (v: number) => (series.value === 'tokens' ? fmt(v) : String(v)),
    },
    legend: { textStyle: { color: '#7E9BA3' }, top: 0, right: 0 },
    grid: { left: 50, right: 20, top: 36, bottom: 28 },
    xAxis: {
      type: 'category', data: labels,
      axisLine: { lineStyle: { color: '#24454F' } },
      // k3 建议：抽稀刻度，避免全部旋转
      axisLabel: { color: '#7E9BA3', interval: granularity.value === 'hour' ? 3 : 1, rotate: 30, fontFamily: 'JetBrains Mono' },
    },
    yAxis: {
      type: 'value',
      splitLine: { lineStyle: { color: '#1B363E' } },
      axisLabel: { color: '#7E9BA3', formatter: (v: number) => (series.value === 'tokens' ? fmtK(v) : String(v)), fontFamily: 'JetBrains Mono' },
    },
    series: [{
      name, type: 'line', data: values, smooth: true, symbol: 'circle', symbolSize: 4,
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
  await loadSummary()
  await loadTs()
  window.addEventListener('resize', onResize)
})

onUnmounted(() => {
  window.removeEventListener('resize', onResize)
  chart?.dispose()
  chart = null
})
</script>

<template>
  <div style="padding-bottom: 16px">
    <div class="view-header">
      <div>
        <div class="vh-title">用量</div>
        <div class="vh-meta">
          <span class="led route"></span>
          总调用 <span class="mono">{{ fmt(summary.total_requests) }}</span> ·
          总 Tokens <span class="mono">{{ fmtK(summary.total_tokens) }}</span>
        </div>
      </div>
    </div>

    <!-- 读数瓦片行（扁平 hairline） -->
    <div class="tiles">
      <div v-for="k in kpis" :key="k.title" class="tile">
        <div class="tile-num mono">{{ k.value }}</div>
        <div class="tile-label">{{ k.title }}</div>
        <div class="tile-underline route"></div>
      </div>
    </div>

    <!-- 折线图卡片 -->
    <a-card title="调用趋势" style="margin-bottom: 16px">
      <div style="display: flex; gap: 12px; align-items: center; margin-bottom: 12px; flex-wrap: wrap">
        <a-radio-group v-model:value="granularity" @change="loadTs">
          <a-radio-button value="hour">按小时</a-radio-button>
          <a-radio-button value="day">按天</a-radio-button>
        </a-radio-group>
        <a-radio-group v-model:value="series" @change="renderChart">
          <a-radio-button value="count">调用次数</a-radio-button>
          <a-radio-button value="tokens">Tokens</a-radio-button>
        </a-radio-group>
      </div>
      <div ref="chartEl" style="width: 100%; height: 200px"></div>
    </a-card>

    <!-- 协议 + 应用 / 模型统计：两列高度均衡（模型多时 Top N + 其他聚合，可展开全部） -->
    <a-row :gutter="[16, 16]">
      <a-col :xs="24" :lg="12">
        <a-card title="按协议统计" style="margin-bottom: 16px">
          <div v-if="!summary.by_protocol.length" style="color: #8a94a6; padding: 12px">暂无数据</div>
          <div v-for="p in summary.by_protocol" :key="p.protocol" class="bar-row">
            <div class="bar-label">{{ p.protocol }}</div>
            <div class="bar-track">
              <div class="bar-fill" :style="{ width: (p.count / protoMax * 100).toFixed(1) + '%' }"></div>
            </div>
            <div class="bar-val">
              {{ p.count }} 次 · {{ fmtK(p.tokens) }} tok
              <span class="bar-pct">{{ protoTotal ? Math.round(p.count / protoTotal * 100) : 0 }}%</span>
            </div>
          </div>
        </a-card>
        <a-card v-if="apps.length" title="按应用统计">
          <div v-for="a in apps" :key="a.app" class="bar-row">
            <div class="bar-label">{{ a.app }}</div>
            <div class="bar-track">
              <div class="bar-fill" :style="{ width: (a.count / appMax * 100).toFixed(1) + '%' }"></div>
            </div>
            <div class="bar-val">
              {{ a.count }} 次 · {{ fmtK(a.tokens) }} tok
              <span class="bar-pct">{{ appTotal ? Math.round(a.count / appTotal * 100) : 0 }}%</span>
            </div>
          </div>
        </a-card>
        <!-- 多账号轮询下查看各账号的请求、token 与本地记录积分。 -->
        <a-card v-if="accountStats.length" title="按账号统计" style="margin-top: 16px">
          <div v-for="a in accountStats" :key="a.account_uid" class="bar-row">
            <!-- 别名可长达 40 字符；与站点/状态挤在 130px 定宽里会被 ellipsis
                 把新增的状态信息吃掉，因此拆成两列：名字一列、站点/状态一列 -->
            <div class="bar-label acct-name-col">
              <a-tooltip :title="a.label || a.account_uid || ''">
                <span>{{ a.label || (a.account_uid || '').slice(0, 8) }}</span>
              </a-tooltip>
            </div>
            <div class="bar-label acct-meta-col">
              <span v-if="a.site_label" class="acct-site">{{ a.site_label }}</span>
              <span v-if="a.removed" class="acct-site">已移除</span>
              <span v-else-if="a.enabled === false" class="acct-site">已停用</span>
            </div>
            <div class="bar-track">
              <div class="bar-fill" :style="{ width: (a.count / acctMax * 100).toFixed(1) + '%' }"></div>
            </div>
            <div class="bar-val">
              {{ a.count }} 次 · {{ fmtK(a.tokens) }} tok
              <span v-if="a.credits > 0" class="bar-pct">{{ a.credits.toFixed(1) }} 积分</span>
              <a-tooltip v-else-if="a.unknown_count === 0 && a.ok_count && a.ok_count > 0" title="该账号的成功调用均明确返回 0 积分"><span class="bar-pct" style="color: #22c55e">免费</span></a-tooltip>
              <a-tooltip v-else title="存在未记录积分的调用，无法判断是否收费"><span class="bar-pct" style="color: #8a94a6">未知</span></a-tooltip>
            </div>
          </div>
        </a-card>
      </a-col>
      <a-col :xs="24" :lg="12">
        <a-card title="按模型统计">
          <div v-if="!summary.by_model.length" style="color: #8a94a6; padding: 12px">暂无数据</div>
          <div v-for="m in modelRows" :key="m.model" class="bar-row">
            <div class="bar-label mono">{{ m.model }}</div>
            <div class="bar-track">
              <div class="bar-fill" :class="{ dim: m.dim }" :style="{ width: (m.count / modelMax * 100).toFixed(1) + '%' }"></div>
            </div>
            <div class="bar-val">
              {{ m.count }} 次 · {{ fmtK(m.tokens) }} tok
              <span class="bar-pct">{{ modelTotal ? Math.round(m.count / modelTotal * 100) : 0 }}%</span>
            </div>
          </div>
          <div v-if="sortedModels.length > MODEL_TOP_N" style="text-align: center; margin-top: 8px">
            <a-button size="small" type="text" @click="showAllModels = !showAllModels">
              {{ showAllModels ? '收起' : `展开全部 ${sortedModels.length} 个模型` }}
            </a-button>
          </div>
        </a-card>
      </a-col>
    </a-row>
  </div>
</template>

<style scoped>
/* 读数瓦片行（扁平 hairline，非渐变卡片） */
.tiles {
  display: grid; grid-template-columns: repeat(4, 1fr); gap: 0;
  border: 1px solid var(--line); border-radius: var(--radius);
  background: var(--panel); margin-bottom: 16px; overflow: hidden;
}
.tile { padding: 14px 18px; border-left: 1px solid var(--line-2); }
.tile:first-child { border-left: none; }
.tile-num { font-size: 22px; font-weight: 500; color: var(--paper); line-height: 1.1; }
.tile-label { font-size: 12px; color: var(--fog); margin-top: 4px; }
.tile-underline { height: 2px; width: 26px; margin-top: 8px; border-radius: 2px; background: var(--route); }
@media (max-width: 640px) {
  .tiles { grid-template-columns: repeat(2, 1fr); }
  .tile:nth-child(odd) { border-left: none; }
  .tile:nth-child(n+3) { border-top: 1px solid var(--line-2); }
}

/* 进度条（route 单色，去多色渐变） */
.bar-row {
  display: flex; align-items: center; gap: 12px;
  padding: 5px 0;
}
.bar-label { width: 130px; font-size: 13px; color: var(--paper); flex-shrink: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.bar-label.mono { font-family: var(--mono); font-size: 12px; }
.bar-track { flex: 1; height: 8px; border-radius: 4px; background: var(--panel-2); overflow: hidden; }
.bar-fill {
  height: 100%; border-radius: 4px;
  background: var(--route);
  transition: width 0.5s ease;
}
.bar-val { width: 180px; font-size: 12px; color: var(--fog); text-align: right; flex-shrink: 0; font-family: var(--mono); }
.bar-pct { color: var(--paper); font-weight: 500; margin-left: 6px; }
.acct-site { font-size: 11px; color: var(--fog); font-family: var(--sans); }
.acct-name-col { width: 150px; }
.acct-meta-col { width: 60px; color: var(--fog); }
.bar-fill.dim { background: var(--line); }
</style>
