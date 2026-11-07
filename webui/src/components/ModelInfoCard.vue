<script setup lang="ts">
// 管理台与门户共用的模型参数展示；操作由包装层插槽提供，本组件不请求管理接口。
import { computed } from 'vue'
import WTag from '@/components/ui/WTag.vue'
import WIcon from '@/components/ui/WIcon.vue'
import { toast } from '@/lib/toast'
import ModelPromotions from '@/components/ModelPromotions.vue'
import { int } from '@/lib/format'
import { providerMeta } from '@/lib/providers'
import type { AABenchmark } from '@/types'

const props = defineProps<{ model: Record<string, any>; provider: string }>()
const m = computed(() => props.model)
async function copyModelName() {
  const id = m.value.id
  if (typeof id !== 'string' || !id) return
  try {
    await navigator.clipboard.writeText(id)
    toast.success('调用名已复制')
  } catch {
    toast.error('复制失败，请手动选择模型调用名')
  }
}
const pm = computed(() => providerMeta(props.provider))
const media = computed(() => ['image', 'video', 'audio'].includes(m.value.model_type))
const video = computed(() => m.value.video_generation || {})
const mediaTags = computed(() => (m.value.media_tags || []).map((t: string) => ({'text-to-image':'文生图','image-to-image':'图片编辑','text-to-video':'文生视频','image-to-video':'图生视频','audio-to-text':'音频转写'} as Record<string,string>)[t]).filter(Boolean))
const multimodal = computed(() => m.value.modality === 'multimodal' || m.value.vision || m.value.supports_image)
const reasoning = computed(() => {
  const value = m.value.reasoning
  if (value && typeof value === 'object') return value
  return { supportsReasoning: value === true || m.value.is_reasoning === true }
})
const ctx = computed(() => m.value.context_length ?? m.value.context)
const maxOut = computed(() => m.value.max_output_tokens ?? m.value.max_output)
const bench = computed(() => {
  const data = m.value.benchmark as AABenchmark | undefined
  if (!data) return null
  const scores = benchmarkScores(data)
  const measurements = benchmarkMeasurements(data)
  const variants = (data.variants || []).map(variant => ({
    ...variant, scores: benchmarkScores(variant), measurements: benchmarkMeasurements(variant),
  }))
  return scores.length || measurements.length || variants.length ? { ...data, scores, measurements, variants } : null
})
function benchmarkScores(data: AABenchmark) {
  return [
    { label: '智能指数', value: data.intelligence_index },
    { label: '编码', value: data.coding_index },
    { label: '数学', value: data.math_index },
  ].filter(score => typeof score.value === 'number' && Number.isFinite(score.value))
}
function benchmarkMeasurements(data: AABenchmark) {
  const measurements: string[] = []
  if (typeof data.median_output_tokens_per_second === 'number') {
    measurements.push(`输出速度 ${data.median_output_tokens_per_second.toFixed(1)} tok/s`)
  }
  if (typeof data.median_time_to_first_answer_token_seconds === 'number') {
    measurements.push(`首个答案 Token ${data.median_time_to_first_answer_token_seconds.toFixed(2)} s`)
  }
  const prices: string[] = []
  if (typeof data.price_1m_input_tokens === 'number') prices.push(`输入 $${data.price_1m_input_tokens}`)
  if (typeof data.price_1m_output_tokens === 'number') prices.push(`输出 $${data.price_1m_output_tokens}`)
  if (prices.length) {
    measurements.push(`${prices.join(' / ')} · USD/百万 Tokens`)
  }
  return measurements
}

// 每站点成本系数（credits_by_region）。同模型两站点不同价时分列展示并标出更便宜的
// 那个（成本优先选号会用它）。全相同或只有一个站点则回退单值 m.credits。
const finiteCost = (v: unknown): v is number => typeof v === 'number' && Number.isFinite(v) && v >= 0
const siteName = (s: string) => (s === 'domestic' ? '国内' : s === 'international' ? '国际' : s)
const costEntries = computed<{ label: string; v: number; cheapest: boolean }[] | null>(() => {
  const cbr = m.value.credits_by_region as Record<string, number> | undefined
  if (!cbr) return null
  const es = Object.entries(cbr).filter(([, v]) => finiteCost(v))
  if (es.length < 2 || new Set(es.map(([, v]) => v)).size < 2) return null
  const min = Math.min(...es.map(([, v]) => v))
  return es
    .sort((a, b) => a[1] - b[1])
    .map(([s, v]) => ({ label: siteName(s), v, cheapest: v === min }))
})
// 指标格里显示的成本：多站点时取最低（即成本优先实际会用到的价），否则用合并值。
const displayCredits = computed(() => {
  const cbr = m.value.credits_by_region as Record<string, number> | undefined
  const prices = cbr ? Object.values(cbr).filter(finiteCost) : []
  if (prices.length) return Math.min(...prices)
  return m.value.credits
})

