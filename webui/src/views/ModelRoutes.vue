<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { api, type ModelRoute, type RouteMember } from '@/api/client'
import { toast } from '@/lib/toast'
import { confirm } from '@/lib/confirm'
import WPage from '@/components/ui/WPage.vue'
import WButton from '@/components/ui/WButton.vue'
import WInput from '@/components/ui/WInput.vue'
import WSpinner from '@/components/ui/WSpinner.vue'
import WEmpty from '@/components/ui/WEmpty.vue'
const routes = ref<ModelRoute[]>([])
const models = ref<RouteMember[]>([])
const suggestions = ref<ModelRoute[]>([])
const previewing = ref(false)
const loading = ref(true)
const saving = ref(false)
const editing = ref(false)
const search = ref('')
const empty = (): ModelRoute => ({ id: 0, model: '', name: '', enabled: true, members: [], revision: 0 })
const draft = ref<ModelRoute>(empty())
const catalog = computed(() => new Map(models.value.map(m => [m.id, m])))
const available = computed(() => models.value.filter(m => !draft.value.members.includes(m.id) && `${m.id} ${m.name || ''}`.toLowerCase().includes(search.value.toLowerCase())))
function edit(route?: ModelRoute) {
 draft.value = route ? { ...route, members: [...route.members] } : empty()
 search.value = ''; editing.value = true
}
function move(index: number, delta: number) {
 const list = draft.value.members; const target = index + delta
 if (target < 0 || target >= list.length) return
 ;[list[index], list[target]] = [list[target], list[index]]
}
async function load() {
 loading.value = true
 try { const data = await api.modelRoutes(); routes.value = data.routes; models.value = data.models; suggestions.value = data.suggestions || [] }
 catch { toast.error('统一模型配置读取失败，请重试') }
 finally { loading.value = false }
}
async function save() {
 if (!draft.value.model.trim() || !draft.value.members.length) { toast.error('请填写调用名称并选择模型成员'); return }
 saving.value = true
 try { await api.saveModelRoute(draft.value); toast.success('统一模型已保存'); editing.value = false; await load() }
 catch (e: any) { toast.error(e?.response?.data?.error?.message || '保存失败，请刷新后重试') }
 finally { saving.value = false }
}
async function generate() {
 if (!suggestions.value.length || saving.value) return
 if (!await confirm({ title: `生成 ${suggestions.value.length} 个统一模型？`, body: '仅保存下方列出的同名模型组合，不修改已有路由。生成后可编辑成员、顺序与显示名称；现有受限 Key 不会自动增加权限。', okText: '生成合并表' })) return
 saving.value = true
 let created = 0; let failed = 0
 try {
  for (const route of suggestions.value) {
   try { await api.saveModelRoute(route); created++ } catch { failed++ }
  }
  if (created) toast.success(`已生成 ${created} 个统一模型`)
  if (failed) toast.error(`${failed} 个配置未生成，目录或名称可能已变化，请刷新后检查`)
  await load()
  if (!suggestions.value.length) previewing.value = false
 } finally { saving.value = false }
}
async function remove(route: ModelRoute) {
 if (!await confirm({ title: `删除 ${route.name}？`, body: '该调用名称将停止可用。已有 Key 的成员授权不会因同名重建而恢复。', tone: 'danger', okText: '删除' })) return
 try { await api.deleteModelRoute(route); if (draft.value.id === route.id) editing.value = false; await load() }
 catch { toast.error('删除失败，配置可能已变更，请刷新后重试') }
}
onMounted(load)
</script>

