<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { SearchOutlined, ReloadOutlined } from '@ant-design/icons-vue'
import { api } from '@/api/client'
import type { ModelInfo } from '@/types'
import ModelsPanel from '@/components/ModelsPanel.vue'

// embedded：作为供应商钻取页的「模型」标签页嵌入时，隐藏自身的大标题/副标题，
// 搜索、刷新、表格等保持不变。
defineProps<{ embedded?: boolean }>()

const models = ref<ModelInfo[]>([])
const loading = ref(false)
const source = ref<'dynamic' | 'static'>('dynamic')
const search = ref('')
const refreshing = ref(false)

const filtered = computed(() => {
  const q = search.value.trim().toLowerCase()
  if (!q) return models.value
  return models.value.filter(
    (m) =>
      m.id.toLowerCase().includes(q) ||
      (m.name || '').toLowerCase().includes(q),
  )
})

async function load() {
  loading.value = true
  try {
    const res = await api.models()
    models.value = res.models
    source.value = res.source === 'static' ? 'static' : 'dynamic'
  } finally {
    loading.value = false
  }
}

async function refresh() {
  refreshing.value = true
  try {
    await api.modelsRefresh()
    await load()
  } catch { /* 拦截器已提示 */ } finally {
    refreshing.value = false
  }
}

onMounted(load)
</script>

<template>
  <div style="padding-bottom: 36px">
    <div class="view-header">
      <div v-if="!embedded">
        <div class="vh-title">模型</div>
        <div class="vh-meta">
          <span class="led route"></span>
          <span class="mono">{{ filtered.length }}</span> 个模型 ·
          {{ source === 'dynamic' ? '动态目录（来自上游）' : '静态兜底目录' }}
        </div>
      </div>
      <div class="vh-actions">
        <a-input
          v-model:value="search"
          placeholder="搜索模型 ID 或名称"
          allow-clear
          class="search"
        >
          <template #prefix><SearchOutlined style="color: var(--fog)" /></template>
        </a-input>
        <a-button :loading="refreshing" @click="refresh">
          <ReloadOutlined />刷新模型
        </a-button>
      </div>
    </div>

    <ModelsPanel
      :models="filtered"
      :source="source"
      :loading="loading"
      :show-accounts="true"
      @refresh="refresh"
    />
  </div>
</template>

<style scoped>
.search { width: 260px; }
</style>