const costMetric = computed(() => {
  if (props.provider === 'qoder') return {
    value: finiteCost(m.value.price_factor) ? `×${m.value.price_factor}` : '-',
    label: '成本系数',
    title: '上游模型目录的相对成本系数，仅在该渠道及相应计费规则内比较，跨渠道数字不能直接比较；不是货币单价，缺失时不推断为免费。',
  }
  if (props.provider === 'opencode') return {
    value: finiteCost(m.value.input_cost) && finiteCost(m.value.output_cost) ? `$${m.value.input_cost} / $${m.value.output_cost}` : '-',
    label: '输入 / 输出价格',
    title: 'models.dev 的 Zen 参考价，单位为美元/百万 tokens；Go 订阅与实际账单以渠道为准。',
  }
  if (props.provider === 'trae') return {
    value: traeRates.value.length ? (traeRates.value[0] === traeRates.value[traeRates.value.length - 1] ? `×${traeRates.value[0]}` : `×${traeRates.value[0]}–${traeRates.value[traeRates.value.length - 1]}`) : '-', label: '成本系数',
    title: traePriceDescription.value,
  }
  return { value: finiteCost(displayCredits.value) ? `×${displayCredits.value}` : '-', label: '成本系数', title: '上游目录成本系数；仅在该渠道及相应计费规则内比较，跨渠道数字不能直接比较；活动或时段优惠以实际扣费为准。' }
})
const traeRates = computed<number[]>(() => [...new Set<number>((Array.isArray(m.value.price_factors) ? m.value.price_factors : []).filter((q: any) => finiteCost(q.rate)).map((q: any) => q.rate))].sort((a, b) => a - b))
const traePriceDescription = computed(() => {
  const quotes = Array.isArray(m.value.price_factors) ? m.value.price_factors : []
  const details = [...new Set(quotes.map((q: any) => `${q.product === 'cn' ? 'IDE' : q.product === 'solo' ? 'Work/SOLO' : '目录'}：${finiteCost(q.rate) ? `×${q.rate}` : '未知'}${q.stale ? '（缓存已过期）' : ''}`))]
  return [...details, '上游目录当前相对倍率，已包含当前优惠；不是积分/百万 Tokens。适用账号与活动可能不同，实际扣费以上游结算为准。'].join('\n')
})
const nativeProtocol = computed(() => ({
  chat: 'Chat', messages: 'Messages', responses: 'Responses',
  openai: 'Chat', anthropic: 'Messages', systemone: 'SystemOne',
} as Record<string, string>)[String(m.value.native_protocol || '').toLowerCase()])

// 思考档位（对齐上游 Models.vue reasoningEfforts）：优先用 supportedEfforts，
// 否则退回单个 defaultEffort；可关思考则补一个 off。
const efforts = computed<string[]>(() => {
  const r = reasoning.value
  let list: string[] = Array.isArray(r.supportedEfforts) ? [...r.supportedEfforts] : []
  if (!list.length && r.defaultEffort) list = [r.defaultEffort]
  if (r.canDisableThinking && !list.includes('off')) list.push('off')
  return list
})
// 单一思考标签：可关思考 > 仅思考 > 思考。
const reasoningTag = computed<{ text: string; tone: 'brand' | 'live' | 'warn' } | null>(() => {
  const r = reasoning.value
  if (!r.supportsReasoning) return null
  if (r.canDisableThinking) return { text: '可关思考', tone: 'live' }
  if (r.onlyReasoning) return { text: '仅思考', tone: 'warn' }
  return { text: '思考', tone: 'brand' }
})

// SystemOne 决策模型（opencode）：native_protocol=systemone。这类模型的请求体是
// 结构化决策 payload，和聊天消息完全不同，用 /v1/chat|messages|responses 调用会被
// 上游 400。保留展示但明确标注，避免误用（后续如接决策端点可直接用）。
const systemOne = computed(() => String(m.value.native_protocol || '').toLowerCase() === 'systemone')

