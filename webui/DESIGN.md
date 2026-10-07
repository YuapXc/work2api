# work2api WebUI — 设计令牌

> 定位：多上游多路复用网关的运维控制台。暗色为默认，`:root.light` 覆盖浅色。
> 权威实现：`src/styles/app.css`；本文只记录令牌与规则，改设计先改 app.css 再同步此文。

## 颜色（CSS 变量，存 "R G B" 三元组，Tailwind `rgb(var(--x)/<alpha>)` 消费）

| 令牌 | 暗色 | 浅色 | 语义 |
|---|---|---|---|
| `--c-bg` | `#090D16` 深蓝黑 | `#F4F7FB` | 页面底 |
| `--c-surface` | `#111825` | `#FFFFFF` | 面板 |
| `--c-elevated` | `#161F2F` | `#F8FAFD` | 悬浮 / 表头 |
| `--c-line` / `--c-hairline` | `#263247` | `#E2E8F0` | 边框 / 内高光 |
| `--c-ink` / `--c-muted` / `--c-faint` | 主→弱三级文字 | 同 | 文字层级 |
| `--c-brand` / `--c-brand-soft` | teal-500/400 | teal-600/500 | 品牌主色 |
| `--c-live` | emerald | emerald-600 | 健康 / 成功 |
| `--c-warn` | amber | amber-600 | 警告 / 额度 |
| `--c-fault` | rose | rose-600 | 错误 / 禁用 / 冷却 |
| `--c-route` | sky | sky-600 | 链接 / 选中 / 信息 |

语义即含义，颜色不做装饰。玻璃质感用 `--glass-bg` + `--glass-alpha` 低透明度叠加，无厚重投影。

## 字体

- UI / 正文：**Inter Variable**（`@fontsource-variable/inter`），回退系统中文无衬线。
- 数字读数 / ID / Key / 代码：**JetBrains Mono**（`@fontsource/jetbrains-mono`），开 `tnum`；`.mono` 工具类只用于数字与标识，不给文字标签。

## 布局与组件

- 侧边导航 + 内容区；窄屏收为顶栏（`App.vue`）。
- 面板：1px `--c-line` 描边 + 轻玻璃叠加，层级靠描边与背景阶，不靠投影。
- 表格数字/ID/token 列 mono 右对齐；状态用小色点 + 标签。
- 图表：ECharts（TrendChart / BarList）。
- 无 UI 组件库：Vue3 + Tailwind 自定义，全部样式走令牌，不写死颜色。

## 动效

可交互元素才有 hover / 过渡；不做每卡片入场动画；尊重 `prefers-reduced-motion`。

## 铁律

表现层与逻辑分离：所有 API 调用与操作逻辑在 `src/api` / views 中，改样式不动请求逻辑。
