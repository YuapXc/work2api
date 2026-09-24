# work2api

**统一的 Go 单体网关**：把 WorkBuddy / CodeBuddy、Qoder、OpenCode 三个「to-API」上游整合进**一个进程**，对外统一提供 OpenAI Chat / Anthropic Messages / OpenAI Responses **三协议兼容** API，并自带 Vue3 深色管理台（WebUI）。

- 一次启动，三家供应商各司其职、互不影响：`workbuddy`（默认，无前缀）、`qoder/*`、`opencode/*`。
- 共享核心（HTTP 服务、应用 Key 鉴权、SQLite 存储与使用记录、账号池冷却/换号、调度器、WebUI），各上游的独特逻辑（签名/身份头/OAuth/模型发现/签到）收敛在各自的 provider 内。
- 纯 Go + `modernc.org/sqlite`（免 CGO，可交叉编译单文件），前端产物 `go:embed` 进二进制，部署即单个 exe。

---

## ⚠️ 免责声明（请务必先读）

> **本项目仅供学习与技术交流使用，请勿用于任何其他用途。**

- 本项目**仅用于学习**编程、API 网关原理、多协议适配、账号池与凭据管理等技术研究。
- **禁止**将其用于商业用途、生产环境、批量调用、代理服务转售，或任何可能违反 WorkBuddy / CodeBuddy / Qoder / OpenCode 服务条款的行为。
- 项目内的自动签到、匿名模式、本机凭据探测等能力可能触碰上游 ToS，**是否使用、如何使用由使用者自行判断并承担全部风险**。
- 使用者须自行遵守各上游平台的服务条款；作者不对任何因使用本项目产生的直接或间接损失负责。
- 若你所在地区或平台规定不允许此类工具，请勿使用。
- 本项目不分发、不代管任何账号或密钥；所有凭据均来自使用者自己的本机环境。

---

## 功能特性

- **三协议兼容**：`/v1/chat/completions`、`/v1/messages`、`/v1/responses`，流式与非流式均支持，协议之间自动互转。
- **多供应商命名空间路由**：请求按模型名前缀自动分发——`qoder/xxx`→Qoder、`opencode/xxx`→OpenCode，无前缀→WorkBuddy（默认）。
- **应用 Key 鉴权**：为不同用途创建独立 `sk-...` Key，使用记录按应用统计。
- **账号池**：加权轮换 / 冷却 / 失败换号 / 额度感知（WorkBuddy），或单激活账号（Qoder），或 zen/go 双 tier + 匿名 + 会话亲和（OpenCode）。
- **WebUI 管理台**：概览三供应商状态卡；每个供应商独立的「账号 / 模型」分页；用量、调用记录、应用 Key 管理。
- **调度器**：定时签到（含追赶重试）、周期额度刷新、每日模型刷新与预热、token 保活、使用记录清理。

## 支持的供应商

| provider | 上游 | 凭据来源 | 签到 | 额度 | 模型命名空间 |
|---|---|---|---|---|---|
| **workbuddy**（默认） | WorkBuddy / CodeBuddy（国内 + 国际） | 扫码 OAuth / 本机 CodeBuddy auth 文件 | ✅（国际站无活动，自动跳过） | ✅ | 无前缀 |
| **qoder** | Qoder（cn / global） | 本机 Qoder 桌面凭据自动探测（Windows）/ 扫码 OAuth / `~/.qoder2api` | ✅（每日 campaigns） | ✅（配额查询） | `qoder/*` |
| **opencode** | OpenCode Zen（zen / go 双 tier + 匿名） | `opencode.json` 配置的 key 列表 | — | — | `opencode/*` |

> 未配置凭据的供应商保持 **inert**（不就绪、不列模型、不影响启动）。

## 技术栈

- 后端：Go 1.25、标准库 `net/http`、`modernc.org/sqlite`（纯 Go，免 CGO）
- 前端：Vue3 + Vite + Ant Design Vue（深色「信号台」设计），`go:embed` 进二进制
- 无外部服务依赖（无需数据库/Redis），数据落在本地 SQLite

## 快速开始

### 1. 环境要求

