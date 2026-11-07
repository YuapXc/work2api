<script setup lang="ts">
// Key 管理：创建（默认收窄到全部可用模型）、启停、模型收窄、删除。
// 明文仅按需读取本人 Key，不持久化到浏览器。
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { api, fmtTokens, fmtTime, type KeyInfo, type Me } from '../api'
import { toast } from '../../src/lib/toast'
import { confirm } from '../../src/lib/confirm'
import ModelSelectionActions from '../../src/components/ModelSelectionActions.vue'
import WInputSearch from '../../src/components/ui/WInput.vue'
import WButton from '../../src/components/ui/WButton.vue'
import WInput from '../../src/components/ui/WInput.vue'

const props = defineProps<{ me: Me }>()
const baseURL = window.location.origin + '/v1'

const keys = ref<KeyInfo[]>([])
const loading = ref(true)
const newName = ref('')
const creating = ref(false)
const newPolicy = ref<'follow' | 'fixed'>('follow')
const freshKey = ref<{ id: number; key: string } | null>(null)
const revealedKeys = ref<Record<number, string>>({})
const showModelsOf = ref<number | null>(null)
const savingModels = ref(false)
const editPolicy = ref<'follow' | 'fixed'>('fixed')
const draftModels = ref<string[]>([])
const modelSearch = ref('')
const visibleModels = computed(() => [...new Set([...props.me.available_models, ...draftModels.value])].filter(m => m.toLowerCase().includes(modelSearch.value.trim().toLowerCase())))
const rowBusy = ref(new Set<number>())
let disposed = false
let loadVersion = 0
onUnmounted(() => { disposed = true; freshKey.value = null; revealedKeys.value = {} })
const loadError = ref('')

async function load() {
  const version = ++loadVersion
  loading.value = true
  loadError.value = ''
  try { const result = await api.keys(); if (!disposed && version === loadVersion) keys.value = result.keys }
  catch (e: any) { if (!disposed && version === loadVersion) loadError.value = e?.message || '加载密钥失败' }
  finally { if (!disposed && version === loadVersion) loading.value = false }
}
onMounted(load)

async function create() {
  if (creating.value) return
  const name = newName.value.trim()
  if (!name) return toast.error('请填写 Key 名称')
  creating.value = true
  try {
    const res = await api.createKey(name, newPolicy.value === 'fixed' ? props.me.available_models : undefined, newPolicy.value)
    if (disposed) return
    freshKey.value = res
    newName.value = ''
    await load()
    toast.success('Key 已创建')
  } catch (e: any) {
    toast.error(e?.message || '创建失败')
  } finally { creating.value = false }
}

async function readKey(k: KeyInfo, copy = false) {
  if (rowBusy.value.has(k.id)) return
  if (!copy && revealedKeys.value[k.id]) { delete revealedKeys.value[k.id]; return }
  rowBusy.value.add(k.id)
  try {
    const res = await api.key(k.id)
    if (disposed) return
    if (!res.ok || !res.key) throw new Error(res.message || '当前 Key 无法恢复，请重新创建')
    if (copy) { await navigator.clipboard.writeText(res.key); toast.success('Key 已复制') }
    else revealedKeys.value[k.id] = res.key
  } catch (e: any) { toast.error(e?.message || '读取失败') }
  finally { rowBusy.value.delete(k.id) }
}

async function toggle(k: KeyInfo) {
  if (rowBusy.value.has(k.id)) return
  rowBusy.value.add(k.id)
  try {
    const res = await api.toggleKey(k.id)
    k.enabled = res.enabled
  } catch (e: any) { toast.error(e?.message || '操作失败') } finally { rowBusy.value.delete(k.id) }
}

async function remove(k: KeyInfo) {
  if (rowBusy.value.has(k.id)) return
  if (!await confirm({ title: `删除 Key「${k.name}」？`, body: '删除后，使用该 Key 的应用将立即失效。', tone: 'danger', okText: '删除' }) || disposed || rowBusy.value.has(k.id)) return
  rowBusy.value.add(k.id)
  try {
    await api.deleteKey(k.id)
    delete revealedKeys.value[k.id]
    if (freshKey.value?.id === k.id) freshKey.value = null
    await load()
    toast.success('已删除')
  } catch (e: any) { toast.error(e?.message || '删除失败') } finally { rowBusy.value.delete(k.id) }
}

