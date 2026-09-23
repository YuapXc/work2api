<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { message, theme } from 'ant-design-vue'
import { KeyOutlined } from '@ant-design/icons-vue'
import { setAdminToken } from '@/api/client'

const route = useRoute()
const router = useRouter()

// 深色算法 + 令牌映射（与 tokens.css 同源）
const darkTheme = {
  algorithm: theme.darkAlgorithm,
  token: {
    colorPrimary: '#5AA9E6',
    colorBgContainer: '#13242B',
    colorBgElevated: '#13242B',
    colorText: '#E6F0EE',
    colorBorder: '#24454F',
    fontFamily: "'Space Grotesk', -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif",
    borderRadius: 6,
  },
}

// 导航项 = 仪表开关。标签固定：概览 / 账号 / 模型 / 用量 / 记录 / 应用
const menuItems = [
  { key: '/overview', label: '概览' },
  { key: '/accounts', label: '账号' },
  { key: '/models', label: '模型' },
  { key: '/usage', label: '用量' },
  { key: '/records', label: '记录' },
  { key: '/apps', label: '应用' },
]

const activeKey = computed(() => route.path)
function go(key: string) {
  if (route.path !== key) router.push(key)
}

// 管理台登录门：进管理页先校验 Token，未通过只渲染登录页
const authChecking = ref(true)
const authOk = ref(false)
const authLoading = ref(false)
const tokenInput = ref('')
const tokenVisible = ref(false)

// Token 格式校验：字母数字、下划线、连字符，至少 8 位
function validateToken(t: string): boolean {
  return /^[a-zA-Z0-9_-]{8,}$/.test(t)
}

async function checkAuth() {
  authChecking.value = true
  try {
    const res = await fetch('/admin/accounts', {
      headers: { 'X-Admin-Token': localStorage.getItem('workbuddy_admin_token') || '' },
    })
    if (res.status === 403) {
      authOk.value = false
    } else if (res.ok) {
      authOk.value = true
    } else {
      // 网络错误等其他情况：放行进壳层，让页面自身重试逻辑处理，避免误锁
      authOk.value = true
    }
  } catch {
    authOk.value = true
  } finally {
    authChecking.value = false
  }
}

async function onLogin() {
  const t = tokenInput.value.trim()
  if (!t || !validateToken(t)) {
    message.error('Token 格式不正确（至少 8 位字母/数字/_-）')
    return
  }
  authLoading.value = true
  setAdminToken(t)
  try {
    const res = await fetch('/admin/accounts', { headers: { 'X-Admin-Token': t } })
    if (res.ok) {
      authOk.value = true
      message.success('登录成功')
    } else {
      setAdminToken('')
      message.error('Token 无效，请检查后重试')
    }
  } catch {
    setAdminToken('')
    message.error('网络异常，请稍后重试')
  } finally {
    authLoading.value = false
  }
}

function saveToken() {
  const t = tokenInput.value.trim()
  if (t && !validateToken(t)) {
    message.error('Token 格式不正确（至少 8 位字母/数字/_-）')
    return
  }
  setAdminToken(t)
  tokenInput.value = ''
  tokenVisible.value = false
  message.success(t ? '已保存管理 Token' : '已清除管理 Token')
  router.go(0) // 重新加载，让后续请求带上 Token
}

// 侧边栏健康账号数指示（每 30s 刷新）
const healthyCount = ref<number | null>(null)
const totalCount = ref<number | null>(null)
let healthTimer: ReturnType<typeof setInterval> | null = null
async function refreshHealth() {
  try {
    const res = await fetch('/admin/accounts', {
      headers: { 'X-Admin-Token': localStorage.getItem('workbuddy_admin_token') || '' },
    })
    if (!res.ok) { healthyCount.value = null; totalCount.value = null; return }
    const data = await res.json()
    const list = (data.accounts || []) as { healthy?: boolean }[]
    healthyCount.value = list.filter((a) => a.healthy).length
    totalCount.value = list.length
  } catch {
    healthyCount.value = null
    totalCount.value = null
  }
}

