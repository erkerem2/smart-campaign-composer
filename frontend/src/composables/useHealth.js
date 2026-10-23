import { ref, computed, getCurrentScope, onScopeDispose } from 'vue'
import { api } from '../api/client.js'

/**
 * useHealth polls GET /healthz and reports one of
 *   checking | ok | degraded | unhealthy | unreachable
 * Polling pauses while the tab is hidden and resumes with an immediate check.
 */
export function useHealth({ intervalMs = 10_000, fetchHealth = api.health, doc = globalThis.document } = {}) {
  const status = ref('checking')
  const body = ref(null)
  const latencyMs = ref(null)
  const lastChecked = ref(null)
  let timer = null
  let inflight = null
  let stopped = false

  async function check() {
    if (inflight) return inflight
    const controller = new AbortController()
    const timeout = setTimeout(() => controller.abort(), 4000)
    inflight = (async () => {
      try {
        const res = await fetchHealth({ signal: controller.signal })
        body.value = res.body
        latencyMs.value = Math.round(res.latencyMs ?? 0)
        const s = res.body?.status
        status.value = s === 'ok' || s === 'degraded' || s === 'unhealthy' ? s : res.httpStatus === 200 ? 'ok' : 'unhealthy'
      } catch {
        body.value = null
        latencyMs.value = null
        status.value = 'unreachable'
      } finally {
        clearTimeout(timeout)
        lastChecked.value = new Date()
        inflight = null
      }
    })()
    return inflight
  }

  function schedule() {
    clearInterval(timer)
    timer = setInterval(check, intervalMs)
  }

  function onVisibility() {
    if (doc.hidden) {
      clearInterval(timer)
      timer = null
    } else if (!stopped) {
      check()
      schedule()
    }
  }

  function stop() {
    stopped = true
    clearInterval(timer)
    timer = null
    doc?.removeEventListener?.('visibilitychange', onVisibility)
  }

  check()
  schedule()
  doc?.addEventListener?.('visibilitychange', onVisibility)
  if (getCurrentScope()) onScopeDispose(stop)

  const checks = computed(() => Object.entries(body.value?.checks || {}).map(([name, c]) => ({ name, ...c })))
  const system = computed(() => body.value?.system || null)

  return { status, body, checks, system, latencyMs, lastChecked, check, stop }
}
