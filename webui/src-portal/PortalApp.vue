<script setup lang="ts">
import { useLiveRead } from '../src/lib/liveRead'
// 门户外壳：登录/注册门 + 已登录的简单顶栏导航。风格与管理 WebUI 一致
// （同一套设计令牌），但结构极简：用户只需要看到资格、密钥和用量。
import { ref, computed, onMounted, onUnmounted, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api, PortalAPIError, type Me } from './api'
import WConfirm from '../src/components/ui/WConfirm.vue'
import WToaster from '../src/components/ui/WToaster.vue'
import WButton from '../src/components/ui/WButton.vue'
import WInput from '../src/components/ui/WInput.vue'
import WLed from '../src/components/ui/WLed.vue'
import PasswordChangeForm from '../src/components/PasswordChangeForm.vue'
import { toast } from '../src/lib/toast'

const route = useRoute()
const router = useRouter()

const checking = ref(true)
const me = ref<Me | null>(null)
const mode = ref<'invite' | 'open' | 'closed'>('invite')
const showLogin = ref(true) // false = 注册表单

// 登录表单
const username = ref('')
const password = ref('')
const invite = ref('')
// Consume before any API request/navigation; keep the code only in this form's memory.
const entryURL = new URL(window.location.href)
if (entryURL.searchParams.has('aff')) {
  const code = (entryURL.searchParams.get('aff') || '').trim().toUpperCase()
  if (/^[A-Z0-9]{6,32}$/.test(code)) { invite.value = code; showLogin.value = false }
  entryURL.searchParams.delete('aff')
  window.history.replaceState(window.history.state, '', entryURL.pathname + entryURL.search + entryURL.hash)
}
const busy = ref(false)
const error = ref('')
const connectionError = ref('')
let lastProbe = 0
let sessionVersion = 0
const identityRead = useLiveRead(async signal => {
  let version=sessionVersion
  try { return { value:await api.me(signal), version } }
  catch(e) {
    if(signal.aborted)throw e
    if(!(e instanceof PortalAPIError) || e.status!==401)throw e
    version=sessionVersion
    return { state:await api.authState(signal), version }
  }
}, result => {
  if(result.version!==sessionVersion)return
  if(result.value) { me.value=result.value;invite.value='' }
  else if(result.state) { me.value=null;mode.value=result.state.mode }
  connectionError.value='';checking.value=false;lastProbe=Date.now()
}, { interval:30000, enabled:()=>!!me.value || !!connectionError.value })
watch(identityRead.error, value => { if(value){connectionError.value=value;checking.value=false} })
const probe=()=>identityRead.refresh()
function expireSession() { sessionVersion++; me.value = null }
function refreshIfStale() { if (Date.now() - lastProbe > 15000) void probe() }

async function submit() {
  if (busy.value) return
  busy.value = true
  error.value = ''
  try {
    if (showLogin.value) {
      const result = await api.login(username.value.trim(), password.value)
      if (result.redirect_to === "/admin-ui/") { window.location.assign(result.redirect_to); return }
    } else {
      await api.register(username.value.trim(), password.value, invite.value.trim())
    }
    sessionVersion++
    await probe()
    username.value = password.value = invite.value = ''
  } catch (e: any) {
    error.value = e?.message || '操作失败'
  } finally {
    busy.value = false
  }
}

async function logout() {
  try { await api.logout(); expireSession() } catch (e: any) { toast.error(e?.message || '退出失败，请重试') }
}

const nav = [
  { key: '/', label: '首页' },
  { key: '/media', label: '工作台' },
  { key: '/keys', label: 'API 密钥' },
  { key: '/models', label: '可用模型' },
  { key: '/contribute', label: '我的账号' },
  { key: '/usage', label: '用量' },
]
const activeKey = computed(() => '/' + (route.path.split('/')[1] || ''))

function go(key: string) { if (route.path !== key) router.push(key) }

const isLight = ref(document.documentElement.classList.contains('light'))
function toggleTheme() {
  isLight.value = !isLight.value
  document.documentElement.classList.toggle('light', isLight.value)
  localStorage.setItem('w2a_theme', isLight.value ? 'light' : 'dark')
}

watch(() => route.path, refreshIfStale)
onMounted(() => { window.addEventListener('portal-session-expired', expireSession) })
onUnmounted(() => { window.removeEventListener('portal-session-expired', expireSession) })
</script>

