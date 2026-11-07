<script setup lang="ts">
// 用量：今日合计（多 Key 合并）+ 各 Key 明细。日限额为 0 表示未启用该维度。
import { ref, onMounted, onUnmounted } from 'vue'
import { api, fmtTokens, type KeyInfo, type Me } from '../api'

import type { UsageRecord } from '../../src/types'
import RequestLogTable from '../../src/components/RequestLogTable.vue'
import RequestLogDetail from '../../src/components/RequestLogDetail.vue'
import WModal from '../../src/components/ui/WModal.vue'
import WButton from '../../src/components/ui/WButton.vue'
import ChannelCosts from '../../src/components/ChannelCosts.vue'
import WInput from '../../src/components/ui/WInput.vue'
defineProps<{ me: Me }>()
const records=ref<UsageRecord[]>([])
const page=ref(1), total=ref(0), logsLoading=ref(false), logsError=ref('')
const model=ref(''), protocol=ref(''), status=ref('')
let appliedFilters = { model: '', protocol: '', status: '' }
const detail=ref<UsageRecord|null>(null), detailOpen=ref(false), detailLoading=ref(false), detailError=ref('')
let version=0, detailVersion=0, disposed=false
onUnmounted(()=>{disposed=true;version++;detailVersion++})
async function loadLogs(reset=false) {
 if(reset) {
  page.value=1
  appliedFilters = {model:model.value.trim(),protocol:protocol.value,status:status.value}
 }
 const current=++version;logsLoading.value=true;logsError.value=''
 try { const result=await api.usageRecords(page.value,appliedFilters);if(disposed || current!==version)return;records.value=result.records;total.value=result.total }
 catch(e:any){if(!disposed && current===version)logsError.value=e?.message || '加载调用记录失败'}
 finally{if(!disposed && current===version)logsLoading.value=false}
}
async function openDetail(id:number) {
 const current=++detailVersion;detailOpen.value=true;detailLoading.value=true;detailError.value='';detail.value=null
 try{const result=await api.usageRecord(id);if(!disposed && current===detailVersion)detail.value=result.record}
 catch(e:any){if(!disposed && current===detailVersion)detailError.value=e?.message || '加载详情失败'}
 finally{if(!disposed && current===detailVersion)detailLoading.value=false}
}
function move(next:number){page.value=next;loadLogs()}


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
onMounted(()=>{load();loadLogs()})

</script>

<template>
  <div class="space-y-5">
    <p v-if="error" class="text-small text-fault">{{ error }} <button class="text-brand" @click="load">重试</button></p>
    <div class="glass rounded-2xl p-6">
      <h1 class="text-base font-semibold">今日用量</h1>
      <p class="mt-1 text-micro text-faint">汇总本人所有 Key 的今日调用。</p>
      <div v-if="!loading && !error" class="mt-5 grid grid-cols-1 gap-3 sm:grid-cols-3">
        <div class="rounded-xl border border-line bg-elevated/50 p-4">
          <div class="text-micro text-faint">请求数</div>
          <div class="mono mt-1 text-xl font-semibold">{{ fmtTokens(today.requests) }}</div>
        </div>
        <div class="rounded-xl border border-line bg-elevated/50 p-4">
          <div class="text-micro text-faint">输入 tokens</div>
          <div class="mono mt-1 text-xl font-semibold" :title="today.tokens_known === false ? '已记录的输入 Tokens 合计；部分请求未上报用量' : ''">{{ fmtTokens(today.recorded_input_tokens ?? today.input_tokens) }}</div>
        </div>
        <div class="rounded-xl border border-line bg-elevated/50 p-4">
          <div class="text-micro text-faint">输出 tokens</div>
          <div class="mono mt-1 text-xl font-semibold" :title="today.tokens_known === false ? '已记录的输出 Tokens 合计；部分请求未上报用量' : ''">{{ fmtTokens(today.recorded_output_tokens ?? today.output_tokens) }}</div>
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
              <th class="py-2 text-right font-medium">累计消耗</th>
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
              <td class="mono py-2.5 pr-4 text-right" :title="k.tokens_known === false ? '已记录的 Tokens 合计；部分请求未上报用量' : ''">{{ fmtTokens(k.tokens) }}</td>
              <td class="mono py-2.5 text-right"><ChannelCosts :total="k.credits" :costs="k.channel_costs" /></td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>
    <div class="glass rounded-2xl p-5">
      <h2 class="font-semibold">我的调用记录</h2>
      <p class="mt-1 text-micro text-faint">查看本人 Key 的调用记录。</p>
      <div class="my-4 flex flex-wrap gap-2">
       <WInput v-model="model" placeholder="模型 ID（精确匹配）" class="max-w-xs" @enter="loadLogs(true)" />
       <select v-model="protocol" class="rounded-lg border border-line bg-bg p-2 text-small" aria-label="筛选协议"><option value="">全部协议</option><option value="chat">Chat</option><option value="anthropic">Messages</option><option value="responses">Responses</option></select>
       <select v-model="status" class="rounded-lg border border-line bg-bg p-2 text-small" aria-label="筛选结果"><option value="">全部结果</option><option value="ok">成功</option><option value="error">失败</option><option value="incomplete">输出未完成</option></select>
       <WButton :loading="logsLoading" @click="loadLogs(true)">查询 / 刷新</WButton>
      </div>
      <p v-if="logsError" class="mb-3 text-small text-fault">{{ logsError }}</p>
      <RequestLogTable :rows="records" @detail="openDetail" />
      <div class="mt-4 flex flex-wrap items-center justify-between gap-2 text-small text-muted"><span>共 {{ total }} 次请求</span><div class="flex items-center gap-2"><WButton size="sm" :disabled="logsLoading || page<=1" @click="move(page-1)">上一页</WButton><span>{{ page }} / {{ Math.max(1,Math.ceil(total/20)) }}</span><WButton size="sm" :disabled="logsLoading || page*20>=total" @click="move(page+1)">下一页</WButton></div></div>
    </div>
    <WModal v-model:open="detailOpen" size="xl" title="调用详情"><p v-if="detailLoading" class="text-small text-faint">加载中…</p><p v-else-if="detailError" class="text-small text-fault">{{ detailError }}</p><RequestLogDetail v-else-if="detail" :record="detail" /></WModal>
  </div>
</template>
