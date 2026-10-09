# work2api

**统一的 Go 单体网关**：把 WorkBuddy / CodeBuddy、Qoder、OpenCode 三个「to-API」上游整合进**一个进程**，对外统一提供 OpenAI Chat / Anthropic Messages / OpenAI Responses **三协议兼容** API，自带 Vue3 明/暗双主题管理台（WebUI）与同域名用户门户。

- 一次启动，三家供应商各司其职、互不影响：`workbuddy`（默认，无前缀）、`qoder/*`、`opencode/*`。
- 共享核心（HTTP 服务、应用 Key 鉴权、SQLite 存储与使用记录、账号池冷却/换号、调度器、WebUI），各上游的独特逻辑（签名/身份头/OAuth/模型发现/签到）收敛在各自的 provider 内。
- 纯 Go + `modernc.org/sqlite`（免 CGO，可交叉编译单文件），前端产物 `go:embed` 进二进制，部署即单个 exe。
- 用户门户入口 `/portal/`（根路径同），管理台 `/admin-ui/`；公网反代应阻断 `/admin/` 与 `/admin-ui/`，管理台经 SSH 隧道访问。用户扫码贡献 WorkBuddy 账号进入共享池，本人调用权不依赖共享授权。

---

## ⚠️ 免责声明（请务必先读）

> **本项目仅供学习与技术交流使用，请勿用于任何其他用途。**

- 本项目**仅用于学习**编程、API 网关原理、多协议适配、账号池与凭据管理等技术研究。
- **禁止**将其用于商业用途、生产环境、批量调用、代理服务转售，或任何可能违反 WorkBuddy / CodeBuddy / Qoder / OpenCode 服务条款的行为。
- 项目内的自动签到、匿名模式、本机凭据探测等能力可能触碰上游 ToS，**是否使用、如何使用由使用者自行判断并承担全部风险**。
- 使用者须自行遵守各上游平台的服务条款；作者不对任何因使用本项目产生的直接或间接损失负责。
- 若你所在地区或平台规定不允许此类工具，请勿使用。
- 本项目不会提供上游账号；凭据由使用者自行导入或授权。启用用户共享时，服务器保存贡献账号凭据，并按所有权和共享池规则限定访问。

---

## 功能特性