</script>

<template>
  <div class="model-card glass min-w-0 rounded-xl p-4 transition-colors hover:border-brand/40">
    <div class="model-card-header flex items-start justify-between gap-2">
      <div class="min-w-0">
        <div class="truncate font-semibold text-ink" :title="m.name || m.id">{{ m.name || m.id }}</div>
        <div class="flex min-w-0 items-center gap-1">
          <span class="mono min-w-0 truncate select-text text-micro text-faint" :title="m.id">{{ m.id }}</span>
          <button v-if="typeof m.id === 'string' && m.id" type="button" class="inline-flex shrink-0 items-center justify-center rounded p-0.5 text-faint transition-colors hover:text-brand focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand" title="复制完整调用名" :aria-label="`复制模型调用名 ${m.id}`" @click="copyModelName">
            <WIcon name="copy" :size="14" />
          </button>
        </div>
      </div>
      <WTag :tone="pm.tone" class="shrink-0 whitespace-nowrap">{{ pm.label }}</WTag>
    </div>

    <div class="model-card-section model-card-capabilities flex flex-wrap content-start items-start gap-1.5">
      <WTag v-for="tag in mediaTags" :key="tag" tone="route">{{ tag }}</WTag>
      <WTag v-if="!media" :tone="multimodal ? 'route' : 'muted'">{{ multimodal ? '多模态' : m.vision === false && m.vision_verified !== false ? '文本' : '图片能力待确认' }}</WTag>
      <WTag v-if="reasoningTag" :tone="reasoningTag.tone">{{ reasoningTag.text }}</WTag>
      <WTag v-if="m.supportsToolCall === true || m.tool_call === true" tone="live">工具调用</WTag>
      <WTag v-if="m.tool_response_multimodal === true" tone="route" title="上游目录声明支持包含图片的工具结果">工具结果图片</WTag>
      <WTag v-if="m.structured_output === true" tone="live" title="上游目录声明结构化输出能力；具体请求参数仍按原生协议处理">结构化输出</WTag>
      <WTag v-if="nativeProtocol && !systemOne" tone="muted" title="网关使用的上游原生协议，不限制网关对外的协议转换">原生 {{ nativeProtocol }}</WTag>
      <WTag v-if="m.catalog_source === 'fallback'" tone="warn" title="尚未获取当前账号的可用模型目录，仅列出保守候选；实际权限以上游为准">目录待验证</WTag>
      <WTag v-else-if="m.catalog_stale" tone="warn" title="目录刷新失败，暂时保留当前账号上次成功获取的结果">目录缓存过期</WTag>
      <WTag v-if="m.pricing_stale" tone="warn" title="参考价格缓存已过期，实际计费以上游账单为准">价格缓存过期</WTag>
      <WTag v-if="systemOne" tone="warn" title="仅支持原生 SystemOne 决策结构，建议使用 /v1/systemone 专用入口">SystemOne</WTag>
    </div>

    <div class="model-card-metrics model-card-divider grid grid-cols-3 content-start items-start gap-2 text-center">
      <template v-if="media">
        <div v-if="video.resolutions?.length" class="col-start-1">
          <div class="mono flex flex-wrap justify-center gap-x-1.5 text-small font-semibold text-ink">
            <span v-for="resolution in video.resolutions" :key="resolution">{{ resolution }}</span>
          </div>
          <div class="text-micro text-faint">分辨率</div>
        </div>
        <div v-if="video.minSeconds && video.maxSeconds" class="col-start-2">
          <div class="mono text-small font-semibold text-ink">{{ video.minSeconds }}–{{ video.maxSeconds }} 秒</div>
          <div class="text-micro text-faint">时长<span v-if="video.defaultSeconds"> · 默认 {{ video.defaultSeconds }} 秒</span></div>
        </div>
        <div class="col-start-3">
          <div class="text-small font-semibold text-ink">按结果结算</div>
          <div class="text-micro text-faint">计费方式</div>
        </div>
      </template>
      <template v-else>
        <div>
          <div class="mono text-small font-semibold text-ink">{{ ctx ? int(ctx) : '—' }}</div>
          <div class="text-micro text-faint" :title="Array.isArray(m.context_tiers) ? m.context_tiers.map((t: any) => `${t.name}${t.is_default ? '（默认）' : ''}`).join(' / ') : undefined">上下文<span v-if="Array.isArray(m.context_tiers) && m.context_tiers.length > 1" class="ml-1 text-muted">· {{ m.context_tiers.length }} 档</span></div>
        </div>
        <div>
          <div class="mono text-small font-semibold text-ink">{{ maxOut ? int(maxOut) : '—' }}</div>
          <div class="text-micro text-faint">最大输出</div>
        </div>
        <div>
          <div class="mono break-words text-small font-semibold text-ink" :title="costMetric.title">{{ costMetric.value }}</div>
          <div class="text-micro text-faint" :title="costMetric.title">{{ costMetric.label }}</div>
          <div v-if="Array.isArray(m.promotions) && m.promotions.length" class="mt-1 flex justify-center">
            <ModelPromotions :promotions="m.promotions" />
          </div>
        </div>
      </template>
    </div>

    <div v-if="efforts.length" class="model-card-section model-card-efforts flex flex-wrap content-start items-center gap-1">
      <span class="text-micro text-faint">思考强度</span>
      <span
        v-for="e in efforts"
        :key="e"
        class="mono rounded-md px-1.5 py-0.5 text-micro"
        :class="e === reasoning.defaultEffort ? 'bg-brand/15 text-brand ring-1 ring-brand/30' : 'bg-elevated text-muted'"
        :title="e === reasoning.defaultEffort ? '默认档位' : ''"
      >{{ e }}</span>
    </div>

    <div class="model-card-section model-card-details space-y-1.5">
      <div v-if="media && video.aspectRatios?.length" class="flex flex-wrap gap-x-1.5 text-micro text-faint">
        <span>画幅</span>
        <span v-for="ratio in video.aspectRatios" :key="ratio" class="mono text-muted">{{ ratio }}</span>
      </div>
      <!-- SystemOne 模型用法提示：这类模型只接受结构化决策 payload，普通聊天/消息/
           Responses 调用会被上游 400，明确标注避免误用。 -->
      <div v-if="systemOne" class="rounded-lg border border-warn/30 bg-warn/5 px-2.5 py-1.5 text-micro leading-relaxed text-warn">
        仅 <span class="mono">SystemOne</span> 决策协议模型：请使用 /v1/systemone 提交原生 state 和 questions；普通聊天消息不能转换成决策请求。
      </div>

      <div v-if="provider === 'opencode' && (finiteCost(m.input_cost) || finiteCost(m.output_cost))" class="text-micro leading-relaxed text-faint">Zen 参考价 · USD / 百万 tokens<span v-if="m.tier === 'go'">；Go 订阅额度另计</span></div>
      <div v-if="provider === 'opencode' && (finiteCost(m.cache_read_cost) || finiteCost(m.cache_write_cost))" class="flex flex-wrap gap-x-3 gap-y-1 text-micro text-faint">
        <span v-if="finiteCost(m.cache_read_cost)">缓存读取 ${{ m.cache_read_cost }}</span>
        <span v-if="finiteCost(m.cache_write_cost)">缓存写入 ${{ m.cache_write_cost }}</span>
        <span>USD / 百万 tokens</span>
      </div>
      <div v-if="m.max_input_tokens > 0" class="text-micro text-faint">最大输入 <span class="mono">{{ int(m.max_input_tokens) }}</span> tokens</div>

      <!-- 多站点成本分列：标出更便宜的站点（成本优先会用它）。同价/单站点则不显示。 -->
      <div v-if="costEntries" class="flex flex-wrap items-center gap-x-3 gap-y-0.5 text-micro">
        <span class="text-faint">分站点</span>
        <span v-for="c in costEntries" :key="c.label" class="inline-flex items-center gap-1">
          <span class="text-faint">{{ c.label }}</span>
          <span class="mono" :class="c.cheapest ? 'text-live' : 'text-muted'">x{{ c.v }}</span>
        </span>
      </div>
    </div>

    <div v-if="m.description" class="model-card-section model-card-description">
      <p class="line-clamp-2 text-micro leading-relaxed text-muted" :title="m.description">{{ m.description }}</p>
    </div>

    <div v-if="bench" class="model-card-section model-card-benchmark">
      <div class="rounded-lg border border-line bg-elevated/40 px-2.5 py-2">
        <div class="flex min-h-5 items-center gap-x-2 text-micro">
          <a v-if="bench.aa_url" :href="bench.aa_url" target="_blank" rel="noopener" class="min-w-0 truncate text-brand hover:underline" title="Artificial Analysis">Artificial Analysis ↗</a>
          <span v-else class="min-w-0 truncate text-faint">Artificial Analysis</span>
          <span class="min-w-0 flex-1 truncate text-faint" :title="bench.name">{{ bench.name }}</span>
          <span v-if="bench.scores[0]" class="shrink-0 whitespace-nowrap text-faint">
            {{ bench.scores[0].label }}
            <span class="mono text-small font-semibold text-ink">{{ Number(bench.scores[0].value).toFixed(1) }}</span>
          </span>
        </div>
        <details v-if="bench.variants.length || bench.measurements.length || bench.scores.length > 1" class="mt-1 text-micro">
          <summary class="cursor-pointer text-brand">{{ bench.variants.length ? '查看分档评测' : '查看评测数据' }}</summary>
          <div v-if="!bench.variants.length" class="mt-1.5 flex flex-wrap gap-x-3 gap-y-1 text-muted">
            <span v-for="score in bench.scores.slice(1)" :key="score.label">{{ score.label }} {{ Number(score.value).toFixed(1) }}</span>
            <span v-for="measurement in bench.measurements" :key="measurement">{{ measurement }}</span>
          </div>
          <p v-if="bench.variants.length && !bench.variants[0]?.effort" class="mt-1 text-faint">以下为 AA 测试档位，渠道档位待确认。</p>
          <div v-for="variant in bench.variants" :key="variant.id" class="mt-1.5 flex flex-wrap items-baseline justify-between gap-x-3 border-t border-line pt-1.5">
            <div class="min-w-0">
              <span v-if="variant.effort" class="mr-1.5 font-medium text-ink">{{ variant.effort === 'off' ? '关闭思考' : variant.effort }}</span>
              <a v-if="variant.aa_url" :href="variant.aa_url" target="_blank" rel="noopener" class="break-words text-brand hover:underline">{{ variant.name }} ↗</a>
              <span v-else class="break-words text-faint">{{ variant.name }}</span>
            </div>
            <div v-if="variant.scores.length" class="flex flex-wrap gap-x-2.5">
              <span v-for="score in variant.scores" :key="score.label" class="text-muted">
                {{ score.label }} <span class="mono font-semibold text-ink">{{ Number(score.value).toFixed(1) }}</span>
              </span>
            </div>
            <span v-else-if="!variant.measurements.length" class="text-faint">暂无评测</span>
            <div v-if="variant.measurements.length" class="flex w-full flex-wrap gap-x-3 gap-y-1 text-muted">
              <span v-for="measurement in variant.measurements" :key="measurement">{{ measurement }}</span>
            </div>
          </div>
          <p v-if="bench.measurements.length || bench.variants.some(v => v.measurements.length)" class="mt-1.5 text-faint">性能为 AA 跨供应商的中位数，价格为 AA 参考价。</p>
        </details>
      </div>
    </div>

    <div class="model-card-actions model-card-divider"><slot name="actions" /></div>
    <div class="model-card-footer model-card-divider"><slot name="footer" /></div>
  </div>
