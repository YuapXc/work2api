<script setup lang="ts">
import { useLiveRead } from '../../src/lib/liveRead'
// 门户账号授权、导入和共享状态。
import { ref, watch, onUnmounted } from 'vue'
import { api, PortalAPIError, fmtTime, type ContributionInfo, type Me } from '../api'
import { toast } from '../../src/lib/toast'
import { confirm } from '../../src/lib/confirm'
import WButton from '../../src/components/ui/WButton.vue'
import WInput from '../../src/components/ui/WInput.vue'

defineProps<{ me: Me }>()
const emit = defineEmits<{ refresh: [] }>()

const list = ref<ContributionInfo[]>([])


const rowBusy = ref(new Set<number>())
const accepted = ref(false)
const begining = ref(false)
const authUrl = ref('')
const taskId = ref('')
const pollStatus = ref('')
const pollError = ref('')
let expiresAt = 0
let retries = 0
let timer: ReturnType<typeof setTimeout> | null = null
let disposed = false
const cancelling = ref(false)
	const channel=ref('workbuddy')
const qoderRegion=ref('cn'), qoderMethod=ref('oauth'), opencodeTier=ref('zen'), credential=ref('')
watch(channel, () => { credential.value = ''; accepted.value = false; callbackURL.value = '' })
const importing=ref(false)
async function importCredential(){if(importing.value||!accepted.value||!credential.value.trim()||disposed)return;importing.value=true;try{await api.importContribution(channel.value as 'qoder'|'opencode',credential.value.trim(),channel.value==='qoder'?{region:qoderRegion.value}:{tier:opencodeTier.value},accepted.value);credential.value='';if(disposed)return;await load();emit('refresh');toast.success('个人凭据已贡献')}catch(e:any){toast.error(e.message)}finally{importing.value=false}}
	const workbuddySite=ref<'cn' | 'intl' | 'intl-codebuddy'>('cn')
	const product=ref('cn')
	const taskChannel=ref('workbuddy')
	const callbackURL=ref('')
	const callbackBusy=ref(false)
	function cancelTask(id:string){return taskChannel.value==='trae'?api.traeCancel(id):taskChannel.value==='qoder'?api.qoderCancel(id):api.contributionCancel(id)}
	async function completeCallback(){if(callbackBusy.value)return;callbackBusy.value=true;try{await api.traeComplete(taskId.value,callbackURL.value.trim());callbackURL.value='';toast.success('回调已提交')}catch(e:any){toast.error(e.message)}finally{callbackBusy.value=false}}

const resource = useLiveRead(signal => api.contributions(signal), result => {
  list.value = result.contributions
}, { interval: 30000 })
const loading = resource.loading
const loadError = resource.error
const load = () => resource.refresh()

onUnmounted(() => { disposed = true; stopPoll(); if (taskId.value) void cancelTask(taskId.value).catch(() => {}) })

async function begin() {
  if (begining.value || taskId.value || cancelling.value) return
  begining.value = true
  pollStatus.value = ''
  try {
    taskChannel.value=channel.value
    const res = channel.value==='trae' ? await api.traeBegin(product.value,accepted.value) : channel.value==='qoder' ? await api.qoderBegin(qoderRegion.value,accepted.value) : await api.contributionBegin(workbuddySite.value, accepted.value)
    if (disposed) { void cancelTask(res.task_id).catch(() => {}); return }
    authUrl.value = res.auth_url
    taskId.value = res.task_id
    expiresAt = res.expires_at * 1000
    retries = 0
    pollError.value = ''
    window.open(res.auth_url, '_blank', 'noopener')
    timer = setTimeout(poll, 3000)
  } catch (e: any) {
    toast.error(e?.message || '发起登录失败')
  } finally { begining.value = false }
}

