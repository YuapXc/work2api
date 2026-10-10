<script setup lang="ts">
// 玻璃面板。可选标题栏（title / 右侧 actions 插槽）与内边距开关。
withDefaults(defineProps<{ title?: string; sub?: string; flush?: boolean; hover?: boolean }>(), {})
</script>

<template>
  <section
    class="glass min-w-0 rounded-xl shadow-glass"
    :class="hover ? 'transition-colors hover:border-brand/50' : ''"
  >
    <header
      v-if="title || $slots.title || $slots.actions"
      class="flex flex-wrap items-center justify-between gap-3 border-b border-line px-4 py-3"
    >
      <div class="min-w-0 flex-1 basis-40">
        <slot name="title">
          <h3 class="truncate text-[0.9rem] font-semibold text-ink">{{ title }}</h3>
        </slot>
        <p v-if="sub" class="mt-0.5 break-words text-micro leading-relaxed text-faint">{{ sub }}</p>
      </div>
      <div v-if="$slots.actions" class="flex max-w-full flex-wrap items-center gap-2"><slot name="actions" /></div>
    </header>
    <div :class="flush ? '' : 'p-4'"><slot /></div>
  </section>
</template>
