<script setup lang="ts">
import { ref, reactive, onMounted, computed } from 'vue'
import { api } from '@/api/client'
import type { Settings } from '@/types'
import { toast } from '@/lib/toast'
import WPage from '@/components/ui/WPage.vue'
import WCard from '@/components/ui/WCard.vue'
import WButton from '@/components/ui/WButton.vue'
import WInput from '@/components/ui/WInput.vue'
import WTextarea from '@/components/ui/WTextarea.vue'
import WToggle from '@/components/ui/WToggle.vue'
import WSpinner from '@/components/ui/WSpinner.vue'

const loading = ref(true)
const saving = ref(false)
const s = reactive<Record<string, string>>({
  checkin_hours: '',
  credit_refresh_min: '',
  model_refresh_hour: '',
  model_ttl_min: '',
  aa_refresh_hour: '',
  keepalive_hour: '',
  model_aliases: '',
  alert_webhook: '',
  alert_threshold_percent: '',
  alert_expiry_days: '',
  qoder_machine_salt: '',
})
const keepalive = ref(true)
const costAware = ref(true)
const alertOn = ref(false)
const aaKeyInput = ref('')
const aaMasked = ref('')
const aaEnabled = ref(false)
const clearAA = ref(false)

async function load() {
  loading.value = true
  try {
    const data = (await api.getSettings()) as Settings & Record<string, string>
    for (const k of Object.keys(s)) if (data[k] != null) s[k] = String(data[k])
    keepalive.value = String(data.keepalive_enabled ?? '1') === '1'
    costAware.value = String(data.cost_aware_routing ?? '1') === '1'
    alertOn.value = String(data.alert_enabled ?? '0') === '1'
    aaEnabled.value = !!data.aa_enabled
    aaMasked.value = data.aa_api_key_masked || ''
  } finally {
    loading.value = false
  }
}

async function save() {
  saving.value = true
  try {
    const payload: Record<string, unknown> = {
      ...s,
      keepalive_enabled: keepalive.value ? '1' : '0',
      cost_aware_routing: costAware.value ? '1' : '0',
      alert_enabled: alertOn.value ? '1' : '0',
    }
    if (clearAA.value) payload.clear_aa_api_key = true
    else if (aaKeyInput.value.trim()) payload.aa_api_key = aaKeyInput.value.trim()
    await api.saveSettings(payload as Partial<Settings>)
    aaKeyInput.value = ''
    clearAA.value = false
    toast.success('设置已保存')
    await load()
  } finally {
    saving.value = false
  }
}

const aaStatus = computed(() => (clearAA.value ? '将在保存后清除' : aaEnabled.value ? `已配置 ${aaMasked.value}` : '未配置'))

onMounted(load)
</script>