- **三协议兼容**：`/v1/chat/completions`、`/v1/messages`、`/v1/responses`，流式与非流式均支持，协议之间自动互转。
- **WorkBuddy 客户端能力保真**：保留客户端提交的技能、Agent、MCP 和项目指令。Anthropic 客户端 ToolSearch 的 `tool_reference` 按当前请求工具定义校验，非延迟及已发现工具加载到 Chat 请求；未发现的延迟工具不会提前展开。并行工具开关与工具错误状态保留。此为协议转换，不等同完整 Anthropic API；不支持的内置工具声明和用户内容块明确报错，签名思考、服务端工具及显式提示缓存不宣称原生支持。语义参考 [工具搜索](https://platform.claude.com/docs/en/agents-and-tools/tool-use/tool-search-tool)、[并行工具调用](https://platform.claude.com/docs/en/agents-and-tools/tool-use/parallel-tool-use) 和 [技能可见性](https://code.claude.com/docs/en/skills)。
- **多供应商命名空间路由**：请求按模型名前缀自动分发——`qoder/xxx`→Qoder、`opencode/xxx`→OpenCode，无前缀→WorkBuddy（默认）。
- **应用 Key 鉴权**：为不同用途创建独立 `sk-...` Key，使用记录按应用统计。
- **密钥模型白名单**：`/v1/models` 只展示该密钥获准调用的模型及别名。省略 `model`（含 `null`、空字符串）时，不限模型的密钥默认使用 `auto`，仅允许一个模型的密钥默认使用该模型，允许多个模型的密钥需明确指定，否则返回 400。显式指定 `auto` 仍需获得授权；它会交给上游选择，并不表示在密钥白名单内自动选择。
- **账号池**：加权轮换 / 冷却 / 失败换号 / 额度感知（WorkBuddy），或单激活账号（Qoder），或 zen/go 双 tier + 匿名 + 会话亲和（OpenCode）。
- **用户门户与共享池**：邀请码注册、扫码贡献 WorkBuddy 账号进共享池、自助建 Key、按组授权模型；停止共享保留本人调用权。目前仅 WorkBuddy 支持用户共享。
- **会话粘性路由**：优先读取 Claude Code 会话头及显式会话 ID，在协议转换前固定识别结果，按用户、Key 应用和模型隔离；同一会话的子 Agent 共用绑定。账号故障自动解粘，管理台可调整后续请求并查看实际执行结果。
- **付费模型账号选择**：成本优先，最低已知付费成本组内优先国内账号，再按到期额度和权重选择；免费及未知成本沿用原规则。旧的自动国际绑定可迁移到该组国内账号，管理员明确指定的国际绑定保留，故障时仍可换号。
- **缓存命中观测**：上游上报的 prompt-cache 命中 tokens 落库并在 WebUI 展示（命中率卡片 + 逐条明细）；WorkBuddy 路径同时把命中映射为 Anthropic `cache_read_input_tokens` 下发，客户端（如 Claude Code）可正确显示缓存命中。上游不报缓存（如 Qoder）时保留未知，不以 0 命中误导；明确上报的 0 与缓存写入分别识别，异常计数保留原值但不计入命中率。
- **Qoder 多模态**：Qoder 模型目录读取上游 `is_vl` 标记并在 WebUI/`/v1/models` 标注多模态；三种入站图片格式（OpenAI `image_url` / Anthropic base64 / Responses `input_image`）归一化透传上游，base64 自动转 data URL。工具结果中的图片也会保留：工具文本和调用 ID 保持配对，图片携带工具来源作为后续用户消息传入；同条消息的普通文本/图片不会被丢弃。
- **WebUI 管理台**：功能优先的信息架构——概览（三供应商状态 + 额度续航预测）、账号、模型、API 密钥、流量（用量分析 + 调用记录）、设置；供应商作为跨页筛选维度，而非独立分页。
- **调度器**：定时签到（含追赶重试）、周期额度刷新、每日模型刷新与预热、token 保活、使用记录清理。

管理台的密钥模型选择器聚合三渠道与模型别名，保留当前目录未发现的已保存选择；清空选择不会自动授予全部模型，需明确开启「不限制模型」。Qoder 模型目录由启动、每日调度或手动刷新更新，普通目录/健康读取使用缓存或静态后备目录。

删除账户会清理网关凭据、注册和缓存，并保留历史记录。桌面原始凭据不会被删除，自动发现的账户会持续隐藏；通过网关重新导入或 OAuth 授权可恢复。Qoder 原生账户同时移除其 secret，本机探测账户可在管理台隐藏。

签到日历按供应商的日期口径展示：WorkBuddy 使用服务端记账日期，Qoder 使用北京时间的 10:00 活动窗口，10:00 前仍属于昨日窗口。模型测试最多 90 秒，客户端断开会取消测试；提示词最多 200 个 Unicode 字符，向上游请求 64 token 输出预算。上游可能采用不同的推理或用量口径，Qoder 实测并不总是严格遵守该预算。

新生成的 `.secret_key` 使用带版本的 hex 格式；既有 32 字节二进制主密钥精确读取，并兼容历史裁剪密钥加密的 v1 数据。旧无 MAC 密文只有在解密结果符合 API Key 格式且与数据库哈希一致时才能查看。主密钥损坏、不可读或既有密钥文件缺失时，不会生成新主密钥覆盖已有加密数据；迁移请同时备份数据库和 `.secret_key`。

## 支持的供应商

| provider | 上游 | 凭据来源 | 签到 | 额度 | 模型命名空间 |
|---|---|---|---|---|---|
| **workbuddy**（默认） | WorkBuddy / CodeBuddy（国内 + 国际） | 扫码 OAuth / 本机 CodeBuddy auth 文件 | ✅（国际站无活动，自动跳过） | ✅ | 无前缀 |
| **qoder** | Qoder（cn / global） | 本机 Qoder 桌面凭据自动探测（Windows）/ 扫码 OAuth / `~/.qoder2api` | ✅（每日 campaigns） | ✅（配额查询） | `qoder/*` |
| **opencode** | OpenCode Zen（zen / go 双 tier + 匿名） | WebUI「编辑配置」或 `data/opencode/opencode.json` 的 key 列表 | — | — | `opencode/*` |

> 未配置凭据的供应商保持 **inert**（不就绪、不列模型、不影响启动）。

> 技术栈：Go 1.26 + 标准库 `net/http` + `modernc.org/sqlite`（纯 Go，免 CGO）；Vue3 + Vite + Tailwind 自定义设计系统，`go:embed` 进二进制；无外部服务依赖，数据落在本地 SQLite。

## 快速开始

### 1. 环境要求

- **Go ≥ 1.26**（Windows 默认安装在 `C:\Program Files\Go\bin`，若未加入 PATH 需自行指定）
- 仅当**要改前端**时才需要 **Node ≥ 22**（前端产物已 `embed` 进仓库，纯后端构建无需 Node）

### 2. 编译后端（前端已内置）

```bash
# 前端 dist 已 embed 进 internal/app/webui/，直接编译即可
CGO_ENABLED=0 go build -o work2api.exe ./cmd/server
./work2api.exe                     # 默认监听 127.0.0.1:8787
./work2api.exe -port 8799          # 自定义端口
```

Windows 上也可用脚本一键构建并运行：

```powershell
pwsh -File scripts/run.ps1          # 构建 work2api.exe 并运行
pwsh -File scripts/run.ps1 -NoRun   # 只构建
```

### 3.（可选）重建前端

只有修改 `webui/` 源码后才需要：

```bash
cd webui
npm install
npx vite build                      # 产物在 webui/dist/
# 把 dist/* 覆盖到 internal/app/webui/（index.html + assets/ + logo.svg），再重新 go build
```

### 4. 打开管理台 & 创建 Key & 调用

1. 浏览器打开 `http://127.0.0.1:8787/admin-ui/`（尚未设置管理 Token 或初始化管理员时，本机回环可直接访问；初始化后须登录）。
2. 进入「API 密钥」页 → 新建应用 → 复制返回的 `sk-...` Key。
3. 进入「账号」页确认目标供应商已就绪（WorkBuddy 需先扫码登录/导入账号；Qoder 若本机装了桌面端会自动探测；OpenCode 点「编辑配置」填 key 或启用匿名层）。
4. 调用（模型名带前缀即路由到对应供应商）：

```bash
curl http://127.0.0.1:8787/v1/chat/completions \
  -H "Authorization: Bearer sk-你的key" -H "Content-Type: application/json" \
  -d '{"model":"qoder/qfmodel","messages":[{"role":"user","content":"你好"}]}'
```

### 管理员与用户身份

- 在私有管理入口的「门户管理 → 用户管理」直接创建管理员，或将已有普通用户升级；已有账号贡献、Key 和模型白名单保留。
- 首次初始化的账号为超级管理员。超级管理员与管理员具有相同的日常管理能力，管理员任免、管理员停用/密码恢复和超级管理员交接由超级管理员负责；普通管理员可管理普通用户。
- 新建账号或管理重置密码后必须先修改临时密码；身份变更、停用及改密会撤销相关登录会话。管理员身份不会绕过门户 Key 的账号资格及模型授权。
- `ADMIN_TOKEN` 保留为恢复入口，操作单独记录；不要向新增管理员分发恢复 Token。身份管理操作须在最近 5 分钟完成密码或 Token 验证。
- 升级至数据库 v19 时，仅有一个旧管理员且状态有效时，自动成为超级管理员；存在多个旧管理员时，通过恢复入口明确指定。超级管理员不可直接停用或降级，需交接给已完成改密的有效管理员。
- 公网反代继续阻断 `/admin/` 与 `/admin-ui/`。新增管理员需单独授权私有访问通道。升级前做完整备份；回退到旧版本需恢复配套数据库，不能只更换二进制。

## 下载与部署（免手动编译）

推荐直接用**预编译产物**，不必本地 `go build`：

### 方式 A：下载 Release 二进制

打 tag（`vX.Y.Z`）后，GitHub Actions 会交叉编译 Windows / Linux / macOS（amd64 + arm64）并发布到 [Releases](https://github.com/YuapXc/work2api/releases)，每个包附 `.sha256` 校验和。下载对应平台的压缩包，校验后解压即用：

```powershell
# Windows：校验后运行
Get-FileHash .\work2api_vX.Y.Z_windows_amd64\work2api.exe -Algorithm SHA256
.\work2api.exe
```

### 方式 B：Docker（GHCR 镜像，绕开 Windows 杀软）

```bash
docker run -d --name work2api -p 8787:8787 \
  -v work2api-data:/data \
  ghcr.io/yuapxc/work2api:vX.Y.Z
# 从宿主机 http://localhost:8787 访问（回环名放行）；镜像内已绑 0.0.0.0。
```

> 从**非回环**地址（局域网 IP / 域名）访问时，必须同时设 `-e ADMIN_TOKEN=<你的令牌> -e ALLOW_EXTERNAL_HOST=1`，否则会被 Host 校验拦截（防 DNS rebinding）。

公网管理台应通过 HTTPS 访问。`ALLOW_EXTERNAL_HOST=1` 时默认启用 Secure Cookie，支持代理以 HTTP 回源；反向代理须保留外部 `Host`（含非默认端口），以便校验管理请求的 `Origin`。服务不会凭 `X-Forwarded-Proto` 放宽 Cookie 或来源校验。仅可信局域网直接使用 HTTP 时，可显式设置 `ADMIN_COOKIE_SECURE=false`；公网应保持开启。脚本管理请求继续使用 `X-Admin-Token`，分发给调用者的应是独立 API Key。

### 关于火绒 `Trojan/Intercept.a`（误报）

自编译或未签名的 exe 可能被火绒等按**行为启发式**误报为 `Trojan/Intercept.a`——本质是「未知未签名 + 监听端口 + 处理凭据」触发的通用规则，依赖仅 `modernc.org/sqlite` 等可信包。处理方式：

- **优先用上面的 Release / Docker 产物**：每个版本是**同一个稳定哈希**，便于火绒云端建立信誉，也便于一次性提交白名单。
- 仍被拦截：到[火绒误报中心](https://www.huorong.cn/)提交该 exe（每个新版本哈希变了需重提），或把 `work2api.exe` / 项目目录加入火绒「信任区」。
- **本地 `go build` 时报 `Access is denied`**：是火绒锁住了链接器在 `%Temp%\go-build*` 下刚产出的临时 exe。`scripts/run.ps1` 已把 `GOTMPDIR` 指到项目内 `.gobuildtmp/` 绕开；手动构建可自行 `set GOTMPDIR=<项目内目录>`。
  - 之所以「挪进项目目录」就能绕开：前提是**你已把项目目录加进了火绒信任区**——链接器临时产物落进受信目录就不再被拦，本质是复用了这层信任，而非 `%Temp%` 这个位置特殊。没加信任区的机器仍可能在 `%Temp%` 被拦，此时把项目目录（含 `.gobuildtmp/`）加信任区即可。
- 自签名（`run.ps1 -Sign`）只去掉「未知发布者」提示，**不产生云端信誉、不解决报毒**；真正一劳永逸需 OV/EV 代码签名证书（需实体 + 年费，学习项目一般不必）。

## 配置（flag > 环境变量 > .env）

| flag | 环境变量 | 默认 | 说明 |
|---|---|---|---|
| `-host` | `HOST` | `127.0.0.1` | 监听地址 |
| `-port` | `PORT` | `8787` | 监听端口 |
| `-data-dir` | `DATA_DIR` | `data` | 数据目录（DB / 凭据 / 密钥） |
| `-db` | `DB_PATH` | `<data-dir>/work2api.db` | SQLite 路径 |
| `-admin-token` | `ADMIN_TOKEN` | 空 | 管理台/`/admin/*` 令牌；**空 = 仅回环可用** |
| `-log-level` | `LOG_LEVEL` | `INFO` | 日志级别 |
| — | `ALLOW_EXTERNAL_HOST` | `false` | 允许非回环 Host 访问（防 DNS rebinding，见注意事项） |
| — | `ADMIN_COOKIE_SECURE` | 随公网模式开启 | 强制管理 Cookie 的 Secure 属性；HTTPS 代理 HTTP 回源时应保持开启 |
| — | `MAX_REQUEST_BYTES` | `16777216` | 请求体上限（字节，必须大于 0）；登录固定 8 KiB，读取期限 30 秒 |
| — | `MAX_CONCURRENT_REQUESTS` | `13` | 模型执行并发（含 SSE 与管理台模型测试）；管理接口独立限额 |
| — | `MODEL_QUEUE_SIZE` / `MODEL_QUEUE_WAIT_SECONDS` | `32` / `60` | 有界排队与累计等待预算，包含账号节流与重新入池 |
| — | `MODEL_KEY_QUEUE_SIZE` / `MODEL_KEY_CONCURRENCY_LIMITS` | `16` / 空 | 每 Key 等待上限；JSON 按应用 ID 配置执行上限，例如 `{"2":1}` |
| — | `ADMIN_CONCURRENCY` / `HEAVY_ADMIN_CONCURRENCY` | `8` / `2` | 轻管理 / 重管理独立限额；同类刷新共享执行结果 |
| — | `QUERY_CONCURRENCY` / `BODY_READ_CONCURRENCY` | `4` / `4` | 模型列表及 Token 统计 / 请求体读取限额 |
| — | `REQUEST_BODY_BUDGET` | `33554432` | 执行与排队共享的请求体缓冲容量预算（32 MiB） |
| — | `MAX_JSON_ITEMS` / `MAX_JSON_DEPTH` | `100000` / `128` | 解码前限制 JSON 结构符号数量与嵌套深度，防止对象数量放大内存 |
| — | `MAX_RESPONSE_BYTES` | `8388608` | 每次上游响应上限，SSE 含封装，防止输出聚合无界增长 |
| — | `DESENSITIZE` | 已停用 | 旧配置忽略；完整保留技能、工具和项目指令，不再摘要替换或插入零宽字符 |
| — | `CHANNEL_IDENTITY_COMPAT` | `true` | 国内明确渠道拒绝且未开始输出时，兼容已验证的 Claude Code CLI/SDK 固定身份声明；保留技能、工具、权限和任务内容；关闭后同时关闭归因精简 |
| — | `CHANNEL_METADATA_COMPAT` | `true` | 身份兼容仍被拒绝时，再移除已验证的 CC billing 归因行；两级兼容合计最多 2 次，连同授权范围内换号共最多 5 次。成功方式仅在当前会话、账号及系统/工具模板一致时复用；重启失效 |
| — | `RATELIMIT` / `RATELIMIT_INTERVAL` | `true` / `1.5` | 每账号限速 |
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
| — | `PORTAL_USER_CONCURRENCY` / `PORTAL_SHARED_CONCURRENCY` | `4` / `12` | 门户每用户 / 共享池执行上限 |
| — | `PORTAL_USER_QUEUE_SIZE` / `PORTAL_SHARED_QUEUE_SIZE` | `6` / `28` | 门户每用户 / 共享池等待上限 |
| — | `WORKBUDDY_ACCOUNT_CONCURRENCY` | `4` | 每个 WorkBuddy 账号 UID 执行上限（跨模型、私人/共享合并） |
| — | `PORTAL_KEY_MAX_PER_USER` / `PORTAL_MAX_TASKS_PER_USER` | `2` / `3` | 每用户 Key 数 / 同时扫码任务数 |
| — | `PORTAL_DAILY_REQUESTS` / `PORTAL_DAILY_INPUT_TOKENS` / `PORTAL_DAILY_OUTPUT_TOKENS` | `0` | 门户每日请求 / 输入 / 输出预算（0=不限） |

## 并发与资源准入

默认全局执行硬上限 13、共享池 12、每用户 4、每个 WorkBuddy 账号 UID 4（跨模型及私人/共享池合并）。实际准入还受授权账号名额、224 MiB 响应预留预算与 Go 在用内存压力约束：流式按响应上限 2 倍预留、非流式 4 倍；至少保留一个私人执行槽及其响应预算。等待上限：全局 32 / 共享 28 / 每用户 6，累计等待预算 60 秒（含账号节流等待），反向代理超时须覆盖排队及模型首包。

本地过载返回 HTTP 429、`error.type=local_overload`、具体 `error.code`（如 `model_queue_full`、`key_queue_full`、`model_queue_timeout`）与 `Retry-After: 2`；调用端仅对此类执行前拒绝加随机退避重试，最多 2 次。多 Key 按用户合并计数，取消/超时/断连及时回收名额；账号忙时排队，不因忙碌换号。每日次数与 token 预算默认不限（0）。

多 Agent 场景：个人 Key 可使用全部空闲名额，分发 Key 可用 `MODEL_KEY_CONCURRENCY_LIMITS` 按应用 ID 设较低上限。原始请求体预算不代表进程内存上限（JSON 解码、协议转换、图片、响应序列化仍会放大）；提高硬上限或请求/响应上限前，应在目标服务器以长上下文、图片和非流式响应实测峰值 RSS 与延迟。

## 备份

管理台「设置」可手动生成完整在线备份，也可启用每日自动任务（默认关闭，保留最近 7 份校验成功的归档）。快照使用 SQLite `VACUUM INTO` + 完整性检查，归档在 `DATA_DIR/backups`（权限 600），成员 SHA-256 记录在 `manifest.json`，含数据库、主密钥、各渠道凭据与有效配置；额外服务器配置（Nginx/证书等）用 `BACKUP_EXTRA_PATHS` 显式加入。恢复须在隔离目录验证后停服替换，不要直接在运行实例上覆盖。凭证 JSON 导出仅用于账号迁移，不能恢复完整用户系统。

## 安全边界

- 对外暴露：默认仅回环。局域网/公网访问必须同时设 `ADMIN_TOKEN` 与 `ALLOW_EXTERNAL_HOST=1`；公网管理台应经 HTTPS，反代保留外部 `Host`，切勿无鉴权暴露。
- 真实客户端 IP：应用仅接受明确可信即时代理的 `X-Real-IP`；反代须强制覆盖此头（不要透传客户端自带值），边缘加速（如 EdgeOne）应用官方回源范围或私密回源鉴权验证。
- 门户登录/注册限流：每 IP 60 次、整体 300 次/10 分钟，登录另有每用户名 10 次/10 分钟。
- 敏感数据：`data/`（DB、含明文对话的使用记录）、`auths/`、`*.db`、`.secret_key`、`*.info` 均含凭据/隐私，已在 `.gitignore` 中，切勿提交或外传；备份主密钥须与数据库同源。

## 各供应商接入与注意事项

### WorkBuddy / CodeBuddy（默认）
- 在管理台「供应商 → WorkBuddy → 账号」里**扫码登录**（cn / 国际），或上传本机 CodeBuddy 的 `.info` auth 文件。
- **国内 CodeBuddy 桌面端新版把 token 加密存储（`$wbEncrypted`），无法离线读取**——这类账号需在 WebUI **重新扫码登录**导入明文凭据。
- 国际站 WorkBuddy 无签到活动，签到会自动跳过。

### Qoder
- **本机装了 Qoder 桌面端时会自动探测凭据**（Windows：解密 Chromium `safeStorage`，用当前用户 DPAPI），无需手动登录即可用。
- 也可在管理台「Qoder → 账号 → 添加账号」**扫码登录**（选 cn / global），账号存到 `~/.qoder2api`。
- 若 Qoder 桌面端升级到新的 App-Bound 加密（本地无法离线解密），请改用扫码登录。
- 「设为激活」= 选择用哪个账号（Qoder 为单激活账号模式，无加权池，故无「优先级」）。

### OpenCode
- **在管理台「账号 → OpenCode → 编辑配置」里填 `zen_keys` / `go_keys` / `anonymous` 等**（保存即热重载，无需重启）；也可用 `OPENCODE_CONFIG` 指定路径，或直接放 `data/opencode/opencode.json`。未配置任何 key 且未启用匿名层时，该供应商保持未就绪。
- 配置与两个模型缓存（`*.models.catalog.json` / `*.models.dev.json`）聚合在同一目录；旧版放在工作目录根的 `opencode.json` 会在启动时自动迁移到 `data/opencode/`。
- 无账号 OAuth、无签到、无额度查询（上游本身不提供），管理台仅展示 key 层与模型。

## API 端点

- 推理（需 `Authorization: Bearer sk-...`）：`POST /v1/chat/completions`、`POST /v1/messages`、`POST /v1/responses`、`GET /v1/models`
- 管理（回环免鉴权，或带 `ADMIN_TOKEN`）：`GET /admin/providers`、`GET/POST /admin/providers/{name}/...`（账号/模型/签到/额度/OAuth/账号管理）、`/admin/apps`、`/admin/apps/{id}/models`（密钥模型白名单）、`/admin/models/catalog`（跨供应商聚合模型目录）、`/admin/checkin/history`（跨供应商签到日历）、`/admin/usage/*`、`/admin/settings`、`/admin/login` + `/admin/logout`（WebUI 会话）
- 健康检查：`GET /health`（只报计数，不含账号 UID）；`GET /admin/health`（管理台健康详情）

## 接入客户端（三协议 base URL + 供应商前缀路由）

work2api 同时开三种协议，**任何兼容客户端都能接**，不只给 Claude 用。base URL 按客户端习惯填：

| 协议 | base URL | 客户端自动拼的路径 | 谁用 |
|---|---|---|---|
| Anthropic Messages | `http://127.0.0.1:8787` | `/v1/messages` | Claude Code、Claude 系客户端 |
| OpenAI Chat | `http://127.0.0.1:8787/v1` | `/v1/chat/completions` | 多数 OpenAI 兼容工具（Cherry Studio、LobeChat 等） |
| OpenAI Responses | `http://127.0.0.1:8787/v1` | `/v1/responses` | Codex CLI 等用 Responses API 的客户端 |

三个端点**共用同一套账号池、同一个 `sk-...`、同一套前缀路由**，协议之间由 work2api 自动互转。

**多供应商靠模型名前缀分流**——切供应商只换模型名，不换地址/Key：

| 供应商 | 模型名写法 |
|---|---|
| WorkBuddy（默认） | `<模型>`（无前缀） |
| Qoder | `qoder/<模型>` |
| OpenCode | `opencode/<模型>` |

> 以 Claude Code + [cc-switch](https://github.com/farion1231/cc-switch) 这类切换器为例：建三个 profile，**base URL 和 Key 完全相同**，只有模型名前缀不同即可在三家间一键切换。注意 Claude Code 会同时用主模型（`ANTHROPIC_MODEL`）和小快模型（`ANTHROPIC_SMALL_FAST_MODEL`）——两个都要填成 work2api 能路由的模型（同一家、同前缀），否则后台调用会因模型不存在报错。

## 注意事项

- **Windows 杀软误报**：火绒等可能把自编译的 exe 报成 `Trojan/Intercept.a`（行为启发式误报）——处理办法见上方[「关于火绒」](#关于火绒-trojanintercepta误报)：优先用 Release/Docker 产物、提交误报中心或加信任区。
- **端口占用**：换端口或重启前先确认旧进程已退出（Windows 可 `Get-NetTCPConnection -LocalPort <port>` 查占用 PID）。

## 致谢 / 参考项目

本项目整合并参考了以下上游（均为各自作者的独立项目）：

- WorkBuddy2API（本项目 workbuddy provider 的行为蓝本，Python/FastAPI）：[Joy4Fire/Workbuddy2API](https://github.com/Joy4Fire/Workbuddy2API)
- [Zhengyuuuui/qoder2api](https://github.com/Zhengyuuuui/qoder2api)
- [jasonxu114514/opencode2api](https://github.com/jasonxu114514/opencode2api)

## License

仅供学习交流，见上方免责声明。使用前请确认符合各上游平台的服务条款与你所在地区的法律法规。

## 附：版本与更新策略说明

新生成的 `.secret_key` 使用带版本的 hex 格式；既有 32 字节二进制主密钥精确读取，并兼容历史裁剪密钥加密的 v1 数据。旧无 MAC 密文只有在解密结果符合 API Key 格式且与数据库哈希一致时才能查看。主密钥损坏、不可读或既有密钥文件缺失时，不会生成新主密钥覆盖已有加密数据；迁移请同时备份数据库和 `.secret_key`。

管理台的密钥模型选择器聚合三渠道与模型别名，保留当前目录未发现的已保存选择；清空选择不会自动授予全部模型，需明确开启「不限制模型」。Qoder 模型目录由启动、每日调度或手动刷新更新，普通目录/健康读取使用缓存或静态后备目录。

删除账户会清理网关凭据、注册和缓存，并保留历史记录。桌面原始凭据不会被删除，自动发现的账户会持续隐藏；通过网关重新导入或 OAuth 授权可恢复。Qoder 原生账户同时移除其 secret，本机探测账户可在管理台隐藏。

签到日历按供应商的日期口径展示：WorkBuddy 使用服务端记账日期，Qoder 使用北京时间的 10:00 活动窗口，10:00 前仍属于昨日窗口。模型测试最多 90 秒，客户端断开会取消测试；提示词最多 200 个 Unicode 字符，向上游请求 64 token 输出预算。上游可能采用不同的推理或用量口径，Qoder 实测并不总是严格遵守该预算。

Qoder 中国站自动签到和定期刷新额度各有独立开关，默认关闭；国际站签到尚未支持，不向中国站发送其令牌。签到依据上游活动和本地窗口记录，失败/暂无活动有界重试，手动与自动合并执行；额度查询失败标记陈旧并保留最近成功值。PAT 不支持设备令牌额度查询。不同上游没有缓存字段时继续显示未知。

私有概览显示最近一小时请求/尝试/上游 429、排队、账号等待、首响应和执行阶段 P95（固定直方图近似上界；溢出显示未知）。首响应是首次成功写出响应正文，包含协议事件，不能当作首个模型文本 token。执行阶段从首个上游尝试开始，包含重试退避，扣除后续准入及账号等待。`X-Request-Id` 由网关生成；管理概览 API 提供最近 128 个完成请求的无正文诊断摘要，内存有界，重启清空，不记录凭据或请求内容。
`/v1/messages/count_tokens` 当前提供文本及工具内容的粗估（含 system、工具定义和结果），不使用供应商 tokenizer；图片和音频未计入，不能作为精确计费或上下文容量依据。
