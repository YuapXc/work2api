<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { setAdminToken } from '@/api/client'
import { toast } from '@/lib/toast'
import WIcon from '@/components/ui/WIcon.vue'
import WButton from '@/components/ui/WButton.vue'
import WInput from '@/components/ui/WInput.vue'
import WModal from '@/components/ui/WModal.vue'
import WLed from '@/components/ui/WLed.vue'
import WToaster from '@/components/ui/WToaster.vue'
import WConfirm from '@/components/ui/WConfirm.vue'

const route = useRoute()
const router = useRouter()

const TOKEN_KEY = 'workbuddy_admin_token'
const nav = [
  { key: '/overview', label: '概览', icon: 'overview' },
  { key: '/accounts', label: '账号', icon: 'accounts' },
  { key: '/models', label: '模型', icon: 'models' },
  { key: '/keys', label: 'API 密钥', icon: 'keys' },
  { key: '/traffic', label: '流量', icon: 'traffic' },
  { key: '/settings', label: '设置', icon: 'settings' },
]
const activeKey = computed(() => '/' + (route.path.split('/')[1] || 'overview'))
const mobileOpen = ref(false)
function go(key: string) {
  if (route.path !== key) router.push(key)
  mobileOpen.value = false
}

// ---------- 主题 ----------
const isLight = ref(document.documentElement.classList.contains('light'))
function toggleTheme() {
  isLight.value = !isLight.value
  document.documentElement.classList.toggle('light', isLight.value)
  localStorage.setItem('w2a_theme', isLight.value ? 'light' : 'dark')
}

// ---------- 登录门 ----------
const authChecking = ref(true)
const authOk = ref(false)
const authLoading = ref(false)
const tokenInput = ref('')
const tokenModal = ref(false)
const validateToken = (t: string) => /^[a-zA-Z0-9_-]{8,}$/.test(t)

async function probe(token: string): Promise<number> {
  try {
    const res = await fetch('/admin/accounts', { headers: { 'X-Admin-Token': token } })
    return res.status
  } catch {
    return 0
  }
}

async function checkAuth() {
  authChecking.value = true
  const status = await probe(localStorage.getItem(TOKEN_KEY) || '')
  // 403 才锁；网络错误等放行，交由页面自身重试，避免误锁
  authOk.value = status !== 403
  authChecking.value = false
}

async function onLogin() {
  const t = tokenInput.value.trim()
  if (!validateToken(t)) return toast.error('Token 格式不正确（至少 8 位字母/数字/_-）')
  authLoading.value = true
  setAdminToken(t)
  const status = await probe(t)
  authLoading.value = false
  if (status && status !== 403) {
    authOk.value = true
    toast.success('登录成功')
  } else {
    setAdminToken('')
    toast.error(status === 403 ? 'Token 无效，请检查后重试' : '网络异常，请稍后重试')
  }
}

function saveToken() {
  const t = tokenInput.value.trim()
  if (t && !validateToken(t)) return toast.error('Token 格式不正确（至少 8 位字母/数字/_-）')
  setAdminToken(t)
  tokenModal.value = false
  toast.success(t ? '已保存管理 Token' : '已清除管理 Token')
  router.go(0)
}

// ---------- 网关健康（30s 轮询） ----------
const healthy = ref<number | null>(null)
const total = ref<number | null>(null)
let timer: ReturnType<typeof setInterval> | null = null
async function refreshHealth() {
  try {
    const res = await fetch('/admin/accounts', { headers: { 'X-Admin-Token': localStorage.getItem(TOKEN_KEY) || '' } })
    if (!res.ok) { healthy.value = total.value = null; return }
    const data = await res.json()
    const list = (data.accounts || []) as { healthy?: boolean }[]
    healthy.value = list.filter((a) => a.healthy).length
    total.value = list.length
  } catch {
    healthy.value = total.value = null
  }
}
const serving = computed(() => (healthy.value ?? 0) > 0)
const gwText = computed(() => (healthy.value == null ? '状态未知' : serving.value ? '正在服务' : '无健康账号'))

onMounted(() => {
  checkAuth()
  refreshHealth()
  timer = setInterval(refreshHealth, 30000)
})
onUnmounted(() => timer && clearInterval(timer))
</script>

