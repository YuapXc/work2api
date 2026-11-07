<script setup lang="ts">
import { ref, computed, onMounted, onBeforeUnmount } from 'vue'
import { api } from '@/api/client'
import type { ModelRefreshResult, ProviderSummary } from '@/types'
import { toast } from '@/lib/toast'
import { providerMeta } from '@/lib/providers'
import WPage from '@/components/ui/WPage.vue'
import WButton from '@/components/ui/WButton.vue'
import WInput from '@/components/ui/WInput.vue'
import WSelect from '@/components/ui/WSelect.vue'
import WSpinner from '@/components/ui/WSpinner.vue'
import WEmpty from '@/components/ui/WEmpty.vue'
import WIcon from '@/components/ui/WIcon.vue'
import WStat from '@/components/ui/WStat.vue'
import ModelCard from '@/components/ModelCard.vue'
import AABenchmarkBinding from '@/components/AABenchmarkBinding.vue'

interface Entry {
  provider: string
  model: Record<string, any>
}
const entries = ref<Entry[]>([])
const source = ref<string>('')
const loading = ref(true)
const refreshing = ref(false)

const search = ref('')
const providerFilter = ref('')
const modalityFilter = ref('')
const aaConfigured = ref(false)
const refreshingAA = ref(false)
const bindingModel = ref('')

const readErrors = ref<Record<string, string>>({})
const knownProviders = ref<ProviderSummary[]>([])
const refreshResults = ref<ModelRefreshResult[]>([])
let loadVersion = 0
let benchLoadVersion = 0

async function load() {
  const version = ++loadVersion
  loading.value = true
  const next = new Map<string, Entry[]>()
  for (const e of entries.value) {
    const list = next.get(e.provider) || []
    list.push(e)
    next.set(e.provider, list)
  }
  const errors: Record<string, string> = {}
  try {
    const [wb, providers] = await Promise.allSettled([api.models(), api.getProviders()])
    if (version !== loadVersion) return
    if (wb.status === 'fulfilled') {
      source.value = wb.value.source || ''
      next.set('workbuddy', (wb.value.models || []).map((model) => ({ provider: 'workbuddy', model: { ...model, id: String(model.id).startsWith('workbuddy/') ? model.id : `workbuddy/${model.id}` } })))
    } else errors.workbuddy = '读取失败，保留上次成功目录'
    if (providers.status === 'fulfilled') {
      knownProviders.value = providers.value.providers || []
      const others = knownProviders.value.filter((p) => !p.default && p.capabilities?.includes('models'))
      const names = new Set(['workbuddy', ...others.map((p) => p.name)])
      for (const name of next.keys()) if (!names.has(name)) next.delete(name)
      const details = await Promise.allSettled(others.map((p) => api.getProvider(p.name)))
      if (version !== loadVersion) return
      details.forEach((d, i) => {
        const name = others[i].name
        if (d.status === 'fulfilled') {
          // A successful empty directory is authoritative: old cards must disappear.
          next.set(name, ((d.value.models as Record<string, any>[]) || []).map((model) => ({ provider: name, model })))
        } else errors[name] = '读取失败，保留上次成功目录'
      })
    } else errors.providers = '渠道列表读取失败，保留上次成功目录'
    entries.value = [...next.values()].flat()
    readErrors.value = errors
    await loadBenchmarks()
  } finally {
    if (version === loadVersion) loading.value = false
  }
}

