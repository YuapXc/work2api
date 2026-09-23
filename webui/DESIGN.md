# work2api WebUI — 设计系统（信号台 / Instrument）

> 立意：work2api 是把多个上游多路复用成单一入口的网关，主题是「路由 / 信号 / 流量调度」。
> UI 按**运维仪表台**设计，不是网站。数字=等宽读数，状态=信号色 LED，连线=布线。
> 唯一的大动作：概览页「状态总线」。其余安静克制。深色为主。

## Color tokens（CSS 变量，:root）
```
--ink:    #0C171B   /* base 背景，深石油蓝 */
--ink-2:  #0F1E24   /* 左侧 spine / chrome */
--panel:  #13242B   /* 抬起面板 */
--panel-2:#172D35   /* 嵌套面板 / 表头 */
--line:   #24454F   /* 1px 描边 / 布线 */
--line-2: #1B363E   /* 更弱分隔线 */
--fog:    #7E9BA3   /* 次要文字 */
--paper:  #E6F0EE   /* 主文字 */
--live:   #35D0A5   /* 健康 / 正在服务 / 成功（去饱和青绿，非荧光绿）*/
--charge: #E9B44C   /* 额度 / 警告 / 登录中 */
--fault:  #F2795E   /* 冷却 / 错误 / 禁用（珊瑚，比朱红柔）*/
--route:  #5AA9E6   /* 链接 / 信息 / 选中导航 / focus 环 */
```
语义即含义，颜色绝不做装饰：健康/serving/成功→live；额度/警告→charge；禁用/冷却/错误→fault；可交互/链接/选中→route。

## Type
- UI/标题/正文：**Space Grotesk**（@fontsource/space-grotesk，400/500/700）
- 数字读数/ID/Key/代码：**JetBrains Mono**（@fontsource/jetbrains-mono，400/500）
- 字阶（≈1.25）：12 / 13 / 14(base) / 16 / 20 / 25 / 31 px；正文 line-height 1.5，标题 1.15。
- 规则：句首大写标签，**不要 ALL-CAPS eyebrow**；mono 只给数字/ID/Key/代码，**绝不给文字标签**；标题不做单词高亮；按钮不加 →；不用中点 meta 串（A · B · C）。

## Layout
- 左侧「设备脊柱」rail（~208px）：顶部网关状态 LED + `work2api` 字标；导航项做成仪表开关（选中=route 左缘竖条+提亮，不是 AntD 填充菜单）；底部实时健康账号读数（●2，30s 轮询）。<820px 收成顶栏。
- 内容区 max-width ~1180，全左对齐；表格内数字右对齐。
- 面板：1px --line 描边，圆角 6px，**无投影**（仪表不悬浮）；层级靠尺寸/描边不是阴影。
- 概览 hero「状态总线」：一条横向仪表带 —— client 节点 →(信号)→ 网关核心(serving●/degraded) → 账号池(一排小方块节点，按健康染 live/fault/fog) → 上游站点(cn/intl chip)。下方读数瓦片行（账号/模型/额度/今日请求/今日 tokens）：mono 数字 + 小 sans 标签 + 细语义下划线；扁平、hairline 分隔，**不是渐变卡片**。
- 表格（账号/模型/记录/用量/应用）：深色，hairline 行分隔，数字/ID/token 列用 mono 右对齐，状态=小 LED 点+标签。

## AntD 主题
ConfigProvider + `theme.darkAlgorithm`，token 映射 colorPrimary=route、容器背景=panel、colorText=paper、fontFamily=Space Grotesk、borderRadius=6；再加定向 CSS 覆盖 Menu(脊柱)/Table 表头/Tag(信号色)/Button。保留 AntD 组件以复用已跑通的逻辑，只换皮。

## Motion
仅一处入场：概览状态总线载入时信号沿总线扫一遍（尊重 prefers-reduced-motion）。可交互元素才有 hover。**不做每卡片入场动画**。

## 反模板自查（已规避）
非 cream+serif+terracotta；非近黑+单荧光色（用石油蓝底+四语义色，绿是去饱和青绿）；hairline 只作布线不做报纸栏；不做统一圆角+软阴影卡片墙；不用 ALL-CAPS eyebrow/中点 meta/spaced em-dash/按钮 →/mono 文字标签。

## 铁律
所有 API 调用与操作逻辑保持不变，只改表现层。六页信息显示与操作照常工作。
