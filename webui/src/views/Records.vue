<script setup lang="ts">
import { ref, reactive, onMounted, onUnmounted } from 'vue'
import { message } from 'ant-design-vue'
import { BulbOutlined, RobotOutlined, InboxOutlined, WarningOutlined, ReloadOutlined, FilterOutlined, DownloadOutlined } from '@ant-design/icons-vue'
import { api } from '@/api/client'
import type { AccountInfo, UsageRecord } from '@/types'
import { buildLabelMap, labelOf as labelOfMap } from '@/utils/accountLabel'
import dayjs from 'dayjs'

const records = ref<UsageRecord[]>([])
const loading = ref(false)
let loadSeq = 0

// uid → 显示名（含重名消歧）。使用记录里的 account_uid 靠它翻译成可读标签。
// 账号删除后历史记录仍带旧 uid，labelOf 会回退为「已移除 · 短码」。
const accounts = ref<AccountInfo[]>([])
const labelMap = ref<Record<string, string>>({})
const labelOf = (uid?: string | null) => labelOfMap(labelMap.value, uid)

// 服务端分页：总数由后端返回，翻页时向后端取对应页
// （旧版是本地分页：只翻一次性拉回来的那批，拉 100 条就只有 5 页）
const pagination = reactive({
  current: 1,
  pageSize: 10,
  total: 0,
  showSizeChanger: true,
  pageSizeOptions: ['10', '20', '50'],
  showTotal: (t: number) => `共 ${t} 条`,
})

// 筛选项（协议 / 模型 / 应用 / 状态）
const filters = ref<{ protocol?: string; model?: string; app_name?: string; status?: string; search?: string }>({})
const filterOptions = ref<{
  protocols: string[]
  models: string[]
  apps: string[]
  apps_history: string[]
  has_unnamed?: boolean
  statuses: string[]
}>({ protocols: [], models: [], apps: [], apps_history: [], statuses: [] })

const detailVisible = ref(false)
const detail = ref<UsageRecord | null>(null)
const detailLoading = ref(false)
let detailSeq = 0
const exporting = ref(false)

async function load() {
  const seq = ++loadSeq
  loading.value = true
  try {
    const res = await api.usageRecent(pagination.current, pagination.pageSize, { ...filters.value })
    // 旧筛选请求可能晚于新请求返回，不能覆盖用户当前看到的结果。
    if (seq !== loadSeq) return
    records.value = res.records ?? []
    pagination.total = res.total ?? 0
    // 当前页超出总页数（如筛选后记录变少）时回退到最后一页
    const lastPage = Math.max(1, Math.ceil(res.total / pagination.pageSize))
    if (pagination.current > lastPage) {
      pagination.current = lastPage
      await load()
    }
  } catch { /* 拦截器已提示 */
  } finally {
    if (seq === loadSeq) loading.value = false
  }
}

function onTableChange(p: { current?: number; pageSize?: number }) {
  pagination.current = p.current || 1
  pagination.pageSize = p.pageSize || 20
  load()
}

async function loadFilters() {
  const r: any = await api.usageFilters()
  filterOptions.value = {
    protocols: r.protocols ?? [],
    models: r.models ?? [],
    apps: r.apps ?? [],
    apps_history: r.apps_history ?? [],
    statuses: r.statuses ?? [],
    has_unnamed: r.has_unnamed ?? false,
  }
}

/** 拉一次账号列表用于把 account_uid 翻译成显示名（别名可能随时改，故每次进页面刷新）。 */
async function loadAccounts() {
  try {
    const res = await api.accounts()
    accounts.value = res.accounts ?? []
    labelMap.value = buildLabelMap(res.accounts ?? [])
  } catch { /* 拦截器已提示；映射失败时 labelOf 退回 uid 短码 */ }
}

function onFilterChange() {
  // 筛选变化后回到第 1 页
  pagination.current = 1
  load()
}

function clearFilters() {
  filters.value = {}
  pagination.current = 1
  load()
}

