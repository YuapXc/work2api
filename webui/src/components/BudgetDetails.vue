<script setup lang="ts">
import { ref } from 'vue'
import WModal from './ui/WModal.vue'
import type { Overview } from '@/types'
import { abbr, credits, dt } from '@/lib/format'

defineProps<{ prediction?: Overview['prediction'] }>()
const open = ref(false)
const cost = (value: number) => new Intl.NumberFormat('zh-CN', { maximumSignificantDigits: 4 }).format(value)
</script>

<template>
  <button type="button" class="whitespace-nowrap text-small text-brand hover:underline" @click="open = true">续航明细</button>
  <WModal v-model:open="open" title="各渠道预计续航" size="xl">
      <p class="mb-4 text-small text-muted">同费率账号合并余额，按近期模型 Tokens 占比和实际消费估算；免费调用不计入付费续航。</p>
      <p v-if="prediction?.error" class="mb-3 text-small text-warn">{{ prediction.error }}</p>
      <div class="grid items-start gap-4 lg:grid-cols-2">
      <article v-for="channel in prediction?.providers || []" :key="channel.provider" class="min-w-0 rounded-xl border border-line bg-surface p-4">
        <div class="flex flex-wrap items-center justify-between gap-2">
          <span class="font-medium">{{ channel.label }}</span>
          <span class="mono text-brand">{{ channel.predicted_tokens == null ? '暂不可估算' : abbr(channel.predicted_tokens, 2) + ' Tokens' }}</span>
        </div>
        <p v-if="channel.partial" class="mt-1 text-small text-warn">部分余额已计入</p>
        <p v-else-if="channel.reason" class="mt-1 text-small text-faint">{{ channel.reason }}</p>
        <details v-if="channel.free_models.length" class="mt-2 text-small"><summary class="cursor-pointer text-live">目录免费模型 · {{ channel.free_models.length }}</summary><p class="mt-1 break-words text-muted">{{ channel.free_models.join('、') }}</p></details>
        <div v-for="(pool, index) in channel.pools" :key="index" class="mt-3 min-w-0 rounded-lg border border-line bg-elevated p-3 text-small">
          <div class="flex flex-wrap justify-between gap-2">
            <span class="break-words text-muted">{{ pool.label }}<span class="ml-2 whitespace-nowrap text-micro text-faint">{{ pool.account_count }} 个账号</span></span>
            <span class="mono whitespace-nowrap">{{ pool.predicted_tokens == null ? '—' : abbr(pool.predicted_tokens, 2) + ' Tokens' }}</span>
          </div>
          <p class="mt-1 text-muted">有效余额 {{ pool.remaining == null ? '未知' : credits(pool.remaining) + ' ' + pool.unit }}</p>
          <p v-if="pool.known_balances < pool.eligible_accounts" class="mt-1 text-micro text-warn">已取得 {{ pool.known_balances }}/{{ pool.eligible_accounts }} 个账号的余额</p>
          <details v-if="pool.models.length" class="mt-2 text-micro text-faint">
            <summary class="cursor-pointer">模型构成与成本</summary>
            <div class="mt-2 max-h-64 overflow-auto rounded-md border border-line">
              <table class="w-full text-left">
                <thead class="bg-surface"><tr><th class="px-2 py-2 font-normal">模型</th><th class="whitespace-nowrap px-2 py-2 text-right font-normal">Tokens 占比</th><th class="whitespace-nowrap px-2 py-2 text-right font-normal">{{ pool.unit }}/百万 Tokens</th></tr></thead>
                <tbody><tr v-for="model in pool.models" :key="model.model" class="border-t border-line"><td class="break-all px-2 py-2 text-muted">{{ model.model }}</td><td class="whitespace-nowrap px-2 py-2 text-right mono">{{ (model.token_share * 100).toFixed(1) }}%</td><td class="whitespace-nowrap px-2 py-2 text-right mono">{{ model.credits_per_million_tokens == null ? '—' : cost(model.credits_per_million_tokens) }}</td></tr></tbody>
              </table>
            </div>
          </details>
          <details class="mt-2 text-micro text-faint">
            <summary class="cursor-pointer">账号余额与计算依据</summary>
            <div v-for="(account, accountIndex) in pool.accounts" :key="accountIndex" class="mt-2">
              <div class="flex min-w-0 items-start justify-between gap-3"><span class="min-w-0 break-words">{{ account.label }}</span><span class="shrink-0">{{ !account.included ? (account.reason || '余额未知') : credits(account.remaining) + ' ' + pool.unit }}</span></div>
              <p v-if="account.included && account.expires_at && account.expiring > 0" class="mt-0.5">{{ dt(account.expires_at) }} 到期 · {{ credits(account.expiring) }} {{ pool.unit }}</p>
            </div>
            <template v-if="pool.status === 'estimated'"><p class="mt-2">近 {{ pool.window_days }} 天消费样本 · {{ pool.samples }} 次付费调用</p><p class="mt-1">每{{ pool.unit }}约 {{ abbr(pool.tokens_per_unit || 0) }} Tokens</p></template>
            <p v-if="pool.excluded_credits > 0" class="mt-1">未计入过期或适用范围未确认额度 {{ credits(pool.excluded_credits) }}</p>
          </details>
          <p v-if="pool.status !== 'estimated'" class="mt-1 text-faint">{{ pool.reason?.replace('（至少5次、1000 Tokens）', '') }}</p>

          <p v-if="pool.stale || pool.deferred" class="mt-1 text-warn">{{ [pool.stale ? '余额待刷新，预测可能滞后' : '', pool.deferred ? '账号暂时冷却，恢复前不可调用' : ''].filter(Boolean).join('；') }}</p>


        </div>
      </article>
      </div>
      <p v-if="!prediction?.providers?.length" class="py-6 text-center text-small text-faint">暂无续航明细，请等待概览数据刷新。</p>
      <details class="mt-3 text-micro text-faint"><summary class="cursor-pointer">估算说明</summary><p class="mt-2">按近 7 天模型占比和实测付费成本估算。样本不足时参考近 30 天；占用量或估算消耗一半以上的模型也需要足够样本。相同计费规则可共享成本样本，余额按适用范围分别统计。</p></details>
  </WModal>
</template>
