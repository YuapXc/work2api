<script setup lang="ts">
// 用量：今日合计（多 Key 合并）+ 各 Key 明细。日限额为 0 表示未启用该维度。
import { ref, onMounted } from 'vue'
import { api, fmtTokens, type KeyInfo, type Me } from '../api'

defineProps<{ me: Me }>()

const loading = ref(true)
const error = ref('')
const today = ref<Awaited<ReturnType<typeof api.usage>>['today']>({ requests: 0, input_tokens: null, output_tokens: null, daily_limit_requests: 0, daily_limit_input: 0, daily_limit_output: 0 })
const keys = ref<KeyInfo[]>([])

async function load() {
  loading.value = true
  error.value = ''
  try {
    const res = await api.usage()
    today.value = res.today
    keys.value = res.keys
  } catch (e: any) { error.value = e?.message || '加载用量失败' } finally { loading.value = false }
}
onMounted(load)

function quota(cur: number | null, limit: number) {
  return limit > 0 ? `${fmtTokens(cur)} / ${fmtTokens(limit)}` : `${fmtTokens(cur)} · 不限额`
}
</script>

<template>
  <div class="space-y-5">
    <p v-if="error" class="text-small text-fault">{{ error }} <button class="text-brand" @click="load">重试</button></p>
    <div class="glass rounded-2xl p-6">
      <h1 class="text-base font-semibold">今日用量</h1>
      <p class="mt-1 text-micro text-faint">同一账号的所有 Key 合并统计，按本地时间每日 0 点重置。</p>
      <div v-if="!loading && !error" class="mt-5 grid grid-cols-1 gap-3 sm:grid-cols-3">
        <div class="rounded-xl border border-line bg-elevated/50 p-4">
          <div class="text-micro text-faint">请求数</div>
          <div class="mono mt-1 text-xl font-semibold">{{ quota(today.requests, today.daily_limit_requests) }}</div>
        </div>
        <div class="rounded-xl border border-line bg-elevated/50 p-4">
          <div class="text-micro text-faint">输入 tokens</div>
          <div class="mono mt-1 text-xl font-semibold">{{ quota(today.input_tokens, today.daily_limit_input) }}</div>
        </div>
        <div class="rounded-xl border border-line bg-elevated/50 p-4">
          <div class="text-micro text-faint">输出 tokens</div>
          <div class="mono mt-1 text-xl font-semibold">{{ quota(today.output_tokens, today.daily_limit_output) }}</div>
        </div>
      </div>
    </div>

    <div class="glass rounded-2xl p-6">
      <h2 class="text-base font-semibold">按 Key 明细（累计）</h2>
      <div v-if="loading" class="mt-4 text-small text-faint">加载中…</div>
      <div v-else-if="error" class="mt-4 text-small text-faint">用量暂不可用。</div>
      <div v-else-if="!keys.length" class="mt-4 text-small text-faint">还没有 Key。</div>
      <div v-else class="mt-4 overflow-x-auto">
        <table class="w-full text-small">
          <thead>
            <tr class="border-b border-line text-left text-micro text-faint">
              <th class="py-2 pr-4 font-medium">名称</th>
              <th class="py-2 pr-4 font-medium">状态</th>
              <th class="py-2 pr-4 text-right font-medium">请求数</th>
              <th class="py-2 pr-4 text-right font-medium">Tokens</th>
              <th class="py-2 text-right font-medium">Credits</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="k in keys" :key="k.id" class="border-b border-line/50 last:border-0">
              <td class="py-2.5 pr-4">{{ k.name }}</td>
              <td class="py-2.5 pr-4">
                <span class="inline-flex items-center gap-1.5 text-micro" :class="k.enabled ? 'text-live' : 'text-faint'">
                  <span class="h-1.5 w-1.5 rounded-full" :class="k.enabled ? 'bg-live' : 'bg-faint'" />{{ k.enabled ? '启用' : '停用' }}
                </span>
              </td>
              <td class="mono py-2.5 pr-4 text-right">{{ k.requests }}</td>
              <td class="mono py-2.5 pr-4 text-right">{{ fmtTokens(k.tokens_known === false ? null : k.tokens) }}</td>
              <td class="mono py-2.5 text-right">{{ k.credits_known === false || k.credits == null ? '未知' : k.credits.toFixed(2) }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>
  </div>
</template>
