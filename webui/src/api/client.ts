import axios, { type AxiosInstance, type InternalAxiosRequestConfig } from 'axios'
import { toast } from '@/lib/toast'
import type {
  AABenchmark,
  AccountInfo,
  AppInfo,
  CheckinResponse,
  ModelInfo,
  OAuthOption,
  Overview,
  ProviderDetail,
  ProviderSummary,
  RecordsResponse,
  Settings,
  UsagePoint,
  UsageRecord,
  UsageSummary,
} from '@/types'

const http: AxiosInstance = axios.create({
  baseURL: '/',
  timeout: 30000,
})

// 请求拦截：附加管理 Token（从 localStorage 读取，支持局域网访问时设置 ADMIN_TOKEN）
http.interceptors.request.use((config) => {
  const token = localStorage.getItem('workbuddy_admin_token') || ''
  if (token) {
    config.headers['X-Admin-Token'] = token
  }
  const userToken = localStorage.getItem('workbuddy_user_token') || ''
  if (userToken) {
    config.headers['X-User-Token'] = userToken
  }
  return config
})

// 响应拦截：统一错误处理 + GET 幂等请求的指数退避重试
// 说明：仅对 GET（读数据、幂等）自动重试，POST/DELETE 等写操作不重试，避免重复副作用。
const MAX_RETRIES = 2

// 错误提示：toast 自带去重（同文案窗口期内只弹一次），这里直接透传。
function toastOnce(msg: string) {
  toast.error(`请求失败：${msg}`)
}

http.interceptors.response.use(
  (resp) => resp.data,
  async (err) => {
    const config = err?.config as (InternalAxiosRequestConfig & { _retry?: number }) | undefined
    const method = typeof config?.method === 'string' ? config.method.toLowerCase() : ''
    const status = err?.response?.status as number | undefined
    // 可重试条件：GET 请求 + （网络错误 / 5xx 服务端错误）+ 未超过重试次数
    const retriable =
      method === 'get' &&
      config !== undefined &&
      (status === undefined || status >= 500) &&
      (config._retry ?? 0) < MAX_RETRIES
    if (retriable) {
      config!._retry = (config!._retry ?? 0) + 1
      const delay = 500 * 2 ** ((config!._retry ?? 1) - 1) // 500ms, 1000ms
      await new Promise((r) => setTimeout(r, delay))
      return http(config!)
    }
    const detail = err?.response?.data?.detail
    const msg =
      typeof detail === 'string'
        ? detail
        : detail?.error?.message || detail?.message || err.message
    toastOnce(msg)
    return Promise.reject(err)
  },
)

/** 设置/清除管理 Token（保存到 localStorage） */
export function setAdminToken(token: string) {
  if (token) localStorage.setItem('workbuddy_admin_token', token)
  else localStorage.removeItem('workbuddy_admin_token')
}

export function setUserToken(token: string) {
  if (token) localStorage.setItem('workbuddy_user_token', token)
  else localStorage.removeItem('workbuddy_user_token')
}