// AA 评测按带命名空间的模型 id 合并进各卡片（后端 key 即 e.model.id）。
async function loadBenchmarks() {
  const version = ++benchLoadVersion
  try {
    const res = await api.benchmarks()
    if (version !== benchLoadVersion) return false
    aaConfigured.value = !!res.configured
    const map = res.models || {}
    for (const e of entries.value) {
      e.model.benchmark = map[e.model.id] || (e.provider === 'workbuddy' ? map[String(e.model.id).replace(/^workbuddy\//, '')] : undefined)
      e.model.benchmark_state = res.binding_states?.[e.model.id]
    }
    return res.configured
  } catch {
    return false
  }
}

async function refreshAA() {
  refreshingAA.value = true
  try {
    await api.benchmarksRefresh()
    if (await loadBenchmarks()) toast.success('已刷新 AA 评测')
  } catch {
    // The API client shows the server's refresh error.
  } finally {
    refreshingAA.value = false
  }
}

function bindingSaved() {
  bindingModel.value = ''
  void loadBenchmarks()
}

async function refresh() {
  refreshing.value = true
  try {
    const result = await api.modelsRefresh(providerFilter.value || undefined)
    refreshResults.value = result.providers || []
    if (result.warnings?.length) {
      if (result.ok) toast.info('部分渠道刷新失败，保留可用目录')
      else toast.error('目录刷新失败，保留可用目录')
    }
    else if (result.ok) toast.success('已刷新模型目录')
    else toast.info('所选渠道尚未配置可用账号或不支持刷新')
    await load()
  } finally {
    refreshing.value = false
  }
}

const providerOptions = computed(() => {
  const set = [...new Set([...knownProviders.value.filter((p) => p.capabilities?.includes('models')).map((p) => p.name), ...entries.value.map((e) => e.provider)])]
  return set.map((p) => ({ value: p, label: providerMeta(p).label }))
})

const filtered = computed(() =>
  entries.value.filter((e) => {
    if (providerFilter.value && e.provider !== providerFilter.value) return false
    const media = ['image', 'video', 'audio'].includes(e.model.model_type)
    if (['image', 'video', 'audio'].includes(modalityFilter.value) && e.model.model_type !== modalityFilter.value) return false
    const isMulti = !media && e.model.modality === 'multimodal' || e.model.vision || e.model.supports_image
    if (modalityFilter.value === 'multimodal' && !isMulti) return false
    if (modalityFilter.value === 'text' && (media || isMulti || e.model.vision_verified === false)) return false
    if (modalityFilter.value === 'unknown' && e.model.vision_verified !== false) return false
    const q = search.value.trim().toLowerCase()
    if (q) {
      const hay = `${e.model.id} ${e.model.name || ''}`.toLowerCase()
      if (!hay.includes(q)) return false
    }
    return true
  }),
)

const stats = computed(() => {
  const total = entries.value.length
  const multi = entries.value.filter((e) => e.model.modality === 'multimodal' || e.model.vision).length
  const tool = entries.value.filter((e) => e.model.supportsToolCall === true || e.model.tool_call === true).length
  const aa = entries.value.filter(({ model: { benchmark } }) => {
    const scored = (b: any) => b && [b.intelligence_index, b.coding_index, b.math_index].some(value => typeof value === 'number')
    return scored(benchmark) || benchmark?.variants?.some(scored)
  }).length
  return { total, multi, tool, aa }
})

onMounted(load)
onBeforeUnmount(() => { loadVersion++; benchLoadVersion++ })
</script>

<template>
  <WPage title="模型" :sub="`统一模型目录 · 来源 ${source === 'dynamic' ? '上游实时' : source === 'static' ? '内置' : '—'}`">
    <template #actions>
      <RouterLink to="/models/routes"><WButton>统一模型路由</WButton></RouterLink>
      <WButton v-if="aaConfigured" variant="ghost" :loading="refreshingAA" @click="refreshAA"><WIcon name="refresh" :size="15" /> 刷新评测</WButton>
      <WButton variant="ghost" :loading="refreshing" @click="refresh"><WIcon name="refresh" :size="15" /> {{ providerFilter ? '刷新该渠道' : '刷新目录' }}</WButton>
    </template>

    <div v-if="Object.keys(readErrors).length" class="mb-4 rounded-lg border border-line bg-elevated/40 px-3 py-2 text-small text-muted" role="status">
      <div v-for="(message, name) in readErrors" :key="name">{{ name === 'providers' ? '渠道列表' : providerMeta(String(name)).label }}：{{ message }}</div>
    </div>
    <div v-if="refreshResults.length" class="mb-4 flex flex-wrap gap-x-5 gap-y-2 rounded-lg border border-line bg-elevated/40 px-3 py-2 text-small" role="status">
      <div v-for="result in refreshResults" :key="result.provider">
        <span class="text-muted">{{ providerMeta(result.provider).label }}：</span>
        <span v-if="result.status === 'ok'">{{ result.count }} 个模型 · 新增 {{ result.added }} / 移除 {{ result.removed }}</span>
        <span v-else class="text-muted">{{ result.message }}<template v-if="result.status !== 'skipped'"> · 当前 {{ result.count }} 个模型</template></span>
      </div>
    </div>

    <WSpinner v-if="loading && !entries.length" center label="加载中" />
    <template v-else>
      <div class="mb-4 grid gap-3" :class="aaConfigured ? 'grid-cols-2 sm:grid-cols-4' : 'grid-cols-2 sm:grid-cols-3'">
        <WStat label="模型总数" :value="stats.total" tone="brand" />
        <WStat label="多模态" :value="stats.multi" tone="route" />
        <WStat label="支持工具" :value="stats.tool" tone="live" />
        <WStat v-if="aaConfigured" label="已评测" :value="stats.aa" tone="warn" />
      </div>

      <div v-if="!aaConfigured" class="mb-4 flex items-center gap-2 rounded-lg border border-line bg-elevated/40 px-3 py-2 text-small text-muted">
        <WIcon name="chart" :size="15" class="text-faint" />
          在「设置」填入 Artificial Analysis API Key 后，这里会展示已匹配模型的第三方评测。
      </div>

      <div class="mb-4 flex flex-wrap items-center gap-2">
        <WInput v-model="search" placeholder="搜索模型 id 或名称" class="w-full sm:w-72">
          <template #prefix><WIcon name="search" :size="16" class="text-faint" /></template>
        </WInput>
        <WSelect v-model="providerFilter" :options="providerOptions" placeholder="全部供应商" class="w-40" />
        <WSelect
          v-model="modalityFilter"
          :options="[{ value: 'text', label: '纯文本' }, { value: 'multimodal', label: '多模态' }, { value: 'unknown', label: '能力待确认' }, { value: 'image', label: '图片生成' }, { value: 'video', label: '视频生成' }, { value: 'audio', label: '音频转写' }]"
          placeholder="全部模态"
          class="w-32"
        />
        <span class="ml-auto text-small text-faint">{{ filtered.length }} / {{ entries.length }}</span>
      </div>

      <div v-if="filtered.length" class="model-grid model-grid-admin">
        <ModelCard
          v-for="e in filtered" :key="e.provider + '/' + e.model.id"
          :model="e.model" :provider="e.provider" :benchmark-binding="aaConfigured"
          @bind-benchmark="bindingModel = e.model.id"
        />
      </div>
      <WEmpty v-else title="没有匹配的模型" hint="调整搜索或筛选条件，或刷新目录。" />
    </template>
    <AABenchmarkBinding v-if="bindingModel" :model="bindingModel" @close="bindingModel = ''" @saved="bindingSaved" />
  </WPage>
</template>
