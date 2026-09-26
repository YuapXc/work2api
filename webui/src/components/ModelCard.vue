<script setup lang="ts">
// 模型卡片：名称/命名空间 id + 供应商标签 + 能力标签 + 关键指标 + 描述 +
// （若后端提供）可调用账号。用于统一模型目录。
import { computed } from 'vue'
import WTag from '@/components/ui/WTag.vue'
import { int } from '@/lib/format'
import { providerMeta } from '@/lib/providers'

const props = defineProps<{ model: Record<string, any>; provider: string; reserveDesc?: boolean; reserveEfforts?: boolean; reserveCost?: boolean }>()
const m = computed(() => props.model)
const pm = computed(() => providerMeta(props.provider))
const multimodal = computed(() => m.value.modality === 'multimodal' || m.value.vision || m.value.supports_image)
const reasoning = computed(() => m.value.reasoning || {})
const ctx = computed(() => m.value.context_length ?? m.value.context)
const maxOut = computed(() => m.value.max_output_tokens ?? m.value.max_output)
const accounts = computed(() => (m.value.accounts || []) as any[])
const bench = computed(() => m.value.benchmark || null)
const fmtScore = (v: number | null | undefined) => (v == null ? '—' : Number(v).toFixed(1))

// 每站点成本系数（credits_by_region）。同模型两站点不同价时分列展示并标出更便宜的
// 那个（成本优先选号会用它）。全相同或只有一个站点则回退单值 m.credits。
const siteName = (s: string) => (s === 'domestic' ? '国内' : s === 'international' ? '国际' : s)
const costEntries = computed<{ label: string; v: number; cheapest: boolean }[] | null>(() => {
  const cbr = m.value.credits_by_region as Record<string, number> | undefined
  if (!cbr) return null
  const es = Object.entries(cbr)
  if (es.length < 2 || new Set(es.map(([, v]) => v)).size < 2) return null
  const min = Math.min(...es.map(([, v]) => v))
  return es
    .sort((a, b) => a[1] - b[1])
    .map(([s, v]) => ({ label: siteName(s), v, cheapest: v === min }))
})
// 指标格里显示的成本：多站点时取最低（即成本优先实际会用到的价），否则用合并值。
const displayCredits = computed(() => {
  const cbr = m.value.credits_by_region as Record<string, number> | undefined
  if (cbr && Object.keys(cbr).length) return Math.min(...Object.values(cbr))
  return m.value.credits
})

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
</script>