// 搜索输入防抖：内容搜索是 LIKE 全表扫描，每敲一个字就请求会拖慢后端，
// 且用户还没输完时结果无意义。400ms 兼顾跟手与压力。
let searchTimer: ReturnType<typeof setTimeout> | undefined
function onSearchInput() {
  clearTimeout(searchTimer)
  searchTimer = setTimeout(() => onFilterChange(), 400)
}

// 点搜索图标/回车：取消待触发的防抖，立即查一次，避免同一次交互发两次请求
function onSearchSubmit() {
  clearTimeout(searchTimer)
  onFilterChange()
}

// 组件卸载时清掉待触发的定时器，避免对已销毁组件调用 load（历史上有轮询泄漏 bug）
onUnmounted(() => { clearTimeout(searchTimer); loadSeq++; detailSeq++ })

// 是否有生效中的筛选。
// 注意 app_name 允许空字符串（=筛"未记录应用"），必须用 != null 判断；
// 其余字段（含新增的 search）空串表示"未筛选"，若也用 != null 判断，
// 用户只要点一下搜索框让它变成 ''，「清除筛选」按钮就会常驻显示。
const hasFilter = () =>
  Object.entries(filters.value).some(([k, v]) =>
    k === 'app_name' ? v != null : v !== undefined && v !== null && v !== '')

async function openDetail(r: UsageRecord) {
  const seq = ++detailSeq
  // 列表是 light 投影（不含大文本 content）：先展示元数据，再按需拉取完整内容
  detail.value = r
  detailVisible.value = true
  detailLoading.value = true
  try {
    const res = await api.usageDetail(r.id)
    if (seq === detailSeq && detailVisible.value) detail.value = res.record
  } catch { /* 拦截器已提示 */ } finally {
    if (seq === detailSeq) detailLoading.value = false
  }
}

function fmtTime(ts: number) {
  return dayjs(ts * 1000).format('YYYY-MM-DD HH:mm:ss')
}

function fmtLatency(ms: number | null | undefined) {
  const v = ms ?? 0
  if (v >= 60000) return `${(v / 60000).toFixed(1)}m`
  return `${(v / 1000).toFixed(1)}s`
}

function effortLabel(effort: string | null | undefined) {
  if (effort == null) return '未记录'
  if (effort === 'default') return '默认'
  if (effort === 'off') return '关闭'
  return effort
}

function effortHint(effort: string | null | undefined) {
  if (effort == null) return '旧记录未采集思考强度'
  if (effort === 'default') return '请求未指定思考强度，使用上游默认档位'
  return '最终发送给上游的思考强度'
}

// 导出当前筛选下的全部记录为 CSV（含 BOM 防 Excel 中文乱码）
// 服务端分页后列表里只有当前页，这里循环拉取所有页（light 投影，只含导出所需的元数据列）
async function exportCsv() {
  if (!pagination.total) {
    message.warning('没有可导出的记录')
    return
  }
  exporting.value = true
  try {
    const all: UsageRecord[] = []
    const pageSize = 200
    const maxPages = 50 // 封顶 1 万条，防止误操作拖垮后端
    for (let p = 1; p <= maxPages && all.length < pagination.total; p++) {
      const res = await api.usageRecent(p, pageSize, filters.value)
      const batch = res.records ?? []
      all.push(...batch)
      if (batch.length < pageSize) break
    }
    if (!all.length) {
      message.warning('没有可导出的记录')
      return
    }
    writeCsv(all)
    message.success(`已导出 ${all.length} 条记录${hasFilter() ? '（按当前筛选条件）' : '（全部记录）'}`)
  } catch { /* 拦截器已提示 */ } finally {
    exporting.value = false
  }
}