<template>
  <WPage title="设置" sub="自动签到、额度刷新、模型缓存、保活与额度预警的全局配置。">
    <template #actions>
      <WButton variant="primary" :loading="saving" @click="save"><span>保存设置</span></WButton>
    </template>

    <WSpinner v-if="loading" center label="加载中" />
    <div v-else class="grid gap-4 lg:grid-cols-2">
      <!-- 定时任务 -->
      <WCard title="定时任务" sub="时间用 0–23 的整点，多个用英文逗号分隔。">
        <div class="space-y-3.5">
          <div class="grid grid-cols-[1fr_9rem] items-center gap-3">
            <label class="text-small text-muted">签到时间（时）</label>
            <WInput v-model="s.checkin_hours" placeholder="9,21" />
          </div>
          <div class="grid grid-cols-[1fr_9rem] items-center gap-3">
            <label class="text-small text-muted">额度刷新间隔（分）</label>
            <WInput v-model="s.credit_refresh_min" placeholder="30" />
          </div>
          <div class="grid grid-cols-[1fr_9rem] items-center gap-3">
            <label class="text-small text-muted">模型目录刷新（时）</label>
            <WInput v-model="s.model_refresh_hour" placeholder="6" />
          </div>
          <div class="grid grid-cols-[1fr_9rem] items-center gap-3">
            <label class="text-small text-muted">模型缓存 TTL（分）</label>
            <WInput v-model="s.model_ttl_min" placeholder="60" />
          </div>
          <div class="grid grid-cols-[1fr_9rem] items-center gap-3">
            <label class="text-small text-muted">AA 评测刷新（时）</label>
            <WInput v-model="s.aa_refresh_hour" placeholder="7" />
          </div>
        </div>
      </WCard>

      <!-- 保活 -->
      <WCard title="Token 保活" sub="定时轻量请求，避免长期不用的账号令牌过期。">
        <div class="space-y-3.5">
          <div class="flex items-center justify-between">
            <label class="text-small text-muted">启用保活</label>
            <WToggle v-model="keepalive" />
          </div>
          <div class="grid grid-cols-[1fr_9rem] items-center gap-3">
            <label class="text-small text-muted">保活时间（时）</label>
            <WInput v-model="s.keepalive_hour" placeholder="22" :disabled="!keepalive" />
          </div>
        </div>
      </WCard>

      <!-- 成本优先选号 -->
      <WCard title="成本优先选号" sub="多账号命中同一模型时，优先用该模型成本更低的站点账号；限流/出错再轮询其它账号。">
        <div class="space-y-3.5">
          <div class="flex items-center justify-between">
            <label class="text-small text-muted">启用成本优先</label>
            <WToggle v-model="costAware" />
          </div>
          <p class="text-micro text-faint">
            关闭则回到纯加权轮换。成本为最高优先级：只要有更便宜的账号可用就用它（更贵账号里临近到期的额度可能因此用不完）；限流/冷却时才轮到较贵的账号。
          </p>
        </div>
      </WCard>

      <WCard title="模型别名" sub="每行一条 别名=真实模型，让客户端用自定义名称调用。">
        <WTextarea v-model="s.model_aliases" :rows="6" placeholder="claude-latest=claude-sonnet-4-5&#10;gpt=gpt-5" />
      </WCard>

      <!-- AA 评测 -->
      <WCard title="AA 评测密钥" :sub="`当前状态：${aaStatus}`">
        <label class="mb-1.5 block text-small text-muted">API Key</label>
        <WInput v-model="aaKeyInput" type="password" placeholder="填写以更新（留空则保持不变）" :disabled="clearAA" class="mb-2.5" />
        <label class="flex items-center gap-2 text-small text-muted">
          <input type="checkbox" v-model="clearAA" class="accent-brand" /> 清除已保存的密钥
        </label>
      </WCard>

      <!-- 额度预警 -->
      <WCard title="额度预警" sub="低于阈值或临近到期时，向 Webhook 推送提醒。" class="lg:col-span-2">
        <div class="space-y-3.5">
          <div class="flex items-center justify-between">
            <label class="text-small text-muted">启用预警推送</label>
            <WToggle v-model="alertOn" />
          </div>
          <div class="grid grid-cols-[10rem_1fr] items-center gap-3">
            <label class="text-small text-muted">Webhook 地址</label>
            <WInput v-model="s.alert_webhook" placeholder="企微 / 飞书 / Bark，按 URL 自动识别" :disabled="!alertOn" />
          </div>
          <div class="grid gap-3 sm:grid-cols-2">
            <div class="grid grid-cols-[10rem_1fr] items-center gap-3">
              <label class="text-small text-muted">余额阈值（%）</label>
              <WInput v-model="s.alert_threshold_percent" placeholder="20" :disabled="!alertOn" />
            </div>
            <div class="grid grid-cols-[10rem_1fr] items-center gap-3">
              <label class="text-small text-muted">到期提前（天）</label>
              <WInput v-model="s.alert_expiry_days" placeholder="7" :disabled="!alertOn" />
            </div>
          </div>
        </div>
      </WCard>

      <!-- 高级 -->
      <WCard title="高级" sub="Qoder 设备指纹盐值；一般无需改动。" class="lg:col-span-2">
        <div class="grid grid-cols-[10rem_1fr] items-center gap-3">
          <label class="text-small text-muted">Qoder 机器盐值</label>
          <WInput v-model="s.qoder_machine_salt" placeholder="留空使用默认" />
        </div>
      </WCard>
    </div>
  </WPage>
</template>