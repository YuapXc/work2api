<script setup lang="ts">
import { useLiveRead } from '@/lib/liveRead'
import { ref, computed } from 'vue'
import { RouterLink } from 'vue-router'
import { api } from '@/api/client'
import type { PortalOverview, PortalInvite, InviteRedemption } from '@/api/portal'
import { toast } from '@/lib/toast'
import WButton from '@/components/ui/WButton.vue'
import WInput from '@/components/ui/WInput.vue'
import IdentityManager from '@/components/IdentityManager.vue'
import ModelAccessPicker from '@/components/ModelAccessPicker.vue'
import InvitationForm from '@/components/InvitationForm.vue'
import WModal from '@/components/ui/WModal.vue'
import { fmtTime } from '../../src-portal/api'

const data = ref<PortalOverview>({ users: [], invites: [], groups: [], accounts: [], contributions: [], registration_mode: '' })
const loaded = ref(false), busy = ref(false), actionError = ref('')
const bootstrapName = ref(''), bootstrapPass = ref('')
const inviteModal = ref(false), editingInvite = ref<PortalInvite | null>(null)
const history = ref<InviteRedemption[]>([]), historyOffset = ref(-1), historyLoading = ref(false), historyError = ref('')
let historyVersion = 0
const publicURL = ref(''), savedPublicURL = ref('')
const settingsLoaded = ref(false)
const models = ref<string[]>([]), modelNames = ref<Record<string,string>>({})
const policy = ref<{mode:'auto'|'manual';include:string[];exclude:string[]}>({mode:'auto',include:[],exclude:[]})
const disabledModels = ref<string[]>([])
const userDrafts = ref<Record<number,string[]>>({})
const expandedUsers = ref(new Set<number>())
const scopedByUser = computed(() => Object.fromEntries(Object.entries(data.value.user_model_grants || {}).map(([id,grants]) => [id,grants.filter(g=>!g.account_uids.includes('*'))])))
const userModelOptions = computed(() => Object.fromEntries(data.value.users.map(u => {
 const scoped = new Set((scopedByUser.value[u.id] || []).map(g=>g.model))
 return [u.id,models.value.filter(m=>!scoped.has(m))]
})))
function toggleUserModels(id:number,event:Event){const next=new Set(expandedUsers.value);if((event.target as HTMLDetailsElement).open)next.add(id);else next.delete(id);expandedUsers.value=next}
const adminsExist = computed(() => data.value.users.some(u => ['admin','owner'].includes(u.role)))
function parseModels(raw: unknown): string[] { if (Array.isArray(raw)) return raw.filter((v):v is string=>typeof v==='string');try {return parseModels(JSON.parse(String(raw || '[]')))}catch{return []} }
function contributionLabel(uid:string) { const a=data.value.accounts?.find(a=>a.uid===uid); return a?.alias || a?.nickname || uid.slice(0,8) }
function username(id:number) {return data.value.users.find(u=>u.id===id)?.username || `用户 ${id}`}
function scopedGrants(id:number) {return scopedByUser.value[id] || []}
let policySnapshot: unknown, settingsSnapshot: { disabled: string[]; url: string } | undefined
let grantsSnapshot: Record<number, string[]> = {}
const equal = (a: unknown, b: unknown) => JSON.stringify(a) === JSON.stringify(b)
const overviewRead = useLiveRead(signal => api.portalOverview(signal), overview => {
 data.value={...overview,users:overview.users||[],invites:overview.invites||[],groups:[]}
 const nextPolicy=overview.model_policy || {mode:'auto' as const,include:[],exclude:[]}
 if(policySnapshot === undefined || equal(policy.value,policySnapshot)) policy.value=structuredClone(nextPolicy)
 policySnapshot=structuredClone(nextPolicy)
 const nextGrants=Object.fromEntries(data.value.users.map(u=>[u.id,(overview.user_model_grants?.[u.id]||[]).filter(g=>g.account_uids.includes('*')).map(g=>g.model)]))
 const drafts: Record<number,string[]>={}
 for(const [id,value] of Object.entries(nextGrants)) drafts[Number(id)]=userDrafts.value[Number(id)] !== undefined && !equal(userDrafts.value[Number(id)],grantsSnapshot[Number(id)]) ? userDrafts.value[Number(id)] : [...value]
 userDrafts.value=drafts;grantsSnapshot=structuredClone(nextGrants)
 loaded.value=true
}, { interval: 30000, enabled: () => !busy.value })
const catalogRead = useLiveRead(signal => api.modelCatalog(signal), catalog => {
 models.value=catalog.models.map(m=>m.id).filter(m=>m!=='auto'&&m!=='workbuddy/auto')
 modelNames.value=Object.fromEntries(catalog.models.map(m=>[m.id,m.name||m.id]))
}, { interval: 60000 })
const settingsRead = useLiveRead(signal => api.getSettings(signal), settings => {
 const disabled=parseModels(settings.portal_disabled_models), url=settings.portal_public_url||''
 if(!settingsSnapshot || equal(disabledModels.value,settingsSnapshot.disabled)) disabledModels.value=[...disabled]
 if(!settingsSnapshot || publicURL.value===settingsSnapshot.url) publicURL.value=url
 savedPublicURL.value=url;settingsSnapshot={disabled:[...disabled],url};settingsLoaded.value=true
}, { interval: 60000, enabled: () => !busy.value })
const loading=overviewRead.loading
const error=computed({ get:()=>actionError.value || overviewRead.error.value || catalogRead.error.value || settingsRead.error.value, set:(value:string)=>{actionError.value=value} })
async function load() { actionError.value='';await Promise.all([overviewRead.refresh(),catalogRead.refresh(),settingsRead.refresh()]) }
async function mutate(action:()=>Promise<unknown>,message:string) {if(busy.value)return;busy.value=true;try{await action();toast.success(message);await load()}catch(e:any){error.value=e?.response?.data?.error?.message||e?.message||'操作失败'}finally{busy.value=false}}
async function bootstrap() {if(!bootstrapName.value.trim()||!bootstrapPass.value)return toast.error('请填写管理员用户名和密码');await mutate(async()=>{await api.portalWrite('bootstrap',{username:bootstrapName.value.trim(),password:bootstrapPass.value});bootstrapPass.value='';window.location.reload()},'超级管理员已初始化')}
async function savePolicy() {if(!loaded.value || catalogRead.loading.value || settingsRead.loading.value || catalogRead.error.value || settingsRead.error.value)return toast.error('模型配置尚未读取成功，请稍后重试');await mutate(()=>api.portalWrite('model-policy',policy.value),'默认模型规则已保存')}
async function saveUser(id:number,remove_scoped:string[]=[]) {if(catalogRead.loading.value || catalogRead.error.value)return toast.error('模型目录尚未读取成功，请稍后重试');await mutate(()=>api.portalWrite(`users/${id}/models`,{models:userDrafts.value[id]||[],remove_scoped}),'用户额外授权已更新')}
async function removeScoped(id:number,model:string) {if(!confirm(`移除 ${model} 的限定账号授权？本人和默认模型权限继续保留。`))return;await saveUser(id,[model])}
type InviteSettings = {max_uses:number;expires_at:number;enabled:boolean}
async function createInvite(settings: InviteSettings) { await mutate(() => api.portalWrite('invites', settings), '邀请码已创建') }
function inviteAvailable(invite: PortalInvite) { return !!invite.enabled && (invite.max_uses === 0 || invite.used_count < invite.max_uses) && (!invite.expires_at || invite.expires_at > Date.now()/1000) }
function inviteStatus(invite: PortalInvite) { if (!invite.enabled) return '已停用'; if (invite.expires_at && invite.expires_at <= Date.now()/1000) return '已过期'; if (invite.max_uses && invite.used_count >= invite.max_uses) return '次数用尽'; return '可使用' }
async function inviteHistory(more = false) {
 const item = editingInvite.value; if (!item || historyLoading.value) return
 const version = ++historyVersion; historyLoading.value=true;historyError.value=''
 try { const result=await api.portalInviteHistory(item.code, more ? historyOffset.value : 0); if (version!==historyVersion || editingInvite.value?.code!==item.code) return; history.value=more ? [...new Map([...history.value,...result.records].map(record=>[record.user_id,record])).values()] : result.records; historyOffset.value=result.next_offset }
 catch(e:any) { if(version===historyVersion)historyError.value=e?.response?.data?.error?.message || '使用记录加载失败' }
 finally { if(version===historyVersion)historyLoading.value=false }
}
function editInvite(invite: PortalInvite) { error.value=''; historyVersion++;historyLoading.value=false;editingInvite.value={...invite};history.value=[];historyOffset.value=-1;historyError.value='';inviteModal.value=true;void inviteHistory() }
async function saveInvite(settings: InviteSettings) { const item=editingInvite.value;if(!item)return;await mutate(async()=>{await api.portalWrite(`invites/${encodeURIComponent(item.code)}`, {...settings,revision:item.revision});inviteModal.value=false}, '邀请码配置已保存') }
async function toggleInvite(invite: PortalInvite) { await mutate(() => api.portalWrite(`invites/${encodeURIComponent(invite.code)}`, {max_uses:invite.max_uses,expires_at:invite.expires_at,enabled:!invite.enabled,revision:invite.revision}), invite.enabled ? '邀请码已停用，使用记录保留' : '邀请码已启用') }
async function savePublicURL() {
  if(settingsRead.loading.value || settingsRead.error.value)return toast.error('门户设置尚未读取成功，请稍后重试')
  await mutate(() => api.saveSettings({ portal_public_url: publicURL.value.trim() }), '公网门户地址已保存')
}
async function saveDisabledModels() {
 if(settingsRead.loading.value || settingsRead.error.value || catalogRead.loading.value || catalogRead.error.value)return toast.error('模型配置尚未读取成功，请稍后重试')
 await mutate(()=>api.saveSettings({portal_disabled_models:JSON.stringify(disabledModels.value)}),'禁用规则已保存')
}
async function copyInvite(invite: PortalInvite, link = false) {
  if (!inviteAvailable(invite)) return toast.error('邀请码已停用、过期或次数用尽')
  let text = invite.code
  if (link) {
    if (!savedPublicURL.value || publicURL.value.trim() !== savedPublicURL.value) return toast.error('请先保存公网门户地址')
    const url = new URL(savedPublicURL.value)
    url.searchParams.set('aff', invite.code)
    text = url.href
  }
  try { await navigator.clipboard.writeText(text); toast.success(link ? '邀请链接已复制' : '邀请码已复制') }
  catch { toast.error('复制失败，请检查浏览器剪贴板权限') }
}
</script>

