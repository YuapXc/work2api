<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { api, fmtTokens, type PortalModel } from '../api'
import WButton from '../../src/components/ui/WButton.vue'
import WInput from '../../src/components/ui/WInput.vue'

const models = ref<PortalModel[]>([])
const query = ref('')
const loading = ref(false)
const error = ref('')
const source = ref('')
const filtered = computed(() => models.value.filter(m => `${m.id} ${m.name || ''}`.toLowerCase().includes(query.value.trim().toLowerCase())))
async function load() {
  if (loading.value) return
  loading.value = true
  error.value = ''
  try { const result = await api.models(); models.value = result.models; source.value = result.source }
  catch (e: any) { error.value = e?.message || '加载模型失败' }
  finally { loading.value = false }
}
onMounted(load)
</script>

<template>
  <div class="space-y-5">
    <div class="glass rounded-2xl p-6">
      <div class="flex items-center justify-between gap-3"><h1 class="text-base font-semibold">可用模型</h1><WButton :loading="loading" @click="load">刷新</WButton></div>
      <p class="mt-2 text-small text-muted">这里展示你的账号及获授权共享池允许的模型。调用时还会检查 Key 的模型范围；账号额度、冷却和上游状态可能影响实际可用性。</p>
      <p class="mt-2 text-micro text-faint">目录来源：{{ source === 'dynamic' ? '上游动态目录（服务器最近一次刷新）' : source ? '服务器模型目录' : '加载中' }}。本页刷新读取服务器现有目录，不额外发起上游请求。</p>
      <div class="mt-4"><WInput v-model="query" placeholder="搜索模型名称或 ID" /></div>
      <p v-if="error" class="mt-4 text-small text-fault">{{ error }}<span v-if="models.length">，当前保留上次结果。</span></p>
      <p v-else-if="!loading && !models.length" class="mt-4 text-small text-muted">暂无可用模型。先到「我的账号」完成扫码；若已有账号，请联系管理员检查账号状态和模型目录。</p>
      <p v-else-if="!loading && !filtered.length" class="mt-4 text-small text-faint">没有匹配的模型。</p>
    </div>
    <div class="grid gap-3 sm:grid-cols-2">
      <article v-for="model in filtered" :key="model.id" class="glass min-w-0 rounded-xl p-5">
        <h2 class="break-words font-semibold">{{ model.name || model.id }}</h2>
        <code class="mt-1 block break-all text-micro text-faint">{{ model.id }}</code>
        <div class="mt-3 flex flex-wrap gap-2 text-micro">
          <span v-if="model.own_account" class="rounded bg-live/12 px-2 py-1 text-live">本人账号</span>
          <span v-if="model.shared_pool" class="rounded bg-brand/12 px-2 py-1 text-brand">共享池授权</span>
          <span v-if="model.vision" class="rounded bg-elevated px-2 py-1 text-muted">图片输入</span>
          <span v-if="model.reasoning?.supportsReasoning" class="rounded bg-elevated px-2 py-1 text-muted">支持思考</span>
        </div>
        <div class="mt-4 flex flex-wrap gap-x-5 gap-y-1 text-micro text-muted">
          <span>上下文 {{ fmtTokens(model.context_length || null) }}</span>
          <span>最大输出 {{ fmtTokens(model.max_output_tokens || null) }}</span>
        </div>
      </article>
    </div>
  </div>
</template>
