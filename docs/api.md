# API 与客户端接入

[返回项目首页](../README.md)

## API 端点

- 推理（需 `Authorization: Bearer sk-...`）：`POST /v1/chat/completions`、`POST /v1/messages`、`POST /v1/responses`、`GET /v1/models`
- 管理（按初始化状态验证管理员会话或 `X-Admin-Token`；只有未初始化、未配置 Token 的真实回环模式允许对应本机入口）：`GET /admin/providers`、`GET/POST /admin/providers/{name}/...`（账号/模型/签到/额度/OAuth/账号管理）、`/admin/apps`、`/admin/apps/{id}/models`（密钥模型白名单）、`/admin/models/catalog`（跨供应商聚合模型目录）、`/admin/checkin/history`（跨供应商签到日历）、`/admin/usage/*`、`/admin/settings`、`/admin/login` + `/admin/logout`（WebUI 会话）
- 健康检查：`GET /health`（只报计数，不含账号 UID）；`GET /admin/health`（管理台健康详情）

## 管理资源指标

`GET /admin/resources` 使用现有管理员鉴权；管理概览同时返回 `resource_metrics`，页面默认折叠展示。包含 Go 内存、堆对象、GC 次数、Goroutines、请求体内存、数据库/WAL 文件大小，以及日志写入平均耗时、失败次数和维护状态。采样缓存10秒，累计计数在服务重启后重置；文件不可读取时大小为 `null`，不伪造为0。

Go 内存不是进程RSS或整机内存。日志写入耗时包含锁等待与写入准备；不代表所有SQL查询耗时。这些指标不包含正文、凭据、账号或文件路径，也不参与调用准入和限额判断。

## 概览续航估算

管理概览的“预计可服务 Tokens”保留一个总数，“续航明细”展示各渠道及账号计费池。总数只汇总已有可靠消费依据的付费余额折算；未覆盖的池、余额未知和免费调用另行说明，不代表所有模型都能共享这些余额。

- 各账号余额只计一次。WorkBuddy 按产品/区域配置隔离，Qoder 按区域与套餐隔离；未知套餐不借用其他账号的消费率。不同渠道的积分或额度不直接相加。
- 优先使用本账号近 7 天的成功调用，样本不足时依次参考本账号近 30 天、同计费池近 7 天和近 30 天。只有上游已报告真实 Tokens 和正数消费的记录参与校准，至少 5 次、1,000 Tokens 才生成预测。
- 每单位余额对应 Tokens = 样本 Tokens 总和 / 样本消费总和；不同成本的模型按实际使用比例共同参与。预测随模型组合、输入输出比例、缓存命中和价格变化，不是特定模型的额度保证。
- 旧版记录缺少上游 Tokens 确认标记时，若消费为正、输入输出均非负且总数一致，也可用于参考估算；明细标明旧版样本数量。明确标为用量未知、消费缺失或数值不一致的记录不会因此纳入。
- 上游目录明确免费的模型独立展示，其免费 Tokens 不放大付费预测。消费为零但未确认免费、消费未知的记录也不用于校准。
- 禁用、未授权、不可用的账号不计入；过期资源包及 Qoder 适用范围未确认的专属额度剔除。暂时冷却的余额保留并提示，过期缓存余额标注待刷新。
- Trae、OpenCode 目前缺少可信的逐次消费或余额依据，不强行换算；有可靠字段后再扩展渠道投影。该估算只影响展示，不改变调用限制、选号、粘性或模型授权。

Trae 余额和资源包在启动后台预热时自动读取一次，无需手动刷新，也不依赖定时刷新开关。开启“自动刷新 Trae 额度”后每 30 分钟更新；启动查询只针对已启用账号、每账号最多等待 30 秒，失败保持未知。余额缓存目前只在内存，读取完成前显示未知，不把旧余额冒充已刷新数据。

账号表格对仍有余额的资源包显示最早到期日期，7 天内黄色提醒、不足 1 天或已到期红色提醒；已用完的包不参与最早到期。模型页的 WorkBuddy、Qoder 均以“成本系数”展示上游相对值，仅在对应渠道计费规则内比较。未确认的成本显示 `-`；Trae 目录的 `model_entitlement_usage_cost` 仍保留在接口元数据中，但不能作为已确认的计费倍率，不在模型卡展示或用于成本选号和续航换算。

