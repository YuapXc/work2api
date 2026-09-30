<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { api } from '@/api/client'
import type { AppInfo, ModelInfo } from '@/types'
import { toast } from '@/lib/toast'
import { confirm } from '@/lib/confirm'
import { int, credits, dt } from '@/lib/format'
import WPage from '@/components/ui/WPage.vue'
import WCard from '@/components/ui/WCard.vue'
import WTable, { type Column } from '@/components/ui/WTable.vue'
import WButton from '@/components/ui/WButton.vue'
import WInput from '@/components/ui/WInput.vue'
import WModal from '@/components/ui/WModal.vue'
import WTag from '@/components/ui/WTag.vue'
import WToggle from '@/components/ui/WToggle.vue'
import WEmpty from '@/components/ui/WEmpty.vue'
import WSpinner from '@/components/ui/WSpinner.vue'
import WIcon from '@/components/ui/WIcon.vue'

const apps = ref<AppInfo[]>([])
const loading = ref(true)

const cols: Column[] = [
  { key: 'name', label: '名称' },
  { key: 'key_prefix', label: '密钥', mono: true },
  { key: 'allowed_models', label: '可用模型' },
  { key: 'requests', label: '请求数', align: 'right', mono: true },
  { key: 'tokens', label: 'Tokens', align: 'right', mono: true },
  { key: 'credits', label: '消耗额度', align: 'right', mono: true, hint: '该密钥累计消耗的额度' },
  { key: 'created_at', label: '创建时间', mono: true },
  { key: 'enabled', label: '状态' },
  { key: 'actions', label: '操作', align: 'right' },
]

async function load() {
  loading.value = true
  try {
    apps.value = (await api.apps()).apps ?? []
  } finally {
    loading.value = false
  }
}

// 新建
const createOpen = ref(false)
const form = ref({ name: '', note: '' })
const newKey = ref('')
async function submitCreate() {
  if (!form.value.name.trim()) return toast.error('请填写名称')
  const res = await api.createApp(form.value.name.trim(), form.value.note.trim())
  newKey.value = res.key
  form.value = { name: '', note: '' }
  await load()
}
function closeCreate() {
  createOpen.value = false
  newKey.value = ''
}

// 查看密钥
const keyOpen = ref(false)
const shownKey = ref('')
const keyMsg = ref('')
async function viewKey(app: any) {
  shownKey.value = ''
  keyMsg.value = ''
  keyOpen.value = true
  const res = await api.appKey(app.id)
  if (res.key) shownKey.value = res.key
  else keyMsg.value = res.message || '该密钥创建于旧版本，无法再明文查看，请重建。'
}

async function copy(text: string) {
  try {
    await navigator.clipboard.writeText(text)
    toast.success('已复制到剪贴板')
  } catch {
    toast.error('复制失败，请手动选择')
  }
}

async function toggle(app: any) {
  await api.toggleApp(app.id)
  await load()
}
async function remove(app: any) {
  if (!(await confirm({ title: `删除密钥「${app.name}」？`, body: '删除后使用该密钥的客户端将立即失效，历史用量记录保留。', tone: 'danger', okText: '删除' })))
    return
  await api.deleteApp(app.id)
  toast.success('已删除')
  await load()
}

// ---------- 可用模型白名单 ----------
// 空 = 不限制（所有模型可用）；勾选后仅白名单内模型可被该密钥调用，
// 越权请求在网关入口直接 403。别名条目按其原样保存（网关按别名/实名双重匹配）。
const modelsOpen = ref(false)
const modelsTarget = ref<AppInfo | null>(null)
const selected = ref<Set<string>>(new Set())
const modelSearch = ref('')
const savingModels = ref(false)
const allModels = ref<ModelInfo[]>([])

const filteredModels = computed(() => {
  const q = modelSearch.value.trim().toLowerCase()
  if (!q) return allModels.value
  return allModels.value.filter((m) => m.id.toLowerCase().includes(q) || (m.name || '').toLowerCase().includes(q))
})

