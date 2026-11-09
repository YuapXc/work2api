import type { UsageRecord, RecordsResponse } from '../src/types'
// 门户 API 面：全部走会话 cookie（w2a_portal_session），无需管理 token。
// 错误信息直接取后端 message（后端已为每个失败场景写了面向用户的中文文案）。
export class PortalAPIError extends Error {
  constructor(message: string, public status: number, public code = '', public retryAfter = 0) { super(message) }
  get retryable() { return this.status === 0 || this.status === 429 || this.status >= 500 }
}

async function call<T>(method: string, path: string, body?: unknown, signal?: AbortSignal): Promise<T> {
  const controller = new AbortController()
  const cancel = () => controller.abort()
  if (signal?.aborted) controller.abort()
  else signal?.addEventListener('abort', cancel, { once: true })
  const timer = setTimeout(() => controller.abort(), 40000)
  try {
    const res = await fetch(path, {
      method,
      headers: body !== undefined ? { 'Content-Type': 'application/json' } : undefined,
      body: body !== undefined ? JSON.stringify(body) : undefined,
      credentials: 'same-origin',
      signal: controller.signal,
    })
    const data = await res.json().catch(() => null)
    if (signal?.aborted) throw new DOMException('Aborted', 'AbortError')
    if (!res.ok) {
      const fallback = data === null && res.status >= 500
        ? `服务返回非 JSON 错误（${res.status}），请管理员检查反代、隧道与服务日志`
        : `请求失败（${res.status}）`
      const msg = (data as any)?.error?.message || (data as any)?.message || fallback
      const rawRetry = res.headers.get('Retry-After') || ''
      const retry = /^\d+$/.test(rawRetry) ? Number(rawRetry) : Math.max(0, (Date.parse(rawRetry) - Date.now()) / 1000)
      if (res.status === 401 && !path.includes('/auth/')) window.dispatchEvent(new Event('portal-session-expired'))
      throw new PortalAPIError(msg, res.status, data?.error?.code || '', Number.isFinite(retry) ? retry : 0)
    }
    if (data === null) throw new PortalAPIError('服务器响应异常，请稍后重试', 502, 'invalid_response')
    return data as T
  } catch (e) {
    if (e instanceof PortalAPIError) throw e
    throw new PortalAPIError(controller.signal.aborted ? '请求超时，请稍后重试' : '网络连接失败，请检查连接后重试', 0)
  } finally { clearTimeout(timer); signal?.removeEventListener('abort', cancel) }
}

export interface Me {
  user: { id: number; username: string; role: string; must_change_password: boolean }
  eligible: boolean
  eligible_accounts: number
  groups_enabled: number
  available_models: string[]
  own_models: string[]
  shared_models: string[]
  default_models: string[]
}

export interface PortalModel {
  id: string
  provider?: string
  description?: string
  name?: string
  context_length?: number
  max_output_tokens?: number
  vision?: boolean | null
  vision_verified?: boolean
  reasoning?: { supportsReasoning?: boolean } | boolean
  is_reasoning?: boolean
  supportsToolCall?: boolean
  tool_call?: boolean
  tool_response_multimodal?: boolean
  structured_output?: boolean
  max_input_tokens?: number
  price_factor?: number | null
  input_cost?: number
  output_cost?: number
  pricing_unit?: string
  pricing_tier?: string
  catalog_stale?: boolean
  own_account: boolean
  default_access: boolean
}

export interface KeyInfo {
  id: number
  name: string
  key_prefix: string
  enabled: boolean
  created_at: number
  requests: number
  tokens: number | null
  credits: number | null
  tokens_known?: boolean
  credits_known?: boolean
	channel_costs?: {provider:string;credits:number|null;partial?:boolean}[]
  allowed_models: string[] | null
  model_policy: "follow" | "fixed"
}

export interface ContributionInfo {
  id: number
  provider: string
  site: string
  status: string
  catalog_status: 'pending' | 'ready' | 'unavailable'
  account: string
  created_at: number
}

