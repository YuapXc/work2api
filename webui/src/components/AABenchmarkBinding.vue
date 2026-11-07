<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { api } from '@/api/client'
import { toast } from '@/lib/toast'
import type { AABenchmarkBindingOptions, AABenchmarkVariant } from '@/types'
import WButton from './ui/WButton.vue'
import WInput from './ui/WInput.vue'
import WModal from './ui/WModal.vue'
import WSelect from './ui/WSelect.vue'

const props = defineProps<{ model: string }>()
const emit = defineEmits<{ close: []; saved: [] }>()
const data = ref<AABenchmarkBindingOptions>()
const query = ref('')
const familyID = ref('')
const effortIDs = ref<Record<string, string>>({})
const loading = ref(false)
const saving = ref(false)
const families = computed(() => data.value?.families || [])
const family = computed(() => families.value.find(item => item.id === familyID.value))

async function search() {
  if (loading.value) return
  loading.value = true
  try {
    const result = await api.benchmarkBinding(props.model, query.value.trim())
    const first = !data.value
    data.value = result
    if (first) {
      familyID.value = families.value.find(item => item.variants.some(v => v.id === result.binding.entry_id))?.id || ''
      effortIDs.value = { ...result.binding.efforts }
    } else if (!family.value) {
      familyID.value = ''
      effortIDs.value = {}
    }
  } catch {
    // The API client reports the request failure.
  } finally {
    loading.value = false
  }
}

function selectFamily(id: string) {
  familyID.value = id
  effortIDs.value = {}
}

function compatible(v: AABenchmarkVariant, effort: string) {
  if (effort === 'off') return v.effort === 'off'
  return !v.effort || v.effort === effort || v.effort === 'thinking' || v.effort === 'reasoning'
}

async function save(clear = false) {
  if (saving.value || loading.value || (!clear && !family.value)) return
  saving.value = true
  try {
    const efforts = Object.fromEntries(Object.entries(effortIDs.value).filter(([level, id]) =>
      id && data.value?.efforts?.includes(level)))
    await api.saveBenchmarkBinding(props.model, clear ? { entry_id: '' } : { entry_id: familyID.value, efforts })
    toast.success(clear ? '已恢复自动匹配' : '已保存评测绑定')
    emit('saved')
  } catch {
    // The API client reports validation and save failures.
  } finally {
    saving.value = false
  }
}

onMounted(search)
</script>

<template>
  <WModal :open="true" title="绑定 AA 评测" :dismissible="!saving" @update:open="emit('close')">
    <div class="space-y-4">
      <p class="mono break-all text-small text-muted">{{ model }}</p>
      <p v-if="data?.state === 'conflict'" class="text-small text-warn">当前渠道目录报告了不同版本，无法使用同一条评测绑定。</p>
      <p v-else-if="data?.state === 'review'" class="text-small text-warn">目录身份、档位或 AA 条目已变化，请复核后重新保存。</p>
      <p v-if="data && !data.ready" class="text-small text-muted">请先在模型页刷新 AA 评测目录。</p>
      <template v-if="data?.ready">
        <form class="flex items-center gap-2" @submit.prevent="search">
          <WInput v-model="query" maxlength="100" placeholder="搜索 AA 模型名、版本或厂商" aria-label="搜索 AA 评测" class="min-w-0 flex-1" />
          <WButton type="submit" :loading="loading" :disabled="saving">搜索</WButton>
        </form>
        <WSelect
          :model-value="familyID"
          :options="families.map(f => ({ value: f.id, label: f.creator ? `${f.name} · ${f.creator}` : f.name }))"
          placeholder="选择已确认的模型版本"
          :disabled="loading || saving"
          @update:model-value="selectFamily"
        />
        <p v-if="!loading && !families.length" class="text-small text-muted">输入模型名称或版本搜索候选。</p>
        <div v-if="family" class="space-y-2 rounded-lg border border-line p-3">
          <p class="text-small text-muted">评测按渠道档位匹配。名称含糊时可指定同版本的 AA 条目。</p>
          <label v-for="effort in data.efforts || []" :key="effort" class="grid items-center gap-2 sm:grid-cols-[7rem_minmax(0,1fr)]">
            <span class="mono text-small text-ink">{{ effort === 'off' ? '关闭思考' : effort }}</span>
            <WSelect
              v-model="effortIDs[effort]"
              :options="family.variants.filter(v => compatible(v, effort)).map(v => ({
                value: v.id,
                label: `${v.name || v.id} · ${v.intelligence_index == null ? '暂无评测' : v.intelligence_index.toFixed(1)}`,
              }))"
              placeholder="自动按同档位匹配"
              :disabled="saving"
            />
          </label>
        </div>
      </template>
      <p v-else-if="loading" class="text-small text-muted">正在读取评测目录…</p>
    </div>
    <template #footer>
      <WButton v-if="data?.binding.entry_id" variant="ghost" :disabled="loading || saving" @click="save(true)">恢复自动匹配</WButton>
      <WButton variant="primary" :loading="saving" :disabled="!family || loading || data?.state === 'conflict'" @click="save()">保存绑定</WButton>
    </template>
  </WModal>
</template>
