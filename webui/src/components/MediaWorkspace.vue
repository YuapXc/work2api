<script setup lang="ts">
import { useRoute } from 'vue-router'
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import WPage from './ui/WPage.vue'
import WSelect from './ui/WSelect.vue'
import WButton from './ui/WButton.vue'
import WEmpty from './ui/WEmpty.vue'
import { confirm } from '@/lib/confirm'

type Row = Record<string, any>
const props = defineProps<{ loadKeys: () => Promise<Row[]>; readKey: (id: number) => Promise<string> }>()
const keys = ref<Row[]>([]), keyID = ref(''), token = ref(''), models = ref<Row[]>([]), modelID = ref('')
const imageParameters = ref('{}')
const decisionBody = ref('{\n  "state": "",\n  "questions": []\n}')
const route=useRoute()
const mode = ref(['image','edit','video','transcription','decision'].includes(String(route.query.mode))?String(route.query.mode):'image'), prompt = ref(''), negative = ref(''), size = ref('1024x1024'), quantity = ref(1)
const seconds = ref(''), resolution = ref(''), ratio = ref(''), audio = ref(true)
const first = ref(''), last = ref(''), references = ref({ images: '', videos: '', audios: '' }), audioURL = ref('')
const uploadImages = ref<string[]>([]), audioFile = ref<File | null>(null)
const uploading = ref(false)
const nextBefore = ref(0)
const listBusy = ref(false)
let listFlight: object | null = null
const tasks = ref<Row[]>([]), selected = ref<Row | null>(null), error = ref(''), loading = ref(false), submitting = ref(false), querying = ref(false)
const model = computed(() => models.value.find(m => m.id === modelID.value))
const config = computed(() => model.value?.video_generation || {})
const tag = computed(() => ({image:'text-to-image',edit:'image-to-image',video:'text-to-video',transcription:'audio-to-text'} as Row)[mode.value])
const available = computed(() => models.value.filter(m => mode.value==='decision' ? m.native_protocol==='systemone' : (m.media_tags || []).includes(tag.value) || mode.value === 'video' && (m.media_tags || []).includes('image-to-video')))
const modelOptions = computed(() => available.value.map(m => ({value:m.id,label:m.name || m.id})))
const keyOptions = computed(() => keys.value.map(k => ({value:String(k.id),label:k.name})))
const isVideo = computed(() => mode.value === 'video')
const mediaOptions = [{value:'image',label:'图片生成'},{value:'edit',label:'图片编辑'},{value:'video',label:'视频生成'},{value:'transcription',label:'音频转写'},{value:'decision',label:'结构化决策'}]
let epoch = 0, mounted = true, ticker: ReturnType<typeof setInterval> | undefined, controller: AbortController | null = null
const requests = new Set<AbortController>()
const lines = (s: string) => s.split('\n').map(s => s.trim()).filter(Boolean)
const terminal = (s: string) => ['completed','failed','cancelled','unknown'].includes(s)
const pollable = (task: Row) => task.can_poll !== false && (!terminal(task.status) || task.status === 'unknown' && task.can_poll === true)
const stateName = (s: string) => ({submitting:'提交中',queued:'排队中',pending:'排队中',in_progress:'处理中',processing:'处理中',completed:'已完成',failed:'失败',cancelled:'已取消',unknown:'结果待确认'} as Row)[s] || s
const date = (n: number) => new Date(n * 1000).toLocaleString()
const safeURL = (s: unknown) => typeof s === 'string' && (s.startsWith('https://') || /^data:image\/(png|jpeg|webp);base64,/.test(s)) ? s : ''
const results = computed<Row[]>(() => (selected.value?.data || []).map((v: Row) => ({...v, src:safeURL(v.url) || (typeof v.b64_json === 'string' ? `data:image/png;base64,${v.b64_json}` : '')})).filter((v: Row) => v.src))
async function request(path: string, body?: unknown, signal?: AbortSignal) {
 const key = token.value
 if (!key) throw new Error('请先选择可查看的 API Key')
 const abort = new AbortController(); requests.add(abort)
 const stop = () => abort.abort(); signal?.addEventListener('abort', stop, {once:true})
 try {
  const res = await fetch(path,{method:body === undefined ? 'GET':'POST',headers:{Authorization:`Bearer ${key}`,...(body instanceof FormData ? {} : {'Content-Type':'application/json'})},body:body === undefined ? undefined : body instanceof FormData ? body : JSON.stringify(body),signal:abort.signal,credentials:'omit'})
  const data = await res.json().catch(() => null)
  if (!res.ok) throw new Error(data?.error?.message || data?.msg || `请求失败（HTTP ${res.status}）`)
  if (!data) throw new Error('服务返回了无效响应')
  return data
 } finally { requests.delete(abort); signal?.removeEventListener('abort',stop) }
}
async function refresh(before=0, merge=false) {
 if (!token.value || loading.value || listBusy.value) return
 const current = epoch, flight = {}
 listFlight = flight; listBusy.value = true
 try {
  const data = await request('/v1/media/tasks'+(before?`?before=${before}`:''))
  if (!mounted || current !== epoch || listFlight !== flight) return
  const incoming: Row[] = data.data || []
  if (before || merge && tasks.value.length) {
   const updated = new Map(incoming.map(task => [task.id, task]))
   const existing = new Set(tasks.value.map(task => task.id))
   const additions = incoming.filter(task => !existing.has(task.id))
   const retained = tasks.value.map(task => updated.get(task.id) || task)
   tasks.value = before ? [...retained,...additions] : [...additions,...retained]
   if (before) nextBefore.value = data.next_before || 0
  } else {
   tasks.value = incoming; nextBefore.value = data.next_before || 0
  }
 } finally { if (listFlight === flight) { listFlight = null; listBusy.value = false } }
}
async function openTask(task: Row, quiet = false) {
 if (querying.value || !token.value) return
 const current = epoch; querying.value = true
 try {
  const data = await request(`/v1/media/tasks/${encodeURIComponent(task.id)}${quiet ? '?cached=1' : ''}`)
  if (!mounted || current !== epoch) return
  selected.value = {...task,...data}; await refresh(0,true)
 } catch (e: any) { if (!quiet && current === epoch) error.value = e.message }
 finally { if (current === epoch) querying.value = false }
}
async function chooseKey() {
 const current = ++epoch
 for (const abort of requests) abort.abort()
 listFlight = null; listBusy.value = false; querying.value = false
 token.value = ''; models.value = []; tasks.value = []; selected.value = null; error.value = ''; loading.value = true
 try {
  if (!keyID.value) return
  const key = await props.readKey(Number(keyID.value))
  if (!mounted || current !== epoch) return
  if (!key) throw new Error('该 Key 无法读取，请在密钥页检查或重新创建')
  token.value = key
  const data = await request('/v1/models')
  if (current !== epoch) return
  models.value = (data.data || []).filter((m: Row) => ['image','video','audio'].includes(m.model_type) || m.native_protocol==='systemone')
  modelID.value = available.value.find(m=>m.id===route.query.model)?.id || available.value[0]?.id || ''
 } catch (e: any) { if (current === epoch) error.value = e.message }
 finally { if (current === epoch) { loading.value = false; await refresh().catch((e: Error) => error.value = e.message) } }
}
watch(keyID,chooseKey)
watch(mode,() => { modelID.value = available.value[0]?.id || ''; uploadImages.value = []; audioFile.value = null; selected.value = null })
watch(modelID,() => { const c = config.value; seconds.value = String(c.defaultSeconds || ''); resolution.value = c.defaultResolution || ''; ratio.value = c.defaultAspectRatio || ''; audio.value = c.defaultEnableAudio ?? true; first.value = ''; last.value = ''; references.value = {images:'',videos:'',audios:''} })
async function imagesChanged(event: Event) {
 const files = [...((event.target as HTMLInputElement).files || [])]
 try { uploadImages.value = await Promise.all(files.map(file => new Promise<string>((resolve,reject) => {const reader = new FileReader(); reader.onload=()=>resolve(String(reader.result));reader.onerror=()=>reject(new Error('图片读取失败'));reader.readAsDataURL(file)}))) } catch (e: any) { error.value = e.message }
}
async function frameFile(event: Event, target: 'first'|'last') {
 const file=(event.target as HTMLInputElement).files?.[0];if(!file)return
 const current=epoch
 try {const value=await new Promise<string>((resolve,reject)=>{const reader=new FileReader();reader.onload=()=>resolve(String(reader.result));reader.onerror=()=>reject(new Error('图片读取失败'));reader.readAsDataURL(file)});if(mounted&&current===epoch){if(target==='first')first.value=value;else last.value=value}}catch(e:any){error.value=e.message}
}
async function referenceFiles(event: Event, target: 'images'|'videos'|'audios') {
 const files=[...((event.target as HTMLInputElement).files || [])];if(!files.length || uploading.value)return
 const current=epoch;uploading.value=true;error.value=''
 try {for(const file of files){const data=await new Promise<string>((resolve,reject)=>{const reader=new FileReader();reader.onload=()=>resolve(String(reader.result).split(',')[1] || '');reader.onerror=()=>reject(new Error('素材读取失败'));reader.readAsDataURL(file)})
  const result=await request('/v1/media/uploads',{model:modelID.value,file:{data,mime:file.type || ({images:'image/png',videos:'video/mp4',audios:'audio/mpeg'}[target])}})
  if(!mounted || current!==epoch)return
  references.value[target]=[...lines(references.value[target]),result.url].join('\n')
 }}catch(e:any){if(current===epoch)error.value=e.message}finally{uploading.value=false}
}
async function submit() {
 if (submitting.value || uploading.value || !model.value || !token.value) return
 submitting.value = true
 if (!await confirm({title:'确认生成',body:'本次调用会消耗所选 Key 对应账号的上游额度，价格以生成结果为准。是否继续？',okText:'开始处理'})) {submitting.value = false;return}
 const current = epoch; submitting.value = true; error.value = ''; controller = new AbortController()
 try {
  let path = '/v1/images/generations', body: Row | FormData = {model:modelID.value,prompt:prompt.value,size:size.value,n:quantity.value}
  if (mode.value==='image' || mode.value==='edit') {let extra: Row;try{extra=JSON.parse(imageParameters.value)}catch{throw new Error('高级图片参数必须是有效 JSON 对象')};if(!extra || Array.isArray(extra) || typeof extra!=='object')throw new Error('高级图片参数必须是 JSON 对象');for(const [k,v] of Object.entries(extra)){if(!['quality','style','background','footnote','revise','input_fidelity','response_format'].includes(k))throw new Error(`不支持的图片参数：${k}`);body[k]=v}}
  if (mode.value === 'edit') {path='/v1/images/edits';body.image=uploadImages.value}
  if (isVideo.value) {
   path='/v1/videos';body={model:modelID.value,prompt:prompt.value,negative_prompt:negative.value,enable_audio:audio.value}
   if (seconds.value) body.seconds = Number(seconds.value)
   if (resolution.value) body.resolution = resolution.value
   if (ratio.value && !first.value && !lines(references.value.images).length && !lines(references.value.videos).length) body.aspect_ratio = ratio.value
   if (first.value) body.image_url = first.value
   if (last.value) body.last_image_url = last.value
   for (const [key,value] of Object.entries(references.value)) if (lines(value).length) body[`reference_${key}`] = lines(value)
  }
  if (mode.value === 'decision') {path='/v1/systemone';body={...JSON.parse(decisionBody.value),model:modelID.value,stream:false}}
  if (mode.value === 'transcription') {
   path='/v1/audio/transcriptions'
   if (audioFile.value) {const form=new FormData();form.set('model',modelID.value);form.set('async','true');form.set('file',audioFile.value);body=form}
   else body={model:modelID.value,audio_url:audioURL.value,async:true}
  }
  const result = await request(path,body,controller.signal)
  if (current !== epoch || !mounted) return
  selected.value={...result,...(mode.value==='decision'?{decision:result}:{}),kind:isVideo.value?'video':mode.value==='transcription'?'audio':mode.value==='decision'?'decision':'image',model:modelID.value,status:result.status || 'completed'}
  await refresh()
 } catch (e: any) {if (current === epoch) error.value = e.name === 'AbortError' ? '请求等待已停止，上游任务可能仍在执行。请刷新任务记录，不要重复提交。' : e.message}
 finally {submitting.value=false;controller=null}
}
onMounted(async()=>{try {keys.value=(await props.loadKeys()).filter(k=>k.enabled !== false && k.enabled !== 0);keyID.value=String(keys.value[0]?.id || '')}catch(e:any){error.value=e.message}
 ticker=setInterval(()=>{if(!document.hidden && !submitting.value && token.value) { if(selected.value?.id && pollable(selected.value)) void openTask(selected.value,true); else if(tasks.value.some(pollable)) void refresh(0,true).catch(()=>{}) }},6000)
})
onBeforeUnmount(()=>{mounted=false;epoch++;clearInterval(ticker);controller?.abort();for(const abort of requests)abort.abort();token.value='';uploadImages.value=[]})
</script>
<template>
 <WPage title="非聊天工作台" subtitle="图片、视频、音频转写与结构化决策，沿用现有模型权限。">
  <div class="mb-4 flex flex-wrap gap-3 items-center"><WSelect v-model="keyID" :options="keyOptions" placeholder="选择 API Key" class="w-full sm:w-64" :disabled="submitting || uploading"/><WSelect v-model="mode" :options="mediaOptions" class="w-full sm:w-44" :disabled="submitting || uploading"/><span class="text-micro text-faint">消费按上游实际结算</span></div>
  <p v-if="error" role="alert" class="mb-4 rounded-xl border border-fault/30 bg-fault/5 p-3 text-small text-fault break-words">{{error}}</p>
  <WEmpty v-if="!keys.length && !loading" title="还没有可用 Key" hint="先在 API 密钥页创建并授权所需媒体模型。"/>
  <div v-else class="grid items-start gap-5 xl:grid-cols-[minmax(320px,420px)_minmax(0,1fr)]">
   <form class="rounded-2xl border border-line bg-surface p-4 sm:p-5 space-y-4" @submit.prevent="submit">
    <label class="media-field"><span>模型</span><WSelect v-model="modelID" :options="modelOptions" placeholder="选择可用模型" :disabled="submitting || uploading || loading"/></label>
    <p v-if="!available.length" class="text-small text-muted">当前 Key 没有此类模型权限，请切换 Key 或请管理员授权。</p>
    <template v-if="model">
     <p v-if="model.capability_source==='official_cli_protocol'" class="text-micro text-faint">官方 CLI 专用转写能力，价格由任务结果返回。</p>
     <label v-if="mode!=='transcription' && mode!=='decision'" class="media-field"><span>提示词</span><textarea v-model="prompt" rows="4" class="media-input resize-y" :required="!isVideo || (!first && !references.images && !references.videos)" :disabled="submitting || uploading" placeholder="描述希望生成或修改的内容"/></label>
     <div v-if="mode==='image' || mode==='edit'" class="grid grid-cols-2 gap-3"><label class="media-field"><span>尺寸</span><input v-model="size" class="media-input" placeholder="1024x1024" :disabled="submitting || uploading"/></label><label class="media-field"><span>数量</span><input v-model.number="quantity" type="number" min="1" step="1" required class="media-input" :disabled="submitting || uploading"/></label></div>
     <details v-if="mode==='image' || mode==='edit'" class="border-t border-line pt-3"><summary class="cursor-pointer text-small text-muted">高级图片参数</summary><label class="media-field mt-3"><span>可选参数（JSON）</span><textarea v-model="imageParameters" rows="3" class="media-input mono resize-y" :disabled="submitting || uploading"/><span class="text-micro text-faint">quality、style、background、footnote、revise、input_fidelity、response_format，具体支持情况以模型为准。</span></label></details>
     <label v-if="mode==='edit'" class="media-field"><span>原始图片</span><input type="file" accept="image/*" multiple required class="media-input" :disabled="submitting || uploading" @change="imagesChanged"/><span class="text-micro text-faint">已选择 {{uploadImages.length}} 张</span></label>
     <template v-if="isVideo">
      <div class="grid grid-cols-2 gap-3"><label class="media-field"><span>时长（秒）</span><input v-model="seconds" type="number" :min="config.minSeconds || 1" :max="config.maxSeconds || undefined" class="media-input" :disabled="submitting || uploading"/></label><label class="media-field"><span>分辨率</span><WSelect v-model="resolution" :options="(config.resolutions || []).map((v:string)=>({value:v,label:v}))" placeholder="上游默认" :disabled="submitting || uploading"/></label></div>
      <div class="grid grid-cols-2 gap-3 items-end"><label class="media-field"><span>画幅</span><WSelect v-model="ratio" :options="(config.aspectRatios || []).map((v:string)=>({value:v,label:v}))" placeholder="上游默认" :disabled="submitting || !!first || !!references.images || !!references.videos"/></label><label class="flex items-center gap-2 min-h-9 text-small"><input v-model="audio" type="checkbox" :disabled="submitting || config.audioMode!=='switchable'"/>生成音频</label></div>
      <details class="border-t border-line pt-3"><summary class="cursor-pointer text-small text-muted">参考素材与高级参数</summary><div class="space-y-3 mt-3">
       <label v-if="config.supportsFirstFrame!==false" class="media-field"><span>首帧图片地址</span><input v-model="first" class="media-input" placeholder="https://… 或图片 data URL" :disabled="submitting || uploading"/><input type="file" accept="image/*" :disabled="submitting || uploading" @change="frameFile($event,'first')"/></label>
       <label v-if="config.supportsFirstLastFrame" class="media-field"><span>尾帧图片地址</span><input v-model="last" class="media-input" placeholder="https://… 或图片 data URL" :disabled="submitting || uploading"/><input type="file" accept="image/*" :disabled="submitting || uploading" @change="frameFile($event,'last')"/></label>
       <label v-for="field in (['images','videos','audios'] as const).filter(k=>Number(config[{images:'maxReferenceImages',videos:'maxReferenceVideos',audios:'maxReferenceAudios'}[k]])>0)" :key="field" class="media-field"><span>参考{{ {images:'图片',videos:'视频',audios:'音频'}[field] }}（每行一个地址）</span><textarea v-model="references[field]" rows="2" class="media-input resize-y" placeholder="https://…" :disabled="submitting || uploading"/><input type="file" multiple :accept="{images:'image/*',videos:'video/*',audios:'audio/*'}[field]" :disabled="submitting || uploading" @change="referenceFiles($event,field)"/></label>
       <label class="media-field"><span>负面提示词</span><textarea v-model="negative" rows="2" class="media-input resize-y" :disabled="submitting || uploading"/></label>
      </div></details>
     </template>
     <label v-if="mode==='decision'" class="media-field"><span>决策请求（JSON）</span><textarea v-model="decisionBody" rows="10" class="media-input mono resize-y" :disabled="submitting || uploading" required/></label>
     <template v-if="mode==='transcription'"><label class="media-field"><span>上传音频或视频</span><input type="file" accept="audio/*,video/*" class="media-input" :disabled="submitting || uploading" @change="audioFile=($event.target as HTMLInputElement).files?.[0] || null"/></label><label class="media-field"><span>或使用音频地址</span><input v-model="audioURL" type="url" placeholder="https://…" class="media-input" :disabled="submitting || !!audioFile" :required="!audioFile"/></label></template>
     <p v-if="uploading" class="text-micro text-muted" role="status">正在上传参考素材…</p><div class="flex flex-wrap items-center gap-3 border-t border-line pt-4"><WButton type="submit" :loading="submitting" :disabled="loading || uploading || !modelID">{{mode==='transcription'?'开始转写':mode==='decision'?'提交决策':'开始生成'}}</WButton><WButton v-if="submitting" variant="ghost" @click="controller?.abort()">停止等待</WButton></div>
     <p class="text-micro text-faint leading-relaxed">停止等待不会取消已受理的上游任务。遇到超时请先查看任务记录，避免重复扣费。</p>
    </template>
   </form>
   <div class="min-w-0 space-y-5">
    <section class="rounded-2xl border border-line bg-surface p-4 sm:p-5"><div class="flex flex-wrap items-center justify-between gap-3 mb-4"><h2 class="font-semibold text-ink">处理结果</h2><span v-if="selected" class="text-small" :class="selected.status==='completed'?'text-live':selected.status==='failed'?'text-fault':'text-muted'">{{stateName(selected.status)}}</span></div>
     <WEmpty v-if="!selected" title="等待开始" hint="提交请求或从下方选择一条任务查看结果。"/>
     <template v-else><div class="flex flex-wrap gap-x-4 gap-y-1 text-micro text-muted mb-3"><span class="break-all">{{selected.model}}</span><span v-if="selected.usage?.credit!=null">消耗 {{selected.usage.credit}} 积分</span><span v-if="selected.usage?.output_video_duration">实际时长 {{selected.usage.output_video_duration}} 秒</span></div>
      <p v-if="selected.error" class="text-small text-fault break-words mb-3">{{typeof selected.error==='string'?selected.error:selected.error.message}}</p>
      <pre v-if="selected.decision" class="max-h-[32rem] overflow-auto whitespace-pre-wrap break-words rounded-xl border border-line bg-bg/40 p-4 mono text-small text-ink">{{JSON.stringify(selected.decision,null,2)}}</pre>
      <pre v-if="selected.text!=null" class="max-h-[32rem] overflow-auto whitespace-pre-wrap break-words rounded-xl border border-line bg-bg/40 p-4 text-small text-ink">{{selected.text}}</pre>
      <div class="grid gap-4" :class="results.length>1?'sm:grid-cols-2':''"><figure v-for="(item,i) in results" :key="i" class="min-w-0 rounded-xl border border-line overflow-hidden"><video v-if="selected.kind==='video' || selected.object==='video'" :src="item.src" controls preload="none" class="max-h-[32rem] w-full bg-bg"/><img v-else :src="item.src" referrerpolicy="no-referrer" class="max-h-[32rem] w-full object-contain bg-bg" :alt="`生成图片 ${i+1}`" loading="lazy"/><figcaption class="p-2 text-right"><a :href="item.src" target="_blank" rel="noopener noreferrer" class="text-small text-brand">打开原始文件</a></figcaption></figure></div>
      <p v-if="selected.result_cleared" class="text-small text-muted mt-3">结果已按正文保留策略清理，任务状态和消费记录仍保留。</p>
      <p v-if="selected.result_ephemeral" class="text-small text-muted mt-3">部分内联产物未长期保存，请在本次返回后及时下载。</p>
      <div v-if="selected.id" class="mt-4 flex flex-wrap items-center gap-3 border-t border-line pt-3"><span class="mono min-w-0 break-all text-micro text-faint">{{selected.id}}</span><WButton size="sm" variant="ghost" :loading="querying" @click="openTask(selected)">查询状态</WButton></div>
      <p class="text-micro text-faint mt-3">产物链接可能过期，请及时保存。媒体实际计费不按聊天 Tokens 推算。</p>
     </template>
    </section>
    <section class="rounded-2xl border border-line bg-surface overflow-hidden"><div class="flex items-center justify-between gap-3 p-4"><h2 class="font-semibold text-ink">当前 Key 的任务</h2><WButton size="sm" variant="ghost" :disabled="!token || loading || listBusy" @click="refresh().catch(e=>error=e.message)">刷新</WButton></div><WEmpty v-if="!tasks.length" title="暂无任务" hint="提交后的任务会保留在这里，可在重新打开页面后查询。"/><div v-else class="divide-y divide-line max-h-[32rem] overflow-y-auto"><button v-for="task in tasks" :key="task.id" type="button" class="grid w-full grid-cols-[minmax(0,1fr)_auto] gap-x-4 gap-y-1 px-4 py-3 text-left hover:bg-elevated focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-brand" :disabled="querying" @click="openTask(task)"><span class="truncate text-small text-ink">{{task.model}}</span><span class="text-micro whitespace-nowrap" :class="task.status==='completed'?'text-live':task.status==='failed'?'text-fault':'text-muted'">{{stateName(task.status)}}</span><span class="text-micro text-faint">{{date(task.created_at)}}</span><span class="text-micro text-faint">{{ {image:'图片',video:'视频',audio:'转写'}[task.kind as 'image'|'video'|'audio'] }}</span><span v-if="task.check_message" class="col-span-2 text-micro text-warn break-words">{{ task.check_message }}</span></button></div><div v-if="nextBefore" class="p-3 border-t border-line"><WButton size="sm" variant="ghost" :disabled="listBusy" @click="refresh(nextBefore).catch(e=>error=e.message)">加载更早任务</WButton></div></section>
   </div>
  </div>
 </WPage>
</template>
<style scoped>
.media-field{display:flex;min-width:0;flex-direction:column;gap:.5rem;font-size:.8125rem;color:rgb(var(--c-muted))}
.media-input{width:100%;min-width:0;min-height:2.25rem;border:1px solid rgb(var(--c-line));border-radius:.5rem;background:rgb(var(--c-bg)/.4);padding:.5rem .75rem;font-size:.8125rem;color:rgb(var(--c-ink));outline:none}
.media-input:focus{border-color:rgb(var(--c-brand))}.media-input:disabled{opacity:.5}
</style>
