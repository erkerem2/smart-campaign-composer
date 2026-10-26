import { ref, computed, getCurrentScope, onScopeDispose } from 'vue'
import { api, apiUrl } from '../api/client.js'

export const FINAL_STATES = Object.freeze(['completed', 'failed', 'interrupted', 'idle'])
export const CHANNELS = Object.freeze(['sms', 'email'])

const WINDOW_MS = 2000 // instantaneous speed is measured over the last ~2 s
const HISTORY = 240 // throughput samples kept for the chart (~2 min at 500 ms)

/**
 * useDispatchStream follows GET /api/campaigns/{id}/dispatch/stream.
 *
 * The server's `speed` is the run's average; the dashboard also derives an
 * instantaneous rate per channel from consecutive snapshots (Δprocessed/Δt
 * over a ~2 s window) so the gauge reacts to throttling and retries.
 *
 * EventSource reconnects by itself after a dropped connection; if it gives
 * up (CLOSED) or the server reports a stream error, the composable falls back
 * to polling the stats endpoint until the run is final. Polling stops for good
 * on a permanent error (the campaign is gone: 404, 400, 401, 403, 410) and
 * backs off exponentially — at least Retry-After — on transient ones (429,
 * 5xx, network), returning to pollMs after the first success.
 */
export function useDispatchStream({
  createSource = (url) => new EventSource(url),
  fetchStats = api.dispatchStats,
  now = () => (globalThis.performance || Date).now(),
  pollMs = 1000,
  maxPollMs = 30_000,
} = {}) {
  const stats = ref(null)
  const connection = ref('idle') // idle | connecting | live | reconnecting | polling | closed | failed
  const instant = ref({ total: 0, sms: 0, email: 0 })
  const history = ref([]) // [{t (s since start), sms, email}]
  const pollError = ref(null) // last polling error (ApiError), null after a success
  let source = null
  let pollTimer = null
  let campaignId = null
  let samples = []
  let t0 = null
  // Poll answers can overlap and resolve out of order. epoch changes on
  // every connect / disconnect / finish, so an answer to a poll started
  // before is dropped; seq / applied keep only answers newer than the last
  // one applied within the same epoch.
  let epoch = 0
  let seq = 0
  let applied = 0

  const state = computed(() => stats.value?.state || 'planning')
  const final = computed(() => FINAL_STATES.includes(state.value))
  const progress = computed(() => {
    const s = stats.value
    if (!s || !s.total) return 0
    return Math.min(1, s.processed / s.total)
  })

  function processedOf(s, ch) {
    return s?.channels?.[ch]?.processed ?? 0
  }

  function apply(s) {
    stats.value = s
    const t = now()
    if (t0 === null) t0 = t
    samples.push({ t, sms: processedOf(s, 'sms'), email: processedOf(s, 'email') })
    // keep one sample older than the window as the reference point
    while (samples.length > 2 && t - samples[1].t >= WINDOW_MS) samples.shift()
    const ref0 = samples[0]
    const dt = (t - ref0.t) / 1000
    if (FINAL_STATES.includes(s.state) || !s.live) {
      instant.value = { total: 0, sms: 0, email: 0 }
    } else if (dt >= 0.25) {
      const sms = Math.max(0, (processedOf(s, 'sms') - ref0.sms) / dt)
      const email = Math.max(0, (processedOf(s, 'email') - ref0.email) / dt)
      instant.value = { total: sms + email, sms, email }
    }
    // A finished run stops the chart where delivery ended instead of
    // drawing a drop to zero.
    if (s.live && !FINAL_STATES.includes(s.state)) {
      const next = history.value.concat({ t: (t - t0) / 1000, sms: instant.value.sms, email: instant.value.email })
      history.value = next.length > HISTORY ? next.slice(next.length - HISTORY) : next
    }
    if (FINAL_STATES.includes(s.state)) finish()
  }

  function finish() {
    epoch++
    closeSource()
    stopPolling()
    connection.value = 'closed'
  }

  function closeSource() {
    if (source) {
      source.close()
      source = null
    }
  }

  let polling = false
  let failures = 0
  let pollDelay = pollMs

  function stopPolling() {
    polling = false
    clearTimeout(pollTimer)
    pollTimer = null
  }

  function schedule(ms) {
    clearTimeout(pollTimer)
    pollTimer = setTimeout(tick, ms)
  }

  const PERMANENT = new Set([400, 401, 403, 404, 410])

  async function tick() {
    if (!polling) return
    const myEpoch = epoch
    const mine = ++seq
    // Fixed cadence: the next poll is planned when this one starts, so a slow
    // answer does not slow the dashboard down (overlapping answers are
    // ordered by seq / epoch below).
    schedule(pollDelay)
    try {
      const s = await fetchStats(campaignId)
      // Stale: the run finished, we disconnected or moved to another
      // campaign, or a newer poll has already been applied.
      if (myEpoch !== epoch || mine < applied) return
      applied = mine
      pollError.value = null
      if (failures > 0) {
        failures = 0
        pollDelay = pollMs
        schedule(pollMs)
      }
      apply(s)
    } catch (err) {
      if (myEpoch !== epoch) return
      pollError.value = err
      if (PERMANENT.has(err?.status)) {
        // The campaign is gone (or we may not see it): polling again cannot
        // help, and must not run forever in a background tab.
        epoch++
        stopPolling()
        connection.value = 'failed'
        return
      }
      failures++
      pollDelay = Math.min(maxPollMs, pollMs * 2 ** failures)
      schedule(Math.max(pollDelay, err?.retryAfterMs || 0))
    }
  }

  function startPolling() {
    closeSource()
    if (polling || campaignId === null) return
    polling = true
    failures = 0
    pollDelay = pollMs
    connection.value = 'polling'
    tick()
  }

  function connect(id, streamUrl) {
    disconnect()
    campaignId = id
    seq = 0
    applied = 0
    samples = []
    t0 = null
    history.value = []
    instant.value = { total: 0, sms: 0, email: 0 }
    connection.value = 'connecting'
    const url = streamUrl ? apiUrl(streamUrl) : api.dispatchStreamUrl(id)
    const es = createSource(url)
    source = es
    es.onopen = () => {
      if (source === es) connection.value = 'live'
    }
    es.addEventListener('stats', (e) => {
      if (source !== es) return
      try {
        apply(JSON.parse(e.data))
      } catch {
        /* ignore a malformed frame */
      }
    })
    es.addEventListener('done', () => {
      if (source !== es) return
      // The last stats frame already carried the final numbers.
      finish()
    })
    es.addEventListener('error', (e) => {
      if (source !== es) return
      if (e && typeof e.data === 'string' && e.data) {
        startPolling() // server-side "event: error" (stats unavailable)
      } else if (es.readyState === 2 /* CLOSED */) {
        startPolling()
      } else {
        connection.value = 'reconnecting'
      }
    })
  }

  function disconnect() {
    epoch++
    closeSource()
    stopPolling()
    connection.value = 'idle'
  }

  function reset() {
    disconnect()
    campaignId = null
    stats.value = null
    pollError.value = null
    history.value = []
    instant.value = { total: 0, sms: 0, email: 0 }
  }

  if (getCurrentScope()) onScopeDispose(disconnect)

  return { stats, state, final, progress, instant, history, connection, pollError, connect, disconnect, reset }
}