// 网关状态：至少一个健康账号 → 正在服务；否则降级
const gatewayServing = computed(() => (healthyCount.value ?? 0) > 0)

onMounted(() => {
  checkAuth()
  refreshHealth()
  healthTimer = setInterval(refreshHealth, 30000)
})
onUnmounted(() => {
  if (healthTimer) clearInterval(healthTimer)
})
</script>

<template>
  <a-config-provider :theme="darkTheme">
    <!-- 登录门：校验中 -->
    <div v-if="authChecking" class="gate">
      <a-spin size="large" />
    </div>

    <!-- 登录门：未通过 -->
    <div v-else-if="!authOk" class="gate">
      <div class="gate-card">
        <div class="gate-brand">
          <span class="led live"></span>
          <span class="wordmark">work2api</span>
        </div>
        <div class="gate-title">管理员登录</div>
        <div class="gate-field">
          <label>管理 Token</label>
          <a-input-password
            v-model:value="tokenInput"
            placeholder="输入 ADMIN_TOKEN"
            @pressEnter="onLogin"
          />
        </div>
        <a-button type="primary" block :loading="authLoading" @click="onLogin">
          <template #icon><KeyOutlined /></template>
          登录
        </a-button>
        <p class="gate-note">本机回环访问且未设置 ADMIN_TOKEN 时无需登录，直接可用。</p>
      </div>
    </div>

    <!-- 控制台外壳：左侧设备脊柱 + 内容区 -->
    <div v-else class="shell">
      <aside class="spine">
        <!-- 顶部：网关状态 LED + 字标 -->
        <div class="spine-head">
          <span class="led" :class="gatewayServing ? 'live' : 'fault'"></span>
          <span class="wordmark">work2api</span>
        </div>

        <!-- 导航：仪表开关（选中 = route 左缘竖条 + 提亮） -->
        <nav class="switches">
          <button
            v-for="item in menuItems"
            :key="item.key"
            class="switch"
            :class="{ on: activeKey === item.key }"
            @click="go(item.key)"
          >
            <span class="switch-bar"></span>
            <span class="switch-label">{{ item.label }}</span>
          </button>
        </nav>

        <!-- 底部：实时健康账号读数 + Token 管理 -->
        <div class="spine-foot">
          <div class="readout">
            <span class="led" :class="gatewayServing ? 'live' : (healthyCount === null ? 'fog' : 'fault')"></span>
            <span v-if="healthyCount !== null" class="readout-text">
              <span class="mono">{{ healthyCount }}</span><span v-if="totalCount !== null" class="readout-slash mono">/{{ totalCount }}</span>
              健康账号
            </span>
            <span v-else class="readout-text fog">账号状态未知</span>
          </div>
          <a-popover v-model:open="tokenVisible" title="管理 Token" trigger="click" placement="topLeft">
            <template #content>
              <div style="width: 260px">
                <p style="font-size: 12px; color: var(--fog); margin-bottom: 8px">
                  仅当后端设置了 <code>ADMIN_TOKEN</code>（如从局域网访问）时才需要填写。
                </p>
                <a-input
                  v-model:value="tokenInput"
                  placeholder="输入 ADMIN_TOKEN"
                  style="margin-bottom: 8px"
                  @pressEnter="saveToken"
                />
                <a-button type="primary" block size="small" @click="saveToken">保存</a-button>
                <a-button size="small" block style="margin-top: 4px" @click="tokenInput = ''; saveToken()">清除</a-button>
              </div>
            </template>
            <button class="token-link">管理 Token</button>
          </a-popover>
        </div>
      </aside>

      <main class="content">
        <router-view />
      </main>
    </div>
  </a-config-provider>
</template>