<template>
  <!-- h-full + flex-col：网格行内三张卡等高（grid 默认 stretch），各区段用 min-h
       预留高度对齐，footer 用 mt-auto 压到底，跨卡横向成带、可读性更好。 -->
  <div class="glass flex h-full flex-col gap-3 rounded-xl p-4 transition-colors hover:border-brand/40">
    <div class="flex min-h-[2.5rem] items-start justify-between gap-2">
      <div class="min-w-0">
        <div class="truncate font-semibold text-ink" :title="m.name || m.id">{{ m.name || m.id }}</div>
        <div class="mono truncate text-micro text-faint" :title="m.id">{{ m.id }}</div>
      </div>
      <WTag :tone="pm.tone">{{ pm.label }}</WTag>
    </div>

    <div class="flex min-h-[1.625rem] flex-wrap gap-1.5">
      <WTag :tone="multimodal ? 'route' : 'muted'">{{ multimodal ? '多模态' : '文本' }}</WTag>
      <WTag v-if="reasoningTag" :tone="reasoningTag.tone">{{ reasoningTag.text }}</WTag>
      <WTag v-if="m.supportsToolCall" tone="live">工具调用</WTag>
    </div>

    <!-- 思考强度档位：低/中/高/… + 可关思考时的 off。仅当本页有带档位的模型时，
         无档位卡才补等高占位，保持跨卡对齐；全都没有则不占位。 -->
    <div v-if="efforts.length" class="flex min-h-[1.625rem] flex-wrap items-center gap-1">
      <span class="text-micro text-faint">思考强度</span>
      <span
        v-for="e in efforts"
        :key="e"
        class="mono rounded-md px-1.5 py-0.5 text-micro"
        :class="e === reasoning.defaultEffort ? 'bg-brand/15 text-brand ring-1 ring-brand/30' : 'bg-elevated text-muted'"
        :title="e === reasoning.defaultEffort ? '默认档位' : ''"
      >{{ e }}</span>
    </div>
    <div v-else-if="reserveEfforts" class="min-h-[1.625rem]" aria-hidden="true"></div>

    <div class="grid grid-cols-3 gap-2 border-t border-line pt-3 text-center">
      <div>
        <div class="mono text-small font-semibold text-ink">{{ ctx ? int(ctx) : '—' }}</div>
        <div class="text-micro text-faint">上下文</div>
      </div>
      <div>
        <div class="mono text-small font-semibold text-ink">{{ maxOut ? int(maxOut) : '—' }}</div>
        <div class="text-micro text-faint">最大输出</div>
      </div>
      <div>
        <div class="mono text-small font-semibold" :class="displayCredits ? 'text-warn' : 'text-ink'">{{ displayCredits ?? '—' }}</div>
        <div class="text-micro text-faint">成本系数</div>
      </div>
    </div>

    <!-- 多站点成本分列：标出更便宜的站点（成本优先会用它）。同价/单站点则不显示。 -->
    <div v-if="costEntries" class="flex min-h-[1.25rem] flex-wrap items-center gap-x-3 gap-y-0.5 text-micro">
      <span class="text-faint">分站点</span>
      <span v-for="c in costEntries" :key="c.label" class="inline-flex items-center gap-1">
        <span class="text-faint">{{ c.label }}</span>
        <span class="mono" :class="c.cheapest ? 'text-live' : 'text-muted'">x{{ c.v }}</span>
      </span>
    </div>
    <div v-else-if="reserveCost" class="min-h-[1.25rem]" aria-hidden="true"></div>

    <!-- 描述：有描述才占两行；仅当本页存在带描述的模型时，无描述卡才补等高占位，
         使评测框跨卡对齐——全都没描述时不留任何空白。 -->
    <p v-if="m.description" class="line-clamp-2 min-h-[2.25rem] text-micro leading-relaxed text-muted">{{ m.description }}</p>
    <div v-else-if="reserveDesc" class="min-h-[2.25rem]" aria-hidden="true"></div>

    <!-- AA 第三方评测（配置了 key 且匹配到时展示） -->
    <div v-if="bench" class="rounded-lg border border-line bg-elevated/40 p-2.5">
      <div class="mb-1.5 flex items-center justify-between">
        <span class="truncate text-micro text-faint">AA 评测<span v-if="bench.name" class="ml-1 text-faint/70">· {{ bench.name }}</span></span>
        <a :href="bench.aa_url || 'https://artificialanalysis.ai/models'" target="_blank" rel="noopener" class="shrink-0 text-micro text-brand hover:underline">榜单 ↗</a>
      </div>
      <div class="grid grid-cols-3 gap-2 text-center">
        <div>
          <div class="mono text-small font-semibold text-ink">{{ fmtScore(bench.intelligence_index) }}</div>
          <div class="text-micro text-faint">智能</div>
        </div>
        <div>
          <div class="mono text-small font-semibold text-ink">{{ fmtScore(bench.coding_index) }}</div>
          <div class="text-micro text-faint">编码</div>
        </div>
        <div>
          <div class="mono text-small font-semibold text-ink">{{ fmtScore(bench.math_index) }}</div>
          <div class="text-micro text-faint">数学</div>
        </div>
      </div>
    </div>

    <!-- 可用账号：mt-auto 压到卡片底部，使各卡 footer 对齐 -->
    <div v-if="accounts.length" class="mt-auto flex flex-wrap items-center gap-1.5 border-t border-line pt-3">
      <span class="text-micro text-faint">可用账号</span>
      <span
        v-for="a in accounts.slice(0, 6)"
        :key="a.uid"
        class="inline-flex items-center gap-1 rounded-md bg-elevated px-1.5 py-0.5 text-micro"
        :class="a.healthy ? 'text-live' : 'text-faint'"
        :title="`${a.label || a.uid}${a.site_label ? ' · ' + a.site_label : ''}${a.model_cooldown ? ' · 本模型冷却中' : ''}`"
      >
        <span class="h-1.5 w-1.5 rounded-full" :class="a.healthy ? 'bg-live' : 'bg-faint'" />
        {{ a.label || a.uid.slice(0, 6) }}
      </span>
      <span v-if="accounts.length > 6" class="text-micro text-faint">+{{ accounts.length - 6 }}</span>
    </div>
  </div>
</template>