- **Go ≥ 1.25**（Windows 默认安装在 `C:\Program Files\Go\bin`，若未加入 PATH 需自行指定）
- 仅当**要改前端**时才需要 **Node ≥ 18**（前端产物已 `embed` 进仓库，纯后端构建无需 Node）

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

1. 浏览器打开 `http://127.0.0.1:8787/`（本机回环访问**无需**登录）。
2. 进入「应用」页 → 新建应用 → 复制返回的 `sk-...` Key。
3. 在「供应商」里确认目标供应商已就绪（WorkBuddy 需先扫码登录/导入账号；Qoder 若本机装了桌面端会自动探测；OpenCode 需放 `opencode.json`）。
4. 调用（模型名带前缀即路由到对应供应商）：

```bash
curl http://127.0.0.1:8787/v1/chat/completions \
  -H "Authorization: Bearer sk-你的key" -H "Content-Type: application/json" \
  -d '{"model":"qoder/qfmodel","messages":[{"role":"user","content":"你好"}]}'
```

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
| — | `DESENSITIZE` | `true` | 对 system/developer 消息脱敏 |
| — | `RATELIMIT` / `RATELIMIT_INTERVAL` | `true` / `1.5` | 每账号限速 |
| — | `CHECKIN_HOURS` | `9,21` | 每日自动签到小时 |
| — | `CREDIT_REFRESH_MIN` | `30` | 额度刷新周期（分钟） |
| — | `MODEL_REFRESH_HOUR` | `6` | 每日模型刷新小时 |
| — | `USAGE_RETENTION_DAYS` | `90` | 使用记录保留天数 |
| — | `OPENCODE_CONFIG` | `<cwd>/opencode.json` | OpenCode 配置文件路径 |
| — | `QODER2API_HOME` | `~/.qoder2api` | Qoder 数据目录 |

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
- **需要放一份 `opencode.json`**（工作目录下，或用 `OPENCODE_CONFIG` 指定），含 `zen_keys` / `go_keys` / `anonymous` 等；否则该供应商保持未就绪。
- 无账号 OAuth、无签到、无额度查询（上游本身不提供），管理台仅展示 key 层与模型。

## API 端点

- 推理（需 `Authorization: Bearer sk-...`）：`POST /v1/chat/completions`、`POST /v1/messages`、`POST /v1/responses`、`GET /v1/models`
- 管理（回环免鉴权，或带 `ADMIN_TOKEN`）：`GET /admin/providers`、`GET/POST /admin/providers/{name}/...`（账号/模型/签到/额度/OAuth/账号管理）、`/admin/apps`、`/admin/usage/*`、`/admin/settings`
- 健康检查：`GET /health`

## 注意事项（重要）

- **对外暴露安全**：默认只允许回环访问。要从局域网/公网访问，必须同时设置 `ADMIN_TOKEN`（否则管理台无鉴权）并设 `ALLOW_EXTERNAL_HOST=1`（放开 Host 校验）。切勿在无鉴权下暴露到公网。
- **敏感数据**：`data/`（DB、使用记录含明文对话）、`auths/`、`*.db`、`.secret_key`、`*.info` 均含凭据/隐私，已在 `.gitignore` 中，**切勿提交或外传**。
- **端口占用**：换端口或重启前先确认旧进程已退出（Windows 可 `Get-NetTCPConnection -LocalPort <port>` 查占用 PID）。
- **Windows 杀软误报**：火绒等可能把自编译的 exe 报成 `Trojan/Intercept.a`（行为启发式误报，依赖仅 `modernc/sqlite` 等可信包）——把项目目录或 `work2api.exe` 加入「信任区」即可。

## 致谢 / 参考项目

本项目整合并参考了以下上游（均为各自作者的独立项目）：

- WorkBuddy2API（本项目 workbuddy provider 的行为蓝本，Python/FastAPI）：[Joy4Fire/Workbuddy2API](https://github.com/Joy4Fire/Workbuddy2API)
- [Zhengyuuuui/qoder2api](https://github.com/Zhengyuuuui/qoder2api)
- [jasonxu114514/opencode2api](https://github.com/jasonxu114514/opencode2api)

## License

仅供学习交流，见上方免责声明。使用前请确认符合各上游平台的服务条款与你所在地区的法律法规。

