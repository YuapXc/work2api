<script setup lang="ts">
// 模型目录展示面板：管理台「模型」页与用户门户「模型」页共用。
// 通过 refresh / aa-refresh 事件把「刷新」动作交回各自的页面，
// 因为管理端与用户端的刷新接口和权限完全不同。
import {
  PictureOutlined,
  FileTextOutlined,
  ToolOutlined,
} from '@ant-design/icons-vue'
import type { ModelAccount, ModelInfo } from '@/types'

const props = withDefaults(
  defineProps<{
    models: ModelInfo[]
    source?: 'dynamic' | 'static'
    loading?: boolean
    /** 是否展示"可用账号"标签。默认关闭——该组件被用户门户共用，
     *  账号池构成属于管理端信息，不应暴露给普通用户。 */
    showAccounts?: boolean
  }>(),
  { source: 'dynamic', loading: false, showAccounts: false },
)

defineEmits<{ refresh: [] }>()

// ---------------- 可用账号标签 ----------------

/** 单个账号的实时状态文案（冷却/停用/额度耗尽/可用）。 */
function accountStateText(a: ModelAccount): string {
  if (a.enabled === false) return '已停用'
  if (a.cooldown_until && a.cooldown_until > Date.now() / 1000) {
    if (a.model_cooldown) {
      const reset = new Date(a.cooldown_until * 1000).toLocaleString('zh-CN', {
        month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hour12: false,
      })
      return `该模型已达每日上限（${reset} 恢复）`
    }
    const sec = Math.ceil(a.cooldown_until - Date.now() / 1000)
    return `冷却中（${sec < 60 ? sec + ' 秒' : Math.ceil(sec / 60) + ' 分钟'}）`
  }
  if (a.healthy === false) return '额度耗尽'
  return '可用'
}

/**
 * 标签颜色：只表达**可用性**（选模型时真正关心的），
 * 站点差异由标签文字下的悬停说明承担，避免一个标签同时承载两种语义导致误读。
 */
function accountTagColor(a: ModelAccount): string {
  const st = accountStateText(a)
  if (st === '可用') return 'green'
  if (st.startsWith('冷却中') || st.startsWith('该模型')) return 'orange'
  return 'default'
}

function fmtTokens(v?: number) {
  if (!v || v <= 0) return '-'
  if (v >= 1000000) return `${(v / 1000000).toFixed(1)}M`
  return `${Math.round(v / 1000)}K`
}

function fmtName(m: ModelInfo) {
  return m.name && m.name !== m.id ? m.name : m.id
}

function reasoningEfforts(m: ModelInfo): string[] {
  const r = m.reasoning
  if (!r?.supportedEfforts?.length) {
    if (r?.defaultEffort) return [r.defaultEffort]
    return []
  }
  const efforts = [...r.supportedEfforts]
  if (r.canDisableThinking && !efforts.includes('off')) efforts.push('off')
  return efforts
}

function reasoningTag(m: ModelInfo) {
  const r = m.reasoning
  if (!r || !r.supportsReasoning) return null
  if (r.canDisableThinking) return { text: '可关思考', color: 'green' }
  if (r.onlyReasoning) return { text: '仅思考', color: 'blue' }
  return { text: '思考', color: 'cyan' }
}
</script>

