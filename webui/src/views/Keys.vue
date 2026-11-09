<script setup lang="ts">
import { useLiveRead } from '@/lib/liveRead'
import { ref, computed } from 'vue'
import { api } from '@/api/client'
import type { AppInfo, ModelInfo } from '@/types'
import { toast } from '@/lib/toast'
import { confirm } from '@/lib/confirm'
import { int, dt } from '@/lib/format'
import ChannelCosts from '@/components/ChannelCosts.vue'
import WPage from '@/components/ui/WPage.vue'
import WCard from '@/components/ui/WCard.vue'
import WTable, { type Column } from '@/components/ui/WTable.vue'
import WButton from '@/components/ui/WButton.vue'
import WInput from '@/components/ui/WInput.vue'
import ModelSelectionActions from '@/components/ModelSelectionActions.vue'
import WModal from '@/components/ui/WModal.vue'
import WTag from '@/components/ui/WTag.vue'
import WToggle from '@/components/ui/WToggle.vue'
import WEmpty from '@/components/ui/WEmpty.vue'
import WSpinner from '@/components/ui/WSpinner.vue'
import WIcon from '@/components/ui/WIcon.vue'

const apps = ref<AppInfo[]>([])


const cols: Column[] = [
  { key: 'name', label: '名称' },
  { key: 'username', label: '归属', nowrap: true },
  { key: 'key_prefix', label: '密钥', mono: true, nowrap: true },
  { key: 'allowed_models', label: '可用模型' },
  { key: 'requests', nowrap: true, label: '请求数', align: 'right', mono: true },
  { key: 'tokens', nowrap: true, label: 'Tokens', align: 'right', mono: true },
  { key: 'channel_costs', label: '累计消耗', align: 'right', mono: true },
  { key: 'enabled', label: '状态', nowrap: true },
  { key: 'actions', label: '操作', align: 'right', minWidth: '14rem', nowrap: true },
]

