<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { api } from '@/api/client'
import type { PortalOverview, PortalGroup, PortalUser, PortalAccount } from '@/api/portal'
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
const disabledModels = ref<string[]>([])
const modelSearch = ref('')
const defaultGroup = ref('0')
const defaultAuto = ref(false)
const sharingDrafts = ref<Record<string, { mode: string; groups: number[] }>>({})
const models = ref<string[]>([])
const modelNames = ref<Record<string, string>>({})
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
function matchesModel(m: string) { return `${m} ${modelNames.value[m] || ''}`.toLowerCase().includes(modelSearch.value.trim().toLowerCase()) }
function pickerModels(g: PortalGroup) { return [...new Set([...models.value, ...parseModels(g.allowed_models)])].filter(isPublicModel).filter(matchesModel) }
const disabledOptions = computed(() => [...new Set([...models.value, ...disabledModels.value])].sort().filter(matchesModel))
function username(id: number) { return data.value.users.find(u => u.id === id)?.username || `用户 ${id}` }
const sharedAccounts = computed(() => (data.value.accounts || []).filter(a => {
  const c = data.value.contributions?.find(c => c.account_uid === a.uid)
  return a.provider === 'workbuddy' && (c?.status === 'active' || ['shared', 'both'].includes(a.sharing_mode || ''))
}))
async function load() {
  loading.value = true
  error.value = ''
  try {
    const [overview, catalog, settings] = await Promise.all([api.portalOverview(), api.modelCatalog(), api.getSettings()])
    data.value = { ...overview, users: overview.users || [], invites: overview.invites || [], groups: overview.groups || [] }
    models.value = catalog.models.filter(m => m.provider === 'workbuddy' && m.id !== 'auto' && m.id !== 'workbuddy/auto').map(m => m.id)
    modelNames.value = Object.fromEntries(catalog.models.map(m => [m.id, m.name || m.id]))
    const disabled = settings.portal_disabled_models
    disabledModels.value = parseModels(typeof disabled === 'string' || Array.isArray(disabled) ? disabled as string | string[] : [])
    defaultGroup.value = String(data.value.default_group_id || 0)
    defaultAuto.value = !!data.value.default_auto_grant
    for (const a of data.value.accounts || []) sharingDrafts.value[a.uid] = { mode: a.sharing_mode || 'private', groups: data.value.groups.filter(g => accountsOf(g).includes(a.uid)).map(g => g.id) }
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
function selectVisibleModels(g: PortalGroup) { groupDrafts.value[g.id] = [...new Set([...(groupDrafts.value[g.id] || []), ...pickerModels(g)])] }
function disableVisibleModels() { disabledModels.value = [...new Set([...disabledModels.value, ...disabledOptions.value])] }
async function saveDisabled() {
  const selected = [...new Set(disabledModels.value)]
  await mutate(() => api.saveSettings({ portal_disabled_models: JSON.stringify(selected) }), '全局禁用模型已更新')
}
function toggleDisabled(model: string, checked: boolean) {
  disabledModels.value = checked ? [...new Set([...disabledModels.value, model])] : disabledModels.value.filter(m => m !== model)
}
function togglePool(a: PortalAccount, id: number, checked: boolean) {
  const draft = sharingDrafts.value[a.uid]
  draft.groups = checked ? [...new Set([...draft.groups, id])] : draft.groups.filter(n => n !== id)
}
async function saveSharing(a: PortalAccount) {
  const draft = sharingDrafts.value[a.uid]
  const ids = draft.mode === 'private' ? [] : draft.groups
  if (draft.mode !== 'private' && !ids.length) return toast.error('请至少选择一个共享池')
  const labels: Record<string, string> = { private: '仅管理员私人池', shared: '仅共享池', both: '私人池和共享池共用' }
  const pools = data.value.groups.filter(g => ids.includes(g.id)).map(g => `${g.name}${g.enabled ? '' : '（未启用）'}`).join('、')
  if (!confirm(`将「${a.nickname || a.uid}」改为${labels[draft.mode]}${pools ? `，目标池：${pools}` : ''}？\n仅共享池会停止历史私人 Key 使用此账号；移出共享池会撤销对应共享访问。账号启停、额度和凭据保持原状。`)) return
  await mutate(() => api.portalWrite(`accounts/${encodeURIComponent(a.uid)}/sharing`, { mode: draft.mode, group_ids: ids }), '账号使用范围已更新')
}
async function saveDefault() {
  if (!confirm('保存默认池后，有效共享贡献账号会加入该池；勾选自动授权时，有效贡献者无需逐个授权。停用的池和空模型范围仍不提供调用权限。')) return
  await mutate(() => api.portalWrite('default-group', { group_id: Number(defaultGroup.value), auto_grant: defaultAuto.value }), '默认共享池已更新')
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
    <section class="glass rounded-xl p-5 space-y-3">
      <h2 class="font-semibold">共享接入范围</h2>
      <p class="text-small text-muted">WorkBuddy 已支持共享和本人调用。Qoder、OpenCode 当前只支持管理员私人调用，暂不能转入用户共享池。</p>
      <p class="text-micro text-faint">初次配置：创建一个池并选择模型 → 指定默认池和准入规则 → 在下方勾选要共享的平台账号。用户自己的账号无需池授权即可本人使用。</p>
    </section>
    <section class="glass rounded-xl p-5 space-y-3">
      <h2 class="font-semibold">默认共享池</h2>
      <p class="text-small text-faint">新贡献只加入这个池，自定义池不会自动混入新账号。选择默认池时，已有有效共享贡献也会加入。自动资格只对有效贡献者生效，撤回或停用后实时失效。</p>
      <select v-model="defaultGroup" :disabled="busy" class="w-full rounded-lg border border-line bg-bg p-2 text-small"><option value="0">不指定默认池</option><option v-for="g in data.groups" :key="g.id" :value="String(g.id)">{{ g.name }}{{ g.enabled ? '' : '（未启用）' }}</option></select>
      <label class="flex items-start gap-2 text-small"><input v-model="defaultAuto" type="checkbox" :disabled="busy || defaultGroup === '0'" class="mt-1" /><span>有有效共享贡献的用户自动获得默认池资格，无需逐个授权</span></label>
      <WButton :disabled="busy || !loaded" @click="saveDefault">保存默认池规则</WButton>
    </section>
    <section class="glass rounded-xl p-5 space-y-3">
      <h2 class="font-semibold">账号使用范围</h2>
      <p class="text-small text-faint">平台账号可以私人、共享或同时使用。用户上传账号始终归原用户所有，共享状态由本人决定；此处不会转移归属或重新启用停用账号。</p>
      <p v-if="!data.accounts?.length" class="text-small text-faint">暂无 WorkBuddy 账号，请先在账号管理页添加。</p>
      <article v-for="a in data.accounts" :key="a.uid" class="rounded-lg border border-line p-4 space-y-3">
        <div class="flex flex-wrap gap-2 text-small"><strong>{{ a.nickname || a.uid }}</strong><span class="text-faint">{{ a.provider }} · {{ a.enabled ? '启用' : '停用' }} · {{ a.owner_kind === 'user' ? username(a.contribution_user_id || 0) + ' 所有' : '平台所有' }}</span></div>
        <template v-if="a.owner_kind !== 'user' && a.provider === 'workbuddy' && sharingDrafts[a.uid]">
          <select v-model="sharingDrafts[a.uid].mode" :disabled="busy" class="w-full rounded-lg border border-line bg-bg p-2 text-small"><option value="private">仅管理员私人池</option><option value="shared">仅共享池</option><option value="both">私人池与共享池共用</option></select>
          <div v-if="sharingDrafts[a.uid].mode !== 'private'" class="flex flex-wrap gap-3"><label v-for="g in data.groups" :key="g.id" class="flex items-center gap-2 text-small"><input type="checkbox" :checked="sharingDrafts[a.uid].groups.includes(g.id)" :disabled="busy" @change="togglePool(a, g.id, ($event.target as HTMLInputElement).checked)" />{{ g.name }}{{ g.enabled ? '' : '（未启用）' }}</label><span v-if="!data.groups.length" class="text-small text-warn">请先创建共享池。</span></div>
          <WButton size="sm" :disabled="busy" @click="saveSharing(a)">预览并保存使用范围</WButton>
        </template>
        <p v-else class="text-small text-faint">{{ a.owner_kind === 'user' ? a.status === 'active' ? '本人可用并已同意共享。可在共享池配置中调整池关联。' : a.status === 'private' ? '仅本人使用，恢复共享需本人授权。' : '授权暂不可用，需要本人重新验证。' : '此渠道暂未接入共享。' }}</p>
      </article>
    </section>
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
      <h2 class="font-semibold">共享池分组</h2>
      <p class="text-small text-muted">一个分组是一套共享池权限：用哪些贡献账号、允许哪些模型、授权给哪些用户。例如「基础组」可以让指定用户通过池内账号调用两种基础模型。</p>
      <p class="text-small text-faint">默认池可以自动授权有效贡献者；自定义池用于指定账号、模型和用户。私人平台账号请先在「账号使用范围」转为共享或共用，再加入池。本人账号使用权不依赖分组。</p>
      <WInput v-model="modelSearch" placeholder="搜索模型 ID（同时过滤下方禁用模型）" />
      <div class="flex gap-2"><WInput v-model="newGroup" placeholder="WorkBuddy 分组名称" /><WButton :disabled="busy || !newGroup.trim()" @click="mutate(async () => { await api.portalWrite('groups', { name: newGroup.trim() }); newGroup = '' }, '分组已创建')">创建分组</WButton></div>
      <article v-for="g in data.groups" :key="g.id" class="rounded-lg border border-line p-4 space-y-3">
        <div class="flex flex-wrap items-center gap-3"><h3 class="font-semibold">{{ g.name }}</h3><span class="text-micro text-faint">{{ g.provider }} · {{ g.enabled ? '启用' : '停用' }}</span><WButton size="sm" :disabled="busy" @click="mutate(() => api.portalWrite(`groups/${g.id}/enable`, { enabled: !g.enabled }), '分组状态已更新')">{{ g.enabled ? '停用' : '启用' }}</WButton><WButton size="sm" variant="danger" :disabled="busy" @click="removeGroup(g)">删除</WButton></div>
        <p class="text-small">这个共享池允许的模型</p><div class="flex flex-wrap gap-2"><WButton size="sm" :disabled="busy" @click="selectVisibleModels(g)">选中当前结果</WButton><WButton size="sm" :disabled="busy" @click="groupDrafts[g.id] = []">清空范围</WButton></div><p class="text-micro text-faint">勾选并保存后，获得本组授权的用户才能通过池内账号调用这些模型。未选模型时，本组不提供调用权限。</p><div class="flex flex-wrap gap-3"><label v-for="model in pickerModels(g)" :key="model" class="flex items-center gap-1 text-micro"><input type="checkbox" :checked="groupDrafts[g.id]?.includes(model)" :disabled="busy" @change="setModel(g, model, ($event.target as HTMLInputElement).checked)" />{{ model }}</label></div><WButton size="sm" :disabled="busy" @click="mutate(() => api.portalWrite(`groups/${g.id}/models`, { allowed_models: groupDrafts[g.id] || [] }), '模型范围已保存')">保存模型范围</WButton>
        <p class="text-small">贡献账号</p><div v-for="uid in accountsOf(g)" :key="uid" class="flex flex-wrap items-center gap-2 text-micro"><code>{{ uid }}</code><WButton size="sm" :disabled="busy" @click="mutate(() => api.portalWrite(`groups/${g.id}/accounts`, { action: 'remove', account_uid: uid }), '账号已移出')">移出</WButton></div>
        <div class="flex gap-2"><select v-model="accountDrafts[g.id]" class="min-w-0 flex-1 rounded-lg border border-line bg-bg px-2 text-small"><option value="">选择有效贡献账号</option><option v-for="a in sharedAccounts.filter(a => !accountsOf(g).includes(a.uid))" :key="a.uid" :value="a.uid">{{ a.nickname || a.uid }}</option></select><WButton size="sm" :disabled="busy || !accountDrafts[g.id]" @click="mutate(() => api.portalWrite(`groups/${g.id}/accounts`, { action: 'add', account_uid: accountDrafts[g.id] }), '账号已加入')">加入</WButton></div>
        <p class="text-small">获授权用户</p><div v-for="id in grantsOf(g)" :key="id" class="flex items-center gap-2 text-small"><span>{{ username(id) }}</span><WButton size="sm" :disabled="busy" @click="mutate(() => api.portalWrite(`groups/${g.id}/grants`, { action: 'revoke', user_id: id }), '授权已撤销')">撤销授权</WButton></div>
        <div class="flex gap-2"><select v-model="userDrafts[g.id]" class="min-w-0 flex-1 rounded-lg border border-line bg-bg px-2 text-small"><option value="">选择用户</option><option v-for="u in data.users.filter(u => u.status === 'active' && !grantsOf(g).includes(u.id))" :key="u.id" :value="String(u.id)">{{ u.username }}</option></select><WButton size="sm" :disabled="busy || !userDrafts[g.id]" @click="mutate(() => api.portalWrite(`groups/${g.id}/grants`, { action: 'grant', user_id: Number(userDrafts[g.id]) }), '授权已授予')">授权</WButton></div>
      </article>
    </section>
    <section class="glass rounded-xl p-5 space-y-3"><h2 class="font-semibold">全局禁用用户模型</h2><p class="text-small text-faint">勾选即禁用，保存后对用户本人账号、共享池及存量用户 Key 生效；管理员历史私人 Key 不受影响。暂时退出目录的禁用项也会保留。</p><WInput v-model="modelSearch" placeholder="搜索模型名称或 ID" /><WButton size="sm" :disabled="busy || !disabledOptions.length" @click="disableVisibleModels">勾选禁用当前结果</WButton><div class="flex flex-wrap gap-3"><label v-for="model in disabledOptions" :key="model" class="flex items-center gap-2 rounded-lg border border-line p-2 text-micro"><input type="checkbox" :checked="disabledModels.includes(model)" :disabled="busy" @change="toggleDisabled(model, ($event.target as HTMLInputElement).checked)" /><span class="break-all">{{ modelNames[model] || model }} · {{ model }}{{ models.includes(model) ? '' : '（当前不在目录）' }}</span></label></div><p v-if="!disabledOptions.length" class="text-small text-faint">暂无匹配模型。可在模型管理页刷新上游目录。</p><WButton :disabled="busy || !loaded" @click="saveDisabled">保存禁用模型（{{ disabledModels.length }} 项）</WButton></section>
    <section class="glass rounded-xl p-5 space-y-3"><h2 class="font-semibold">贡献状态</h2><p v-if="!data.contributions?.length" class="text-small text-faint">暂无贡献。</p><div v-for="c in data.contributions" :key="c.id" class="flex flex-wrap gap-3 border-t border-line py-2 text-small"><span>{{ username(c.user_id) }}</span><code>{{ c.account_uid }}</code><span class="text-faint">{{ c.status }}</span></div></section>
  </div>
</template>
