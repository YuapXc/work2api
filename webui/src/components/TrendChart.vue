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

function cssRaw(name: string) {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim()
}
function cssVar(name: string) {
  return `rgb(${cssRaw(name)})`
}
// 主题色变量是空格分隔的 "20 184 166"；构造带透明度时必须转成合法的
// rgba(20, 184, 166, a)——旧写法 rgb(v).replace() 会得到 "rgba(20 184 166, a)"
// （空格分量 + 逗号 alpha），是非法颜色，会让 echarts 渐变解析失败、面积不渲染。
function rgba(name: string, a: number) {
  const parts = cssRaw(name).split(/\s+/).filter(Boolean)
  return parts.length >= 3 ? `rgba(${parts[0]}, ${parts[1]}, ${parts[2]}, ${a})` : `rgba(0,0,0,${a})`
}

function render() {
  if (!chart) return
  const brand = cssVar('--c-brand')
  const line = cssVar('--c-line')
  const muted = cssVar('--c-muted')
  chart.setOption({
    grid: { left: 8, right: 12, top: 16, bottom: 8, containLabel: true },
    tooltip: {
      trigger: 'axis',
      backgroundColor: cssVar('--c-elevated'),
      borderColor: line,
      textStyle: { color: cssVar('--c-ink'), fontSize: 12 },
      axisPointer: { type: 'line', lineStyle: { color: line } },
    },
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
        // 悬停时不改变系列外观：echarts 5 默认 hover 会对系列做 emphasis/blur，
        // 单系列 + areaStyle 下会把线/面积淡出，表现为"鼠标放上去曲线消失"。
        emphasis: { disabled: true },
        data: props.data.map((d) => (props.metric === 'tokens' ? d.tokens : d.count)),
        lineStyle: { color: brand, width: 2 },
        areaStyle: {
          color: new echarts.graphic.LinearGradient(0, 0, 0, 1, [
            { offset: 0, color: rgba('--c-brand', 0.25) },
            { offset: 1, color: rgba('--c-brand', 0) },
          ]),
        },
      },
    ],
  })
}

function resize() { chart?.resize() }
let ro: ResizeObserver | null = null

onMounted(async () => {
  await nextTick()
  if (el.value) {
    chart = echarts.init(el.value)
    render()
    window.addEventListener('resize', resize)
    // 容器在 grid/flex 里可能挂载瞬间宽高为 0，echarts 会画成空白。用 ResizeObserver
    // 在拿到实际尺寸后重绘一次，避免"图表不显示"。
    ro = new ResizeObserver(() => resize())
    ro.observe(el.value)
  }
})
onUnmounted(() => {
  window.removeEventListener('resize', resize)
  ro?.disconnect()
  chart?.dispose()
})
watch(() => [props.data, props.metric], render, { deep: true })
</script>

<template>
  <div ref="el" class="h-64 w-full" />
</template>
