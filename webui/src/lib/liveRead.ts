import { onMounted, onUnmounted, ref } from 'vue'

const managedSignals = new WeakSet<object>()
export function isLiveRead(signal: unknown): boolean {
  return typeof signal === 'object' && signal !== null && managedSignals.has(signal)
}

export function useLiveRead<T>(read: (signal: AbortSignal) => Promise<T>, publish: (value: T) => void,
  options: { interval?: number; enabled?: () => boolean; immediate?: boolean } = {}) {
  const loading = ref(true), refreshing = ref(false), error = ref('')
  let alive = true, request: AbortController | null = null
  let timer: ReturnType<typeof setTimeout> | undefined
  let failures = 0, unauthorized = false, retryAt = 0, attempted = false
  const interval = options.interval ?? 30000
  const enabled = () => !document.hidden && navigator.onLine !== false &&
    ((!attempted && options.immediate !== false) || (options.enabled?.() ?? true))
  function schedule() {
    clearTimeout(timer)
    const delay = failures ? Math.min(120000, Math.min(interval, 3000) * 2 ** Math.min(failures - 1, 6)) : interval
    if (alive && interval > 0 && !unauthorized) timer = setTimeout(tick, Math.max(delay, retryAt - Date.now()))
  }
  async function refresh() {
    if (!alive) return false
    clearTimeout(timer)
    request?.abort()
    const current = new AbortController()
    managedSignals.add(current.signal)
    request = current
    attempted = true
    refreshing.value = true
    unauthorized = false
    try {
      const value = await read(current.signal)
      if (!alive || current.signal.aborted || request !== current) return false
      publish(value)
      loading.value = false
      error.value = ''
      failures = 0
      retryAt = 0
      return true
    } catch (e: any) {
      if (!alive || current.signal.aborted || request !== current) return false
      const status = e?.response?.status ?? e?.status
      unauthorized = status === 401 || status === 403
      error.value = e?.response?.data?.error?.message || e?.message || '读取失败，稍后自动重试'
      loading.value = false
      const hint = e?.response?.headers?.['retry-after']
      const seconds = e?.retryAfter || (/^\d+$/.test(String(hint)) ? Number(hint) : Math.max(0, (Date.parse(hint) - Date.now()) / 1000))
      retryAt = Number.isFinite(seconds) && seconds > 0 ? Date.now() + seconds * 1000 : 0
      failures++
      return false
    } finally {
      if (request === current) {
        request = null
        refreshing.value = false
        schedule()
      }
    }
  }
  function tick() {
    if (!alive || unauthorized) return
    if (enabled() && !request && Date.now() >= retryAt) void refresh()
    else schedule()
  }
  function resume() { if (!unauthorized) tick() }
  onMounted(() => {
    document.addEventListener('visibilitychange', resume)
    window.addEventListener('online', resume)
    window.addEventListener('focus', resume)
    if (options.immediate !== false) tick()
    else schedule()
  })
  onUnmounted(() => {
    alive = false
    clearTimeout(timer)
    request?.abort()
    document.removeEventListener('visibilitychange', resume)
    window.removeEventListener('online', resume)
    window.removeEventListener('focus', resume)
  })
  return { refresh, loading, refreshing, error }
}
