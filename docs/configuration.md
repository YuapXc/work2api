# 配置与调用容量

[返回项目首页](../README.md)

本地通常无需配置。公网 HTTPS 部署通常只需设置随机 `ADMIN_TOKEN`；监听地址、端口、数据目录有需要时再修改。配置模板见 [`.env.example`](../.env.example)，运行参数优先级为命令行 > 环境变量 > `.env`。

旧配置中的显式值会覆盖新的自动默认值；想使用自动策略时，删除对应覆盖项后重启。反代与 Tunnel 接线见 [部署说明](deployment.md)。


## 配置（flag > 环境变量 > .env）

| flag | 环境变量 | 默认 | 说明 |
|---|---|---|---|
| `-host` | `HOST` | `127.0.0.1` | 监听地址 |
| `-port` | `PORT` | `8787` | 监听端口 |
| `-data-dir` | `DATA_DIR` | `data` | 数据目录（DB / 凭据 / 密钥） |
| `-db` | `DB_PATH` | `<data-dir>/work2api.db` | SQLite 路径 |
| `-admin-token` | `ADMIN_TOKEN` | 空 | 管理台/`/admin/*` 令牌；**空 = 仅回环可用** |
| `-log-level` | `LOG_LEVEL` | `INFO` | 日志级别 |
| — | `ALLOW_EXTERNAL_HOST` | 自动 | 有 ADMIN_TOKEN 默认开启，否则关闭；显式值可覆盖（防 DNS rebinding） |
| — | `ADMIN_COOKIE_SECURE` | 自动 | 有令牌/允许外部 Host 时默认开启；真实回环 HTTP 自动兼容，显式值覆盖；直连 HTTPS 始终 Secure |
| — | `MAX_REQUEST_BYTES` | `268435456` | 请求体上限（256 MiB，必须大于 0）；登录固定 8 KiB |
| — | `REQUEST_BODY_READ_TIMEOUT_SECONDS` | `120` | 接收请求正文的期限（秒，必须大于 0）；不限制模型思考或响应时间 |
| — | `ADMIN_CONCURRENCY` / `HEAVY_ADMIN_CONCURRENCY` | `8` / `2` | 轻管理 / 重管理独立限额；同类刷新共享执行结果 |
| — | `QUERY_CONCURRENCY` / `BODY_READ_CONCURRENCY` | `4` / `4` | 模型列表及 Token 统计 / 请求体读取限额 |
| — | `REQUEST_BODY_BUDGET` | `0` | 不附加全局原始缓冲额度；正数可主动启用字节预算 |
| — | `MAX_JSON_ITEMS` / `MAX_JSON_DEPTH` | `0` / `0` | 默认不附加结构符号/深度门槛；正数主动启用额外保护，JSON合法性仍验证 |
| — | `MAX_RESPONSE_BYTES` | `134217728` | 非流式响应读取/聚合上限128MiB；SSE不累计封顶，单行解析40MiB |
| — | `DESENSITIZE` | 已停用 | 旧配置忽略；完整保留技能、工具和项目指令，不再摘要替换或插入零宽字符 |
| — | `CHANNEL_IDENTITY_COMPAT` | `true` | WorkBuddy 国内/国际账号遇到结构化 400/403 + 11128 且尚未输出时，优先兼容已验证的 Claude Code/SDK、Codex CLI 身份或 ZCode 分支模板；保留技能、工具、权限和任务内容。明确首条 system 校验走既有协议规范化；关闭后同时关闭归因精简 |
| — | `CHANNEL_METADATA_COMPAT` | `true` | 身份兼容仍被拒绝时，再移除已验证的 CC billing 归因行，并把同一批客户端固定模板从整条对话（历史消息、reasoning、工具参数与工具结果）里一并改写，因为这些位置同样出站；除这些已知模板外不改动内容。两级同账号兼容重试合计最多 2 次，连同授权范围内换号共最多 5 次；已启用的兼容随当前请求换号保留。成功方式仅在当前会话、账号及系统/工具模板一致时复用；重启失效 |
| — | `CHECKIN_HOURS` | `9,21` | 每日自动签到小时 |
| — | `CREDIT_REFRESH_MIN` | `30` | 额度刷新周期（分钟） |
| — | `MODEL_REFRESH_HOUR` | `6` | 每日模型刷新小时 |
| — | `KEEPALIVE_HOUR` | `22` | 每日 token 保活小时 |
| — | `USAGE_RETENTION_DAYS` | `90` | 使用记录保留天数 |
| — | `USAGE_MAX_ROWS` | `100000` | 使用记录总行数上限（0=不限），兜住高频调用把库撑爆 |
| — | `USAGE_CHECK_INTERVAL_MIN` | `30` | 行数上限检查周期（分钟，MIN/MAX id 廉价预检） |
| — | `USAGE_CONTENT_MAX_BYTES` | `32768` | 单条对话内容截断上限（0=不限），主要截断 base64 图片等大 payload |
| — | `LOG_TO_FILE` | `1` | 日志是否写文件（`0` 关闭） |
| — | `OPENCODE_CONFIG` | `<data-dir>/opencode/opencode.json` | OpenCode 配置文件路径（显式设置优先） |
| — | `QODER2API_HOME` | `~/.qoder2api` | Qoder 数据目录 |
| — | `PORTAL_ENABLED` | `true` | 用户门户总开关；关闭后注册/贡献/共享调用停用，登录与查看保留 |
| — | `PORTAL_REGISTRATION_MODE` | `invite` | 注册方式（兼容旧名 `PORTAL_INVITE_MODE`） |
| — | `PORTAL_KEY_MAX_PER_USER` / `PORTAL_MAX_TASKS_PER_USER` | `2` / `3` | 每用户 Key 数 / 同时扫码任务数 |

