<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { api } from '@/api/client'
import type { AppInfo } from '@/types'
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
        <template #cell-requests="{ value }">{{ int(value) }}</template>
        <template #cell-tokens="{ value }">{{ int(value) }}</template>
        <template #cell-credits="{ value }">{{ credits(value) }}</template>
        <template #cell-created_at="{ value }">{{ dt(value, 'YYYY-MM-DD') }}</template>
        <template #cell-enabled="{ row }">
          <WTag :tone="row.enabled ? 'live' : 'muted'" dot>{{ row.enabled ? '启用' : '停用' }}</WTag>
        </template>
        <template #cell-actions="{ row }">
          <div class="flex items-center justify-end gap-1.5">
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
  </WPage>
</template>
