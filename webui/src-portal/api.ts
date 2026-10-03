// 门户 API 面：全部走会话 cookie（w2a_portal_session），无需管理 token。
// 错误信息直接取后端 message（后端已为每个失败场景写了面向用户的中文文案）。
async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: body !== undefined ? { 'Content-Type': 'application/json' } : undefined,
    body: body !== undefined ? JSON.stringify(body) : undefined,
    credentials: 'same-origin',
  })
  const data = await res.json().catch(() => ({}))
  if (!res.ok) {
    const msg = (data as any)?.error?.message || (data as any)?.message || `请求失败（${res.status}）`
    throw new Error(msg)
  }
  return data as T
}

export interface Me {
  user: { id: number; username: string; role: string }
  eligible: boolean
  eligible_accounts: number
  groups_enabled: number
  available_models: string[]
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
  allowed_models: string[] | null
}

export interface ContributionInfo {
  id: number
  provider: string
  site: string
  status: string
  account: string
  created_at: number
}

export const api = {
  authState: () => call<{ mode: 'invite' | 'open' | 'closed'; portal_enabled: boolean }>('GET', '/portal/api/auth/state'),
  register: (username: string, password: string, invite_code: string) =>
    call<{ ok: boolean; user: Me['user'] }>('POST', '/portal/api/auth/register', { username, password, invite_code }),
  login: (username: string, password: string) =>
    call<{ ok: boolean; user: Me['user'] }>('POST', '/portal/api/auth/login', { username, password }),
  logout: () => call<{ ok: boolean }>('POST', '/portal/api/auth/logout'),
  changePassword: (old_password: string, new_password: string) =>
    call<{ ok: boolean }>('POST', '/portal/api/auth/password', { old_password, new_password }),
  me: () => call<Me>('GET', '/portal/api/me'),
  keys: () => call<{ keys: KeyInfo[] }>('GET', '/portal/api/keys'),
  createKey: (name: string, allowed_models?: string[]) =>
    call<{ id: number; key: string }>('POST', '/portal/api/keys', { name, allowed_models }),
  toggleKey: (id: number) => call<{ ok: boolean; enabled: boolean }>('POST', `/portal/api/keys/${id}/toggle`),
  setKeyModels: (id: number, allowed_models: string[]) =>
    call<{ ok: boolean }>('POST', `/portal/api/keys/${id}/models`, { allowed_models }),
  deleteKey: (id: number) => call<{ ok: boolean }>('DELETE', `/portal/api/keys/${id}`),
  contributions: () => call<{ contributions: ContributionInfo[] }>('GET', '/portal/api/contributions'),
  contributionBegin: (site: string, accepted: boolean) =>
    call<{ task_id: string; auth_url: string; site: string; expires_at: number }>('POST', '/portal/api/contributions/begin', { site, accepted }),
  contributionPoll: (task_id: string) =>
    call<{ status: 'pending' | 'ready' | 'failed' | 'expired'; message?: string; account_uid_masked?: string }>(
      'POST',
      '/portal/api/contributions/poll',
      { task_id },
    ),
  contributionCancel: (task_id: string) => call<{ ok: boolean }>('POST', '/portal/api/contributions/cancel', { task_id }),
  revokeContribution: (id: number) => call<{ ok: boolean }>('POST', `/portal/api/contributions/${id}/revoke`, {}),
  usage: () =>
    call<{
       today: { requests: number; input_tokens: number | null; output_tokens: number | null; daily_limit_requests: number; daily_limit_input: number; daily_limit_output: number }
      keys: KeyInfo[]
    }>('GET', '/portal/api/usage'),
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