## 模型调用与资源保护

模型调用不设网关本地的执行并发、每用户/Key/账号名额、等待队列、每日次数/Token额度或每账号请求间隔。概览只统计正在执行的数量，账号选择仍按原有成本、会话粘性、权限及真实上游状态进行；不因本地账号忙碌强制换号。管理员用户并发设置已移除。

旧 `MAX_CONCURRENT_REQUESTS`、`MODEL_QUEUE_*`、`MODEL_KEY_*`、`PORTAL_USER_CONCURRENCY`、`PORTAL_SHARED_CONCURRENCY`、`PORTAL_*_QUEUE_SIZE`、`WORKBUDDY_ACCOUNT_CONCURRENCY`、`PORTAL_DAILY_*`、`RATELIMIT*` 以及模型响应预留/内存闸配置不再限制模型调用，无需手工删除旧环境变量才能生效。

请求正文默认256MiB，非流式响应默认128MiB；SSE只限制单行40MiB，不限制累计总量。JSON额外结构门槛和总请求体缓冲额度默认0（关闭），可由管理员按实际资源主动启用。模型请求不再受 `BODY_READ_CONCURRENCY` 的读取名额/等待数量限制；请求体读取超时只约束接收正文，不限制模型思考或 SSE 时长。管理、登录、授权及目录查询的独立保护保留。

Key授权、用户归属、模型真实上下文/输出能力及上游的鉴权、额度、429等仍生效；上游明确限流时保留退避和冷却，取消本地限制不代表上游无限并发。重试次数仍有界，响应开始后不重放请求。

### sub2api对齐依据

2026-10-12核实：sub2api最新 `backend/internal/config/config.go` 的 `DefaultUpstreamResponseReadMaxBytes` 与Viper默认为128MiB，示例配置仍写8MiB；本项目按源码实际默认对齐。其请求正文为256MiB，SSE单行为40MiB。没有复制其各渠道超时、用户并发、H2C或商业计费策略；本项目维持此前确认的无本地执行并发/每日额度策略。管理登录的小请求保护不变。


## 调用日志正文保留

默认 `USAGE_CONTENT_RETENTION_DAYS=7`、`USAGE_CONTENT_BUDGET_BYTES=536870912`（512MiB）。0分别关闭对应清理规则。预算是全部日志 input/output/reasoning 字段的UTF-8字节总和，不是数据库文件硬上限，也不是模型调用额度。超出周期或预算时，从最旧正文开始分批清除，保留原行的Tokens、消费、状态和性能；管理员详情提示正文已清理，正文搜索不再命中这些片段。现有完整备份里的历史正文不会因此被删去。

维护启动后会执行一次，随后沿用 `USAGE_CHECK_INTERVAL_MIN`（默认30分钟）；每批最多64行，单轮最多约1秒，未完成下次继续。行数和日期保留也使用短事务，清理失败记录错误并重试，不把数据库失败当作调用失败。`USAGE_CONTENT_MAX_BYTES`仍只控制保存片段，不改变转发的请求和回答。配置在应用更新后生效，更新前建议保留完整数据备份。数据库schema22→23只新增正文数字节计数及索引，新记录直接计数、旧记录后台分批补计，不在每轮维护中反复扫描全部正文。此预算是定期维护目标，高峰期间可能暂时超出，不在调用入口拒绝模型请求。

日志维护使用独立串行后台任务，不等待渠道网络预热或额度刷新；仍参与完整备份协调，停机时取消并等待退出。管理员概览可展开资源与日志维护指标，用于观察存储和写入开销，不新增模型并发、额度或节流限制。
