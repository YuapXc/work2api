<script setup lang="ts">
// 贡献账号：WorkBuddy 扫码（设备授权流程）。弹出授权链接，用户在新窗口
// 打开登录后这里轮询；完成即获得资格。也可随时撤回（立即停止共享调度）。
import { ref, onMounted, onUnmounted } from 'vue'
import { api, fmtTime, type ContributionInfo, type Me } from '../api'
import { toast } from '../../src/lib/toast'
import WButton from '../../src/components/ui/WButton.vue'

defineProps<{ me: Me }>()
const emit = defineEmits<{ refresh: [] }>()

const list = ref<ContributionInfo[]>([])
const loading = ref(true)
const accepted = ref(false)
const begining = ref(false)
const authUrl = ref('')
const taskId = ref('')
const pollStatus = ref('')
let timer: ReturnType<typeof setTimeout> | null = null
let disposed = false
const cancelling = ref(false)

async function load() {
  loading.value = true
  try { list.value = (await api.contributions()).contributions } catch (e: any) { toast.error(e?.message || '加载贡献失败') } finally { loading.value = false }
}
onMounted(() => { load() })
onUnmounted(() => { disposed = true; stopPoll(); if (taskId.value) void api.contributionCancel(taskId.value).catch(() => {}) })

async function begin() {
  if (begining.value || taskId.value || cancelling.value) return
  begining.value = true
  pollStatus.value = ''
  try {
    const res = await api.contributionBegin('workbuddy', accepted.value)
    if (disposed) { void api.contributionCancel(res.task_id).catch(() => {}); return }
    authUrl.value = res.auth_url
    taskId.value = res.task_id
    window.open(res.auth_url, '_blank', 'noopener')
    timer = setTimeout(poll, 3000)
  } catch (e: any) {
    toast.error(e?.message || '发起登录失败')
  } finally { begining.value = false }
}

async function poll() {
  const id = taskId.value
  if (!id || disposed || cancelling.value) return
  try {
    const res = await api.contributionPoll(id)
    if (disposed || id !== taskId.value || cancelling.value) return
    pollStatus.value = res.status
    if (res.status === 'pending') { timer = setTimeout(poll, 3000); return }
    stopPoll()
    if (res.status === 'ready') {
      toast.success(`账号 ${res.account_uid_masked} 已添加，可查看本人可用模型并创建 Key；共享池另需管理员授权`)
      taskId.value = authUrl.value = ''
      await load()
      emit('refresh')
    } else if (res.status === 'failed') {
      toast.error(res.message || '登录失败')
      taskId.value = authUrl.value = ''
    } else {
      toast.error('登录会话已过期，请重新发起')
      taskId.value = authUrl.value = ''
    }
  } catch (e: any) {
    if (disposed || id !== taskId.value || cancelling.value) return
    stopPoll()
    toast.error(e?.message || '轮询失败')
    taskId.value = authUrl.value = ''
  }
}

function stopPoll() { if (timer) { clearTimeout(timer); timer = null } }
async function cancelPoll() {
  if (cancelling.value || !taskId.value) return
  cancelling.value = true
  stopPoll()
  try { await api.contributionCancel(taskId.value); taskId.value = authUrl.value = ''; pollStatus.value = '' }
  catch (e: any) { toast.error(e?.message || '取消失败'); if (!disposed) timer = setTimeout(poll, 3000) }
  finally { cancelling.value = false }
}

async function revoke(c: ContributionInfo) {
  if (!confirm('停止共享后，该账号仅供你本人使用。其他用户正在执行的请求可以完成，后续请求不能再选中该账号。确定停止共享？')) return
  try {
    await api.revokeContribution(c.id)
    await load()
    emit('refresh')
    toast.success('已停止共享，本人仍可使用该账号')
  } catch (e: any) { toast.error(e?.message || '撤回失败') }
}

const statusText: Record<string, string> = {
  active: '共享中', private: '仅本人使用', revoked: '已撤回，需重新验证', unavailable: '暂不可用', invalid: '已失效', verifying: '验证中',
}
async function share(c: ContributionInfo) {
  if (!confirm('恢复共享后，获授权用户可使用此账号并消耗它的额度。你仍保留本人使用权，账号原有停用和冷却状态不会被清除。确认恢复共享？')) return
  try { await api.shareContribution(c.id); await load(); emit('refresh'); toast.success('已恢复共享') }
  catch (e: any) { toast.error(e?.message || '恢复共享失败') }
}
</script>

<template>
  <div class="space-y-5">
    <div class="glass rounded-2xl p-6">
      <h1 class="text-base font-semibold">我的 WorkBuddy 账号</h1>
      <p class="mt-1 text-micro text-faint">
        扫码添加并共享账号后，你无需共享分组授权即可调用本人账号。其他用户只能通过获授权的共享池使用它，调用消耗该账号额度。
        你可以随时停止共享，账号会保留为仅本人使用。服务器会保存授权凭据；共享记录和凭据也会保留，用于本人调用及重新授权。
      </p>
      <label class="mt-4 flex cursor-pointer items-start gap-2 text-small text-muted">
        <input v-model="accepted" type="checkbox" class="mt-0.5" />
        <span>我已了解：贡献的账号会进入共享分组供其他具备资格的用户调用，且我可在本页随时撤回。</span>
      </label>
      <div class="mt-4 flex flex-wrap gap-2">
        <WButton variant="primary" :disabled="!accepted || !!taskId || cancelling" :loading="begining" @click="begin">扫码登录并贡献</WButton>
        <WButton v-if="taskId" variant="subtle" :loading="cancelling" @click="cancelPoll">取消</WButton>
      </div>
      <div v-if="taskId" class="mt-4 rounded-xl border border-route/40 bg-route/8 p-4">
        <div class="text-small text-ink">{{ pollStatus === 'pending' ? '等待登录确认…' : '处理中…' }}</div>
        <p class="mt-1 text-micro text-faint">
          已在新窗口打开授权页面；若未弹出，
          <a :href="authUrl" target="_blank" rel="noopener" class="text-brand hover:underline">点此打开授权链接</a>。
          会话 10 分钟内有效。
        </p>
      </div>
    </div>

    <div class="glass rounded-2xl p-6">
      <h2 class="text-base font-semibold">我的账号与共享状态</h2>
      <div v-if="loading" class="mt-4 text-small text-faint">加载中…</div>
      <div v-else-if="!list.length" class="mt-4 text-small text-faint">还没有贡献记录。</div>
      <div v-else class="mt-4 space-y-3">
        <div v-for="c in list" :key="c.id" class="flex flex-wrap items-center gap-3 rounded-xl border border-line bg-elevated/40 p-4">
          <span class="h-2 w-2 rounded-full" :class="c.status === 'active' ? 'bg-live' : 'bg-faint'" />
          <code class="mono text-small">{{ c.account }}</code>
          <span class="rounded-md bg-bg/60 px-2 py-0.5 text-micro text-muted">{{ statusText[c.status] || c.status }}</span>
          <span class="mono ml-auto text-micro text-faint">{{ fmtTime(c.created_at) }}</span>
          <WButton v-if="c.status === 'active'" size="sm" variant="subtle" @click="revoke(c)">停止共享</WButton>
          <WButton v-if="c.status === 'private'" size="sm" variant="subtle" @click="share(c)">恢复共享</WButton>
          <span v-if="c.status === 'revoked'" class="text-micro text-faint">重新验证：在上方扫码同一账号并授权。</span>
        </div>
      </div>
    </div>
  </div>
</template>