<template>
  <div>
    <!-- 头部：搜索 + 统计 + (可选)刷新 -->
    <div class="toolbar">
      <slot name="toolbar" />
      <a-tag class="src-tag" :class="props.source === 'dynamic' ? 'src-dyn' : 'src-static'">
        {{ props.source === 'dynamic' ? '动态（来自上游）' : '静态兜底' }}
      </a-tag>
      <span class="count">共 {{ models.length }} 个模型</span>
    </div>

    <div class="metric-strip">
      <div class="metric-chip"><span class="m-num">{{ models.length }}</span><span class="m-label">模型</span></div>
      <div class="metric-chip"><span class="m-num">{{ models.filter((m) => m.modality === 'multimodal').length }}</span><span class="m-label">多模态</span></div>
      <div class="metric-chip"><span class="m-num">{{ models.filter((m) => m.supportsToolCall).length }}</span><span class="m-label">支持工具</span></div>
    </div>

    <a-spin :spinning="loading">
      <div v-if="!models.length" class="empty-state">
        <p class="empty-title">暂无模型数据</p>
        <p class="empty-desc">
          需要管理员先在「账号」页配置账号并刷新模型目录，之后这里会显示真实可用的模型。
        </p>
      </div>
      <div v-else class="card-grid">
        <div v-for="m in models" :key="m.id" class="model-card" :class="m.modality">
          <div class="card-head">
            <div class="card-title-row">
              <span class="model-name">{{ fmtName(m) }}</span>
              <a-tag v-if="m.provider === 'qoder'" color="cyan">Qoder</a-tag>
              <a-tag :color="m.modality === 'multimodal' ? 'purple' : 'default'" class="modality-tag">
                <PictureOutlined v-if="m.modality === 'multimodal'" style="margin-right: 4px" /><FileTextOutlined v-else style="margin-right: 4px" />{{ m.modality === 'multimodal' ? '多模态' : '纯文本' }}
              </a-tag>
            </div>
            <code class="model-id">{{ m.id }}</code>
          </div>

          <div class="tag-row">
            <a-tag v-if="reasoningTag(m)" :color="reasoningTag(m)!.color">{{ reasoningTag(m)!.text }}</a-tag>
            <a-tag v-if="m.supportsToolCall" color="gold"><ToolOutlined style="margin-right: 4px" />工具调用</a-tag>
            <a-tag v-for="e in reasoningEfforts(m)" :key="e" color="purple" class="effort-tag">{{ e }}</a-tag>
          </div>

          <div class="metric-grid">
            <div class="mi"><span class="mi-k">成本</span><span class="mi-v">{{ m.credits !== undefined && m.credits !== null ? `x${m.credits.toFixed(2)}` : '-' }}</span></div>
            <div class="mi"><span class="mi-k">上下文</span><span class="mi-v">{{ fmtTokens(m.context_length) }}</span></div>
            <div class="mi"><span class="mi-k">最大输出</span><span class="mi-v">{{ fmtTokens(m.max_output_tokens) }}</span></div>
            <div class="mi"><span class="mi-k">温度</span><span class="mi-v">{{ m.temperature !== undefined ? m.temperature : '-' }}</span></div>
          </div>

          <div v-if="m.description" class="desc">{{ m.description }}</div>

          <!-- 可调用账号：多账号场景下说明"这个模型能用哪些账号跑"。
               标签文字**直接显示账号别名**（用户自己起的名字最有辨识度）；
               颜色只表达**可用性**（绿=可用、橙=冷却、灰=停用/额度耗尽），
               站点信息放在悬停里——一个标签不同时承载两种语义，避免误读。
               悬停明细：别名 / 站点(中文) · 状态 · uid 短码。
               仅在管理端渲染（showAccounts），避免朋友入口看到账号池构成。 -->
          <div v-if="showAccounts && (m.accounts?.length || m.profiles?.length)" class="acct-row">
            <span class="acct-k">可用账号</span>
            <a-tooltip v-for="a in (m.accounts || [])" :key="a.uid">
              <template #content>
                <div style="white-space: nowrap">{{ a.label }}</div>
                <div style="white-space: nowrap; color: #9db3cf">
                  {{ a.site_label || a.site || '未知' }} · {{ accountStateText(a) }} · {{ (a.uid || '').slice(0, 8) }}
                </div>
              </template>
              <a-tag :color="accountTagColor(a)" class="acct-tag">{{ a.label }}</a-tag>
            </a-tooltip>
            <a-tag v-if="!m.accounts?.length" color="default" class="acct-tag">暂无可用账号</a-tag>
          </div>
        </div>
      </div>
    </a-spin>
  </div>