<template>
 <WPage title="统一模型路由" sub="一个调用名称，关联多个渠道的实际模型">
  <template #actions>
   <RouterLink to="/models"><WButton>返回模型目录</WButton></RouterLink>
   <WButton :disabled="loading || saving" @click="load">刷新</WButton>
   <WButton :disabled="loading || saving" @click="previewing = !previewing">自动合并同名</WButton>
   <WButton variant="primary" :disabled="saving" @click="edit()">新建统一模型</WButton>
  </template>
  <div class="rounded-xl border border-line bg-panel p-4 text-small text-muted leading-relaxed mb-5">
   按成员顺序选择渠道，成功后保持会话粘性。每个渠道继续使用自己的账号成本、区域和故障切换策略。
   仅在响应开始前的限流或服务故障时尝试下一成员；权限和请求能力不符合的成员会跳过。
  </div>
  <section v-if="previewing" class="rounded-xl border border-line bg-panel p-4 mb-5 space-y-3">
   <div class="flex flex-wrap items-start justify-between gap-3">
    <div><h2 class="font-semibold text-ink">同名模型合并预览</h2><p class="text-small text-muted mt-1">仅匹配模型 ID 完全相同的不同渠道；已参与手工路由的模型跳过。名称冲突时使用 group/ 前缀。</p></div>
    <WButton variant="primary" :disabled="loading || saving || !suggestions.length" :loading="saving" @click="generate">生成合并表 · {{ suggestions.length }}</WButton>
   </div>
   <div v-if="suggestions.length" class="grid gap-3 md:grid-cols-2">
    <article v-for="route in suggestions" :key="route.model" class="rounded-lg border border-line p-3 min-w-0">
     <div class="flex items-start justify-between gap-2"><p class="font-mono text-small text-brand break-all min-w-0">{{ route.model }}</p><WButton size="sm" :disabled="saving" @click="edit(route); previewing = false">单独编辑</WButton></div>
     <ol class="mt-2 space-y-1"><li v-for="(member, index) in route.members" :key="member" class="text-xs text-muted break-all">{{ index + 1 }}. {{ member }}</li></ol>
    </article>
   </div>
   <p v-else class="text-small text-muted">当前没有新的可合并同名模型。已有配置不会被覆盖。</p>
  </section>
  <WSpinner v-if="loading && !routes.length" />
  <div v-else class="grid gap-5 items-start" :class="editing ? 'xl:grid-cols-[minmax(0,1fr)_minmax(0,1.2fr)]' : ''">
   <section class="min-w-0 space-y-3">
    <WEmpty v-if="!routes.length" title="尚无统一模型" hint="新建一个调用名称，再选择需要合并的渠道模型。" />
    <article v-for="route in routes" :key="route.id" class="rounded-xl border border-line bg-panel p-4">
     <div class="flex items-start justify-between gap-3">
      <div class="min-w-0"><h3 class="font-semibold text-ink break-words">{{ route.name }}</h3><p class="font-mono text-small text-brand break-all mt-1">{{ route.model }}</p></div>
      <span class="text-small shrink-0" :class="route.enabled ? 'text-brand' : 'text-faint'">{{ route.enabled ? '已启用' : '已停用' }}</span>
     </div>
     <ol class="mt-3 space-y-2">
      <li v-for="(member, index) in route.members" :key="member" class="flex items-start gap-2 text-small text-muted"><span class="text-faint shrink-0 w-4">{{ index + 1 }}</span><span class="font-mono break-all min-w-0">{{ member }}</span><span v-if="!catalog.has(member)" class="text-warn shrink-0">暂不可用</span></li>
     </ol>
     <div class="flex justify-end gap-2 mt-4"><WButton size="sm" :disabled="saving" @click="edit(route)">编辑</WButton><WButton size="sm" variant="danger" :disabled="saving" @click="remove(route)">删除</WButton></div>
    </article>
   </section>
   <form v-if="editing" class="min-w-0 rounded-xl border border-line bg-panel p-5 space-y-4" @submit.prevent="save">
    <h2 class="font-semibold text-ink">{{ draft.id ? '编辑统一模型' : '新建统一模型' }}</h2>
    <div class="space-y-2"><label class="text-small text-muted">调用名称</label><WInput v-model="draft.model" aria-label="调用名称" placeholder="例如 deepseek-flash-pool" :disabled="!!draft.id || saving" /><p class="text-xs text-faint">可使用无前缀名称，不能覆盖现有模型、别名或渠道命名空间。保存后调用名称固定。</p></div>
    <div class="space-y-2"><label class="text-small text-muted">显示名称</label><WInput v-model="draft.name" aria-label="显示名称" placeholder="例如 DeepSeek Flash 共享路由" :disabled="saving" /></div>
    <label class="flex gap-2 items-center text-small text-ink"><input v-model="draft.enabled" type="checkbox" :disabled="saving" />启用该调用名称</label>
    <div><h3 class="text-small font-semibold text-ink mb-2">成员优先顺序</h3>
     <p v-if="!draft.members.length" class="text-small text-faint py-3">从下方模型目录添加成员。</p>
     <ol class="space-y-2"><li v-for="(id, index) in draft.members" :key="id" class="rounded-lg border border-line p-3">
      <div class="flex gap-2 items-start"><span class="text-brand text-small shrink-0">{{ index + 1 }}</span><div class="min-w-0 flex-1"><p class="text-small text-ink break-words">{{ catalog.get(id)?.name || id }}</p><p class="font-mono text-xs text-faint break-all mt-1">{{ id }}</p><span v-if="!catalog.has(id)" class="text-xs text-warn">不在当前可用目录中</span></div></div>
      <div class="flex justify-end gap-1 mt-2"><WButton size="sm" :disabled="index === 0 || saving" @click="move(index, -1)">上移</WButton><WButton size="sm" :disabled="index === draft.members.length - 1 || saving" @click="move(index, 1)">下移</WButton><WButton size="sm" :disabled="saving" @click="draft.members.splice(index, 1)">移除</WButton></div>
     </li></ol>
    </div>
    <div class="border-t border-line pt-4"><WInput v-model="search" aria-label="搜索实际模型" placeholder="搜索模型名称或渠道" />
     <div class="max-h-72 overflow-y-auto mt-2 space-y-1">
      <button v-for="model in available" :key="model.id" type="button" class="w-full text-left rounded-lg p-3 hover:bg-elevated disabled:opacity-50" :disabled="saving" @click="draft.members.push(model.id)"><div class="flex justify-between gap-2 text-small"><span class="text-ink min-w-0 break-words">{{ model.name || model.id }}</span><span class="text-brand shrink-0">添加</span></div><p class="font-mono text-xs text-faint break-all mt-1">{{ model.id }}</p><p class="text-xs text-muted mt-1">{{ model.vision ? '图文输入' : '文本输入' }}<template v-if="model.context_length"> · 上下文 {{ model.context_length.toLocaleString() }}</template><template v-if="model.max_output_tokens"> · 输出上限 {{ model.max_output_tokens.toLocaleString() }}</template></p></button>
      <p v-if="!available.length" class="text-small text-faint p-3">没有匹配的可添加模型。</p>
     </div>
    </div>
    <p class="text-xs text-muted leading-relaxed">新增成员不会自动扩大已有受限 Key 的授权；重新保存 Key 的模型权限后，才授权其使用当前成员。用户调用仍需满足当前模型和账号权限。</p>
    <div class="flex justify-end gap-2"><WButton :disabled="saving" @click="editing = false">取消</WButton><WButton type="submit" variant="primary" :loading="saving">保存配置</WButton></div>
   </form>
  </div>
 </WPage>
</template>