## 接入客户端（三协议 base URL + 供应商前缀路由）

work2api 同时开三种协议，客户端按实际支持的协议接入；具体能力边界见 [功能与协议能力](features.md)。base URL 按客户端习惯填：

| 协议 | base URL | 客户端自动拼的路径 | 谁用 |
|---|---|---|---|
| Anthropic Messages | `http://127.0.0.1:8787` | `/v1/messages` | Claude Code、Claude 系客户端 |
| OpenAI Chat | `http://127.0.0.1:8787/v1` | `/v1/chat/completions` | 多数 OpenAI 兼容工具（Cherry Studio、LobeChat 等） |
| OpenAI Responses | `http://127.0.0.1:8787/v1` | `/v1/responses` | Codex CLI 等用 Responses API 的客户端 |

三个端点**共用同一套账号池、同一个 `sk-...`、同一套前缀路由**，协议之间由 work2api 自动互转。

Responses 的 `custom` 自由文本工具（如 Codex 的 `apply_patch`）会为函数调用上游包装为一个字符串 `input` 参数，返回时恢复为 `custom_tool_call`，保留调用 ID 与工具结果往返。流式工具输入在完整 JSON 包装校验后交给客户端，正文和思考仍实时输出。若工具声明了 grammar，定义会作为工具约束提示保留；函数调用渠道不能提供上游原生的 grammar 受限解码，最终工具格式仍由客户端校验。上游的审核拒绝与模型调用失败不会因此绕过或变成成功。

Responses 的 `namespace` 工具可包含 function/custom 子工具。网关以请求级映射展平工具名，保留原命名空间和工具描述、历史调用 ID、工具结果与指定子工具的 `tool_choice`；流式和非流式返回恢复原 `namespace`/`name`。重复子工具或名称冲突明确报错。指定整个 namespace 时，仅向上游提供该组工具并使用自动选择，不强迫模型额外调用工具。内建 `tool_search` 和服务端执行工具不属于这项兼容能力。

WorkBuddy 已产生结构化工具调用时，会纠正上游错误的 `stop` 结束原因；缺失原因仅在收到 `[DONE]` 后补齐。`length` 与 `content_filter` 保留，Messages 分别映射 `max_tokens` 与 `refusal`，不会把截断或审核拒绝改成正常工具完成。

同协议流式透传在收到真正终态后及时结束，不等待上游连接 EOF；终态之前的用量及字节保留，不把普通 `finish_reason` 当成最终结束。OpenCode 在提交下游响应前暂存角色/用量等前置事件，首个有效思考、正文、工具事件或正常终态到达后立即放行。输出前可分类的上游故障沿用现有重试预算；未知业务拒绝直接返回，输出后故障不重放。未新增短首包超时，客户端取消会关闭正在等待的上游读取。

WorkBuddy 在单个请求的同步流处理链中复用已校验的片段解析结果，普通单行 JSON 不重复折叠编码；多行 SSE 仍规范化，工具和结束帧仍完整校验，实际输出不会攒批。模型路由按缓存索引读取单条目录，账号权限仍在派发时复核。Trae 凭据刷新按具体授权身份合并并发请求，其他有效账号无需等待其网络刷新；写回仍经过归属核实、备份协调和原子保存，失败的保存会在后续调用重试。这些优化减少网关自身开销，不改变思考强度、模型真实生成速度或原选号规则。

WorkBuddy 会修复客户端上下文压缩后损坏的工具历史：清理缺少有效调用 ID、函数名或不属于函数调用的历史项，保留同回合的正文与合法调用；工具结果仅按准确的调用 ID 配对，不会强行绑定到其他工具。无法匹配的结果不发送上游，缺失的有效调用结果继续使用明确的缺失提示，不伪造执行成功。正常工具历史保持原有内容。

**多供应商靠模型名前缀分流**——切供应商只换模型名，不换地址/Key：

| 供应商 | 模型名写法 |
|---|---|
| WorkBuddy（默认） | `workbuddy/<模型>`，兼容 `<模型>`（无前缀） |
| Qoder | `qoder/<模型>` |
| OpenCode | `opencode/<模型>` |
| Trae | `trae/<模型>` |

