<script setup lang="ts">
import { ref, computed, watch } from 'vue'
import { api } from '@/api/client'
import type { AdminIdentity, PortalUser } from '@/api/portal'
import { toast } from '@/lib/toast'
import { dt } from '@/lib/format'
import WButton from './ui/WButton.vue'
import WInput from './ui/WInput.vue'
import WModal from './ui/WModal.vue'

const props = defineProps<{ users: PortalUser[]; maxConcurrency: number; overrides?: Record<string, number> }>()
const emit = defineEmits<{ refresh: [] }>()
const identity = ref<AdminIdentity | null>(null), ownerExists = ref(true), error = ref(''), busy = ref(false)
const filter = ref('user'), search = ref(''), showCreate = ref(false)
const username = ref(''), password = ref(''), createRole = ref('user')
const resets = ref<Record<number, string>>({}), concurrency = ref<Record<number, string>>({})
const verification = ref(false), proof = ref(''), operationLabel = ref('')
const operationHint = ref('')
let pending: (() => Promise<unknown>) | undefined
const audit = ref<Awaited<ReturnType<typeof api.identityAudit>>['records']>([]), showAudit = ref(false)
const isOwner = computed(() => identity.value?.role === 'owner')
const users = computed(() => props.users.filter(u => (filter.value === 'user' ? u.role === 'user' : u.role !== 'user') && u.username.toLowerCase().includes(search.value.trim().toLowerCase())))
const roleLabel = (role: string) => ({ user: '普通用户', admin: '管理员', owner: '超级管理员' } as Record<string, string>)[role] || role
const actionLabel = (action: string) => ({ create: '创建账号', role: '调整身份', status: '启用/停用', password: '重置临时密码', transfer_owner: '交接超级管理员', claim_owner: '指定超级管理员', self_password: '本人改密', bootstrap: '初始化超级管理员' } as Record<string,string>)[action] || action
const authLabel = (method: string) => ({ password: '账号密码', recovery_session: 'Token 恢复', token_header: 'Token 请求头', bootstrap: '初始化入口' } as Record<string,string>)[method] || method
function auditDetail(action: string, before: string, after: string) {
  if (action === 'create') return roleLabel(after)
  if (action === 'role') return `${roleLabel(before)} → ${roleLabel(after)}`
  if (action === 'status') return after === 'active' ? '已启用' : '已停用'
  return ''
}
function canManage(u: PortalUser) { return !!identity.value && u.id !== identity.value.id && (u.role === 'user' || (isOwner.value && u.role === 'admin')) }
async function loadIdentity() {
  error.value = ''
  try { const result = await api.adminIdentity(); identity.value = result.identity; ownerExists.value = result.owner_exists; if (result.identity.role !== 'owner') createRole.value = 'user' }
  catch (e: any) { identity.value = null; error.value = e?.response?.data?.error?.message || '身份加载失败' }
}
watch(() => props.users, () => { for (const u of props.users) concurrency.value[u.id] = String(props.overrides?.[String(u.id)] || 0); void loadIdentity() }, { immediate: true })
function ask(label: string, action: () => Promise<unknown>, hint = '') {
  if (busy.value || !identity.value) return
  operationLabel.value = label; operationHint.value = hint; pending = action; proof.value = ''; error.value = ''; verification.value = true
}
watch(verification, value => { if (!value && !busy.value) { proof.value = ''; pending = undefined } })
async function verifyAndRun() {
  if (busy.value || !pending || !proof.value) return
  busy.value = true; error.value = ''
  try {
    await api.adminReauth(identity.value?.recovery ? { token: proof.value } : { password: proof.value })
    proof.value = ''
    const action = pending; const result = await action()
    verification.value = false; pending = undefined; showCreate.value = false; password.value = ''; resets.value = {}
    if ((result as { login_required?: boolean } | undefined)?.login_required) {
      toast.success('身份已交接，请重新登录'); window.location.reload(); return
    }
    toast.success('操作已完成'); emit('refresh'); await loadIdentity()
    if (showAudit.value) await loadAudit()
  } catch (e: any) { error.value = e?.response?.data?.error?.message || '操作失败' }
  finally { proof.value = ''; busy.value = false }
}
function create() {
  if (!username.value.trim() || !password.value) return toast.error('请填写用户名和临时密码')
  const payload = { username: username.value.trim().toLowerCase(), password: password.value, role: createRole.value }
  ask(`创建${roleLabel(payload.role)}「${payload.username}」`, async () => {
    await api.portalWrite('users', payload)
    filter.value = payload.role === 'user' ? 'user' : 'staff'; search.value = payload.username; username.value = ''
  })
}
function reset(u: PortalUser) {
  if (!resets.value[u.id]) return toast.error('请填写临时密码')
  const password = resets.value[u.id]
  ask(`重置「${u.username}」密码并退出全部设备`, () => api.portalWrite(`users/${u.id}/password`, { new_password: password }))
}
function transfer(u: PortalUser) {
  const claim = !ownerExists.value
  const hint = claim ? '此账号将成为唯一超级管理员。' : identity.value?.recovery
    ? '原超级管理员将转为普通管理员；双方的账号会话将失效，恢复入口保持可用。'
    : '你将转为普通管理员并退出当前登录；对方需重新登录取得超级管理员权限。'
  ask(`${claim ? '指定' : '交接'}超级管理员为「${u.username}」`, () => api.adminOwner(u.id, claim), hint)
}
async function loadAudit() {
  try { audit.value = (await api.identityAudit()).records }
  catch (e: any) { error.value = e?.response?.data?.error?.message || '审计查询失败' }
}
async function saveConcurrency(u: PortalUser) {
  if (busy.value) return
  busy.value = true; error.value = ''
  try { await api.portalWrite(`users/${u.id}/concurrency`, { limit: Number(concurrency.value[u.id]) }); toast.success('门户执行并发已更新'); emit('refresh') }
  catch (e: any) { error.value = e?.response?.data?.error?.message || '更新失败' }
  finally { busy.value = false }
}
async function toggleAudit() { showAudit.value = !showAudit.value; if (showAudit.value) await loadAudit() }
</script>