<template>
  <!-- 登录门：校验中 -->
  <div v-if="authChecking" class="flex min-h-screen items-center justify-center bg-bg">
    <span class="h-6 w-6 animate-spin rounded-full border-2 border-brand border-t-transparent" />
  </div>

  <!-- 登录门：未通过 -->
  <div v-else-if="!authOk" class="flex min-h-screen items-center justify-center bg-bg p-4">
    <div class="glass w-full max-w-sm rounded-2xl p-7 shadow-glass">
      <div class="mb-6 flex items-center gap-2.5">
        <WLed tone="live" pulse />
        <span class="mono text-lg font-semibold tracking-tight text-ink">work2api</span>
      </div>
      <h1 class="mb-1 text-base font-semibold text-ink">管理员登录</h1>
      <p class="mb-5 text-micro text-faint">本机回环访问且未设置 ADMIN_TOKEN 时无需登录，可直接进入。</p>
      <label class="mb-1.5 block text-small text-muted">管理 Token</label>
      <WInput v-model="tokenInput" type="password" placeholder="输入 ADMIN_TOKEN" class="mb-4" @enter="onLogin" />
      <WButton variant="primary" block :loading="authLoading" @click="onLogin">
        <WIcon name="keys" :size="16" /> 登录
      </WButton>
    </div>
  </div>

  <!-- 控制台外壳 -->
  <div v-else class="flex h-screen overflow-hidden bg-bg text-ink">
    <!-- 移动端遮罩 -->
    <div v-if="mobileOpen" class="fixed inset-0 z-30 bg-black/50 md:hidden" @click="mobileOpen = false" />

    <!-- 侧栏 -->
    <aside
      class="fixed inset-y-0 left-0 z-40 flex w-60 flex-col border-r border-line bg-surface/80 backdrop-blur-xl transition-transform md:static md:translate-x-0"
      :class="mobileOpen ? 'translate-x-0' : '-translate-x-full'"
    >
      <div class="flex items-center gap-2.5 border-b border-line px-5 py-4">
        <WLed :tone="serving ? 'live' : healthy == null ? 'muted' : 'fault'" :pulse="serving" />
        <span class="mono text-[0.95rem] font-semibold tracking-tight text-ink">work2api</span>
        <span class="ml-auto text-micro text-faint">网关</span>
      </div>

      <nav class="flex-1 space-y-1 overflow-y-auto p-3">
        <button
          v-for="item in nav"
          :key="item.key"
          class="group flex w-full items-center gap-3 rounded-lg px-3 py-2.5 text-small font-medium transition-colors"
          :class="activeKey === item.key ? 'bg-brand/12 text-brand' : 'text-muted hover:bg-elevated hover:text-ink'"
          @click="go(item.key)"
        >
          <WIcon :name="item.icon" :size="18" />
          <span>{{ item.label }}</span>
          <span
            v-if="activeKey === item.key"
            class="ml-auto h-1.5 w-1.5 rounded-full bg-brand"
          />
        </button>
      </nav>

      <div class="space-y-3 border-t border-line px-4 py-4">
        <div class="flex items-center gap-2 text-micro text-muted">
          <WLed :tone="serving ? 'live' : healthy == null ? 'muted' : 'fault'" />
          <span>{{ gwText }}</span>
          <span v-if="healthy != null" class="mono ml-auto text-ink">{{ healthy }}<span class="text-faint">/{{ total }}</span></span>
        </div>
        <div class="flex items-center gap-2">
          <button
            class="flex h-8 flex-1 items-center justify-center gap-1.5 rounded-lg border border-line text-micro text-muted transition-colors hover:border-brand hover:text-brand"
            @click="toggleTheme"
          >
            <WIcon :name="isLight ? 'moon' : 'sun'" :size="15" /> {{ isLight ? '暗色' : '浅色' }}
          </button>
          <button
            class="flex h-8 flex-1 items-center justify-center gap-1.5 rounded-lg border border-line text-micro text-muted transition-colors hover:border-brand hover:text-brand"
            @click="tokenInput = ''; tokenModal = true"
          >
            <WIcon name="keys" :size="15" /> Token
          </button>
        </div>
      </div>
    </aside>

    <!-- 主区 -->
    <div class="flex min-w-0 flex-1 flex-col">
      <!-- 移动端顶栏 -->
      <header class="flex items-center gap-3 border-b border-line px-4 py-3 md:hidden">
        <button class="text-muted" @click="mobileOpen = true"><WIcon name="menu" :size="22" /></button>
        <span class="mono font-semibold">work2api</span>
      </header>
      <main class="flex-1 overflow-auto"><router-view /></main>
    </div>
  </div>

  <!-- Token 管理 -->
  <WModal v-model:open="tokenModal" size="sm" title="管理 Token">
    <p class="mb-3 text-small text-muted">
      仅当后端设置了 <code class="mono text-brand">ADMIN_TOKEN</code>（如从局域网访问）时才需要填写。留空保存即清除。
    </p>
    <WInput v-model="tokenInput" type="password" placeholder="输入 ADMIN_TOKEN" @enter="saveToken" />
    <template #footer>
      <WButton variant="subtle" @click="tokenModal = false">取消</WButton>
      <WButton variant="primary" @click="saveToken">保存并重载</WButton>
    </template>
  </WModal>

  <WToaster />
  <WConfirm />
</template>

