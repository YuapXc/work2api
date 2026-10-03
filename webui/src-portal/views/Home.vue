<script setup lang="ts">
// Personal account access and shared-pool permission are separate rights.
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import { api, type Me } from '../api'
import { toast } from '../../src/lib/toast'
import WButton from '../../src/components/ui/WButton.vue'
import WInput from '../../src/components/ui/WInput.vue'

defineProps<{ me: Me }>()
defineEmits<{ refresh: [] }>()
const router = useRouter()

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
          <h1 class="text-base font-semibold">我的调用权限</h1>
          <p class="mt-1 text-micro text-faint">自己的账号可直接使用；共享池需要有效贡献和管理员授权。两者都遵守 Key 的模型范围及管理员禁用规则。</p>
        </div>
        <span
          class="shrink-0 rounded-full px-3 py-1 text-micro font-medium"
          :class="me.eligible ? 'bg-live/12 text-live' : 'bg-warn/12 text-warn'"
        >{{ me.eligible ? '可以调用' : me.eligible_accounts ? '暂不可调用' : '待添加账号' }}</span>
      </div>
      <div class="mt-5 grid grid-cols-2 gap-3 sm:grid-cols-3">
        <div class="rounded-xl border border-line bg-elevated/50 p-4">
          <div class="text-micro text-faint">本人账号</div>
          <div class="mono mt-1 text-xl font-semibold">{{ me.eligible_accounts }}</div>
        </div>
        <div class="rounded-xl border border-line bg-elevated/50 p-4">
          <div class="text-micro text-faint">本人可用模型</div>
          <div class="mono mt-1 text-xl font-semibold">{{ me.own_models.length }}</div>
        </div>
        <div class="col-span-2 rounded-xl border border-line bg-elevated/50 p-4 sm:col-span-1">
          <div class="text-micro text-faint">共享池可用模型</div>
          <div class="mono mt-1 text-xl font-semibold">{{ me.shared_models.length }}</div>
        </div>
      </div>
      <div v-if="!me.eligible" class="mt-4 rounded-xl border border-warn/30 bg-warn/8 p-4 text-small text-muted">
        {{ me.eligible_accounts ? '账号已添加，但暂未找到符合权限的模型。请检查账号状态，或联系管理员刷新目录与确认禁用规则。' : '先到「我的账号」扫码添加 WorkBuddy 账号，再查看可用模型、创建 Key。不需要等待共享分组授权即可使用自己的账号。' }}
      </div>
      <p v-if="me.own_models.length && !me.shared_models.length" class="mt-4 text-small text-muted">你已经可以使用本人账号。共享池尚未授权或没有可用资源，不影响本人调用。</p>
      <p class="mt-3 text-micro text-faint">撤回共享后，账号保留为「仅本人使用」，不会再供其他用户调用。额度、冷却或授权失效仍可能影响调用。</p>
      <div class="mt-4 flex flex-wrap gap-2">
        <WButton v-if="!me.eligible_accounts" variant="primary" @click="router.push('/contribute')">添加我的账号</WButton>
        <WButton variant="subtle" @click="router.push('/models')">查看可用模型</WButton>
        <WButton :disabled="!me.available_models.length" @click="router.push('/keys')">创建 API Key</WButton>
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
