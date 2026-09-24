// 后端 /admin/* API 返回的 TS 类型定义

export interface AccountInfo {
  uid: string
  /** 上游供应商：workbuddy / qoder */
  provider?: 'workbuddy' | 'qoder'
  /** Qoder 登录方式；WorkBuddy 固定为 workbuddy。 */
  auth_mode?: 'oauth' | 'pat' | 'workbuddy'
  enabled: boolean
  /** 用户自定义别名（可重复）；为空时回退 nickname → uid 短码 */
  alias?: string
  /** 上游账号昵称（可能含乱码，仅作回退） */
  nickname?: string
  /** 禁用原因（手动停用/保活连续失败…），启用时为空 */
  disabled_reason?: string
  healthy: boolean
  /** 站点/产品档位：cn-cli / cn-work / intl-cli / intl-work */
  profile?: string
  /** 站点显示名（国内 / 国际） */
  site?: string
  /** 选号权重（加权随机用） */
  weight?: number
  cooldown_until: number
  failure_count: number
  credits_remaining: number | null
  credits_total: number | null
  /** 积分最早到期时间戳（秒），无则 null */
  credits_expire_at?: number | null
  /** 选号优先级（越大权重越高，0=默认） */
  priority?: number
  checkin_today?: boolean
  /** 账号来源：project=项目 auths/（上传/扫码），local=本机 CodeBuddy 目录 */
  source?: 'project' | 'local' | 'qoder' | 'unknown'
}

export interface Settings {
  checkin_hours: string
  credit_refresh_min: string
  model_refresh_hour: string
  /** 模型缓存 TTL（分钟）：推理端点 /v1/models 惰性刷新的缓存新鲜度上限 */
  model_ttl_min?: string
  aa_refresh_hour: string
  keepalive_hour: string
  /** token 保活开关：'1' 开启（默认），'0' 关闭 */
  keepalive_enabled?: string
  aa_api_key?: string
  aa_api_key_masked?: string
  aa_enabled?: boolean
  /** 清除 AA key 的标记 */
  clear_aa_api_key?: boolean
  /** 模型别名映射，每行 `别名=真实模型` */
  model_aliases?: string
  /** 积分预警开关：'1' 开启，'0' 关闭（默认关闭） */
  alert_enabled?: string
  /** 预警推送地址（企微/飞书/Bark，按 URL 自动识别平台） */
  alert_webhook?: string
  /** 总余额占总容量低于该百分比时告警 */
  alert_threshold_percent?: string
  /** 有余额的账号在这么多天内到期时告警 */
  alert_expiry_days?: string
}

/** /admin/overview 的预警条目（只读展示，推送由后端定时任务负责） */
export interface OverviewAlert {
  level: 'warning' | 'info'
  kind: 'balance' | 'expiry'
  uid?: string
  message: string
}

export interface ModelReasoning {
  supportsReasoning?: boolean
  onlyReasoning?: boolean
  canDisableThinking?: boolean
  defaultEffort?: string
  supportedEfforts?: string[]
}

export interface AABenchmark {
  name?: string
  creator?: string
  intelligence_index?: number
  coding_index?: number
  /** AA 已不再提供 agentic 指标，第三指标为数学 */
  math_index?: number
  source?: string
  aa_url?: string
}

export interface ModelInfo {
  id: string
  /** 模型所属供应商。 */
  provider?: 'workbuddy' | 'qoder'
  name?: string
  context_length?: number
  max_output_tokens?: number
  reasoning?: ModelReasoning
  /** 模态：multimodal=多模态, text=纯文本 */
  modality?: 'multimodal' | 'text'
  supportsToolCall?: boolean
  supportsImages?: boolean
  /** 成本系数（倍率） */
  credits?: number
  temperature?: number
  top_p?: number
  vendor?: string
  description?: string
  /** AA 评测数据（可选） */
  benchmark?: AABenchmark
  /** 支持该模型的档位集合（拉目录时按账号收集） */
  profiles?: string[]
  /** 逐账号模型目录确认的可用账号 UID，仅管理端使用。 */
  account_uids?: string[]
  /** 能调用该模型的账号（后端已叠加实时状态），供账号标签展示 */
  accounts?: ModelAccount[]
}