function writeCsv(rowsAll: UsageRecord[]) {
  const headers = ['时间', '协议', '模型', '账号', '账号UID', '应用', '输入Tokens', '输出Tokens', '思考强度', '积分', '耗时ms', '状态', '错误']
  const rows = rowsAll.map((r) => [
    fmtTime(r.ts),
    r.protocol || '',
    r.model || '',
    // 显示名（别名/昵称/短码）；单独留一列原始 uid，方便与数据库核对
    labelOf(r.account_uid),
    r.account_uid || '',
    r.app_name || '',
    r.input_tokens || 0,
    r.output_tokens || 0,
    effortLabel(r.reasoning_effort),
    r.credits ?? '',
    Math.round(r.latency_ms || 0),
    r.status || '',
    (r.error || '').replace(/[\r\n]+/g, ' '),
  ])
  const esc = (v: unknown) => `"${String(v ?? '').replace(/"/g, '""')}"`
  const csv = [headers, ...rows].map((row) => row.map(esc).join(',')).join('\r\n')
  const blob = new Blob(['\uFEFF' + csv], { type: 'text/csv;charset=utf-8' })
  const a = document.createElement('a')
  a.href = URL.createObjectURL(blob)
  a.download = `usage_${dayjs().format('YYYY-MM-DD_HH-mm')}.csv`
  document.body.appendChild(a)
  a.click()
  document.body.removeChild(a)
  URL.revokeObjectURL(a.href)
}

onMounted(async () => {
  // 三个请求并行：账号列表只用于把 uid 翻译成显示名，失败也不阻塞记录加载
  await Promise.allSettled([loadFilters(), loadAccounts()])
  await load()
})
</script>