<style scoped>
/* ---------- 登录门 ---------- */
.gate {
  min-height: 100vh; display: flex; align-items: center; justify-content: center;
  background: var(--ink); padding: 16px;
}
.gate-card {
  width: 380px; max-width: 100%;
  background: var(--panel); border: 1px solid var(--line); border-radius: var(--radius);
  padding: 26px 24px;
}
.gate-brand { display: flex; align-items: center; gap: 10px; margin-bottom: 20px; }
.gate-title { font-size: 16px; color: var(--paper); margin-bottom: 16px; }
.gate-field { margin-bottom: 14px; }
.gate-field label { display: block; font-size: 13px; color: var(--fog); margin-bottom: 6px; }
.gate-note { margin-top: 14px; color: var(--fog); font-size: 12px; line-height: 1.5; }

.wordmark {
  font-family: var(--mono); font-weight: 500; font-size: 16px; letter-spacing: -0.02em;
  color: var(--paper);
}

/* ---------- 外壳 ---------- */
.shell { display: flex; height: 100vh; overflow: hidden; background: var(--ink); }

/* ---------- 设备脊柱 ---------- */
.spine {
  width: 208px; flex-shrink: 0;
  background: var(--ink-2);
  border-right: 1px solid var(--line);
  display: flex; flex-direction: column;
}
.spine-head {
  display: flex; align-items: center; gap: 10px;
  padding: 20px 18px; border-bottom: 1px solid var(--line);
}

.switches { flex: 1; padding: 12px 10px; overflow-y: auto; display: flex; flex-direction: column; gap: 2px; }
.switch {
  position: relative; display: flex; align-items: center;
  width: 100%; height: 40px; padding: 0 12px 0 16px;
  background: transparent; border: none; cursor: pointer;
  color: var(--fog); font-family: var(--sans); font-size: 14px; text-align: left;
  border-radius: var(--radius); transition: color 0.15s ease, background 0.15s ease;
}
.switch-bar {
  position: absolute; left: 0; top: 9px; bottom: 9px; width: 3px; border-radius: 2px;
  background: transparent; transition: background 0.15s ease;
}
.switch:hover { color: var(--paper); background: rgba(90, 169, 230, 0.06); }
.switch.on { color: var(--paper); background: rgba(90, 169, 230, 0.1); }
.switch.on .switch-bar { background: var(--route); }
.switch-label { line-height: 1; }

.spine-foot { padding: 14px 16px; border-top: 1px solid var(--line); }
.readout { display: flex; align-items: center; gap: 8px; font-size: 12px; color: var(--fog); }
.readout-text { color: var(--fog); }
.readout-text .mono { color: var(--paper); font-size: 13px; }
.readout-slash { color: var(--fog); }
.token-link {
  margin-top: 10px; background: none; border: none; padding: 0; cursor: pointer;
  color: var(--route); font-family: var(--sans); font-size: 12px;
}
.token-link:hover { color: #6FB6EC; }

/* ---------- 内容区（独立滚动） ---------- */
.content { flex: 1; overflow: auto; padding: 24px 28px; max-width: 100%; }

/* ---------- 响应式：<820px 收成顶栏 ---------- */
@media (max-width: 820px) {
  .shell { flex-direction: column; height: 100vh; }
  .spine {
    width: 100%; flex-direction: row; align-items: center;
    border-right: none; border-bottom: 1px solid var(--line);
    overflow-x: auto;
  }
  .spine-head { border-bottom: none; border-right: 1px solid var(--line); padding: 12px 14px; flex-shrink: 0; }
  .switches { flex-direction: row; padding: 8px; gap: 2px; overflow-x: auto; overflow-y: hidden; }
  .switch { width: auto; height: 34px; padding: 0 12px; }
  .switch-bar { top: auto; bottom: 0; left: 8px; right: 8px; width: auto; height: 2px; }
  .spine-foot { border-top: none; border-left: 1px solid var(--line); padding: 10px 14px; flex-shrink: 0; display: flex; align-items: center; gap: 12px; }
  .spine-foot .token-link { margin-top: 0; }
  .content { padding: 18px 16px; }
}
</style>
