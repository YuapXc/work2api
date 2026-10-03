<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { api } from '@/api/client'
import type { PortalOverview, PortalGroup, PortalUser } from '@/api/portal'
import { toast } from '@/lib/toast'
import WButton from '@/components/ui/WButton.vue'
import WInput from '@/components/ui/WInput.vue'
import { fmtTime } from '../../src-portal/api'

const data = ref<PortalOverview>({ users: [], invites: [], groups: [], accounts: [], contributions: [], registration_mode: '' })
const loading = ref(true)
const loaded = ref(false)
const busy = ref(false)
const error = ref('')
const bootstrapName = ref('')
const bootstrapPass = ref('')
const inviteDays = ref('7')
const newGroup = ref('')
const disabledModels = ref('')
const models = ref<string[]>([])
const groupDrafts = ref<Record<number, string[]>>({})
const passwords = ref<Record<number, string>>({})
const accountDrafts = ref<Record<number, string>>({})
const userDrafts = ref<Record<number, string>>({})
const adminsExist = computed(() => data.value.users.some(u => u.role === 'admin'))
function parseModels(value: string[] | string): string[] {
  if (Array.isArray(value)) return value
  try { const parsed: unknown = JSON.parse(value || '[]'); return Array.isArray(parsed) ? parsed.filter((m): m is string => typeof m === 'string') : [] } catch { return [] }
}
function accountsOf(g: PortalGroup) { return g.accounts || g.account_uids || [] }
function grantsOf(g: PortalGroup) { return g.grants || g.user_ids || [] }
function isPublicModel(model: string) { return model !== 'auto' && model !== 'workbuddy/auto' }
function pickerModels(g: PortalGroup) { return [...new Set([...models.value, ...parseModels(g.allowed_models)])].filter(isPublicModel) }
function username(id: number) { return data.value.users.find(u => u.id === id)?.username || `用户 ${id}` }
const sharedAccounts = computed(() => (data.value.accounts || []).filter(a => {
  const c = data.value.contributions?.find(c => c.account_uid === a.uid)
  return a.provider === 'workbuddy' && (c?.status === 'active' || (a.contribution_user_id && a.status === 'active'))
}))
async function load() {
  loading.value = true
  error.value = ''
  try {
    const [overview, catalog, settings] = await Promise.all([api.portalOverview(), api.modelCatalog(), api.getSettings()])
    data.value = { ...overview, users: overview.users || [], invites: overview.invites || [], groups: overview.groups || [] }
    models.value = catalog.models.filter(m => m.provider === 'workbuddy' && m.id !== 'auto' && m.id !== 'workbuddy/auto').map(m => m.id)
    const disabled = settings.portal_disabled_models
    disabledModels.value = parseModels(typeof disabled === 'string' || Array.isArray(disabled) ? disabled as string | string[] : []).join('\n')
    for (const g of data.value.groups) groupDrafts.value[g.id] = parseModels(g.allowed_models).filter(isPublicModel)
    loaded.value = true
  } catch (e: any) { error.value = e?.response?.data?.error?.message || e?.message || '加载失败' }
  finally { loading.value = false }
}
onMounted(load)
async function mutate(action: () => Promise<unknown>, message: string) {
  if (busy.value) return
  busy.value = true
  try { await action(); toast.success(message); await load() }
  catch (e: any) { error.value = e?.response?.data?.error?.message || e?.message || '操作失败' }
  finally { busy.value = false }
}
async function bootstrap() {
  if (!bootstrapName.value.trim() || !bootstrapPass.value) return toast.error('请填写管理员用户名和密码')
  await mutate(async () => { await api.portalWrite('bootstrap', { username: bootstrapName.value.trim(), password: bootstrapPass.value }); bootstrapPass.value = '' }, '管理员账号已初始化')
}
async function resetPassword(u: PortalUser) {
  const password = passwords.value[u.id]
  if (!password) return toast.error('请填写新密码')
  if (!confirm(`重置「${u.username}」的密码并撤销该用户全部会话？`)) return
  await mutate(async () => { await api.portalWrite(`users/${u.id}/password`, { new_password: password }); passwords.value[u.id] = '' }, '密码已重置')
}
async function removeGroup(g: PortalGroup) {
  if (!confirm(`删除分组「${g.name}」及其授权？`)) return
  await mutate(() => api.portalDelete(`groups/${g.id}`), '分组已删除')
}
function setModel(g: PortalGroup, model: string, checked: boolean) {
  const selected = groupDrafts.value[g.id] || []
  groupDrafts.value[g.id] = checked ? [...new Set([...selected, model])] : selected.filter(m => m !== model)
}
async function saveDisabled() {
  const selected = [...new Set(disabledModels.value.split(/[\n,，]/).map(m => m.trim()).filter(Boolean))]
  await mutate(() => api.saveSettings({ portal_disabled_models: JSON.stringify(selected) }), '全局禁用模型已更新')
}
async function createInvite() {
  const days = Number(inviteDays.value)
  if (!Number.isFinite(days) || days < 1 || days > 365) return toast.error('有效天数需为 1–365')
  await mutate(() => api.portalWrite('invites', { valid_days: days }), '邀请码已创建')
}
</script>