</template>

<style scoped>
.toolbar {
  display: flex; gap: 10px; flex-wrap: wrap; align-items: center; margin-bottom: 14px;
}
.src-tag { margin-left: 4px; }
.src-dyn { background: rgba(90,169,230,0.14) !important; color: var(--route) !important; border-color: rgba(90,169,230,0.4) !important; }
.src-static { background: rgba(233,180,76,0.14) !important; color: var(--charge) !important; border-color: rgba(233,180,76,0.4) !important; }
.count { color: var(--fog); font-size: 13px; margin-left: auto; }

.metric-strip { display: flex; gap: 0; margin-bottom: 18px; border: 1px solid var(--line); border-radius: var(--radius); overflow: hidden; background: var(--panel); width: fit-content; }
.metric-chip {
  display: flex; align-items: baseline; gap: 8px;
  padding: 12px 20px; color: var(--paper);
  border-left: 1px solid var(--line-2);
}
.metric-chip:first-child { border-left: none; }
.m-num { font-size: 20px; font-weight: 500; color: var(--paper); font-family: var(--mono); }
.m-label { font-size: 12px; color: var(--fog); }

.empty-state {
  padding: 48px 24px; text-align: center; color: var(--fog);
  background: var(--panel);
  border: 1px solid var(--line); border-radius: var(--radius);
}
.empty-title { font-size: 15px; font-weight: 500; color: var(--paper); margin-bottom: 8px; }
.empty-desc { font-size: 13px; max-width: 480px; margin: 0 auto; line-height: 1.6; }

.card-grid {
  display: grid; grid-template-columns: repeat(auto-fill, minmax(320px, 1fr));
  gap: 12px;
}
.model-card {
  position: relative; border-radius: var(--radius); padding: 16px;
  background: var(--panel);
  border: 1px solid var(--line);
  color: var(--paper);
  transition: border-color 0.15s ease;
}
.model-card:hover { border-color: var(--route); }

.card-head { margin-bottom: 10px; }
.card-title-row { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.model-name { font-size: 16px; font-weight: 500; }
.modality-tag { margin-left: auto; }
.modality-tag.ant-tag { background: rgba(126,155,163,0.14) !important; border-color: rgba(126,155,163,0.35) !important; color: var(--fog) !important; }
.model-card.multimodal .modality-tag.ant-tag { background: rgba(126,155,163,0.16) !important; border-color: rgba(126,155,163,0.4) !important; color: #A9C4CB !important; }
.model-id { font-size: 12px; color: var(--fog); font-family: var(--mono); }

.tag-row { display: flex; flex-wrap: wrap; gap: 4px; margin-bottom: 12px; }
.effort-tag { margin-right: 0 !important; font-family: var(--mono); }

.metric-grid {
  display: grid; grid-template-columns: repeat(2, 1fr); gap: 8px;
  background: var(--panel-2); border: 1px solid var(--line-2); border-radius: var(--radius); padding: 10px 14px; margin-bottom: 10px;
}
.mi { display: flex; justify-content: space-between; align-items: center; min-width: 0; }
.mi-k { font-size: 12px; color: var(--fog); flex-shrink: 0; }
.mi-v { font-size: 14px; font-weight: 500; color: var(--paper); padding-left: 8px; white-space: nowrap; font-family: var(--mono); }

.desc { font-size: 12px; color: var(--fog); line-height: 1.5; margin-bottom: 10px; }

.acct-row {
  display: flex; flex-wrap: wrap; gap: 4px; align-items: center;
  margin-bottom: 2px; padding-top: 8px;
  border-top: 1px solid var(--line-2);
}
.acct-k { font-size: 11px; color: var(--fog); margin-right: 2px; }
.acct-tag {
  margin-inline-end: 0;
  cursor: default;
  max-width: 120px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
</style>