<template>
  <section class="glass rounded-xl p-5 space-y-4">
    <div class="flex flex-wrap items-center justify-between gap-3"><h2 class="font-semibold">用户管理</h2><div class="flex gap-2"><WButton size="sm" :disabled="busy || !identity || !['admin', 'owner'].includes(identity.role)" @click="showCreate = !showCreate">创建账号</WButton><WButton size="sm" :disabled="busy || !identity || !['admin', 'owner'].includes(identity.role)" @click="toggleAudit">身份操作记录</WButton></div></div>
    <p v-if="identity" class="text-micro text-faint">当前：{{ identity.recovery ? '恢复入口' : `${identity.username} · ${roleLabel(identity.role)}` }}。管理人员通过私有入口登录；角色不会自动授予门户 Key 调用资格。</p>
    <p v-if="!ownerExists && identity?.role" class="text-small text-warn">尚未指定超级管理员。请使用恢复 Token 登录，从已完成改密的有效管理员中明确选择。</p>
    <p v-if="error && !verification" role="alert" class="text-small text-fault">{{ error }} <button @click="loadIdentity" class="text-brand">重新加载身份</button></p>
    <form v-if="showCreate" class="rounded-lg border border-line p-3 space-y-3" @submit.prevent="create">
      <div class="flex flex-wrap gap-2"><WInput v-model="username" placeholder="用户名（统一小写）" /><WInput v-model="password" type="password" autocomplete="new-password" aria-label="新账号临时密码" placeholder="临时密码（至少 8 字符，最多 72 字节）" /><select v-model="createRole" class="rounded-lg border border-line bg-bg p-2" aria-label="创建账号身份"><option value="user">普通用户</option><option v-if="isOwner" value="admin">管理员</option></select></div>
      <p class="text-micro text-faint">首次登录必须改密。{{ isOwner ? '管理员无需贡献账号即可管理服务。' : '管理员任免由超级管理员负责。' }}</p><WButton variant="primary" :disabled="busy" @click="create">创建并验证身份</WButton>
    </form>
    <div class="flex flex-wrap gap-2"><select v-model="filter" aria-label="用户身份筛选" class="rounded-lg border border-line bg-bg p-2"><option value="user">普通用户</option><option value="staff">管理人员</option></select><WInput v-model="search" placeholder="搜索用户名" /></div>
    <p v-if="!users.length" class="text-small text-faint">暂无匹配账号。</p>
    <article v-for="u in users" :key="u.id" class="rounded-lg border border-line p-3 space-y-3">
      <div class="flex flex-wrap items-center gap-2"><span>{{ u.username }}</span><span class="text-micro text-faint">#{{ u.id }} · {{ roleLabel(u.role) }} · {{ u.status === 'active' ? '有效' : '已停用' }}{{ u.must_change_password ? ' · 待首次改密' : '' }}</span>
        <WButton v-if="canManage(u)" size="sm" :disabled="busy" @click="ask(`${u.status === 'active' ? '停用' : '启用'}「${u.username}」`, () => api.portalWrite(`users/${u.id}/status`, { status: u.status === 'active' ? 'disabled' : 'active' }))">{{ u.status === 'active' ? '停用' : '启用' }}</WButton>
        <WButton v-if="isOwner && canManage(u)" size="sm" :disabled="busy" @click="ask(`将「${u.username}」${u.role === 'user' ? '升级为管理员' : '降级为普通用户'}`, () => api.portalWrite(`users/${u.id}/role`, { role: u.role === 'user' ? 'admin' : 'user' }))">{{ u.role === 'user' ? '授予管理员' : '撤销管理员' }}</WButton>
        <WButton v-if="isOwner && u.role === 'admin' && u.status === 'active' && !u.must_change_password && (ownerExists || identity?.recovery)" size="sm" :disabled="busy" @click="transfer(u)">{{ ownerExists ? '交接超级管理员' : '指定超级管理员' }}</WButton>
      </div>
      <div v-if="canManage(u) || (identity?.recovery && u.role === 'owner')" class="flex gap-2"><WInput v-model="resets[u.id]" type="password" autocomplete="new-password" :aria-label="`重置 ${u.username} 的临时密码`" placeholder="新临时密码" /><WButton size="sm" :disabled="busy" @click="reset(u)">重置密码并退出设备</WButton></div>
      <div class="flex flex-wrap items-center gap-2 text-small"><label :for="`concurrency-${u.id}`">门户执行并发</label><select :id="`concurrency-${u.id}`" v-model="concurrency[u.id]" :disabled="busy" class="rounded-lg border border-line bg-bg p-2"><option value="0">默认</option><option v-for="n in maxConcurrency" :key="n" :value="String(n)">{{ n }}</option></select><WButton size="sm" :disabled="busy" @click="saveConcurrency(u)">保存</WButton></div>
    </article>
    <div v-if="showAudit" class="space-y-2"><h3 class="text-small font-medium">最近 100 条身份操作</h3><p v-if="!audit.length" class="text-micro text-faint">暂无记录。</p><div v-for="record in audit" :key="record.id" class="break-words text-micro text-muted">{{ dt(record.ts, 'MM-DD HH:mm:ss') }} · {{ record.actor_name }}（{{ authLabel(record.auth_method) }}）→ {{ record.target_name }} · {{ actionLabel(record.action) }} {{ auditDetail(record.action, record.before_value, record.after_value) }}</div></div>
    <WModal v-model:open="verification" :dismissible="!busy" :title="operationLabel" size="sm"><p class="mb-3 text-small text-muted">{{ identity?.recovery ? '请重新输入恢复 Token。' : '请验证你当前的管理密码。' }}操作将记录操作者；身份、停用和改密变更会退出相关设备。</p><p v-if="operationHint" class="mb-3 text-small text-warn">{{ operationHint }}</p><WInput v-model="proof" :disabled="busy" type="password" autocomplete="off" aria-label="身份验证凭据" :placeholder="identity?.recovery ? '恢复 Token' : '当前管理密码'" @enter="verifyAndRun" /><p v-if="error" role="alert" class="mt-2 text-small text-fault">{{ error }}</p><template #footer><WButton :disabled="busy" @click="verification = false">取消</WButton><WButton variant="primary" :loading="busy" :disabled="!proof" @click="verifyAndRun">验证并执行</WButton></template></WModal>
  </section>
</template>
