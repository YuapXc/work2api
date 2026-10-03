<script setup lang="ts">
import { api } from '@/api/client'
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { toast } from '@/lib/toast'
import WIcon from '@/components/ui/WIcon.vue'
import WButton from '@/components/ui/WButton.vue'
import WInput from '@/components/ui/WInput.vue'
import WLed from '@/components/ui/WLed.vue'
import WToaster from '@/components/ui/WToaster.vue'
import WConfirm from '@/components/ui/WConfirm.vue'

const route = useRoute()
const router = useRouter()

const nav = [
  { key: '/overview', label: '概览', icon: 'overview' },
  { key: '/accounts', label: '账号', icon: 'accounts' },
  { key: '/models', label: '模型', icon: 'models' },
  { key: '/keys', label: 'API 密钥', icon: 'keys' },
  { key: '/traffic', label: '流量', icon: 'traffic' },
  { key: '/settings', label: '设置', icon: 'settings' },
  { key: '/portal', label: '门户管理', icon: 'accounts' },
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
// ADMIN_TOKEN 已设置：POST /admin/login 校验并签发 HttpOnly 会话 cookie（24h，
// 服务端滑动续期），token 不再落 localStorage。未设置 ADMIN_TOKEN：后端 /admin
// 仅回环可达，本机直接放行（后端 403 会把远程访问者挡在登录页外）。
const authChecking = ref(true)
const authOk = ref(false)
const authLoading = ref(false)
const tokenInput = ref('')
const loginMode = ref<'password' | 'token'>('password')
const adminUsername = ref('')
const adminPassword = ref('')
const passwordEnabled = ref(false)
const authError = ref('')

async function checkAuth() {
  authChecking.value = true
  authError.value = ''
  try {
    // 空 body 的 POST /admin/login 即会话探测：后端校验 cookie/头部，不消耗限速。
    const res = await fetch('/admin/login', { method: 'POST', body: '{}' })
    const state = await res.json().catch(() => ({}))
    passwordEnabled.value = !!state.password_enabled
    loginMode.value = passwordEnabled.value ? 'password' : 'token'
    authOk.value = res.ok
    if (authOk.value) void refreshHealth()
  } catch {
    authOk.value = false
    authError.value = '网络异常，请稍后重试'
  }
  authChecking.value = false
}

async function onLogin() {
  const t = tokenInput.value.trim()
  if (loginMode.value === 'token' ? !t : !adminUsername.value.trim() || !adminPassword.value) return toast.error('请填写登录信息')
  authLoading.value = true
  authError.value = ''
  try {
    const res = await fetch('/admin/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(loginMode.value === 'token' ? { token: t } : { username: adminUsername.value.trim(), password: adminPassword.value }),
    })
    const data = await res.json().catch(() => ({}))
    if (res.ok) {
      authOk.value = true
      tokenInput.value = ''
      adminPassword.value = ''
      toast.success('登录成功')
      void refreshHealth()
    } else {
      authError.value = data.error?.message || data.message || (res.status === 429 ? '尝试次数过多，请稍后再试' : '登录信息无效，请检查后重试')
      toast.error(authError.value)
    }
  } catch {
    authError.value = '网络异常，请稍后重试'
    toast.error(authError.value)
  } finally {
    authLoading.value = false
  }
}

async function logout() {
  try {
    await fetch('/admin/logout', { method: 'POST' })
  } catch { /* 忽略 */ }
  authOk.value = false
  toast.success('已退出登录')
}

// ---------- 网关健康（30s 轮询） ----------
const healthy = ref<number | null>(null)
const total = ref<number | null>(null)
let timer: ReturnType<typeof setInterval> | null = null
const healthRefreshFailed = ref(false)
let healthRefreshing = false
async function refreshHealth() {
  if (!authOk.value) return
  if (healthRefreshing) return
  healthRefreshing = true
  try {
    const data = await api.health()
    healthRefreshFailed.value = false
    healthy.value = data.available_providers ?? 0
    total.value = data.total_providers ?? 0
  } catch (err: any) {
    healthRefreshFailed.value = true
    if (err?.response?.status === 401 || err?.response?.status === 403) {
      authOk.value = false
      healthy.value = total.value = null
    }
  } finally { healthRefreshing = false }
}
const serving = computed(() => (healthy.value ?? 0) > 0)
const gwText = computed(() => (healthRefreshFailed.value ? '刷新失败（保留上次状态）' : healthy.value == null ? '状态未知' : serving.value ? '有可用渠道' : '无可用渠道'))

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
      <p class="mb-5 text-micro text-faint">
        使用管理员账号密码登录。管理 Token 可用于初始化账号与恢复管理访问。
      </p>
      <template v-if="loginMode === 'password'">
        <label class="mb-1.5 block text-small text-muted">管理员用户名</label>
        <WInput v-model="adminUsername" placeholder="管理员用户名" class="mb-3" />
        <label class="mb-1.5 block text-small text-muted">密码</label>
        <WInput v-model="adminPassword" type="password" class="mb-3" @enter="onLogin" />
      </template>
      <template v-else>
        <label class="mb-1.5 block text-small text-muted">管理 Token</label>
        <WInput v-model="tokenInput" type="password" placeholder="输入 ADMIN_TOKEN" class="mb-3" @enter="onLogin" />
      </template>
      <button v-if="passwordEnabled" class="mb-3 text-micro text-brand" @click="loginMode = loginMode === 'password' ? 'token' : 'password'; authError = ''">{{ loginMode === 'password' ? '使用 Token 恢复访问' : '使用账号密码登录' }}</button>
      <p v-if="authError" class="mb-2 text-micro text-fault">{{ authError }}</p>
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
            class="flex h-8 flex-1 items-center justify-center gap-1.5 rounded-lg border border-line text-micro text-muted transition-colors hover:border-fault hover:text-fault"
            @click="logout"
          >
            <WIcon name="keys" :size="15" /> 退出登录
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

  <WToaster />
  <WConfirm />
</template>