> 以 Claude Code + [cc-switch](https://github.com/farion1231/cc-switch) 这类切换器为例：按需建立 profile，**base URL 和 Key 完全相同**，只有模型名前缀不同即可切换渠道。注意 Claude Code 会同时用主模型（`ANTHROPIC_MODEL`）和小快模型（`ANTHROPIC_SMALL_FAST_MODEL`）——两个都要填成 work2api 能路由的模型（同一家、同前缀），否则后台调用会因模型不存在报错。

## 模型授权与默认模型

WorkBuddy 国内、国际账号遇到结构化 HTTP 400/403 + `11128` 时，可在输出开始前有限重试，不依赖特定的英文错误文案。先在当前账号尝试最小兼容，再按原选号策略换授权账号；同一请求换号保留已启用的兼容，两级同账号兼容重试最多 2 次、全部上游尝试共最多 5 次。仅替换已验证的 Claude Code/SDK 或旧版 Codex CLI 固定身份开头，以及 ZCode 等客户端的固定分支说明行。分支说明仅将 `Main branch (you will usually use this for PRs):` 改为 `Default branch (you will usually use this for PRs):`，实际分支值、其余指令、skills、工具和权限说明均保留。首次请求保留原文；成功的兼容方式会按账号和指令指纹在当前会话复用。明确“首条消息不是 system”走既有协议规范化，不触发身份替换；没有结构化 11128 的内容拒绝保持原处理，未知客户端模板不被整段裁剪。此兼容不能保证所有上游拒绝都恢复调用；可用 `CHANNEL_IDENTITY_COMPAT=false` 关闭。

- 第二级兼容（`CHANNEL_METADATA_COMPAT`）在移除归因行之外，还会把同一批固定模板从历史消息、reasoning、工具参数与工具结果里一并改写：这些位置同样会出站，而只清洗开头 system 会漏掉它们。除这些已知模板外不改动任何内容。

- **密钥模型白名单**：`/v1/models` 只展示该密钥获准调用的模型及别名。省略 `model`（含 `null`、空字符串）时，不限模型的密钥默认使用 `auto`，仅允许一个模型的密钥默认使用该模型，允许多个模型的密钥需明确指定，否则返回 400。显式指定 `auto` 仍需获得授权；它会交给上游选择，并不表示在密钥白名单内自动选择。

`/v1/messages/count_tokens` 当前提供文本及工具内容的粗估（含 system、工具定义和结果），不使用供应商 tokenizer；图片和音频未计入，不能作为精确计费或上下文容量依据。


## 用户模型权限

用户 Key 的 `model_policy` 为 `follow`（跟随当前权限）或 `fixed`（固定白名单）。旧用户 Key 保持 `fixed`；新建默认 `follow`。固定空列表拒绝所有模型，管理员私人 Key 的原有空列表语义不变。

门户登录响应返回服务端决定的 `redirect_to`，管理员为 `/admin-ui/`，普通用户为 `/portal/`。两种会话 Cookie 仍隔离。

管理接口 `POST /admin/portal/model-policy` 接受 `mode`（auto/manual）、`include`、`exclude` 字符串数组；`POST /admin/portal/users/{id}/models` 接受 `models` 与 `remove_scoped` 数组，后者用于明确移除历史账号范围授权。默认权限只决定模型可用范围，不保证实际命中免费账号。

Qoder 贡献授权使用 `/portal/api/providers/qoder/oauth/{begin,poll,cancel}`；个人凭据使用 `POST /portal/api/providers/{qoder,opencode}/contribute`，包含 `accepted:true`、`token` 及 Qoder 的 `region` 或 OpenCode 的 `tier`。用户不能传入上游地址、配置文件路径或指定其他人归属。已有 WorkBuddy / Trae 授权路径保留。


## 邀请码管理接口

`POST /admin/portal/invites` 可传 `max_uses`（默认1，0表示明确不限）与 `expires_at`（Unix秒，0表示永不过期）。省略有效期时默认7天；兼容旧 `valid_days` 字段，但不能同时传两种时间参数。非法参数返回400，不再静默回退成7天。

`POST /admin/portal/invites/{code}` 保存 `max_uses`、`expires_at`、`enabled`（布尔）和 `revision`。版本冲突或次数上限低于已使用次数返回409；使用次数不会因编辑或恢复而重置。原有DELETE接口现在停用邀请码并保留使用记录。

`GET /admin/portal/invites/{code}/history?offset=0` 返回 `records`（每页最多50条，含注册用户名、用户ID和时间）及 `next_offset`，-1表示没有下一页。列表新增 `max_uses`、`used_count`、`enabled`、`revision`；`used_by/used_at`只为旧客户端保留首次使用信息，不能再用于判断邀请码是否可用。所有接口仍要求管理员身份及原有同源保护。

### 门户本人 Key 与日志

以下接口使用门户登录会话，需启用门户并完成登录：

- `GET /portal/api/keys/{id}/key`：按需恢复本人 Key；不存在或不属于本人均返回 404，响应禁用缓存。
- `GET /portal/api/usage/records?page=1&page_size=20`：本人调用日志，支持 `model`（请求或实际模型）、`protocol`、`status` 精确筛选；每页最多 100 条。
- `GET /portal/api/usage/records/{id}`：本人调用详情及已记录的关联尝试。

管理台 `GET /admin/usage/recent?group=request` 使用相同请求归并；不传该参数保留原始用量行接口。计费与额度统计仍使用原始记录，归并不会合并不同渠道积分或覆盖未知值。


### 调用日志用量细分

管理与门户日志的 `diagnostics.token_details` 可包含 `reasoning_tokens` 和 `cache_creation_tokens`。仅保存上游实际报告且合法的计数，缺失表示未上报或历史未保存；数字 0 表示明确上报为零。输入总量含缓存，输出总量含思考及工具调用，细分不能重复累加。非思考输出仅在输出总量及思考计数均已知时计算，不代表纯正文。门户继续按用户隔离，既有正文及私有账号信息保护保持有效。细分保存在现有诊断 JSON 内，不新增数据库版本，也不回填旧记录。

列表采用紧凑数值布局、结果徽标与可选列；管理与门户分别保存列偏好，只有列名写入浏览器存储。首响应仍表示网关首字节而非首 Token，不将其用作生成速度依据。`length` 表示生成预算或上下文边界，详情保留客户端预算、实际发送预算和结束原因以供排查。


日志平均输出速度采用 `output_tokens × 1000 / duration_ms`，优先使用 `diagnostics.performance.total_ms`，历史记录使用已有 `latency_ms`（历史耗时口径可能不同）。这是包含思考、等待及重试的平均值，不是纯解码速度，也不是 Token 逐个到达的采样记录。仅已知正输出、有效正耗时且成功或未完成的记录展示，失败或未知记录显示「—」。思考 Tokens 是输出总量的子集，不再加入分子。


旧日志展示兼容：`tokens_known` 缺失/NULL，但已存输入、输出均为非负整数、总量为正且等于两者之和时，直接展示原计数。页面不显示“历史计数”等版本说明。这是显示兼容，不修改数据库确认标记或计费统计；显式 `tokens_known=0/false` 仍为未知，缺失思考细分不从正文估算。历史平均速度按旧记录耗时计算，仅供参考。


调用详情中的“网关请求 ID”由普通模型接口入口生成，为32位随机十六进制字符串，同时返回于 `X-Request-Id` 响应头；同一网关请求的重试共用此 ID，用于日志关联和反馈定位，并非会话 ID 或账号 ID。管理台模型测试未经过该入口、旧记录未保存时显示「—」，不补造。“上游请求 ID”来自上游实际响应头，各次已记录 ID 在详情的上游尝试中分别保留，缺失不补造。请求 ID 不在列表展示，详情支持复制完整 ID。


日志列表最左侧状态标识用勾号、感叹号、叉号及语义色区分成功、未完成、失败，悬停查看结束状态、HTTP 状态与重试次数；不另占结果列。默认展示时间、模型/渠道、Key/协议、用量、首响应/总耗时/平均速度，思考强度及管理员消耗可按需开启。请求 ID、完整错误及逐次尝试集中在详情。


管理员日志详情可含 `diagnostics.content_cleared=true`，表示已保存正文按独立保留规则清理，不代表模型输出为空或调用失败。该记录的用量、成本、状态、性能和请求标识保持原值；正文搜索不再匹配已清理文本。详见配置文档的正文保留规则。