function editModels(k: KeyInfo) {
  if (savingModels.value) return
  if (showModelsOf.value === k.id) { showModelsOf.value = null; return }
  modelSearch.value = ''
  editPolicy.value = k.model_policy
  draftModels.value = [...modelListOf(k)]
  showModelsOf.value = k.id
}
function setDraftModel(model: string, checked: boolean) {
  draftModels.value = checked ? [...new Set([...draftModels.value, model])] : draftModels.value.filter(m => m !== model)
}
function choosePolicy(k: KeyInfo, policy: 'follow' | 'fixed') {
  editPolicy.value = policy
  if (policy === 'fixed' && k.model_policy === 'follow' && !draftModels.value.length) draftModels.value = [...props.me.available_models]
}
// Preserve the requested fixed set; the server validates current permissions.
function listedModel(model: string) { const canonical = (m: string) => m.replace(/^workbuddy\//, ''); return props.me.available_models.some(m => canonical(m) === canonical(model)) }
function modelListOf(k: KeyInfo): string[] { return k.allowed_models ?? [] }
async function saveModels(k: KeyInfo, selected: string[], policy: "follow" | "fixed" = "fixed") {
  if (savingModels.value) return
  savingModels.value = true
  try {
    const allowed = policy === 'follow' ? [] : [...new Set(selected)]
    const result = await api.setKeyModels(k.id, allowed, policy)
    if (disposed) return
    k.allowed_models = result.allowed_models
    k.model_policy = result.model_policy
    showModelsOf.value = null
    toast.success('模型范围已更新')
  } catch (e: any) { toast.error(e?.message || '保存失败') }
  finally { savingModels.value = false }
}
</script>

<template>
  <div class="space-y-5">
    <div class="glass rounded-2xl p-6">
      <h1 class="text-base font-semibold">创建 API 密钥</h1>
      <p class="mt-1 text-micro text-faint">
        跟随权限的 Key 自动使用当前授权模型；固定范围只调用所选模型，并始终受当前用户权限约束。贡献账号不是创建 Key 的前提。Base URL：<code class="mono text-brand">{{ baseURL }}</code>
      </p>
      <div class="mt-4 flex flex-wrap gap-2">
        <WInput v-model="newName" placeholder="Key 名称（如：我的笔记应用）" class="max-w-xs" @enter="create" />
        <select v-model="newPolicy" aria-label="Key 模型权限模式" class="rounded-lg border border-line bg-bg p-2 text-small"><option value="follow">跟随当前权限</option><option value="fixed">固定当前模型</option></select>
        <WButton variant="primary" :loading="creating" @click="create">创建</WButton>
      </div>
      <p v-if="!me.available_models.length" class="mt-3 text-small text-muted">当前暂无可用模型，仍可创建跟随权限的 Key，待模型开放后使用。</p>
      <div v-if="freshKey" class="mt-4 rounded-xl border border-brand/40 bg-brand/8 p-4">
        <div class="text-micro text-brand">Key 已创建，也可在下方随时查看或复制：</div>
        <code class="mono mt-1.5 block break-all text-small text-ink">{{ freshKey.key }}</code>
        <button class="mt-2 text-micro text-brand hover:underline" @click="freshKey = null">隐藏</button>
      </div>
    </div>

    <div class="glass rounded-2xl p-6">
      <h2 class="text-base font-semibold">我的密钥</h2>
      <div v-if="loading" class="mt-4 text-small text-faint">加载中…</div>
      <div v-else-if="loadError" class="mt-4 text-small text-fault">{{ loadError }} <button class="text-brand" @click="load">重试</button></div>
      <div v-else-if="!keys.length" class="mt-4 text-small text-faint">还没有 Key，先在上面创建一个。</div>
      <div v-else class="mt-4 space-y-3">
        <div v-for="k in keys" :key="k.id" class="rounded-xl border border-line bg-elevated/40 p-4">
          <div class="flex flex-wrap items-center gap-3">
            <span class="h-2 w-2 rounded-full" :class="k.enabled ? 'bg-live' : 'bg-faint'" />
            <span class="font-medium">{{ k.name }}</span>
            <code class="mono text-micro text-faint">{{ k.key_prefix }}</code>
            <span class="mono ml-auto text-micro text-muted" :title="k.tokens_known === false ? '已记录的 Tokens 合计；部分请求未上报用量' : ''">{{ fmtTokens(k.tokens) }} tokens · {{ k.requests }} 次</span>
          </div>
          <div class="mt-2 flex flex-wrap items-center gap-3 text-micro text-faint">
            <span>创建于 {{ fmtTime(k.created_at) }}</span>
            <span>模型范围：{{ k.model_policy === 'follow' ? '跟随当前用户权限' : modelListOf(k).length ? modelListOf(k).join('、') : '（空 = 全部拒绝）' }}</span>
          </div>
          <code v-if="revealedKeys[k.id]" class="mono mt-3 block break-all rounded-lg bg-bg p-3 text-small">{{ revealedKeys[k.id] }}</code>
          <div class="mt-3 flex flex-wrap gap-2">
            <WButton size="sm" variant="subtle" :disabled="rowBusy.has(k.id)" @click="readKey(k)">{{ revealedKeys[k.id] ? '隐藏 Key' : '查看 Key' }}</WButton>
            <WButton size="sm" variant="subtle" :disabled="rowBusy.has(k.id)" @click="readKey(k, true)">复制 Key</WButton>
            <WButton size="sm" variant="subtle" :disabled="rowBusy.has(k.id) || savingModels" @click="toggle(k)">{{ k.enabled ? '停用' : '启用' }}</WButton>
            <WButton size="sm" variant="subtle" :disabled="rowBusy.has(k.id) || savingModels" @click="editModels(k)">模型范围</WButton>
            <WButton size="sm" variant="danger" :disabled="rowBusy.has(k.id) || savingModels" @click="remove(k)">删除</WButton>
          </div>
          <!-- 模型收窄面板 -->
          <div v-if="showModelsOf === k.id" class="mt-3 border-t border-line pt-3">
            <div class="mb-3 flex flex-wrap gap-2"><WButton size="sm" :variant="editPolicy === 'follow' ? 'primary' : 'subtle'" :disabled="savingModels" @click="choosePolicy(k, 'follow')">跟随当前权限</WButton><WButton size="sm" :variant="editPolicy === 'fixed' ? 'primary' : 'subtle'" :disabled="savingModels" @click="choosePolicy(k, 'fixed')">固定模型范围</WButton></div>
            <template v-if="editPolicy === 'fixed'">
            <WInputSearch v-model="modelSearch" class="mb-3" placeholder="搜索模型 ID" />
            <ModelSelectionActions v-model:selected="draftModels" :results="visibleModels" :selectable="me.available_models" :disabled="savingModels" class="mb-3" />
            <div class="flex flex-wrap gap-2">
              <label v-for="m in visibleModels" :key="m" class="flex cursor-pointer items-center gap-1.5 rounded-md bg-bg/60 px-2 py-1 text-micro">
                <input
                  type="checkbox"
                  :disabled="savingModels"
                  :checked="draftModels.includes(m)"
                  @change="setDraftModel(m, ($event.target as HTMLInputElement).checked)"
                />
                <span class="mono text-muted">{{ m }}{{ listedModel(m) ? '' : '（当前目录未列出）' }}</span>
              </label>
            </div>
            </template>
            <p v-if="editPolicy === 'fixed'" class="mt-2 text-micro text-faint">可添加当前有权限的模型；不会自动移除已保存模型；若某模型失去权限，请取消勾选后保存。全不勾选表示该 Key 拒绝全部调用。</p>
            <p v-else class="mt-2 text-micro text-faint">跟随当前用户权限，模型开放、本人账号和额外授权变化会自动生效。</p>
            <WButton class="mt-3" size="sm" :loading="savingModels" :disabled="rowBusy.has(k.id)" @click="saveModels(k, draftModels, editPolicy)">保存模型范围</WButton>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>