const keyRead = useLiveRead(signal => api.apps(signal), result => { apps.value = result.apps ?? [] })
const loading = keyRead.loading
const loadError = keyRead.error
const load = () => keyRead.refresh()

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
const keyTarget = ref<AppInfo | null>(null)
let keyLoadVersion = 0
async function viewKey(app: Record<string, any>) {
  const version = ++keyLoadVersion
  keyTarget.value = apps.value.find(item => item.id === app.id) || null
  shownKey.value = ''
  keyMsg.value = ''
  keyOpen.value = true
  try {
    const res = await api.appKey(app.id)
    if (version !== keyLoadVersion || !keyOpen.value) return
    if (res.key) shownKey.value = res.key
    else keyMsg.value = res.message || '此密钥无法查看，请新建密钥并更新客户端配置。'
  } catch (e: any) {
    if (version === keyLoadVersion && keyOpen.value) keyMsg.value = e?.message || '密钥读取失败，请关闭后重试。'
  }
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
// 私人 Key 空范围 = 不限制；门户用户 Key 空范围 = 拒绝全部。
// 勾选后仅白名单内模型可被该密钥调用，
// 越权请求在网关入口直接 403。别名条目按其原样保存（网关按别名/实名双重匹配）。
const modelsOpen = ref(false)
const modelsTarget = ref<AppInfo | null>(null)
const modelPolicy = ref<"follow" | "fixed">("fixed")
const selected = ref<Set<string>>(new Set())
const modelSearch = ref('')
const savingModels = ref(false)
const allModels = ref<ModelInfo[]>([])
const loadingModels = ref(false)
const unrestricted = ref(true)
const catalogModels = computed(() => {
  const byId = new Map(allModels.value.map((m) => [m.id, m]))
  for (const id of selected.value) if (!byId.has(id)) byId.set(id, { id, name: '已保存，当前目录未发现', provider: id.startsWith('qoder/') ? 'qoder' : id.startsWith('opencode/') ? 'opencode' : id.startsWith('trae/') ? 'trae' : 'workbuddy' })
  return [...byId.values()].sort((a, b) => (a.provider || 'workbuddy').localeCompare(b.provider || 'workbuddy') || a.id.localeCompare(b.id))
})

const filteredModels = computed(() => {
  const q = modelSearch.value.trim().toLowerCase()
  if (!q) return catalogModels.value
  return catalogModels.value.filter((m) => m.id.toLowerCase().includes(q) || (m.name || '').toLowerCase().includes(q))
})

async function openModels(app: any) {
  modelsTarget.value = app
  modelPolicy.value = app.model_policy || "fixed"
  selected.value = new Set(app.allowed_models || [])
  unrestricted.value = !app.user_id && selected.value.size === 0
  modelSearch.value = ''
  modelsOpen.value = true
  loadingModels.value = true
  try {
    const res = await api.modelCatalog()
    allModels.value = res.models ?? []
  } catch { /* 保留缓存和已保存选择 */ }
  finally { loadingModels.value = false }
}

function toggleModel(id: string) {
  unrestricted.value = false
  const next = new Set(selected.value)
  if (next.has(id)) next.delete(id)
  else next.add(id)
  selected.value = next
}

async function saveModels() {
  const target = modelsTarget.value
  if (!target) return
  if (!target.user_id && !unrestricted.value && !selected.value.size) return toast.error('请至少选择一个模型，或明确开启「不限制模型」')
  savingModels.value = true
  try {
    // 仅明确开启「不限制」时发送 null，空选择不能隐式解除限制。
    const list = unrestricted.value ? [] : [...selected.value].sort()
    const res = await api.setAppModels(target.id, target.user_id ? list : list.length ? list : null, target.user_id ? modelPolicy.value : "fixed")
    if (res.warnings?.length) toast.info(res.warnings[0])
    toast.success(target.user_id && modelPolicy.value === 'follow' ? '已跟随当前用户权限' : list.length ? `已限定 ${list.length} 个模型` : target.user_id ? '该用户 Key 已拒绝全部模型' : '已恢复不限制')
    modelsOpen.value = false
    await load()
  } finally {
    savingModels.value = false
  }
}

function modelCell(app: any) {
  const list = app.allowed_models || []
  if (app.user_id && app.model_policy === "follow") return "跟随当前用户权限"
  if (!list.length) return app.user_id > 0 ? '拒绝全部模型' : '全部模型'
  if (list.length <= 3) return list.join('、')
  return `${list[0]}、${list[1]} 等 ${list.length} 个`
}


</script>

<template>
  <WPage title="API 密钥" sub="为每个客户端签发独立密钥，单独统计用量、随时停用。">
    <template #actions>
      <WButton variant="ghost" @click="load"><WIcon name="refresh" :size="15" /> 刷新</WButton>
      <WButton variant="primary" @click="createOpen = true"><WIcon name="plus" :size="16" /> 新建密钥</WButton>
    </template>

    <WCard flush>
      <p v-if="loadError" role="status" class="mb-3 text-small text-warn">密钥读取失败，稍后自动重试；已有数据继续保留。</p>
      <WSpinner v-if="loading" center label="加载中" />
      <WTable v-else :columns="cols" :rows="apps" row-key="id" min-width="880px">
        <template #cell-name="{ row }">
          <div class="w-28 truncate font-medium text-ink" :title="row.name">{{ row.name }}</div>
          <div v-if="row.note" class="w-28 truncate text-micro text-faint" :title="row.note">{{ row.note }}</div>
        </template>
        <template #cell-key_prefix="{ value }">
          <span class="text-muted">{{ value }}…</span>
        </template>
        <template #cell-username="{ row }"><span class="text-small">{{ row.username || '管理员' }}</span></template>
        <template #cell-allowed_models="{ row }">
          <button
            type="button"
            class="block w-40 truncate text-left text-small transition-colors"
            :class="row.allowed_models?.length ? 'text-ink' : 'text-muted hover:text-brand'"
            @click="openModels(row)"
            :title="modelCell(row) + '（点击编辑）'"
          >
            {{ modelCell(row) }}
          </button>
        </template>
        <template #cell-requests="{ value }">{{ int(value) }}</template>
        <template #cell-tokens="{ row }"><span :title="row.tokens_known === false ? '已记录的 Tokens 合计；部分请求未上报用量' : ''">{{ int(row.tokens) }}</span></template>
        <template #cell-channel_costs="{ row }"><ChannelCosts :total="row.credits" :costs="row.channel_costs" /></template>
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
        <p class="mb-3 text-micro text-faint">请复制并妥善保存。授权管理员可以再次查看完整密钥，请勿分享管理权限。</p>
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
    <WModal v-model:open="keyOpen" size="sm" title="密钥详情">
      <p v-if="keyTarget" class="mb-3 text-micro text-muted">{{ keyTarget.name }} · 创建于 {{ dt(keyTarget.created_at, 'YYYY-MM-DD') }}<span class="ml-2" :title="'密钥记录 ID：' + keyTarget.id">#{{ keyTarget.id }}</span></p>
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
        <span class="text-faint">{{ modelsTarget?.user_id ? '用户 Key 空选择拒绝全部模型，并始终受当前用户权限限制。' : '仅开启「不限制模型」才允许全部模型。' }}</span>
      </p>
      <label v-if="modelsTarget?.user_id" class="mb-3 flex items-center gap-2 text-small">权限模式<select v-model="modelPolicy" class="rounded-lg border border-line bg-bg p-2"><option value="follow">跟随当前用户权限</option><option value="fixed">固定模型范围</option></select></label>
      <label v-if="!modelsTarget?.user_id" class="mb-3 flex items-center gap-2 text-small"><WToggle v-model="unrestricted" /> 不限制模型</label>
      <p v-if="modelsTarget?.user_id && modelPolicy === 'follow'" class="mb-3 text-small text-muted">调用范围随默认开放规则、本人账号和额外授权更新；管理员禁用即时生效。</p>
      <template v-else>
      <WInput v-model="modelSearch" placeholder="搜索模型…" class="mb-2.5" />
      <div class="max-h-72 space-y-1 overflow-y-auto rounded-lg border border-line p-2">
        <WSpinner v-if="loadingModels" center label="加载模型目录" />
        <p v-if="!loadingModels && !catalogModels.length" class="px-2 py-3 text-center text-micro text-faint">当前没有可用模型目录，请先配置供应商或刷新模型。</p>
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
          <span class="shrink-0 text-micro text-faint">{{ m.provider || 'workbuddy' }} · {{ m.name }}</span>
        </label>
        <p v-if="allModels.length && !filteredModels.length" class="px-2 py-3 text-center text-micro text-faint">
          没有匹配「{{ modelSearch }}」的模型
        </p>
      </div>
<ModelSelectionActions class="mt-3" :selected="[...selected]" :results="filteredModels.map(m => m.id)" :selectable="allModels.map(m => m.id)" :disabled="loadingModels || savingModels" @update:selected="selected = new Set($event); unrestricted = false" />
      </template>
      <template #footer>
        <WButton variant="subtle" @click="modelsOpen = false">取消</WButton>
        <WButton variant="primary" :loading="savingModels" @click="saveModels">保存</WButton>
      </template>
    </WModal>
  </WPage>
</template>
