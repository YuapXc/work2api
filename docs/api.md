# 客户端接入

[返回项目首页](../README.md)

## 地址与鉴权

先在管理台或用户门户创建 API Key，从模型页复制完整模型 ID。

| 协议 | 客户端 Base URL | 请求端点 |
|---|---|---|
| OpenAI Chat | `http://127.0.0.1:8787/v1` | `POST /v1/chat/completions` |
| Anthropic Messages | `http://127.0.0.1:8787` | `POST /v1/messages` |
| SystemOne 决策 | `http://127.0.0.1:8787/v1` | `POST /v1/systemone` |
| OpenAI Responses | `http://127.0.0.1:8787/v1` | `POST /v1/responses` |

公网使用时将本机地址替换为你的 HTTPS 域名。Base URL 是否包含 `/v1` 取决于客户端的拼接方式，最终请求路径应与表中一致。

三种协议共用 Key 和模型路由，支持流式与非流式调用。OpenAI 兼容接口使用 `Authorization: Bearer sk-...`；Messages 也支持 `x-api-key`。`GET /v1/models` 返回当前 Key 有权使用的模型。`ADMIN_TOKEN` 是管理凭据，不用于模型调用。

```bash
curl http://127.0.0.1:8787/v1/chat/completions \
  -H "Authorization: Bearer sk-你的Key" \
  -H "Content-Type: application/json" \
  -d '{"model":"从模型页复制的完整ID","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

## 模型与 Key 范围

- 渠道前缀为 `workbuddy/`、`qoder/`、`opencode/`、`trae/`；旧裸模型名仍指向 WorkBuddy。
- 同一地址和 Key 下，切换模型 ID 即可切换渠道；跨渠道共用调用名见 [统一模型路由](model-routing.md)。
- Key 的模型白名单始终生效；用户 Key 还受当前用户权限及全局禁用约束。
- 建议始终显式传入 `model`。省略时，单模型 Key 使用唯一获准模型；多模型受限 Key 返回 400；不限制模型的 Key 使用 WorkBuddy `auto`。显式 `auto` 仍需授权，它表示上游选模，不是按白名单自动选择。

Claude Code 等会使用多个模型的客户端，主模型及辅助模型均应填写网关可用的调用名。

## 协议能力

| 能力 | 说明 |
|---|---|
| 流式与非流式 | Chat、Messages、Responses 可跨协议转换 |
| 工具调用 | 保留定义、调用 ID、参数与结果，支持并行调用 |
| Responses 工具 | 支持 namespace 与 custom 的客户端类型及结果往返 |
| 技能与项目指令 | 保留客户端提交的技能、Agent、MCP 与项目规则 |
| 多模态 | 按渠道目录确认的图片与工具结果能力转发 |
| 输出状态 | 区分正常完成、输出未完成及调用失败 |

实际能力取决于客户端、模型和渠道。网关不代替客户端执行工具，也不通用模拟服务端工具、原生签名思考或 grammar 受限解码。不可转换的可选工具会被过滤，强制指定不可用工具时明确报错。

WorkBuddy 聊天请求支持 `n=1`，多个 Agent 可分别发起独立请求。`POST /v1/messages/count_tokens` 提供文本和工具内容的估算，不是精确 tokenizer 或计费依据。

## 会话与输出

各渠道保留自身账号选择策略。WorkBuddy 支持成本与额度感知、国内账号偏好、会话粘性和故障切换；其他渠道差异见 [渠道接入](providers.md)。管理台「流量 → 会话」可调整后续请求的账号绑定，正在执行的请求不迁移。

优先使用客户端提供的稳定会话标识；缺失时尝试从最早用户消息识别，无法保证区分开场完全相同的会话。输出开始后不自动换号重放，客户端取消会取消对应上游读取。

OpenCode 保留客户端原生会话 ID；由网关合成的 ID 在 UTC 日期变化时刷新，跨日请求可能重新选择账号或出口。

`length` 表示输出预算或上下文边界导致未完成，不等于网络断流。状态与性能的查看方式见 [运行维护](operations.md#调用记录与续航)。


## 媒体生成与音频转写

WorkBuddy 支持独立媒体接口，模型页按上游目录展示文生图、图片编辑、文生视频和图生视频能力。它们不使用聊天接口，也不参与聊天模型合并路由；请显式传入实际模型 ID。

| 能力 | 接口 |
|---|---|
| 图片生成 | `POST /v1/images/generations`，JSON |
| 图片编辑 | `POST /v1/images/edits`，JSON 或 multipart/form-data |
| 视频提交 | `POST /v1/videos`，JSON，成功返回 202 和网关任务 ID |
| 视频查询 | `GET /v1/videos/{id}`，使用提交时的同一 API Key |
| 音频转写 | `POST /v1/audio/transcriptions`，JSON 或 multipart/form-data |
| 素材上传 | `POST /v1/media/uploads`，绑定获准的视频模型 |
| 任务记录 | `GET /v1/media/tasks?before=时间戳`，分页读取当前 Key 的任务 |
| 通用任务查询 | `GET /v1/media/tasks/{id}` |

图片请求包含 `model`、`prompt`，可传 `size`（默认 `1024x1024`）和 `n`。图片编辑的 JSON `image` 是图片 data URL 数组；multipart 使用 `image` 或 `image[]` 文件字段。网关不读取调用者填写的服务器本地路径，不自动下载用户提供的图片链接。上游未提供 mask 能力时明确报错；部分模型只返回 URL，不能要求它返回 base64。

```json
{"model":"workbuddy/hunyuan-image-alpha","prompt":"白色背景上的蓝色圆形","size":"1024x1024","n":1}
```

视频请求包含 `model`、`prompt`，可传 `seconds`、`resolution`、`aspect_ratio`、`enable_audio`、`negative_prompt`。图生视频使用 `image_url`，支持首尾帧的模型可再提供 `last_image_url`；输入为 HTTP(S) 图片地址或图片 data URL。时长、分辨率等按当前所选账号的真实模型目录校验，未返回的限制不凭空补设。此接口是网关的 JSON 媒体契约，不宣称兼容所有视频 SDK 的 multipart 参数。

```json
{"model":"workbuddy/kling-video-v3","prompt":"固定镜头下的海边日落","seconds":3,"resolution":"720P","enable_audio":false,"aspect_ratio":"16:9"}
```

提交返回的 `id` 用于后续查询，状态包括 `queued`、`in_progress`、`completed`、`failed`；完成后的 `data` 是产物数组。客户端按需查询，网关不常驻轮询。媒体工作台仅在页面可见且正在查看活动任务时刷新状态。任务绑定原账号并持久保存，重启后可继续查询；终态查询使用保存结果，不重复提交或重复记账。查询仍要求当前 Key、用户及账号权限有效。

媒体模型沿用 Key 范围、本人账号权利、管理员授权及全局禁用规则。价格未知的模型不会因此自动加入免费范围。消费以结果 `usage.credit` 为准，视频请求时长与上游实际计费时长可能不同；视频的合成 token 用量不用于聊天 tok/s 或续航估算。

生成请求不会自动换号或重发；连接中断、超时或不明确的响应可能发生在上游受理之后，不要自动重新提交。响应头 `X-Media-Task-Id` 提供网关任务标识。媒体任务及日志按日志保留周期清理；产物 URL 可能更早过期，网关不永久保存媒体文件。图片和视频会消耗上游额度，示例不是免费调用承诺。

SystemOne 仅用于 OpenCode 目录标记为 `native_protocol=systemone` 的模型，请按上游契约提交 `state` 和类型化 `questions`；不做聊天消息转换。它与图片、视频生成是独立能力。


### 视频参考素材

视频支持 `reference_images`、`reference_videos`、`reference_audios` 数组，每项为 HTTP(S) 地址；参考图片也支持图片 data URL。请求到上游时分别转换为 `image_url`、`video_url`、`audio_url` 对象。最大数量、混用、首尾帧互斥、音频是否需要视觉素材、离散时长等以所选账号的 `videoGeneration` 为准。

本地素材可通过工作台上传，也可请求 `POST /v1/media/uploads`：传入已授权的视频 `model` 和 `file:{data:"纯base64",mime:"image/png"}`。接口仅获取并使用上游签名存储地址，不读取服务器路径，不代抓任意 URL，也不向存储服务发送账号 Token。返回临时 `url` 用于生成。上传不代表已创建生成任务。

### 音频转写

国内 WorkBuddy 的 `workbuddy/wb-asr` 是官方 CLI 专用服务标识，不来自聊天模型列表；目录注明 `official_cli_protocol`，不编造价格或自动开放免费权限。请求可使用 JSON 的 HTTP(S) `audio_url`，或 multipart 的 `file` 文件字段。音频、视频均可作为转写输入。

默认等待转写完成并返回 `{text,usage}`，`response_format=text` 返回纯文本；传 `async=true` 则返回 202 和任务 ID，通过通用任务查询接口读取 `text` 与 `usage`。任务持续执行时中止客户端等待不会自动取消上游任务，后续应查询原任务。

### 媒体工作台

管理台和用户门户均提供媒体入口，先选择现有 API Key，再选择其有权使用的模型；Key 只在当前页面内存中使用。工作台提供图片生成/编辑、视频参考素材、音频上传转写，以及原生 SystemOne JSON 请求。表单与任务/结果区共用组件，窄屏单列排列。

每次生成或转写需确认实际额度消费。任务结果支持图片预览、视频播放、转写文本和结构化决策结果；任务记录按 Key 隔离并可分页查询。媒体不等同于聊天图片理解，不会自动向聊天模型注入生成工具。内联 base64 产物在当前响应中返回，不长期存入数据库；需要长期保留请及时保存，临时链接也可能失效。

任务的已保存结果沿用正文保留天数清理（默认7天），不影响已保存的任务状态与实际消费记录。生成响应中的完整内容不因该清理策略截断。