<template>
  <div class="mx-auto max-w-[1320px] space-y-5 p-4 md:p-6">
    <div class="flex items-center justify-between"><div><h1 class="text-lg font-semibold">门户管理</h1><p class="mt-1 text-small text-faint">注册方式：{{ data.registration_mode || '未知' }} · 管理共享资格与模型范围</p></div><WButton :loading="loading" :disabled="busy" @click="load">刷新</WButton></div>
    <p v-if="error" class="rounded-lg border border-fault/30 p-3 text-small text-fault">{{ error }}</p>
    <div v-if="loaded && !adminsExist" class="glass rounded-xl p-5 space-y-3">
      <h2 class="font-semibold">初始化管理员账号</h2><p class="text-small text-faint">通过私有入口的管理 Token 登录后设置密码；Token 保留用于恢复管理访问。</p>
      <WInput v-model="bootstrapName" placeholder="管理员用户名" /><WInput v-model="bootstrapPass" type="password" placeholder="8–72 字节密码" /><WButton variant="primary" :loading="busy" @click="bootstrap">初始化</WButton>
    </div>
    <section class="glass rounded-xl p-5 space-y-3">
      <h2 class="font-semibold">用户与密码恢复</h2><p v-if="!data.users.length" class="text-small text-faint">暂无用户。</p>
      <div v-for="u in data.users" :key="u.id" class="rounded-lg border border-line p-3 space-y-2">
        <div class="flex flex-wrap items-center gap-3"><span>{{ u.username }} <span class="text-micro text-faint">#{{ u.id }} · {{ u.role }} · {{ u.status }}</span></span><WButton size="sm" :disabled="busy" @click="mutate(() => api.portalWrite(`users/${u.id}/status`, { status: u.status === 'active' ? 'disabled' : 'active' }), '用户状态已更新')">{{ u.status === 'active' ? '停用' : '启用' }}</WButton></div>
        <div class="flex gap-2"><WInput v-model="passwords[u.id]" type="password" placeholder="新密码（8–72 字节）" /><WButton size="sm" :disabled="busy" @click="resetPassword(u)">重置密码并退出设备</WButton></div>
      </div>
    </section>
    <section class="glass rounded-xl p-5 space-y-3">
      <h2 class="font-semibold">邀请码</h2><div class="flex gap-2"><WInput v-model="inviteDays" placeholder="有效天数（1–365）" /><WButton :disabled="busy" @click="createInvite">生成邀请码</WButton></div>
      <p v-if="!data.invites.length" class="text-small text-faint">暂无邀请码。</p>
      <div v-for="invite in data.invites" :key="invite.code" class="flex flex-wrap items-center gap-3 border-t border-line py-2 text-small"><code>{{ invite.code }}</code><span class="text-faint">{{ invite.used_by ? `已由 ${username(invite.used_by)} 使用` : invite.expires_at && invite.expires_at < Date.now() / 1000 ? '已过期' : '未使用' }} · {{ invite.expires_at ? fmtTime(invite.expires_at) : '长期有效' }}</span><WButton v-if="!invite.used_by" size="sm" variant="danger" :disabled="busy" @click="mutate(() => api.portalDelete(`invites/${encodeURIComponent(invite.code)}`), '邀请码已撤销')">撤销</WButton></div>
    </section>
    <section class="glass rounded-xl p-5 space-y-3">
      <h2 class="font-semibold">共享分组</h2><p class="text-small text-faint">分组必须显式配置模型、贡献账号及用户授权。空模型范围拒绝全部调用，私人账号不供选择。</p>
      <div class="flex gap-2"><WInput v-model="newGroup" placeholder="WorkBuddy 分组名称" /><WButton :disabled="busy || !newGroup.trim()" @click="mutate(async () => { await api.portalWrite('groups', { name: newGroup.trim() }); newGroup = '' }, '分组已创建')">创建分组</WButton></div>
      <article v-for="g in data.groups" :key="g.id" class="rounded-lg border border-line p-4 space-y-3">
        <div class="flex flex-wrap items-center gap-3"><h3 class="font-semibold">{{ g.name }}</h3><span class="text-micro text-faint">{{ g.provider }} · {{ g.enabled ? '启用' : '停用' }}</span><WButton size="sm" :disabled="busy" @click="mutate(() => api.portalWrite(`groups/${g.id}/enable`, { enabled: !g.enabled }), '分组状态已更新')">{{ g.enabled ? '停用' : '启用' }}</WButton><WButton size="sm" variant="danger" :disabled="busy" @click="removeGroup(g)">删除</WButton></div>
        <p class="text-small">允许模型</p><div class="flex flex-wrap gap-3"><label v-for="model in pickerModels(g)" :key="model" class="flex items-center gap-1 text-micro"><input type="checkbox" :checked="groupDrafts[g.id]?.includes(model)" :disabled="busy" @change="setModel(g, model, ($event.target as HTMLInputElement).checked)" />{{ model }}</label></div><WButton size="sm" :disabled="busy" @click="mutate(() => api.portalWrite(`groups/${g.id}/models`, { allowed_models: groupDrafts[g.id] || [] }), '模型范围已保存')">保存模型范围</WButton>
        <p class="text-small">贡献账号</p><div v-for="uid in accountsOf(g)" :key="uid" class="flex flex-wrap items-center gap-2 text-micro"><code>{{ uid }}</code><WButton size="sm" :disabled="busy" @click="mutate(() => api.portalWrite(`groups/${g.id}/accounts`, { action: 'remove', account_uid: uid }), '账号已移出')">移出</WButton></div>
        <div class="flex gap-2"><select v-model="accountDrafts[g.id]" class="min-w-0 flex-1 rounded-lg border border-line bg-bg px-2 text-small"><option value="">选择有效贡献账号</option><option v-for="a in sharedAccounts.filter(a => !accountsOf(g).includes(a.uid))" :key="a.uid" :value="a.uid">{{ a.nickname || a.uid }}</option></select><WButton size="sm" :disabled="busy || !accountDrafts[g.id]" @click="mutate(() => api.portalWrite(`groups/${g.id}/accounts`, { action: 'add', account_uid: accountDrafts[g.id] }), '账号已加入')">加入</WButton></div>
        <p class="text-small">获授权用户</p><div v-for="id in grantsOf(g)" :key="id" class="flex items-center gap-2 text-small"><span>{{ username(id) }}</span><WButton size="sm" :disabled="busy" @click="mutate(() => api.portalWrite(`groups/${g.id}/grants`, { action: 'revoke', user_id: id }), '授权已撤销')">撤销授权</WButton></div>
        <div class="flex gap-2"><select v-model="userDrafts[g.id]" class="min-w-0 flex-1 rounded-lg border border-line bg-bg px-2 text-small"><option value="">选择用户</option><option v-for="u in data.users.filter(u => u.status === 'active' && !grantsOf(g).includes(u.id))" :key="u.id" :value="String(u.id)">{{ u.username }}</option></select><WButton size="sm" :disabled="busy || !userDrafts[g.id]" @click="mutate(() => api.portalWrite(`groups/${g.id}/grants`, { action: 'grant', user_id: Number(userDrafts[g.id]) }), '授权已授予')">授权</WButton></div>
      </article>
    </section>
    <section class="glass rounded-xl p-5 space-y-3"><h2 class="font-semibold">全局禁用共享模型</h2><p class="text-small text-faint">每行一个完整模型 ID。对所有共享分组和存量用户 Key 生效。</p><textarea v-model="disabledModels" class="min-h-24 w-full rounded-lg border border-line bg-bg p-3 mono text-small" /><WButton :disabled="busy" @click="saveDisabled">保存禁用模型</WButton></section>
    <section class="glass rounded-xl p-5 space-y-3"><h2 class="font-semibold">贡献状态</h2><p v-if="!data.contributions?.length" class="text-small text-faint">暂无贡献。</p><div v-for="c in data.contributions" :key="c.id" class="flex flex-wrap gap-3 border-t border-line py-2 text-small"><span>{{ username(c.user_id) }}</span><code>{{ c.account_uid }}</code><span class="text-faint">{{ c.status }}</span></div></section>
  </div>
</template>
