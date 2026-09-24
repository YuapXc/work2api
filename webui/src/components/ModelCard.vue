<script setup lang="ts">
// 模型卡片：名称/命名空间 id + 供应商标签 + 能力标签 + 关键指标 + 描述 +
// （若后端提供）可调用账号。用于统一模型目录。
import { computed } from 'vue'
import WTag from '@/components/ui/WTag.vue'
import { int } from '@/lib/format'
import { providerMeta } from '@/lib/providers'

const props = defineProps<{ model: Record<string, any>; provider: string }>()
const m = computed(() => props.model)
const pm = computed(() => providerMeta(props.provider))
const multimodal = computed(() => m.value.modality === 'multimodal' || m.value.vision || m.value.supports_image)
const reasoning = computed(() => m.value.reasoning || {})
const ctx = computed(() => m.value.context_length ?? m.value.context)
const maxOut = computed(() => m.value.max_output_tokens ?? m.value.max_output)
const accounts = computed(() => (m.value.accounts || []) as any[])
</script>

<template>
  <div class="glass flex flex-col gap-3 rounded-xl p-4 transition-colors hover:border-brand/40">
    <div class="flex items-start justify-between gap-2">
      <div class="min-w-0">
        <div class="truncate font-semibold text-ink" :title="m.name || m.id">{{ m.name || m.id }}</div>
        <div class="mono truncate text-micro text-faint" :title="m.id">{{ m.id }}</div>
      </div>
      <WTag :tone="pm.tone">{{ pm.label }}</WTag>
    </div>

    <div class="flex flex-wrap gap-1.5">
      <WTag :tone="multimodal ? 'route' : 'muted'">{{ multimodal ? '多模态' : '文本' }}</WTag>
      <WTag v-if="reasoning.supportsReasoning" tone="brand">推理</WTag>
      <WTag v-if="m.supportsToolCall" tone="live">工具调用</WTag>
      <WTag v-if="reasoning.onlyReasoning" tone="warn">仅推理</WTag>
    </div>

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
        <div class="mono text-small font-semibold" :class="m.credits ? 'text-warn' : 'text-ink'">{{ m.credits ?? '—' }}</div>
        <div class="text-micro text-faint">成本系数</div>
      </div>
    </div>

    <p v-if="m.description" class="line-clamp-2 text-micro leading-relaxed text-muted">{{ m.description }}</p>

    <div v-if="accounts.length" class="flex flex-wrap items-center gap-1.5 border-t border-line pt-3">
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
