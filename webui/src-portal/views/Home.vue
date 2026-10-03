<script setup lang="ts">
// 首页：资格状态一目了然 + 改密。资格 = 活跃贡献 + 启用中的分组授权，
// 两者缺一即不可调用（后端每请求实时校验，这里只是展示）。
import { ref } from 'vue'
import { api, type Me } from '../api'
import { toast } from '../../src/lib/toast'
import WButton from '../../src/components/ui/WButton.vue'
import WInput from '../../src/components/ui/WInput.vue'

defineProps<{ me: Me }>()
defineEmits<{ refresh: [] }>()

const oldPass = ref('')
const newPass = ref('')
const busy = ref(false)

async function changePassword() {
  if (busy.value) return
  if (!oldPass.value || !newPass.value) return toast.error('请填写原密码与新密码')
  busy.value = true
  try {
    await api.changePassword(oldPass.value, newPass.value)
    oldPass.value = newPass.value = ''
    toast.success('密码已修改，其它设备已全部下线')
  } catch (e: any) {
    toast.error(e?.message || '修改失败')
  } finally { busy.value = false }
}
</script>

<template>
  <div class="space-y-5">
    <!-- 资格卡片 -->
    <div class="glass rounded-2xl p-6">
      <div class="flex items-start justify-between gap-4">
        <div>
          <h1 class="text-base font-semibold">共享资格</h1>
          <p class="mt-1 text-micro text-faint">资格由「活跃的贡献」与「启用的分组授权」共同决定，撤回贡献或管理员调整都会即时生效。</p>
        </div>
        <span
          class="shrink-0 rounded-full px-3 py-1 text-micro font-medium"
          :class="me.eligible ? 'bg-live/12 text-live' : 'bg-warn/12 text-warn'"
        >{{ me.eligible ? '已具备资格' : '未获得资格' }}</span>
      </div>
      <div class="mt-5 grid grid-cols-2 gap-3 sm:grid-cols-3">
        <div class="rounded-xl border border-line bg-elevated/50 p-4">
          <div class="text-micro text-faint">贡献账号</div>
          <div class="mono mt-1 text-xl font-semibold">{{ me.eligible_accounts }}</div>
        </div>
        <div class="rounded-xl border border-line bg-elevated/50 p-4">
          <div class="text-micro text-faint">启用中的分组</div>
          <div class="mono mt-1 text-xl font-semibold">{{ me.groups_enabled }}</div>
        </div>
        <div class="col-span-2 rounded-xl border border-line bg-elevated/50 p-4 sm:col-span-1">
          <div class="text-micro text-faint">可用模型</div>
          <div class="mono mt-1 text-xl font-semibold">{{ me.available_models.length }}</div>
        </div>
      </div>
      <div v-if="!me.eligible" class="mt-4 rounded-xl border border-warn/30 bg-warn/8 p-4 text-small text-muted">
        还没有可用资格：先到「贡献账号」完成一次 WorkBuddy 扫码共享，等待管理员把你加入共享分组即可。
      </div>
      <div v-else-if="me.available_models.length" class="mt-4 flex flex-wrap gap-1.5">
        <span v-for="m in me.available_models" :key="m" class="mono rounded-md bg-elevated px-2 py-1 text-micro text-muted">{{ m }}</span>
      </div>
    </div>

    <!-- 修改密码 -->
    <div class="glass rounded-2xl p-6">
      <h2 class="text-base font-semibold">修改密码</h2>
      <p class="mt-1 text-micro text-faint">修改后所有已登录设备（含当前浏览器以外的）都会被强制下线。</p>
      <div class="mt-4 grid gap-3 sm:grid-cols-2">
        <div>
          <label class="mb-1.5 block text-small text-muted">原密码</label>
          <WInput v-model="oldPass" type="password" />
        </div>
        <div>
          <label class="mb-1.5 block text-small text-muted">新密码（8–72 字节）</label>
          <WInput v-model="newPass" type="password" @enter="changePassword" />
        </div>
      </div>
      <WButton variant="ghost" class="mt-4" :loading="busy" @click="changePassword">保存新密码</WButton>
    </div>
  </div>
</template>