<template>
 <div class="mx-auto max-w-[1320px] space-y-5 p-4 md:p-6">
  <div class="flex flex-wrap items-center justify-between gap-3"><div><h1 class="text-lg font-semibold">用户与访问权限</h1><p v-if="loaded" class="mt-1 text-small text-faint">注册方式：{{ data.registration_mode === 'open' ? '开放注册' : data.registration_mode === 'closed' ? '关闭注册' : '邀请码注册' }}</p></div><WButton :loading="loading" :disabled="busy || loading" @click="load">刷新</WButton></div>
  <p v-if="error" role="alert" class="rounded-lg border border-fault/30 p-3 text-small text-fault">{{ error }}</p>
  <section v-if="loaded && !adminsExist" class="glass rounded-xl p-5 space-y-3"><h2 class="font-semibold">初始化超级管理员</h2><WInput v-model="bootstrapName" placeholder="管理员用户名" /><WInput v-model="bootstrapPass" type="password" placeholder="8–72 字节密码" /><WButton :loading="busy" @click="bootstrap">初始化</WButton></section>
  <IdentityManager v-if="loaded" :users="data.users" :max-concurrency="data.user_concurrency_max || 4" :overrides="data.user_concurrency_overrides" @refresh="load" />
  <section class="glass rounded-xl p-5 space-y-4">
   <h2 class="font-semibold">默认开放模型</h2>
   <p class="text-small leading-relaxed text-muted">所有用户均可创建 Key，无需贡献账号。自动模式按明确的免费来源发现模型；授权后仍沿用正常账号命中策略，可能消耗其他账号额度，并不保证每次调用免费。撤回的贡献账号仅本人可用。</p>
   <select v-model="policy.mode" :disabled="busy || loading || !loaded" aria-label="默认模型模式" class="rounded-lg border border-line bg-bg p-2 text-small"><option value="auto">自动发现免费来源＋手工增删</option><option value="manual">仅手工维护列表</option></select>
   <p class="text-micro text-faint">当前开放 {{ data.default_models?.length || 0 }} 个模型；跟随权限的 Key 自动更新，固定范围 Key 保留原选择。</p>
   <details class="rounded-lg border border-line p-3"><summary class="cursor-pointer text-small">{{ policy.mode === 'auto' ? '额外开放模型' : '手工开放模型' }}（{{ policy.include.length }}）</summary><ModelAccessPicker v-model="policy.include" :options="models" :names="modelNames" :disabled="busy || loading || !loaded" class="mt-3" /></details>
   <details class="rounded-lg border border-line p-3"><summary class="cursor-pointer text-small">排除默认模型（{{ policy.exclude.length }}）</summary><p class="mt-2 text-micro text-faint">排除只影响默认权限；本人账号或额外授权仍可调用。需要禁止所有用户调用时，使用下方全局禁用。</p><ModelAccessPicker v-model="policy.exclude" :options="models" :names="modelNames" :disabled="busy || loading || !loaded" class="mt-3" /></details>
   <WButton :disabled="busy || loading || !loaded" @click="savePolicy">保存默认规则</WButton>
  </section>
  <section class="glass rounded-xl p-5 space-y-3"><h2 class="font-semibold">用户额外授权</h2><p class="text-small text-faint">在默认模型和本人账号之外增加调用权限。限定账号授权仅适用于指定账号。</p>
   <details v-for="u in data.users.filter(u=>u.role==='user')" :key="u.id" class="rounded-lg border border-line p-3" @toggle="toggleUserModels(u.id,$event)"><summary class="cursor-pointer text-small">{{ u.username }} · 额外授权 {{ userDrafts[u.id]?.length || 0 }}<span v-if="scopedGrants(u.id).length"> · 限定账号授权 {{ scopedGrants(u.id).length }}</span></summary>
    <div v-for="g in scopedGrants(u.id)" :key="g.model" class="mt-3 flex flex-wrap items-center gap-2 text-small"><code class="break-all">{{ g.model }}</code><span class="text-faint">限定 {{ g.account_uids.length }} 个账号</span><WButton size="sm" :disabled="busy || loading" @click="removeScoped(u.id,g.model)">移除授权</WButton></div>
    <ModelAccessPicker v-if="expandedUsers.has(u.id)" v-model="userDrafts[u.id]" :options="userModelOptions[u.id] || []" :names="modelNames" :disabled="busy || loading" class="mt-3" /><WButton size="sm" :disabled="busy || loading" class="mt-3" @click="saveUser(u.id)">保存额外授权</WButton>
   </details><p v-if="!data.users.some(u=>u.role==='user')" class="text-small text-faint">暂无普通用户。</p>
  </section>
  <section class="glass rounded-xl p-5 space-y-3"><h2 class="font-semibold">全局禁用用户模型</h2><p class="text-small text-faint">对默认权限、本人账号、额外授权和现有用户 Key 均生效。管理员 Key 保持其原有配置。</p><ModelAccessPicker v-model="disabledModels" :options="models" :names="modelNames" :disabled="busy || loading || !settingsLoaded" /><WButton :disabled="busy || loading || !settingsLoaded" @click="saveDisabledModels">保存禁用规则</WButton></section>
    <section class="glass rounded-xl p-5 space-y-3">
      <h2 class="font-semibold">邀请码</h2><InvitationForm :disabled="busy || loading" @save="createInvite" />
      <label for="portal-public-url" class="block text-small text-muted">公网门户地址</label>
      <div class="flex flex-wrap gap-2"><WInput id="portal-public-url" v-model="publicURL" :disabled="!settingsLoaded" placeholder="https://portal.example.com/" class="min-w-0 flex-1" /><WButton :disabled="busy || loading || !settingsLoaded" @click="savePublicURL">保存地址</WButton></div>
      <p class="text-micro text-faint">邀请链接使用此地址，避免复制出管理员当前访问的本机地址。可配置使用次数与到期时间；不限次数、永不过期需要明确选择。</p>
      <p v-if="!data.invites.length" class="text-small text-faint">暂无邀请码。</p>
      <div v-for="invite in data.invites" :key="invite.code" class="space-y-2 border-t border-line py-3 text-small">
       <div class="flex flex-wrap items-center gap-3"><code>{{ invite.code }}</code><span :class="inviteAvailable(invite) ? 'text-live' : 'text-faint'">{{ inviteStatus(invite) }}</span><span class="text-faint">已用 {{ invite.used_count }} / {{ invite.max_uses || '不限' }} 次</span><span class="text-faint">{{ invite.expires_at ? '到期 ' + fmtTime(invite.expires_at) : '永不过期' }}</span></div>
       <div class="flex flex-wrap gap-2"><WButton size="sm" :disabled="busy || loading || !inviteAvailable(invite)" @click="copyInvite(invite)">复制邀请码</WButton><WButton size="sm" :disabled="busy || loading || !inviteAvailable(invite) || !savedPublicURL || publicURL.trim() !== savedPublicURL" @click="copyInvite(invite, true)">复制邀请链接</WButton><WButton size="sm" :disabled="busy || loading" @click="editInvite(invite)">编辑与使用记录</WButton><WButton size="sm" :variant="invite.enabled ? 'danger' : 'subtle'" :disabled="busy || loading" @click="toggleInvite(invite)">{{ invite.enabled ? '停用' : '启用' }}</WButton></div>
      </div>
    </section>

  <section class="glass rounded-xl p-5 space-y-3"><h2 class="font-semibold">贡献账号</h2><p class="text-small text-faint"><RouterLink to="/accounts" class="text-brand hover:underline">管理账号</RouterLink> · <RouterLink to="/keys" class="text-brand hover:underline">管理用户 Key</RouterLink></p><p v-if="!data.contributions?.length" class="text-small text-faint">暂无贡献。</p><div v-for="c in data.contributions" :key="c.id" class="flex flex-wrap gap-3 border-t border-line py-2 text-small"><span>{{ username(c.user_id) }}</span><RouterLink to="/accounts" class="text-brand hover:underline" :title="c.account_uid">{{ contributionLabel(c.account_uid) }}</RouterLink><span class="text-faint">{{ c.status === 'active' ? '已贡献' : c.status === 'private' ? '仅本人使用' : c.status }}</span></div></section>
  <WModal v-model:open="inviteModal" :title="'邀请码 ' + (editingInvite?.code || '')" :dismissible="!busy" size="lg">
   <div v-if="editingInvite" class="space-y-5"><p v-if="error" role="alert" class="text-small text-fault">{{ error }}</p><InvitationForm :key="editingInvite.code + ':' + editingInvite.revision" :initial="editingInvite" :disabled="busy" @save="saveInvite" />
    <section class="space-y-2 border-t border-line pt-4"><h3 class="font-semibold">使用记录</h3><p class="text-micro text-faint">注册成功才消耗次数，停用或到期不会影响已有用户。</p><p v-if="historyError" role="alert" class="text-small text-fault">{{ historyError }} <button class="text-brand" @click="inviteHistory()">重试</button></p><p v-if="historyLoading" class="text-small text-faint">读取中…</p><p v-else-if="!historyError && !history.length" class="text-small text-faint">暂无使用记录。</p><div v-for="record in history" :key="record.user_id" class="flex flex-wrap justify-between gap-2 text-small"><span>{{ record.username || '用户 ' + record.user_id }}</span><span class="text-faint">{{ fmtTime(record.used_at) }}</span></div><WButton v-if="historyOffset >= 0" size="sm" :disabled="historyLoading" @click="inviteHistory(true)">加载更多</WButton></section>
   </div>
  </WModal>
 </div>
</template>