/** 模型条目里的"可调用账号"（含实时状态） */
export interface ModelAccount {
  uid: string
  /** 显示名：别名 → 昵称(若可读) → uid 短码 */
  label: string
  profile?: string
  /** 站点内部标识（domestic/international），一般用 site_label 展示 */
  site?: string
  /** 站点中文名（国内/国际），由后端下发，前端不再自己维护映射 */
  site_label?: string | null
  enabled?: boolean
  healthy?: boolean
  cooldown_until?: number
  /** 仅当前模型达到每日上限；账号的其它模型仍可使用 */
  model_cooldown?: boolean
}

export interface ProtocolStat {
  protocol: string
  count: number
  tokens: number
}

export interface ModelStat {
  model: string
  count: number
  tokens: number
}

/** 按账号聚合的调用统计 */
export interface AccountStat {
  account_uid: string
  /** 显示名（后端已解析别名，账号删除时回退 uid 短码） */
  label?: string
  site?: string | null
  /** 站点中文名（国内/国际） */
  site_label?: string | null
  profile?: string | null
  enabled?: boolean | null
  healthy?: boolean | null
  /** 账号已被删除（历史记录仍保留） */
  removed?: boolean
  count: number
  tokens: number
  credits: number
  ok_count?: number
  unknown_count?: number
}

export interface UsageSummary {
  total_requests: number
  total_tokens: number
  today_requests: number
  today_tokens: number
  by_protocol: ProtocolStat[]
  by_model: ModelStat[]
  by_app?: { app: string; count: number; tokens: number }[]
  /** 按账号聚合（多账号轮询下的核心视角） */
  by_account?: AccountStat[]
}

export interface UsagePoint {
  bucket_ts: number
  bucket: string
  count: number
  tokens: number
}

export interface UsageRecord {
  id: number
  ts: number
  protocol: string
  model: string
  account_uid: string
  input_tokens: number
  output_tokens: number
  total_tokens: number
  latency_ms: number
  status: string
  error?: string | null
  input_content?: string | null
  output_content?: string | null
  reasoning_content?: string | null
  reasoning_effort?: string | null
  credits?: number | null
  /** 只有上游明确提供积分时才为 true；旧服务未提供此字段。 */
  credit_known?: boolean
  app_name?: string | null
}

export interface Overview {
  accounts: AccountInfo[]
  /** 预警提示（后端按当前额度状态实时计算，仅用于展示） */
  alerts?: OverviewAlert[]
  models: string[]
  usage: UsageSummary
  recent: UsageRecord[]
  prediction?: {
    remaining_credits: number
    tokens_per_credit: number | null
    predicted_tokens: number | null
    credits_used: number
    tokens_used: number
    models: {
      model: string
      credits: number
      tokens: number
      tokens_per_credit: number
      weight: number
      predicted_tokens: number
    }[]
  }
}

export interface CheckinResult {
  uid: string
  ok: boolean
  already: boolean
  message?: string
}

export interface CheckinResponse {
  ok: boolean
  results: CheckinResult[]
  skipped?: string[]
}

export interface RecordsResponse {
  records: UsageRecord[]
  /** 符合筛选条件的总数（服务端分页） */
  total: number
  page?: number
  page_size?: number
}

/** 供应商摘要（GET /admin/providers 的每一项） */
export interface ProviderSummary {
  name: string
  display_name: string
  ready: boolean
  default?: boolean
  /** 能力标记，驱动动作按钮/列的显隐：accounts/models/checkin/credits/oauth/upload/config/local_detect */
  capabilities: string[]
  /** 概览卡片用的摘要数字（各供应商形状不同） */
  status: Record<string, unknown>
  /** 未就绪时的配置指引 */
  notes?: string
}

/** 供应商详情（GET /admin/providers/{name}）：摘要字段 + 账号/模型行 */
export interface ProviderDetail extends ProviderSummary {
  accounts: Record<string, unknown>[]
  models: Record<string, unknown>[]
}

/** 扫码登录可选项（GET /admin/providers/{name}/oauth/options 的每一项） */
export interface OAuthOption {
  key: string
  label: string
  default?: string
  values: { value: string; label: string }[]
}

/** 应用（API Key）信息 */
export interface AppInfo {
  id: number
  name: string
  key_prefix: string
  note: string
  enabled: boolean
  created_at: number
  requests: number
  tokens: number
  credits: number
  user_id?: number | null
}
