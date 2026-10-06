export interface PortalUser { id: number; username: string; role: string; status: string }
export interface PortalInvite { code: string; used_by: number | null; expires_at: number }
export interface PortalGroup {
  id: number; name: string; provider: string; enabled: boolean; allowed_models: string[] | string
  ready_accounts?: number; usable_models?: number; eligible_users?: number
  accounts?: string[]; grants?: number[]; account_uids?: string[]; user_ids?: number[]
}
export interface PortalAccount { uid: string; provider: string; site?: string; alias?: string; nickname?: string; enabled: boolean; contribution_user_id?: number; status?: string; owner_kind?: 'platform' | 'user'; sharing_mode?: string }
export interface PortalContribution { id: number; user_id: number; account_uid: string; status: string }
export interface PortalOverview {
  users: PortalUser[]; invites: PortalInvite[]; groups: PortalGroup[]; accounts?: PortalAccount[]
  contributions?: PortalContribution[]; registration_mode: string
  user_concurrency?: number; user_concurrency_max?: number; user_concurrency_overrides?: Record<string, number>
  default_group_id?: number; default_auto_grant?: boolean
}