async function openModels(app: any) {
  modelsTarget.value = app
  selected.value = new Set(app.allowed_models || [])
  modelSearch.value = ''
  modelsOpen.value = true
  if (!allModels.value.length) {
    try {
      const res = await api.models()
      allModels.value = res.models ?? []
    } catch { /* 目录加载失败时仍可手动编辑已选项 */ }
  }
}

function toggleModel(id: string) {
  const next = new Set(selected.value)
  if (next.has(id)) next.delete(id)
  else next.add(id)
  selected.value = next
}

async function saveModels() {
  const target = modelsTarget.value
  if (!target) return
  savingModels.value = true
  try {
    // 全不选视为「不限制」——与「清空白名单」语义一致，避免把密钥锁死成 0 个模型
    const list = selected.value.size ? [...selected.value].sort() : []
    const res = await api.setAppModels(target.id, list.length ? list : null)
    if (res.warnings?.length) toast.info(res.warnings[0])
    toast.success(list.length ? `已限定 ${list.length} 个模型` : '已恢复不限制')
    modelsOpen.value = false
    await load()
  } finally {
    savingModels.value = false
  }
}

function modelCell(app: any) {
  const list = app.allowed_models || []
  if (!list.length) return '全部模型'
  if (list.length <= 3) return list.join('、')
  return `${list[0]}、${list[1]} 等 ${list.length} 个`
}

onMounted(load)
</script>

