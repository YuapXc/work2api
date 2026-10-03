<script setup lang="ts">
// Key 管理：创建（默认收窄到全部可用模型）、启停、模型收窄、删除。
// 明文 key 只在创建时展示一次（后端不回传明文）。
import { ref, onMounted } from 'vue'
import { api, fmtTokens, fmtTime, type KeyInfo, type Me } from '../api'
import { toast } from '../../src/lib/toast'
import WButton from '../../src/components/ui/WButton.vue'
import WInput from '../../src/components/ui/WInput.vue'

const props = defineProps<{ me: Me }>()
const baseURL = window.location.origin + '/v1'

const keys = ref<KeyInfo[]>([])
const loading = ref(true)
const newName = ref('')
const creating = ref(false)
const freshKey = ref('') // 刚创建的明文 key，只展示一次
const showModelsOf = ref<number | null>(null)
const savingModels = ref(false)
const loadError = ref('')

async function load() {
  loading.value = true
  loadError.value = ''
  try { keys.value = (await api.keys()).keys } catch (e: any) { loadError.value = e?.message || '加载密钥失败' } finally { loading.value = false }
}
onMounted(load)

async function create() {
  if (creating.value) return
  if (!props.me.available_models.length) return toast.error('请先添加账号并确认可用模型，再创建 Key')
  const name = newName.value.trim()
  if (!name) return toast.error('请填写 Key 名称')
  creating.value = true
  try {
    const res = await api.createKey(name) // 默认收窄到当前可用模型
    freshKey.value = res.key
    newName.value = ''
    await load()
    toast.success('Key 已创建')
  } catch (e: any) {
    toast.error(e?.message || '创建失败')
  } finally { creating.value = false }
}

async function toggle(k: KeyInfo) {
  try {
    const res = await api.toggleKey(k.id)
    k.enabled = res.enabled
  } catch (e: any) { toast.error(e?.message || '操作失败') }
}

async function remove(k: KeyInfo) {
  if (!confirm(`确定删除 Key「${k.name}」？删除后使用该 Key 的应用将立即失效。`)) return
  try {
    await api.deleteKey(k.id)
    await load()
    toast.success('已删除')
  } catch (e: any) { toast.error(e?.message || '删除失败') }
}

// 模型收窄：勾选集合来自 me.available_models
function modelListOf(k: KeyInfo): string[] { return k.allowed_models ?? [] }
async function saveModels(k: KeyInfo, selected: string[]) {
  if (savingModels.value) return
  savingModels.value = true
  try {
    const allowed = selected.filter(m => props.me.available_models.includes(m))
    await api.setKeyModels(k.id, allowed)
    k.allowed_models = allowed
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
        新 Key 默认允许当前本人账号与获授权共享池的全部可用模型；之后可调整范围。新增模型不会自动扩大旧 Key 权限。Base URL：<code class="mono text-brand">{{ baseURL }}</code>
      </p>
      <div class="mt-4 flex gap-2">
        <WInput v-model="newName" placeholder="Key 名称（如：我的笔记应用）" class="max-w-xs" @enter="create" />
        <WButton variant="primary" :disabled="!me.available_models.length" :loading="creating" @click="create">创建</WButton>
      </div>
      <p v-if="!me.available_models.length" class="mt-3 text-small text-muted">暂无可用模型。先到「我的账号」添加账号，再到「可用模型」确认范围。</p>
      <div v-if="freshKey" class="mt-4 rounded-xl border border-brand/40 bg-brand/8 p-4">
        <div class="text-micro text-brand">请立即复制，明文只显示这一次：</div>
        <code class="mono mt-1.5 block break-all text-small text-ink">{{ freshKey }}</code>
        <button class="mt-2 text-micro text-brand hover:underline" @click="freshKey = ''">我已保存，关闭</button>
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
            <span class="mono ml-auto text-micro text-muted">{{ fmtTokens(k.tokens_known === false ? null : k.tokens) }} tokens · {{ k.requests }} 次</span>
          </div>
          <div class="mt-2 flex flex-wrap items-center gap-3 text-micro text-faint">
            <span>创建于 {{ fmtTime(k.created_at) }}</span>
            <span>模型范围：{{ modelListOf(k).length ? modelListOf(k).join('、') : '（空 = 全部拒绝）' }}</span>
          </div>
          <div class="mt-3 flex flex-wrap gap-2">
            <WButton size="sm" variant="subtle" @click="toggle(k)">{{ k.enabled ? '停用' : '启用' }}</WButton>
            <WButton size="sm" variant="subtle" @click="showModelsOf = showModelsOf === k.id ? null : k.id">模型范围</WButton>
            <WButton size="sm" variant="danger" @click="remove(k)">删除</WButton>
          </div>
          <!-- 模型收窄面板 -->
          <div v-if="showModelsOf === k.id" class="mt-3 border-t border-line pt-3">
            <div class="flex flex-wrap gap-2">
              <label v-for="m in [...new Set([...me.available_models, ...modelListOf(k)])]" :key="m" class="flex cursor-pointer items-center gap-1.5 rounded-md bg-bg/60 px-2 py-1 text-micro">
                <input
                  type="checkbox"
                  :disabled="savingModels"
                  :checked="modelListOf(k).includes(m)"
                  @change="($event.target as HTMLInputElement).checked
                    ? saveModels(k, [...modelListOf(k), m])
                    : saveModels(k, modelListOf(k).filter((x) => x !== m))"
                />
                <span class="mono text-muted">{{ m }}{{ me.available_models.includes(m) ? '' : '（当前无权限）' }}</span>
              </label>
            </div>
            <p class="mt-2 text-micro text-faint">可添加当前有权限的模型；保存时会移除已经失去权限的旧模型。全不勾选表示该 Key 拒绝全部调用。</p>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>
