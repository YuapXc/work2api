<script setup lang="ts">
// 用量趋势折线图（echarts）。随主题重绘，取 CSS 变量当色值。
import { ref, watch, onMounted, onUnmounted, nextTick } from 'vue'
import * as echarts from 'echarts/core'
import { LineChart } from 'echarts/charts'
import { GridComponent, TooltipComponent } from 'echarts/components'
import { CanvasRenderer } from 'echarts/renderers'
import type { UsagePoint } from '@/types'

echarts.use([LineChart, GridComponent, TooltipComponent, CanvasRenderer])

const props = defineProps<{ data: UsagePoint[]; metric: string }>()
const el = ref<HTMLDivElement | null>(null)
let chart: echarts.ECharts | null = null

function cssVar(name: string) {
  const v = getComputedStyle(document.documentElement).getPropertyValue(name).trim()
  return `rgb(${v})`
}

function render() {
  if (!chart) return
  const brand = cssVar('--c-brand')
  const line = cssVar('--c-line')
  const muted = cssVar('--c-muted')
  chart.setOption({
    grid: { left: 8, right: 12, top: 16, bottom: 8, containLabel: true },
    tooltip: { trigger: 'axis', backgroundColor: cssVar('--c-elevated'), borderColor: line, textStyle: { color: cssVar('--c-ink'), fontSize: 12 } },
    xAxis: {
      type: 'category',
      data: props.data.map((d) => d.bucket),
      axisLine: { lineStyle: { color: line } },
      axisLabel: { color: muted, fontSize: 11 },
      axisTick: { show: false },
    },
    yAxis: {
      type: 'value',
      splitLine: { lineStyle: { color: line, opacity: 0.5 } },
      axisLabel: { color: muted, fontSize: 11 },
    },
    series: [
      {
        type: 'line',
        smooth: true,
        symbol: 'none',
        data: props.data.map((d) => (props.metric === 'tokens' ? d.tokens : d.count)),
        lineStyle: { color: brand, width: 2 },
        areaStyle: {
          color: new echarts.graphic.LinearGradient(0, 0, 0, 1, [
            { offset: 0, color: brand.replace('rgb', 'rgba').replace(')', ', 0.25)') },
            { offset: 1, color: brand.replace('rgb', 'rgba').replace(')', ', 0)') },
          ]),
        },
      },
    ],
  })
}

function resize() { chart?.resize() }

onMounted(async () => {
  await nextTick()
  if (el.value) {
    chart = echarts.init(el.value)
    render()
    window.addEventListener('resize', resize)
  }
})
onUnmounted(() => {
  window.removeEventListener('resize', resize)
  chart?.dispose()
})
watch(() => [props.data, props.metric], render, { deep: true })
</script>

<template>
  <div ref="el" class="h-64 w-full" />
</template>
