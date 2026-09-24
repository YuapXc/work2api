import { createApp } from 'vue'
import { createRouter, createWebHashHistory } from 'vue-router'
import '@fontsource-variable/inter'
import '@fontsource/jetbrains-mono/400.css'
import '@fontsource/jetbrains-mono/500.css'
import '@fontsource/jetbrains-mono/700.css'
import './styles/app.css'
import App from './App.vue'

// 主题初始化：显式选择优先，否则跟随系统偏好；默认暗色
const savedTheme = localStorage.getItem('w2a_theme')
const wantLight = savedTheme
  ? savedTheme === 'light'
  : !!window.matchMedia?.('(prefers-color-scheme: light)').matches
document.documentElement.classList.toggle('light', wantLight)

// 功能优先的信息架构：账号/模型跨供应商聚合，供应商降为筛选维度
const router = createRouter({
  history: createWebHashHistory(),
  routes: [
    { path: '/', redirect: '/overview' },
    { path: '/overview', component: () => import('./views/Overview.vue'), meta: { title: '概览' } },
    { path: '/accounts', component: () => import('./views/Accounts.vue'), meta: { title: '账号' } },
    { path: '/models', component: () => import('./views/Models.vue'), meta: { title: '模型' } },
    { path: '/keys', component: () => import('./views/Keys.vue'), meta: { title: 'API 密钥' } },
    { path: '/traffic', component: () => import('./views/Traffic.vue'), meta: { title: '流量' } },
    { path: '/settings', component: () => import('./views/Settings.vue'), meta: { title: '设置' } },
    // 旧路径重定向，避免收藏失效
    { path: '/usage', redirect: '/traffic' },
    { path: '/records', redirect: '/traffic?tab=logs' },
    { path: '/apps', redirect: '/keys' },
    { path: '/providers/:name', redirect: '/accounts' },
    { path: '/:pathMatch(.*)*', redirect: '/overview' },
  ],
})

createApp(App).use(router).mount('#app')