<template>
  <WPage title="API 密钥" sub="为每个客户端签发独立密钥，单独统计用量、随时停用。">
    <template #actions>
      <WButton variant="ghost" @click="load"><WIcon name="refresh" :size="15" /> 刷新</WButton>
      <WButton variant="primary" @click="createOpen = true"><WIcon name="plus" :size="16" /> 新建密钥</WButton>
    </template>

    <WCard flush>
      <WSpinner v-if="loading" center label="加载中" />
      <WTable v-else :columns="cols" :rows="apps" row-key="id" min-width="880px">
        <template #cell-name="{ row }">
          <div class="font-medium text-ink">{{ row.name }}</div>
          <div v-if="row.note" class="text-micro text-faint">{{ row.note }}</div>
        </template>
        <template #cell-key_prefix="{ value }">
          <span class="text-muted">{{ value }}…</span>
        </template>
        <template #cell-allowed_models="{ row }">
          <button
            type="button"
            class="text-left text-small transition-colors"
            :class="row.allowed_models?.length ? 'text-ink' : 'text-muted hover:text-brand'"
            @click="openModels(row)"
            title="点击编辑可用模型"
          >
            {{ modelCell(row) }}
          </button>
        </template>
        <template #cell-requests="{ value }">{{ int(value) }}</template>
        <template #cell-tokens="{ value }">{{ int(value) }}</template>
        <template #cell-credits="{ value }">{{ credits(value) }}</template>
        <template #cell-created_at="{ value }">{{ dt(value, 'YYYY-MM-DD') }}</template>
        <template #cell-enabled="{ row }">
          <WTag :tone="row.enabled ? 'live' : 'muted'" dot>{{ row.enabled ? '启用' : '停用' }}</WTag>
        </template>
        <template #cell-actions="{ row }">
          <div class="flex items-center justify-end gap-1.5">
            <WButton size="sm" variant="subtle" @click="openModels(row)">模型</WButton>
            <WButton size="sm" variant="subtle" @click="viewKey(row)">查看</WButton>
            <WToggle :model-value="row.enabled" @update:model-value="toggle(row)" />
            <WButton size="sm" variant="danger" @click="remove(row)">删除</WButton>
          </div>
        </template>
        <template #empty>
          <WEmpty title="还没有 API 密钥" hint="新建一个密钥，把它配置到你的客户端即可开始调用。">
            <WButton variant="primary" @click="createOpen = true"><WIcon name="plus" :size="16" /> 新建密钥</WButton>
          </WEmpty>
        </template>
      </WTable>
    </WCard>

    <!-- 新建 -->
    <WModal v-model:open="createOpen" title="新建 API 密钥" @update:open="(v) => !v && closeCreate()">
      <template v-if="!newKey">
        <label class="mb-1.5 block text-small text-muted">名称</label>
        <WInput v-model="form.name" placeholder="例如：我的 Claude 客户端" class="mb-3" />
        <label class="mb-1.5 block text-small text-muted">备注（可选）</label>
        <WInput v-model="form.note" placeholder="用途说明" />
      </template>
      <template v-else>
        <div class="mb-2 flex items-center gap-2 text-small text-live"><WIcon name="check" :size="16" /> 密钥已生成，请立即复制保存</div>
        <p class="mb-3 text-micro text-faint">出于安全，完整密钥只显示这一次。关闭后将无法再次查看明文。</p>
        <div class="flex items-center gap-2 rounded-lg border border-line bg-bg/50 p-3">
          <code class="mono flex-1 break-all text-small text-brand">{{ newKey }}</code>
          <WButton size="sm" variant="ghost" @click="copy(newKey)"><WIcon name="copy" :size="15" /> 复制</WButton>
        </div>
      </template>
      <template #footer>
        <template v-if="!newKey">
          <WButton variant="subtle" @click="closeCreate">取消</WButton>
          <WButton variant="primary" @click="submitCreate">创建</WButton>
        </template>
        <WButton v-else variant="primary" @click="closeCreate">完成</WButton>
      </template>
    </WModal>

    <!-- 查看密钥 -->
    <WModal v-model:open="keyOpen" size="sm" title="密钥明文">
      <WSpinner v-if="!shownKey && !keyMsg" label="读取中" />
      <div v-else-if="shownKey" class="flex items-center gap-2 rounded-lg border border-line bg-bg/50 p-3">
        <code class="mono flex-1 break-all text-small text-brand">{{ shownKey }}</code>
        <WButton size="sm" variant="ghost" @click="copy(shownKey)"><WIcon name="copy" :size="15" /> 复制</WButton>
      </div>
      <p v-else class="text-small text-warn">{{ keyMsg }}</p>
      <template #footer><WButton variant="primary" @click="keyOpen = false">关闭</WButton></template>
    </WModal>

    <!-- 可用模型白名单编辑 -->
    <WModal v-model:open="modelsOpen" title="可用模型">
      <p class="mb-3 text-small text-muted">
        为「{{ modelsTarget?.name }}」限定可调用的模型；白名单外的请求会被网关拒绝（403）。
        <span class="text-faint">全部不选 = 不限制。</span>
      </p>
      <WInput v-model="modelSearch" placeholder="搜索模型…" class="mb-2.5" />
      <div class="max-h-72 space-y-1 overflow-y-auto rounded-lg border border-line p-2">
        <WSpinner v-if="!allModels.length" center label="加载模型目录" />
        <label
          v-for="m in filteredModels"
          :key="m.id"
          class="flex cursor-pointer items-center gap-2.5 rounded-md px-2 py-1.5 text-small transition-colors hover:bg-elevated"
        >
          <input
            type="checkbox"
            class="accent-brand"
            :checked="selected.has(m.id)"
            @change="toggleModel(m.id)"
          />
          <span class="mono min-w-0 flex-1 truncate text-ink">{{ m.id }}</span>
          <span class="shrink-0 text-micro text-faint">{{ m.name }}</span>
        </label>
        <p v-if="allModels.length && !filteredModels.length" class="px-2 py-3 text-center text-micro text-faint">
          没有匹配「{{ modelSearch }}」的模型
        </p>
      </div>
      <div class="mt-2 flex items-center justify-between text-micro text-faint">
        <span>已选 {{ selected.size }} 个</span>
        <button class="transition-colors hover:text-brand" @click="selected = new Set()">清空</button>
      </div>
      <template #footer>
        <WButton variant="subtle" @click="modelsOpen = false">取消</WButton>
        <WButton variant="primary" :loading="savingModels" @click="saveModels">保存</WButton>
      </template>
    </WModal>
  </WPage>
</template>
