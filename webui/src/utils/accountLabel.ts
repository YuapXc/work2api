/**
 * 账号显示名工具 —— 多账号场景下统一"怎么称呼一个账号"。
 *
 * 回退链：alias（用户自定义）→ nickname（若可读）→ uid 短码。
 *
 * 为什么需要可读性判断：实测有的账号昵称含 DEL 控制符与 Unicode 私用区字符
 * （例如 '\x7f\x7f \ue423.'），直接渲染是一串方块，不如退回 uid 短码。
 *
 * 为什么清空别名是安全的：uid 是 UNIQUE 且永不为空，最终回退永远可用，
 * 因此用户清掉别名后一定能回到可辨识的初始显示。
 */
import type { AccountInfo } from '@/types'

/** 昵称是否适合展示（过滤控制字符 / 私用区 / 替换字符）。 */
export function isReadableName(text?: string | null): boolean {
  if (!text) return false
  for (const ch of text) {
    const o = ch.codePointAt(0) as number
    if (o < 0x20 || o === 0x7f) return false      // 控制字符（含 DEL）
    if (o >= 0xe000 && o <= 0xf8ff) return false  // Unicode 私用区
    if (o === 0xfffd) return false                // 替换字符
  }
  return true
}

/** 单个账号的显示名（不处理重名）。 */
export function accountLabel(acc?: Pick<AccountInfo, 'alias' | 'nickname' | 'uid'> | null): string {
  if (!acc) return '-'
  const alias = (acc.alias || '').trim()
  if (alias) return alias
  const nickname = (acc.nickname || '').trim()
  if (isReadableName(nickname)) return nickname
  return (acc.uid || '').slice(0, 8) || '-'
}

/**
 * 构建 uid → 显示名 的映射，并对**重名做消歧**。
 *
 * 别名允许重复（只是显示名），但重名会让标签失去区分度——这恰恰是本功能的目的。
 * 因此同一显示名出现 ≥2 次时，统一追加 uid 短码：`主力号 · 05d11bc8`。
 */
export function buildLabelMap(accounts: AccountInfo[] = []): Record<string, string> {
  const raw: Record<string, string> = {}
  for (const a of accounts) raw[a.uid] = accountLabel(a)

  const counts: Record<string, number> = {}
  for (const uid of Object.keys(raw)) {
    counts[raw[uid]] = (counts[raw[uid]] || 0) + 1
  }
  const out: Record<string, string> = {}
  for (const uid of Object.keys(raw)) {
    out[uid] = counts[raw[uid]] > 1 ? `${raw[uid]} · ${uid.slice(0, 8)}` : raw[uid]
  }
  return out
}

/** 从映射取显示名；账号已删除时回退为 `已移除 · uid短码`。 */
export function labelOf(map: Record<string, string>, uid?: string | null): string {
  if (!uid) return '—'
  return map[uid] || `已移除 · ${uid.slice(0, 8)}`
}

/**
 * 站点内部标识 → 中文。
 *
 * site 的值是 `domestic` / `international`（见后端 site_routing.py 的常量），
 * 直接渲染成标签对用户没有可读性——实测用户看到 "domestic ×2" 不知所云。
 * 后端的 by_account 已直接给出 site_label，此函数用于只拿到原始 site 的场景
 * （如 /admin/accounts 的账号列表）。
 */
export function siteLabel(site?: string | null): string {
  if (!site) return '未知'
  return site === 'domestic' ? '国内' : site === 'international' ? '国际' : site
}
