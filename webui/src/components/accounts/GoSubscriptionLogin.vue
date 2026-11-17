<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'
import { api } from '@/api/client'
import WButton from '@/components/ui/WButton.vue'
import WSelect from '@/components/ui/WSelect.vue'

const emit = defineEmits<{ changed: [] }>()
const state = ref<Record<string, any>>({})
const busy = ref(false)
const error = ref('')
const organization = ref('')
let timer: ReturnType<typeof setTimeout> | undefined
let request: AbortController | undefined
let alive = true

function schedule() {
  clearTimeout(timer)
  if (!alive || document.hidden || !state.value.pending || state.value.pending.status === 'choosing_org') return
  timer = setTimeout(() => perform('poll'), Math.max(1, state.value.pending.retry_after_seconds || 5) * 1000)
}

async function perform(action: string) {
  if (busy.value || !alive) return
  clearTimeout(timer)
  busy.value = true
  error.value = ''
  const controller = new AbortController()
  request = controller
  try {
    const pendingID = state.value.pending?.id
    const result = await api.providerDeviceAuthorization('opencode', action, {
      id: pendingID,
      organization_id: organization.value || undefined,
    }, controller.signal)
    if (!alive) return
    if (result.pending?.id !== pendingID) organization.value = ''
    state.value = result
    if (!result.pending && action !== 'status') emit('changed')
  } catch (e: any) {
    if (alive && !controller.signal.aborted) error.value = e.response?.data?.error?.message || 'Go 订阅授权暂不可用，请重试'
  } finally {
    if (alive) {
      busy.value = false
      if (!error.value) schedule()
    }
  }
}

function resume() {
  if (document.hidden) { clearTimeout(timer); return }
  if (!document.hidden && !busy.value && state.value.pending && !error.value) schedule()
}
onMounted(() => {
  document.addEventListener('visibilitychange', resume)
  perform('status')
})
onUnmounted(() => {
  alive = false
  clearTimeout(timer)
  request?.abort()
  document.removeEventListener('visibilitychange', resume)
})
</script>

<template>
  <section class="rounded-lg border border-line p-3 space-y-2">
    <div class="flex flex-wrap items-center justify-between gap-2">
      <span class="text-small font-medium text-ink">Go 订阅登录</span>
      <span class="text-micro text-muted">{{ state.needs_login ? '需要重新登录' : state.connected ? '已连接' : '未连接' }}</span>
    </div>
    <p v-if="state.connected" class="break-words text-small text-muted">{{ state.organization || state.email }}<span v-if="state.models"> · {{ state.models }} 个模型</span></p>
    <p class="text-micro text-faint">订阅登录期间暂停手动 Go Key 备用；断开后恢复。只使用官方订阅通道，不自动转按量付费，额度显示为未知。</p>
    <p v-if="state.storage_error" class="text-micro text-warn">授权已续期，但凭据保存失败，请检查服务器目录权限。</p>
    <template v-if="state.pending">
      <div class="flex flex-wrap items-center gap-3 text-small">
        <a :href="state.pending.verification_url" target="_blank" rel="noopener noreferrer" class="text-brand hover:underline">打开官方授权页 ↗</a>
        <code class="select-all mono text-ink">{{ state.pending.user_code }}</code>
      </div>
      <p class="text-micro text-faint">确认这是你发起的授权后输入验证码，完成后此处会自动更新。</p>
      <WSelect v-if="state.pending.status === 'choosing_org'" v-model="organization" :options="state.pending.organizations.map((o: any) => ({ value: o.id, label: o.name || o.id }))" placeholder="选择订阅所在组织" />
    </template>
    <p v-if="error" role="alert" class="break-words text-micro text-warn">{{ error }}</p>
    <div class="flex flex-wrap gap-2">
      <WButton v-if="!state.pending" size="sm" variant="subtle" :loading="busy" @click="perform('start')">{{ state.connected ? '重新登录' : '浏览器登录' }}</WButton>
      <template v-else>
        <WButton size="sm" variant="subtle" :loading="busy" :disabled="state.pending.status === 'choosing_org' && !organization" @click="perform('poll')">{{ state.pending.status === 'choosing_org' ? '确认组织' : '检查授权' }}</WButton>
        <WButton v-if="error" size="sm" variant="subtle" :disabled="busy" @click="perform('start')">重新授权</WButton>
        <WButton size="sm" variant="ghost" :disabled="busy" @click="perform('cancel')">取消授权</WButton>
      </template>
      <WButton v-if="state.connected" size="sm" variant="ghost" :disabled="busy" @click="perform('disconnect')">断开订阅</WButton>
    </div>
  </section>
</template>