async function poll() {
  const id = taskId.value
  if (!id || disposed || cancelling.value) return
  if (Date.now() >= expiresAt) { void cancelTask(id).catch(() => {}); taskId.value = authUrl.value = ''; toast.error('登录会话已过期，请重新发起'); return }
  try {
    const res = taskChannel.value==='trae' ? await api.traePoll(id) : taskChannel.value==='qoder' ? await api.qoderPoll(id) : await api.contributionPoll(id)
    if (disposed || id !== taskId.value || cancelling.value) return
    retries = 0
    pollError.value = ''
    pollStatus.value = res.status
    if (res.status === 'pending') { timer = setTimeout(poll, 3000); return }
    stopPoll()
    if (res.status === 'ready') {
      toast.success(res.models_verified ? `账号 ${res.account_uid_masked} 已添加，可查看本人可用模型并创建 Key` : `账号 ${res.account_uid_masked} 已添加，模型目录暂未验证，可在账号列表重试`)
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
    if (e instanceof PortalAPIError && e.retryable) {
      pollError.value = `${e.message}，会话已保留，将自动重试。`
      const delay = Math.max(Math.min(30000, 3000 * 2 ** Math.min(retries++, 3)), e.retryAfter * 1000)
      timer = setTimeout(poll, Math.min(delay, Math.max(0, expiresAt - Date.now())))
    } else {
      void cancelTask(id).catch(() => {})
      toast.error(e?.message || '轮询失败')
      taskId.value = authUrl.value = ''
    }
  }
}

function stopPoll() { if (timer) { clearTimeout(timer); timer = null } }
async function cancelPoll() {
  if (cancelling.value || !taskId.value) return
  cancelling.value = true
  stopPoll()
  try { await cancelTask(taskId.value); taskId.value = authUrl.value = ''; pollStatus.value = '' }
  catch (e: any) { toast.error(e?.message || '取消失败'); if (!disposed) timer = setTimeout(poll, 3000) }
  finally { cancelling.value = false }
}

async function revoke(c: ContributionInfo) {
  if (rowBusy.value.has(c.id) || refreshing.value === c.id) return
  if (!await confirm({ title: '撤回贡献？', body: '该账号将仅供你本人使用。其他用户正在执行的请求可以完成。', okText: '撤回贡献' }) || disposed) return
  rowBusy.value.add(c.id)
  try {
    await api.revokeContribution(c.id)
    if (disposed) return
    await load()
    emit('refresh')
    toast.success('已撤回贡献，本人仍可使用该账号')
  } catch (e: any) { if (!disposed) toast.error(e?.message || '撤回失败') } finally { rowBusy.value.delete(c.id) }
}

const refreshing = ref<number | null>(null)
async function refreshModels(c: ContributionInfo) {
 if (refreshing.value !== null || rowBusy.value.has(c.id)) return
 refreshing.value = c.id
 try { await api.refreshContributionModels(c.id); await load(); emit('refresh'); toast.success('模型目录已验证') }
 catch (e: any) { toast.error(e.message) }
 finally { refreshing.value = null }
}
const statusText: Record<string, string> = {
  active: '已贡献', private: '仅本人使用', revoked: '已撤回，需重新验证', unavailable: '暂不可用', invalid: '已失效', verifying: '验证中',
}
async function share(c: ContributionInfo) {
  if (rowBusy.value.has(c.id) || refreshing.value === c.id) return
  if (!await confirm({ title: '重新贡献账号？', body: '获授权用户可以使用此账号并消耗额度。你仍保留本人使用权，现有停用和冷却状态继续生效。', okText: '重新贡献' }) || disposed) return
  rowBusy.value.add(c.id)
  try { await api.shareContribution(c.id); if (disposed) return; await load(); emit('refresh'); toast.success('已重新贡献') }
  catch (e: any) { if (!disposed) toast.error(e?.message || '重新贡献失败') } finally { rowBusy.value.delete(c.id) }
}
</script>

<template>
  <div class="space-y-5">
    <div class="glass rounded-2xl p-6">
      <h1 class="text-base font-semibold">我的账号</h1>
		<div class="mt-3 flex flex-wrap gap-2">
        <select v-model="channel" aria-label="账号渠道" :disabled="!!taskId || begining || importing" class="rounded-lg border border-line bg-bg p-2 text-small"><option value="workbuddy">WorkBuddy</option><option value="trae">Trae</option><option value="qoder">Qoder</option><option value="opencode">OpenCode</option></select>
        <select v-if="channel==='workbuddy'" v-model="workbuddySite" aria-label="登录站点" :disabled="!!taskId || begining || importing" class="rounded-lg border border-line bg-bg p-2 text-small"><option value="cn">国内 CodeBuddy</option><option value="intl">国际 WorkBuddy</option><option value="intl-codebuddy">国际 CodeBuddy</option></select>
        <select v-if="channel==='trae'" v-model="product" aria-label="Trae 产品" :disabled="!!taskId || begining || importing" class="rounded-lg border border-line bg-bg p-2 text-small"><option value="cn">国内 IDE</option><option value="solo">国内 Work/SOLO</option></select>
        <template v-if="channel==='qoder'"><select v-model="qoderRegion" aria-label="Qoder 区域" :disabled="!!taskId || begining || importing" class="rounded-lg border border-line bg-bg p-2 text-small"><option value="cn">国内</option><option value="global">国际</option></select><select v-model="qoderMethod" aria-label="Qoder 授权方式" :disabled="!!taskId || begining || importing" class="rounded-lg border border-line bg-bg p-2 text-small"><option value="oauth">扫码授权</option><option value="pat">个人 PAT</option></select></template>
        <select v-if="channel==='opencode'" v-model="opencodeTier" aria-label="OpenCode 密钥类型" :disabled="importing" class="rounded-lg border border-line bg-bg p-2 text-small"><option value="zen">Zen API Key</option><option value="go">Go API Key</option></select>
      </div>
      <p class="mt-1 text-micro text-faint">
        贡献可选。添加个人账号后可调用本人支持的模型；贡献账号进入统一池，其他用户可按默认或管理员授权范围调用，并可能消耗账号额度。
        你可以随时撤回贡献，账号会保留为仅本人使用。服务器会保存授权凭据；凭据保留用于本人调用及重新授权。
      </p>
      <label class="mt-4 flex cursor-pointer items-start gap-2 text-small text-muted">
        <input v-model="accepted" type="checkbox" class="mt-0.5" />
        <span>我已了解：贡献的账号会进入统一账号池供获授权用户调用，且我可在本页随时撤回。</span>
      </label>
      <div v-if="channel==='opencode' || (channel==='qoder' && qoderMethod==='pat')" class="mt-4 space-y-3"><WInput v-model="credential" type="password" autocomplete="off" :placeholder="channel==='qoder' ? 'Qoder 个人 PAT' : 'OpenCode 个人 API Key'" /><p v-if="channel==='opencode'" class="text-micro text-faint">匿名 public 入口是系统资源，不能作为个人贡献。</p><WButton variant="primary" :disabled="!accepted || !credential.trim() || !!taskId" :loading="importing" @click="importCredential">验证凭据并贡献</WButton></div>
      <div class="mt-4 flex flex-wrap gap-2">
        <WButton v-if="channel!=='opencode' && !(channel==='qoder' && qoderMethod==='pat')" variant="primary" :disabled="!accepted || !!taskId || cancelling" :loading="begining" @click="begin">扫码登录并贡献</WButton>
        <WButton v-if="taskId" variant="subtle" :loading="cancelling" @click="cancelPoll">取消</WButton>
      </div>
      <div v-if="taskId" class="mt-4 rounded-xl border border-route/40 bg-route/8 p-4">
        <div class="text-small text-ink">{{ pollStatus === 'pending' ? '等待登录确认…' : '处理中…' }}</div>
        <p v-if="pollError" class="mt-1 text-micro text-muted">{{ pollError }}</p>
        <p class="mt-1 text-micro text-faint">
          已在新窗口打开授权页面；若未弹出，
          <a :href="authUrl" target="_blank" rel="noopener" class="text-brand hover:underline">点此打开授权链接</a>。
          会话 10 分钟内有效。
        </p>
		<div v-if="taskChannel==='trae'" class="mt-3 space-y-2"><p class="text-micro text-muted">若回调 localhost 页面无法连接，请将完整回调地址粘贴到此处，不要发送给他人。</p><textarea v-model="callbackURL" class="w-full rounded-lg border border-line bg-bg p-2 text-small" rows="2" placeholder="完整授权回调地址" /><WButton :loading="callbackBusy" :disabled="!callbackURL.trim()" @click="completeCallback">提交回调</WButton></div>
      </div>
    </div>

    <div class="glass rounded-2xl p-6">
      <h2 class="text-base font-semibold">我的账号与贡献状态</h2>
      <div v-if="loading" class="mt-4 text-small text-faint">加载中…</div>
      <div v-if="loadError" role="alert" class="mt-4 text-small text-fault">{{ loadError }} <button class="text-brand" @click="load">重试</button></div>
      <div v-if="!loading && !loadError && !list.length" class="mt-4 text-small text-faint">还没有贡献记录。</div>
      <div v-if="list.length" class="mt-4 space-y-3">
        <div v-for="c in list" :key="c.id" class="flex flex-wrap items-center gap-3 rounded-xl border border-line bg-elevated/40 p-4">
          <span class="h-2 w-2 rounded-full" :class="c.status === 'active' ? 'bg-live' : 'bg-faint'" />
          <span class="text-micro text-muted">{{ c.provider }}</span><code class="mono text-small">{{ c.account }}</code>
          <span class="rounded-md bg-bg/60 px-2 py-0.5 text-micro text-muted">{{ statusText[c.status] || c.status }}</span>
          <span v-if="c.catalog_status !== 'ready'" class="text-micro text-faint">{{ c.catalog_status === 'unavailable' ? '账号暂不可用，请联系管理员' : '模型待验证' }}</span>
          <WButton v-if="c.catalog_status === 'pending' && ['active', 'private'].includes(c.status)" size="sm" variant="subtle" :loading="refreshing === c.id" :disabled="refreshing !== null || rowBusy.has(c.id)" @click="refreshModels(c)">验证模型</WButton>
          <span class="mono ml-auto text-micro text-faint">{{ fmtTime(c.created_at) }}</span>
          <WButton v-if="c.status === 'active'" size="sm" variant="subtle" :loading="rowBusy.has(c.id)" :disabled="refreshing === c.id" @click="revoke(c)">撤回贡献</WButton>
          <WButton v-if="c.status === 'private'" size="sm" variant="subtle" :loading="rowBusy.has(c.id)" :disabled="refreshing === c.id" @click="share(c)">重新贡献</WButton>
          <span v-if="c.status === 'revoked'" class="text-micro text-faint">重新验证：在上方使用对应渠道的授权或个人凭据添加方式。</span>
        </div>
      </div>
    </div>
  </div>
</template>