export const api = {
  overview: () => http.get<unknown, Overview>('/admin/overview'),
  accounts: () => http.get<unknown, { accounts: AccountInfo[] }>('/admin/accounts'),
  setEnabled: (uid: string, enabled: boolean) =>
    http.post<unknown, { ok: boolean }>(`/admin/accounts/${uid}/${enabled ? 'enable' : 'disable'}`),
  setPriority: (uid: string, priority: number) =>
    http.post<unknown, { ok: boolean; priority: number }>(`/admin/accounts/${uid}/priority`, { priority }),
  // 设置/清除账号别名（传空串即清除，展示回退 nickname → uid 短码）
  setAccountAlias: (uid: string, alias: string) =>
    http.post<unknown, { ok: boolean; alias: string }>(`/admin/accounts/${uid}/alias`, { alias }),
  deleteAccount: (uid: string) => http.delete<unknown, { ok: boolean }>(`/admin/accounts/${uid}`),
  refreshCredits: () => http.post<unknown, { ok: boolean; accounts: AccountInfo[] }>('/admin/credits/refresh'),
  checkin: () => http.post<unknown, CheckinResponse>('/admin/checkin'),
  usageSummary: () => http.get<unknown, UsageSummary>('/admin/usage/summary'),
  usageTimeseries: (granularity = 'hour', points = 24, model?: string) =>
    http.get<unknown, { granularity: string; points: number; data: UsagePoint[] }>(
      `/admin/usage/timeseries?granularity=${granularity}&points=${points}${model ? `&model=${encodeURIComponent(model)}` : ''}`,
    ),
  // 服务端分页：page/page_size + 筛选条件，返回 records + total（总数驱动分页器）
  usageRecent: (page = 1, pageSize = 20, filters: { protocol?: string; model?: string; app_name?: string; status?: string; search?: string } = {}) => {
    const p = new URLSearchParams({ page: String(page), page_size: String(pageSize), light: '1' })
    if (filters.protocol) p.set('protocol', filters.protocol)
    if (filters.model) p.set('model', filters.model)
    // app_name 允许空字符串（筛"未记录应用"的旧记录），用 != null 判断而非真值
    if (filters.app_name != null) p.set('app_name', filters.app_name)
    if (filters.status) p.set('status', filters.status)
    // 内容关键字（在输入/输出/思考链三列里做字面匹配，服务端已转义通配符）
    if (filters.search) p.set('search', filters.search)
    return http.get<unknown, RecordsResponse>(`/admin/usage/recent?${p.toString()}`)
  },
  // 单条记录详情（含完整输入/输出/COT）：列表走 light 投影，点开详情才按需取大文本
  usageDetail: (id: number) => http.get<unknown, { record: UsageRecord }>(`/admin/usage/${id}`),
  usageFilters: () =>
    http.get<unknown, {
      protocols: string[]
      models: string[]
      apps: string[]          // 现存应用（与应用页一致）
      apps_history: string[]  // 已删除应用的历史记录名
      has_unnamed?: boolean   // 是否存在未记录应用的旧记录
      statuses: string[]
    }>('/admin/usage/filters'),
  models: () => http.get<unknown, { models: ModelInfo[]; source?: 'dynamic' | 'static' }>('/admin/models'),
  modelsRefresh: () =>
    http.post<unknown, { ok: boolean; models: ModelInfo[]; source?: 'dynamic' | 'static' }>('/admin/models/refresh'),
  // AA（Artificial Analysis）评测：按带命名空间的模型 id 返回智能/编码/数学指数
  benchmarks: () =>
    http.get<unknown, { configured: boolean; models: Record<string, AABenchmark> }>('/admin/models/benchmarks'),
  benchmarksRefresh: () =>
    http.post<unknown, { ok: boolean; configured: boolean }>('/admin/models/benchmarks/refresh'),

  // ---------- 多供应商（workbuddy / qoder / opencode）统一管理 ----------
  // 摘要列表：驱动左侧导航的供应商子项与概览页的供应商卡片
  getProviders: () => http.get<unknown, { providers: ProviderSummary[] }>('/admin/providers'),
  // 单供应商详情：摘要字段 + accounts[] + models[]
  getProvider: (name: string) =>
    http.get<unknown, ProviderDetail>(`/admin/providers/${encodeURIComponent(name)}`),
  // 供应商签到（qoder 返回逐账号结果；workbuddy 另含 skipped/accounts）
  providerCheckin: (name: string) =>
    http.post<unknown, Record<string, unknown>>(`/admin/providers/${encodeURIComponent(name)}/checkin`),
  // 供应商额度刷新（qoder 返回 quota 或 unsupported；workbuddy 返回 accounts）
  providerCredits: (name: string) =>
    http.post<unknown, Record<string, unknown>>(`/admin/providers/${encodeURIComponent(name)}/credits/refresh`),

  // ---------- 供应商逐账号动作（qoder 原生账号）：激活 / 重命名 / 删除 ----------
  // 设为激活账号（qoder 对应 workbuddy 的优先级：由哪个账号服务请求）
  providerActivateAccount: (name: string, id: string) =>
    http.post<unknown, { ok: boolean }>(
      `/admin/providers/${encodeURIComponent(name)}/accounts/${encodeURIComponent(id)}/activate`),
  // 重命名账号（body 传 name）
  providerRenameAccount: (name: string, id: string, name2: string) =>
    http.post<unknown, { ok: boolean }>(
      `/admin/providers/${encodeURIComponent(name)}/accounts/${encodeURIComponent(id)}/rename`, { name: name2 }),
  // 删除账号
  providerDeleteAccount: (name: string, id: string) =>
    http.delete<unknown, { ok: boolean }>(
      `/admin/providers/${encodeURIComponent(name)}/accounts/${encodeURIComponent(id)}`),

  // ---------- 供应商扫码登录（OAuth）：options / begin / poll ----------
  // 登录可选项（如 qoder 的区域 cn / global），驱动弹窗里的选择项
  providerOAuthOptions: (name: string) =>
    http.get<unknown, { options: OAuthOption[] }>(`/admin/providers/${encodeURIComponent(name)}/oauth/options`),
  // 发起扫码登录：返回 login_id 与可在浏览器打开的授权链接
  providerOAuthBegin: (name: string, opts: Record<string, unknown>) =>
    http.post<unknown, { login_id: string; login_url: string }>(`/admin/providers/${encodeURIComponent(name)}/oauth/begin`, opts),
  // 轮询登录状态：pending 等待授权，ready 成功（含 account），error 失败（含 message）
  providerOAuthPoll: (name: string, loginId: string) =>
    http.post<unknown, { status: 'pending' | 'ready' | 'error'; message?: string; account?: Record<string, unknown> }>(
      `/admin/providers/${encodeURIComponent(name)}/oauth/poll`, { login_id: loginId }),

  // 供应商配置编辑（opencode 密钥层级）：读取 / 保存并热重载
  providerGetConfig: (name: string) =>
    http.get<unknown, Record<string, unknown>>(`/admin/providers/${encodeURIComponent(name)}/config`),
  providerSaveConfig: (name: string, patch: Record<string, unknown>) =>
    http.post<unknown, { ok: boolean; config?: Record<string, unknown> }>(
      `/admin/providers/${encodeURIComponent(name)}/config`, patch),

  // 自动签到 / 额度刷新 设置
  getSettings: () => http.get<unknown, Settings>('/admin/settings'),
  saveSettings: (data: Partial<Settings>) => http.post<unknown, Settings & { ok: boolean }>('/admin/settings', data),

  // 上传 auth 文件（后端接收 multipart 的 file 字段）
  uploadAuth: (file: File) => {
    const form = new FormData()
    form.append('file', file)
    return http.post<unknown, { ok: boolean; account: AccountInfo }>('/admin/accounts/upload', form, {
      headers: { 'Content-Type': 'multipart/form-data' },
    })
  },

  // 扫码登录（对齐 work2api Go 后端：sites / begin / poll）
  oauthSites: () => http.get<unknown, { sites: string[] }>('/admin/oauth/sites'),
  oauthBegin: (site: string) =>
    http.post<unknown, { state: string; authUrl: string; site: string }>('/admin/oauth/begin', { site }),
  oauthPoll: (state: string, site: string) =>
    http.post<unknown, { status: 'pending' | 'ready'; uid?: string; added?: boolean; ok?: boolean }>(
      '/admin/oauth/poll', { state, site }),

  // 应用 API Key
  apps: () => http.get<unknown, { apps: AppInfo[] }>('/admin/apps'),
  createApp: (name: string, note = '', user_id?: number) =>
    http.post<unknown, { ok: boolean; id: number; app_id: number; name: string; key: string }>('/admin/apps', { name, note, user_id }),
  appKey: (id: number) =>
    http.get<unknown, { ok: boolean; key: string | null; unavailable?: boolean; message?: string }>(`/admin/apps/${id}/key`),
  toggleApp: (id: number) => http.post<unknown, { ok: boolean; enabled: boolean }>(`/admin/apps/${id}/toggle`),
  deleteApp: (id: number) => http.delete<unknown, { ok: boolean }>(`/admin/apps/${id}`),
}