<template>
  <!-- 会话探测中 -->
  <div v-if="checking" class="flex min-h-screen items-center justify-center bg-bg">
    <span class="h-6 w-6 animate-spin rounded-full border-2 border-brand border-t-transparent" />
  </div>

  <!-- 登录 / 注册 -->
  <div v-else-if="!me" class="flex min-h-screen items-center justify-center bg-bg p-4">
    <div class="glass w-full max-w-sm rounded-2xl p-7 shadow-glass">
      <div class="mb-6 flex items-center gap-2.5">
        <WLed tone="live" pulse />
        <span class="mono text-lg font-semibold tracking-tight text-ink">work2api</span>
        <span class="ml-auto text-micro text-faint">用户门户</span>
      </div>

      <p v-if="connectionError" class="mb-4 text-micro text-fault">{{ connectionError }} <button class="text-brand hover:underline" @click="probe">重试连接</button></p>

      <template v-if="mode === 'closed' && !showLogin">
        <h1 class="mb-1 text-base font-semibold text-ink">暂未开放注册</h1>
        <p class="text-micro text-faint">管理员已关闭自助注册，如需使用请联系管理员开通账号。</p>
        <div class="mt-5 border-t border-line pt-4">
          <button class="text-micro text-brand hover:underline" @click="showLogin = true">已有账号？直接登录</button>
        </div>
      </template>

      <template v-else>
        <h1 class="mb-1 text-base font-semibold text-ink">{{ showLogin ? '登录' : '注册' }}</h1>
        <p class="mb-5 text-micro text-faint">
          {{ showLogin
            ? '登录后根据账号身份进入管理台或用户门户。'
            : mode === 'invite'
              ? '注册需要有效邀请码，可用次数与有效期由管理员设置。'
              : '当前开放注册，注册后可创建 Key 使用默认开放模型，账号贡献可选。' }}
        </p>

        <label class="mb-1.5 block text-small text-muted">用户名</label>
        <WInput v-model="username" placeholder="2–64 位字母、数字、- _ ." class="mb-3" />
        <label class="mb-1.5 block text-small text-muted">密码</label>
        <WInput v-model="password" type="password" placeholder="8–72 字节" class="mb-3" @enter="submit" />
        <template v-if="!showLogin && mode === 'invite'">
          <label class="mb-1.5 block text-small text-muted">邀请码</label>
          <WInput v-model="invite" placeholder="管理员发放的邀请码" class="mb-1" @enter="submit" />
        </template>
        <p v-if="error" class="mb-2 text-micro text-fault">{{ error }}</p>
        <WButton variant="primary" block :loading="busy" class="mt-2" @click="submit">
          {{ showLogin ? '登录' : '注册并登录' }}
        </WButton>
        <div v-if="mode !== 'closed'" class="mt-4 border-t border-line pt-4 text-center">
          <button class="text-micro text-brand hover:underline" @click="showLogin = !showLogin; error = ''">
            {{ showLogin ? '没有账号？' + (mode === 'invite' ? '用邀请码注册' : '注册新账号') : '已有账号？直接登录' }}
          </button>
        </div>
      </template>
    </div>
  </div>

  <!-- 已登录外壳 -->
  <div v-else class="flex min-h-screen flex-col bg-bg text-ink">
    <header class="sticky top-0 z-20 border-b border-line bg-surface/80 backdrop-blur-xl">
      <div class="mx-auto flex h-14 w-full max-w-4xl items-center gap-4 px-4">
        <div class="flex items-center gap-2.5">
          <WLed tone="live" pulse />
          <span class="mono font-semibold tracking-tight">work2api</span>
          <span class="text-micro text-faint">门户</span>
        </div>
        <nav v-if="!me.user.must_change_password" class="ml-4 hidden gap-1 md:flex">
          <button
            v-for="item in nav" :key="item.key"
            class="rounded-lg px-3 py-1.5 text-small font-medium transition-colors"
            :class="activeKey === item.key ? 'bg-brand/12 text-brand' : 'text-muted hover:bg-elevated hover:text-ink'"
            @click="go(item.key)"
          >{{ item.label }}</button>
        </nav>
        <div class="ml-auto flex items-center gap-2">
          <span class="hidden max-w-24 truncate text-micro text-muted lg:inline" :title="me.user.username">{{ me.user.username }}</span>
          <button class="flex h-8 items-center gap-1.5 rounded-lg border border-line px-2.5 text-micro text-muted transition-colors hover:border-brand hover:text-brand" @click="toggleTheme">
            {{ isLight ? '暗色' : '浅色' }}
          </button>
          <button class="flex h-8 items-center rounded-lg border border-line px-2.5 text-micro text-muted transition-colors hover:border-fault hover:text-fault" @click="logout">
            退出
          </button>
        </div>
      </div>
      <!-- 移动端导航 -->
      <nav v-if="!me.user.must_change_password" class="flex gap-1 overflow-x-auto border-t border-line px-3 py-2 md:hidden">
        <button
          v-for="item in nav" :key="item.key"
          class="whitespace-nowrap rounded-lg px-3 py-1.5 text-small font-medium transition-colors"
          :class="activeKey === item.key ? 'bg-brand/12 text-brand' : 'text-muted hover:bg-elevated hover:text-ink'"
          @click="go(item.key)"
        >{{ item.label }}</button>
      </nav>
    </header>
    <main class="mx-auto w-full max-w-4xl flex-1 px-4 py-6"><p v-if="connectionError" class="mb-4 text-micro text-fault">{{ connectionError }} <button class="text-brand hover:underline" @click="probe">重试连接</button></p><section v-if="me.user.must_change_password" class="glass mx-auto max-w-md rounded-xl p-6 space-y-4"><h1 class="font-semibold">设置个人密码</h1><PasswordChangeForm :change="api.changePassword" forced @done="probe" /></section><router-view v-else :me="me" @refresh="probe" /></main>
  </div>

  <WConfirm />
  <WToaster />
</template>