<template>
  <div style="width: 100%">
    <div class="view-header">
      <div>
        <div class="vh-title">记录</div>
        <div class="vh-meta">
          <span class="led route"></span>
          共 <span class="mono">{{ pagination.total }}</span> 条调用日志
        </div>
      </div>
      <div class="vh-actions">
        <a-button @click="load"><ReloadOutlined />刷新</a-button>
        <a-button type="primary" @click="exportCsv" :loading="exporting" :disabled="!pagination.total"><DownloadOutlined />导出 CSV</a-button>
      </div>
    </div>

    <div style="margin-bottom: 12px; display: flex; gap: 8px; align-items: center; flex-wrap: wrap">
      <span style="color: var(--fog); font-size: 13px; display: inline-flex; align-items: center; gap: 5px">
        <FilterOutlined />筛选
      </span>
      <a-select v-model:value="filters.protocol" placeholder="全部协议" allow-clear style="width: 130px" @change="onFilterChange">
        <a-select-option v-for="p in filterOptions.protocols" :key="p" :value="p">{{ p }}</a-select-option>
      </a-select>
      <a-select v-model:value="filters.model" placeholder="全部模型" allow-clear show-search style="width: 180px" @change="onFilterChange">
        <a-select-option v-for="m in filterOptions.models" :key="m" :value="m">{{ m }}</a-select-option>
      </a-select>
      <a-select v-model:value="filters.app_name" placeholder="全部应用" allow-clear style="width: 150px" @change="onFilterChange">
        <!-- 只列现存应用（与应用页一致）；已删除应用的历史记录名单独分组，避免对不上 -->
        <a-select-opt-group v-if="filterOptions.apps.length" label="应用">
          <a-select-option v-for="a in filterOptions.apps" :key="a" :value="a">{{ a }}</a-select-option>
        </a-select-opt-group>
        <a-select-opt-group v-if="filterOptions.apps_history.length" label="历史应用（已删除）">
          <a-select-option v-for="a in filterOptions.apps_history" :key="a" :value="a">{{ a }}</a-select-option>
        </a-select-opt-group>
        <a-select-option v-if="filterOptions.has_unnamed" value="">（未记录应用）</a-select-option>
      </a-select>
      <a-select v-model:value="filters.status" placeholder="全部状态" allow-clear style="width: 120px" @change="onFilterChange">
        <a-select-option v-for="s in filterOptions.statuses" :key="s" :value="s">{{ s === 'ok' ? '成功' : s }}</a-select-option>
      </a-select>
      <a-input-search
        v-model:value="filters.search"
        placeholder="搜索对话内容"
        allow-clear
        style="width: 200px"
        @search="onSearchSubmit"
        @input="onSearchInput"
      />
      <a-button v-if="hasFilter()" size="small" @click="clearFilters">清除筛选</a-button>
    </div>
    <a-card title="使用记录">
      <a-table
        :data-source="records"
        :loading="loading"
        row-key="id"
        :pagination="pagination"
        @change="onTableChange"
        :scroll="{ x: 1050 }"
        size="small"
        table-layout="fixed"
      >
        <a-table-column title="时间" key="ts" :width="120">
          <template #default="{ record }"><span class="mono">{{ fmtTime(record.ts) }}</span></template>
        </a-table-column>
        <a-table-column title="协议" data-index="protocol" key="protocol" :width="70" />
        <a-table-column title="模型" data-index="model" key="model" :width="150" ellipsis>
          <template #default="{ record }"><span class="mono">{{ record.model }}</span></template>
        </a-table-column>
        <!-- 多账号场景：显示这次调用落到哪个账号，便于排查与成本归因 -->
        <a-table-column title="账号" key="account" :width="130" ellipsis>
          <template #default="{ record }">
            <a-tooltip :title="record.account_uid || ''">
              <span :style="{ color: labelMap[record.account_uid] ? '#9db3cf' : '#5b6476' }">
                {{ labelOf(record.account_uid) }}
              </span>
            </a-tooltip>
          </template>
        </a-table-column>
        <a-table-column title="应用" key="app" :width="100">
          <template #default="{ record }">
            <a-tag v-if="record.app_name" color="purple" style="max-width: 100%; overflow: hidden; text-overflow: ellipsis">{{ record.app_name }}</a-tag>
            <span v-else style="color: #5b6476">-</span>
          </template>
        </a-table-column>
        <a-table-column title="Tokens" key="tokens" :width="110" align="right">
          <template #default="{ record }"><span class="mono">{{ record.input_tokens || 0 }}/{{ record.output_tokens || 0 }}</span></template>
        </a-table-column>
        <a-table-column title="思考强度" key="reasoning_effort" :width="90" align="center">
          <template #default="{ record }">
            <a-tooltip :title="effortHint(record.reasoning_effort)">
              <span :style="{ color: record.reasoning_effort == null ? '#5b6476' : '#c4b5fd' }">{{ effortLabel(record.reasoning_effort) }}</span>
            </a-tooltip>
          </template>
        </a-table-column>
        <a-table-column title="积分" key="credits" :width="80" align="right">
          <template #default="{ record }">
            <span v-if="record.credits" class="mono">{{ record.credits.toFixed(2) }}</span>
            <a-tooltip v-else-if="record.status === 'ok' && record.credit_known === true && record.credits === 0" title="上游明确返回 0 积分">
              <span style="color: var(--live)">免费</span>
            </a-tooltip>
            <a-tooltip v-else-if="record.status === 'ok'" title="上游积分未返回或旧版未保存，无法判断是否收费">
              <span style="color: var(--fog)">未知</span>
            </a-tooltip>
            <span v-else style="color: var(--fog)">-</span>
          </template>
        </a-table-column>
        <a-table-column title="耗时" key="latency_ms" :width="80" align="right">
          <template #default="{ record }">
            <a-tooltip :title="`${Math.round(record.latency_ms || 0)} ms`"><span class="mono">{{ fmtLatency(record.latency_ms) }}</span></a-tooltip>
          </template>
        </a-table-column>
        <a-table-column title="状态" key="status" :width="70">
          <template #default="{ record }">
            <span class="cell-status">
              <span class="led" :class="record.status === 'ok' ? 'live' : 'fault'"></span>
              {{ record.status === 'ok' ? '成功' : record.status }}
            </span>
          </template>
        </a-table-column>
        <a-table-column title="操作" key="action" :width="70">
          <template #default="{ record }">
            <a-button size="small" type="link" @click="openDetail(record)">详情</a-button>
          </template>
        </a-table-column>
      </a-table>
    </a-card>

    <!-- 详情弹窗 -->
    <a-modal
      v-model:open="detailVisible"
      :title="detail ? `请求详情 · ${detail.model} (${detail.protocol})` : '请求详情'"
      :footer="null"
      width="720px"
    >
      <a-spin :spinning="detailLoading" tip="加载详情…">
        <div v-if="detail">
          <div style="margin-bottom: 12px; color: #8a94a6; font-size: 12px">
            {{ fmtTime(detail.ts) }} · {{ detail.input_tokens || 0 }}/{{ detail.output_tokens || 0 }} tokens ·
            {{ Math.round(detail.latency_ms || 0) }}ms
            · <a-tooltip :title="effortHint(detail.reasoning_effort)"><span>思考强度 {{ effortLabel(detail.reasoning_effort) }}</span></a-tooltip>
            <template v-if="detail.credits"> · <span style="color: #fbbf24">{{ detail.credits.toFixed(2) }} 积分</span></template>
            <template v-else-if="detail.status === 'ok' && detail.credit_known === true && detail.credits === 0"> · <a-tooltip title="上游明确返回 0 积分"><span style="color: #22c55e">免费</span></a-tooltip></template>
            <template v-else-if="detail.status === 'ok'"> · <a-tooltip title="上游积分未返回或旧版未保存，无法判断是否收费"><span style="color: #8a94a6">积分未知</span></a-tooltip></template>
            · 账号 <a-tooltip :title="detail.account_uid || ''"><span style="color: #9db3cf">{{ labelOf(detail.account_uid) }}</span></a-tooltip>
            <a-tag :color="detail.status === 'ok' ? 'green' : 'red'" style="margin-left: 6px">{{ detail.status }}</a-tag>
          </div>

          <!-- 思考链 / COT -->
          <div v-if="detail.reasoning_content" class="sec">
            <div class="sec-title reasoning"><BulbOutlined style="margin-right: 5px" />思考链 (COT)</div>
            <pre class="content-box reasoning">{{ detail.reasoning_content }}</pre>
          </div>

          <!-- 输出 -->
          <div class="sec">
            <div class="sec-title output"><RobotOutlined style="margin-right: 5px" />模型输出</div>
            <pre class="content-box" v-if="detail.output_content">{{ detail.output_content }}</pre>
            <div v-else class="empty">（无输出内容）</div>
          </div>

          <!-- 输入 -->
          <div class="sec">
            <div class="sec-title input"><InboxOutlined style="margin-right: 5px" />请求输入</div>
            <pre class="content-box input" v-if="detail.input_content">{{ detail.input_content }}</pre>
            <div v-else class="empty">（无输入内容）</div>
          </div>

          <div v-if="detail.error" class="sec">
            <div class="sec-title" style="color: #ff6b6b"><WarningOutlined style="margin-right: 5px" />错误</div>
            <pre class="content-box error">{{ detail.error }}</pre>
          </div>
        </div>
      </a-spin>
    </a-modal>
  </div>
</template>

<style scoped>
.cell-status { display: inline-flex; align-items: center; gap: 6px; color: var(--paper); }
.sec { margin-top: 14px; }
.sec-title { font-size: 13px; font-weight: 500; margin-bottom: 6px; color: var(--paper); }
.sec-title.reasoning { color: #A9C4CB; }
.sec-title.output { color: var(--route); }
.sec-title.input { color: var(--live); }
.content-box {
  background: var(--ink-2); border: 1px solid var(--line);
  border-radius: var(--radius); padding: 12px; max-height: 260px; overflow: auto;
  white-space: pre-wrap; word-break: break-word;
  font-size: 12.5px; line-height: 1.55; color: var(--paper); margin: 0;
  font-family: var(--mono);
}
.content-box.reasoning { border-color: rgba(126,155,163,0.3); color: #C7D8DC; }
.content-box.input { border-color: rgba(53,208,165,0.25); }
.content-box.error { border-color: rgba(242,121,94,0.4); color: var(--fault); }
.empty { color: var(--fog); font-size: 12px; }
</style>
