import { createApp } from 'vue'
import { createRouter, createWebHashHistory } from 'vue-router'
import Antd from 'ant-design-vue'
import 'ant-design-vue/dist/reset.css'
// 字体：UI/标题用 Space Grotesk，读数/ID/Key 用 JetBrains Mono
import '@fontsource/space-grotesk/400.css'
import '@fontsource/space-grotesk/500.css'
import '@fontsource/space-grotesk/700.css'
import '@fontsource/jetbrains-mono/400.css'
import '@fontsource/jetbrains-mono/500.css'
// 设计令牌 + AntD 覆盖：必须在 reset.css 之后引入，覆盖才生效
import './styles/tokens.css'
import App from './App.vue'

// 路由懒加载：每个页面独立 chunk，首屏只加载当前页
const router = createRouter({
  // 使用 hash 模式，避免后端需要 history 回退（静态托管更简单）
  history: createWebHashHistory(),
  routes: [
    { path: '/', redirect: '/overview' },
    { path: '/overview', component: () => import('./views/Overview.vue'), meta: { title: '概览' } },
    { path: '/accounts', component: () => import('./views/Accounts.vue'), meta: { title: '账号' } },
    { path: '/models', component: () => import('./views/Models.vue'), meta: { title: '模型' } },
    { path: '/usage', component: () => import('./views/Usage.vue'), meta: { title: '用量' } },
    { path: '/records', component: () => import('./views/Records.vue'), meta: { title: '使用记录' } },
    { path: '/apps', component: () => import('./views/Apps.vue'), meta: { title: '应用' } },
  ],
})

const app = createApp(App)
app.use(router)
app.use(Antd)
app.mount('#app')
