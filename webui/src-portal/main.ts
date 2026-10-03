// 用户门户入口（独立于管理 WebUI 的构建产物，HANDOFF §8 阶段 A）。
// 复用管理端的设计令牌与基础组件（styles/app.css、components/ui/*），
// 但路由、状态与 API 面完全独立，避免两个界面互相耦合。
import { createApp } from 'vue'
import { createRouter, createWebHashHistory } from 'vue-router'
import '@fontsource-variable/inter'
import '@fontsource/jetbrains-mono/400.css'
import '@fontsource/jetbrains-mono/500.css'
import '../src/styles/app.css'
import PortalApp from './PortalApp.vue'

const savedTheme = localStorage.getItem('w2a_theme')
const wantLight = savedTheme
  ? savedTheme === 'light'
  : !!window.matchMedia?.('(prefers-color-scheme: light)').matches
document.documentElement.classList.toggle('light', wantLight)

const router = createRouter({
  history: createWebHashHistory(),
  routes: [
    { path: '/models', component: () => import('./views/Models.vue'), meta: { title: '可用模型' } },
    { path: '/', component: () => import('./views/Home.vue'), meta: { title: '首页' } },
    { path: '/keys', component: () => import('./views/Keys.vue'), meta: { title: 'API 密钥' } },
    { path: '/contribute', component: () => import('./views/Contribute.vue'), meta: { title: '贡献账号' } },
    { path: '/usage', component: () => import('./views/Usage.vue'), meta: { title: '用量' } },
    { path: '/:pathMatch(.*)*', redirect: '/' },
  ],
})

createApp(PortalApp).use(router).mount('#app')
