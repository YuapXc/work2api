# 配置说明

[返回项目首页](../README.md)

本地通常无需 `.env`。公网 HTTPS 使用随机 `ADMIN_TOKEN`，其他项按需设置。模板见 [`.env.example`](../.env.example)。优先级为命令行 > 环境变量 > 工作目录中的 `.env`；修改启动配置后需重启。

## 常用项

| 环境变量 | 默认 | 用途 |
|---|---|---|
| `HOST` | `127.0.0.1` | 监听地址；直接接受外部连接时改为 `0.0.0.0` |
| `PORT` | `8787` | 监听端口 |
| `ADMIN_TOKEN` | 空 | 管理与恢复令牌；模型调用另用 API Key |
| `DATA_DIR` | `data` | 数据目录 |
| `DB_PATH` | `<DATA_DIR>/work2api.db` | 数据库位置 |
| `LOG_LEVEL` | `INFO` | 日志级别 |
| `LOG_TO_FILE` | `1` | 是否写日志文件 |

设置非空 `ADMIN_TOKEN` 后，默认允许外部 Host，并为公网域名启用 Secure Cookie；不会自动改变监听地址。真实回环 HTTP 访问保留本地登录兼容。`ALLOW_EXTERNAL_HOST`、`ADMIN_COOKIE_SECURE` 只用于覆盖自动策略，旧显式配置仍优先。反代与容器差异见 [部署说明](deployment.md)。

额度刷新、签到、模型目录、门户注册、备份和更新策略优先在管理台「设置」中调整。

## 请求与响应保护

模型调用不设网关本地的执行并发、排队、每日次数 / Tokens 或账号请求间隔。Key 权限、模型真实能力与上游额度、限流仍生效。

| 环境变量 | 默认 | 作用 |
|---|---|---|
| `MAX_REQUEST_BYTES` | `268435456` | 单次请求体 256 MiB |
| `REQUEST_BODY_READ_TIMEOUT_SECONDS` | `120` | 接收请求体的期限，不限制模型思考或响应时长 |
| `MAX_RESPONSE_BYTES` | `134217728` | 非流式响应 128 MiB；流式不限制累计大小，单行 / 单事件保护为 40 MiB |
| `MAX_JSON_ITEMS` / `MAX_JSON_DEPTH` | `1000000` / `512` | JSON 结构保护；各自设为 0 可关闭 |
| `REQUEST_BODY_BUDGET` | `0` | 总请求体缓冲预算，0 表示不附加预算 |

管理、登录和授权接口仍有独立保护。日志存储限制只影响历史记录，不截断实际模型回答。

## 日志保留

| 环境变量 | 默认 | 作用 |
|---|---|---|
| `USAGE_RETENTION_DAYS` | `90` | 调用记录保留天数 |
| `USAGE_MAX_ROWS` | `100000` | 调用记录行数上限 |
| `USAGE_CONTENT_MAX_BYTES` | `32768` | 每段输入 / 输出 / 思考保存前缀，单位字节 |
| `USAGE_CONTENT_RETENTION_DAYS` | `7` | 日志正文保留天数 |
| `USAGE_CONTENT_BUDGET_BYTES` | `536870912` | 全部日志正文预算 512 MiB |
| `USAGE_CHECK_INTERVAL_MIN` | `30` | 清理检查周期，单位分钟 |

正文超期或超预算时，分批清理最旧正文，保留对应记录的 Tokens、消费、状态和性能。正文预算不是数据库文件硬上限，清理后正文搜索不再命中。上述天数及存储上限可设为 0 关闭对应规则；清理不会删除已有完整备份中的历史内容。

## Responses 历史续接

| 环境变量 | 默认 | 作用 |
|---|---|---|
| `RESPONSE_HISTORY_BYTES` | `268435456` | 全进程续接历史缓存预算 256 MiB，按需使用，不预分配 |
| `RESPONSE_HISTORY_TTL_SECONDS` | `21600` | 响应 ID 有效期 6 小时 |

任一项设为 0 可关闭网关历史缓存。历史按 API Key 和用户隔离，轮次共享已有内容；过期或超预算淘汰旧 ID。预算不等于进程总内存上限，恢复请求仍需解析完整上下文。响应捕获使用最多 64 MiB 的临时文件，完成后删除；捕获失败或超出预算不会阻断模型响应，但该响应可能无法按 ID 续接。