export const api = {
  usageRecords: (page: number, filters: { model?: string; protocol?: string; status?: string } = {}, signal?: AbortSignal) => {
    const params = new URLSearchParams({ page: String(page), page_size: '20' })
    for (const [key, value] of Object.entries(filters)) if (value) params.set(key, value)
    return call<RecordsResponse>('GET', '/portal/api/usage/records?' + params.toString(), undefined, signal)
  },
  usageRecord: (id: number) => call<{ record: UsageRecord }>('GET', `/portal/api/usage/records/${id}`),
  authState: (signal?: AbortSignal) => call<{ mode: 'invite' | 'open' | 'closed'; portal_enabled: boolean }>('GET', '/portal/api/auth/state', undefined, signal),
  register: (username: string, password: string, invite_code: string) =>
    call<{ ok: boolean; user: Me['user'] }>('POST', '/portal/api/auth/register', { username, password, invite_code }),
  login: (username: string, password: string) =>
    call<{ ok: boolean; user: Me['user']; redirect_to: string }>('POST', '/portal/api/auth/login', { username, password }),
  logout: () => call<{ ok: boolean }>('POST', '/portal/api/auth/logout'),
  changePassword: (old_password: string, new_password: string) =>
    call<{ ok: boolean }>('POST', '/portal/api/auth/password', { old_password, new_password }),
  me: (signal?: AbortSignal) => call<Me>('GET', '/portal/api/me', undefined, signal),
  models: (signal?: AbortSignal) => call<{ models: PortalModel[]; source: string }>('GET', '/portal/api/models', undefined, signal),
  key: (id: number) => call<{ ok: boolean; key?: string; message?: string }>('GET', `/portal/api/keys/${id}/key`),
  keys: (signal?: AbortSignal) => call<{ keys: KeyInfo[] }>('GET', '/portal/api/keys', undefined, signal),
  createKey: (name: string, allowed_models?: string[], model_policy: "follow" | "fixed" = "follow") =>
    call<{ id: number; key: string }>('POST', '/portal/api/keys', { name, allowed_models, model_policy }),
  toggleKey: (id: number) => call<{ ok: boolean; enabled: boolean }>('POST', `/portal/api/keys/${id}/toggle`),
  setKeyModels: (id: number, allowed_models: string[], model_policy: "follow" | "fixed" = "fixed") =>
    call<{ ok: boolean; allowed_models: string[]; model_policy: 'follow' | 'fixed' }>('POST', `/portal/api/keys/${id}/models`, { allowed_models, model_policy }),
  deleteKey: (id: number) => call<{ ok: boolean }>('DELETE', `/portal/api/keys/${id}`),
  contributions: (signal?: AbortSignal) => call<{ contributions: ContributionInfo[] }>('GET', '/portal/api/contributions', undefined, signal),
  contributionBegin: (site: string, accepted: boolean) =>
    call<{ task_id: string; auth_url: string; site: string; expires_at: number }>('POST', '/portal/api/contributions/begin', { site, accepted }),
	traeBegin: (product:string,accepted:boolean) => call<{task_id:string;auth_url:string;expires_at:number}>('POST','/portal/api/providers/trae/oauth/begin',{product,accepted}),
	traePoll: (task_id:string) => call<{status:string;message?:string;account_uid_masked?:string;models_verified?:boolean}>('POST','/portal/api/providers/trae/oauth/poll',{task_id}),
	traeComplete: (task_id:string,callback_url:string) => call('POST','/portal/api/providers/trae/oauth/complete',{task_id,callback_url}),
	traeCancel: (task_id:string) => call('POST','/portal/api/providers/trae/oauth/cancel',{task_id}),
  qoderBegin: (region:string,accepted:boolean)=>call<{task_id:string;auth_url:string;expires_at:number}>('POST','/portal/api/providers/qoder/oauth/begin',{region,accepted}),
  qoderPoll: (task_id:string)=>call<{status:string;message?:string;account_uid_masked?:string;models_verified?:boolean}>('POST','/portal/api/providers/qoder/oauth/poll',{task_id}),
  qoderCancel: (task_id:string)=>call('POST','/portal/api/providers/qoder/oauth/cancel',{task_id}),
  importContribution: (name:'qoder'|'opencode',token:string,options:{region?:string;tier?:string},accepted:boolean)=>call('POST',`/portal/api/providers/${name}/contribute`,{token,...options,accepted}),
  contributionPoll: (task_id: string) =>
    call<{ status: 'pending' | 'ready' | 'failed' | 'expired'; message?: string; account_uid_masked?: string; models_verified?: boolean }>(
      'POST',
      '/portal/api/contributions/poll',
      { task_id },
    ),
  contributionCancel: (task_id: string) => call<{ ok: boolean }>('POST', '/portal/api/contributions/cancel', { task_id }),
  refreshContributionModels: (id: number) => call<{ ok: boolean }>('POST', `/portal/api/contributions/${id}/models/refresh`, {}),
  revokeContribution: (id: number) => call<{ ok: boolean }>('POST', `/portal/api/contributions/${id}/revoke`, {}),
  shareContribution: (id: number) => call<{ ok: boolean }>('POST', `/portal/api/contributions/${id}/share`, { accepted: true }),
  usage: (signal?: AbortSignal) =>
    call<{
       today: { requests: number; input_tokens: number | null; output_tokens: number | null; recorded_input_tokens?:number; recorded_output_tokens?:number; tokens_known?:boolean; daily_limit_requests: number; daily_limit_input: number; daily_limit_output: number }
      keys: KeyInfo[]
    }>('GET', '/portal/api/usage', undefined, signal),
}

export function fmtTokens(n: number | null | undefined): string {
  if (n == null || !Number.isFinite(n)) return '未知'
  if (n >= 1e9) return (n / 1e9).toFixed(2) + 'B'
  if (n >= 1e6) return (n / 1e6).toFixed(2) + 'M'
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'K'
  return String(n)
}

export function fmtTime(ts: number | undefined): string {
  if (!ts) return '—'
  const ms = ts < 1e12 ? ts * 1000 : ts
  return new Date(ms).toLocaleString('zh-CN', { hour12: false })
}
