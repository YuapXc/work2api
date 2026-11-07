<script setup lang="ts">
// 模态框。v-model:open 控制显隐；title + 默认插槽 + footer 插槽。
// 点遮罩/ESC 关闭。宽度经 size 控制。
import { computed, nextTick, onMounted, onUnmounted, ref, useId, watch } from 'vue'
import { confirmState } from '@/lib/confirm'
const titleID = useId()
const props = withDefaults(defineProps<{ open: boolean; title?: string; size?: 'sm' | 'md' | 'lg' | 'xl'; dismissible?: boolean; confirmation?: boolean }>(), {
  size: 'md',
  dismissible: true,
})
const emit = defineEmits<{ 'update:open': [v: boolean] }>()
const width = { sm: 'max-w-md', md: 'max-w-xl', lg: 'max-w-3xl', xl: 'max-w-5xl' }
const covered = computed(() => !props.confirmation && confirmState.open)
const dialog = ref<HTMLElement | null>(null)
let returnFocus: HTMLElement | null = null

function close() {
  if (!props.dismissible || covered.value) return
  emit('update:open', false)
}
function onKey(e: KeyboardEvent) {
  if (!props.open || covered.value || e.defaultPrevented) return
  if (e.key === 'Escape') {
    if (props.confirmation) e.preventDefault()
    close()
    return
  }
  if (!props.confirmation || e.key !== 'Tab') return
  const buttons = dialog.value?.querySelectorAll<HTMLButtonElement>('button:not(:disabled)')
  if (!buttons?.length) return
  const first = buttons[0]
  const last = buttons[buttons.length - 1]
  const outside = !dialog.value?.contains(document.activeElement)
  if (outside || (e.shiftKey ? document.activeElement === first : document.activeElement === last)) {
    e.preventDefault()
    const target = e.shiftKey ? last : first
    target.focus()
  }
}
watch(() => props.confirmation && confirmState.open, async (open) => {
  if (open) {
    returnFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null
    await nextTick()
    if (confirmState.open) dialog.value?.querySelector<HTMLElement>('[autofocus]')?.focus()
  } else {
    const target = returnFocus
    returnFocus = null
    await nextTick()
    if (!confirmState.open && target?.isConnected) target.focus()
  }
}, { flush: 'sync' })
onMounted(() => window.addEventListener('keydown', onKey, props.confirmation))
onUnmounted(() => window.removeEventListener('keydown', onKey, props.confirmation))
</script>

<template>
  <Teleport to="body">
    <Transition name="modal">
      <div
        v-if="open"
        class="fixed inset-0 flex items-start justify-center overflow-y-auto overscroll-contain bg-black/55 p-4 backdrop-blur-sm sm:items-center"
        :class="confirmation ? 'z-[100]' : 'z-50'"
        :inert="covered"
        @click.self="close"
      >
        <div ref="dialog" role="dialog" :aria-modal="covered ? undefined : true" :aria-labelledby="title || $slots.title ? titleID : undefined" :aria-label="!title && !$slots.title ? '操作窗口' : undefined" class="glass w-full rounded-2xl shadow-glass" :class="width[size]">
          <header v-if="title || $slots.title" class="flex items-center justify-between border-b border-line px-5 py-3.5">
            <div :id="titleID"><slot name="title"><h3 class="text-[0.95rem] font-semibold text-ink">{{ title }}</h3></slot></div>
            <button class="text-faint hover:text-ink disabled:opacity-45" :disabled="!dismissible" @click="close" aria-label="关闭">
              <svg class="h-5 w-5" viewBox="0 0 20 20" fill="currentColor"><path d="M6.28 5.22a.75.75 0 00-1.06 1.06L8.94 10l-3.72 3.72a.75.75 0 101.06 1.06L10 11.06l3.72 3.72a.75.75 0 101.06-1.06L11.06 10l3.72-3.72a.75.75 0 00-1.06-1.06L10 8.94 6.28 5.22z" /></svg>
            </button>
          </header>
          <div class="max-h-[70vh] overflow-y-auto overscroll-contain px-5 py-4"><slot /></div>
          <footer v-if="$slots.footer" class="flex justify-end gap-2 border-t border-line px-5 py-3.5">
            <slot name="footer" />
          </footer>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>

<style scoped>
.modal-enter-active, .modal-leave-active { transition: opacity 0.2s ease; }
.modal-enter-from, .modal-leave-to { opacity: 0; }
</style>