</template>

<style>
/* Shared by the admin and portal catalogs. No DOM measurement or fixed heights. */
.model-grid {
  display: grid;
  grid-template-columns: minmax(0, 1fr);
  gap: 0 0.75rem;
  align-items: stretch;
}
.model-card {
  display: flex;
  flex-direction: column;
  margin-bottom: 0.75rem;
}
.model-card-section:not(:empty) { padding-top: 0.625rem; }
.model-card-divider:not(:empty) {
  margin-top: 0.625rem;
  padding-top: 0.625rem;
  border-top: 1px solid rgb(var(--c-line));
}
@media (min-width: 640px) {
  .model-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); }
}
@media (min-width: 1280px) {
  .model-grid-admin { grid-template-columns: repeat(3, minmax(0, 1fr)); }
}
@supports (grid-template-rows: subgrid) {
  .model-card {
    display: grid;
    grid-row: span 9;
    grid-template-columns: minmax(0, 1fr);
    grid-template-rows: subgrid;
    row-gap: 0;
  }
  .model-card-header { grid-row: 1; }
  .model-card-capabilities { grid-row: 2; }
  .model-card-metrics { grid-row: 3; }
  .model-card-efforts { grid-row: 4; }
  .model-card-details { grid-row: 5; }
  .model-card-description { grid-row: 6; }
  .model-card-benchmark { grid-row: 7; }
  .model-card-actions { grid-row: 8; }
  .model-card-footer { grid-row: 9; }
}
</style>
