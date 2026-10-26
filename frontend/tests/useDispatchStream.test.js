import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { effectScope } from 'vue'
import { useDispatchStream } from '../src/composables/useDispatchStream.js'
import { FakeEventSource, flush } from './helpers.js'
import { ApiError } from '../src/api/client.js'

function snap(processedSms, processedEmail, extra = {}) {
  return {
    state: 'sending',
    live: true,
    total: 200,
    processed: processedSms + processedEmail,
    sent: processedSms + processedEmail,
    failed: 0,
    retried: 3,
    speed: 90,
    channels: { sms: { processed: processedSms }, email: { processed: processedEmail } },
    ...extra,
  }
}

describe('useDispatchStream', () => {
  let clock
  beforeEach(() => {
    FakeEventSource.instances = []
    clock = 0
  })

  function setup(opts = {}) {
    const scope = effectScope()
    const d = scope.run(() =>
      useDispatchStream({ createSource: (u) => new FakeEventSource(u), now: () => clock, ...opts }),
    )
    return { d, scope, es: () => FakeEventSource.instances.at(-1) }
  }

  it('connects to the stream URL from the dispatch response', () => {
    const { d, es } = setup()
    d.connect(7, '/api/campaigns/7/dispatch/stream')
    expect(es().url).toBe('/api/campaigns/7/dispatch/stream')
    es().open()
    expect(d.connection.value).toBe('live')
  })

  it('derives progress and an instantaneous per-channel speed from snapshots', () => {
    const { d, es } = setup()
    d.connect(1)
    es().emit('stats', snap(0, 0))
    clock = 1000
    es().emit('stats', snap(90, 95))
    expect(d.progress.value).toBeCloseTo(185 / 200)
    expect(d.instant.value.sms).toBeCloseTo(90)
    expect(d.instant.value.email).toBeCloseTo(95)
    expect(d.history.value.length).toBe(2)
  })

  it('closes the EventSource on a final snapshot so it does not reconnect', () => {
    const { d, es } = setup()
    d.connect(1)
    const source = es()
    source.emit('stats', snap(100, 100, { state: 'completed', live: false, processed: 200 }))
    source.emit('done', { state: 'completed' })
    expect(source.closed).toBe(true)
    expect(d.final.value).toBe(true)
    expect(d.instant.value.total).toBe(0)
    expect(d.connection.value).toBe('closed')
  })

  it('falls back to polling when the stream is closed by the browser', async () => {
    vi.useFakeTimers()
    const fetchStats = vi.fn(async () => snap(10, 10))
    const { d, es } = setup({ fetchStats, pollMs: 1000 })
    d.connect(3)
    es().readyState = 2
    es().emit('error')
    expect(d.connection.value).toBe('polling')
    await vi.advanceTimersByTimeAsync(0)
    expect(fetchStats).toHaveBeenCalledWith(3)
    fetchStats.mockResolvedValue(snap(100, 100, { state: 'completed', live: false }))
    await vi.advanceTimersByTimeAsync(1000)
    expect(d.final.value).toBe(true)
    const calls = fetchStats.mock.calls.length
    await vi.advanceTimersByTimeAsync(3000)
    expect(fetchStats.mock.calls.length).toBe(calls) // polling stopped
    vi.useRealTimers()
  })

  it('treats a transient error as reconnecting (EventSource retries itself)', () => {
    const { d, es } = setup()
    d.connect(1)
    es().readyState = 0
    es().emit('error')
    expect(d.connection.value).toBe('reconnecting')
  })

  it('ignores frames from a previous connection', async () => {
    const { d } = setup()
    d.connect(1)
    const old = FakeEventSource.instances[0]
    d.connect(2)
    old.emit('stats', snap(1, 1))
    await flush()
    expect(d.stats.value).toBeNull()
    expect(old.closed).toBe(true)
  })

  // ── D3: answers to polls that are no longer current ─────────────────────
  // fetchStats calls can overlap (a slow API, a 1 s interval) and resolve in
  // any order. A late answer must never overwrite a newer or final state,
  // nor leak into the next campaign's dashboard.
  describe('stale poll answers', () => {
    afterEach(() => vi.useRealTimers())

    function deferred() {
      let resolve
      const promise = new Promise((r) => (resolve = r))
      return { promise, resolve }
    }

    function pollingSetup(id = 3) {
      vi.useFakeTimers()
      const pending = []
      const fetchStats = vi.fn(() => {
        const d = deferred()
        pending.push(d)
        return d.promise
      })
      const ctx = setup({ fetchStats, pollMs: 1000 })
      ctx.d.connect(id)
      ctx.es().readyState = 2
      ctx.es().emit('error') // the browser gave up: poll #1 is in flight
      return { ...ctx, pending, fetchStats }
    }

    it('keeps the final state when an older poll answers after it', async () => {
      const { d, pending } = pollingSetup()
      await vi.advanceTimersByTimeAsync(1000) // poll #2 in flight too
      expect(pending.length).toBe(2)
      pending[1].resolve(snap(100, 100, { state: 'completed', live: false, processed: 200 }))
      await vi.advanceTimersByTimeAsync(0)
      expect(d.state.value).toBe('completed')

      pending[0].resolve(snap(10, 10)) // the slow, older request
      await vi.advanceTimersByTimeAsync(0)
      expect(d.state.value).toBe('completed')
      expect(d.final.value).toBe(true)
      expect(d.stats.value.processed).toBe(200)
    })

    it('keeps the newest snapshot when polls answer out of order', async () => {
      const { d, pending } = pollingSetup()
      await vi.advanceTimersByTimeAsync(1000)
      pending[1].resolve(snap(50, 50))
      await vi.advanceTimersByTimeAsync(0)
      pending[0].resolve(snap(10, 10))
      await vi.advanceTimersByTimeAsync(0)
      expect(d.stats.value.processed).toBe(100)
      expect(d.progress.value).toBeCloseTo(100 / 200)
    })

    it('does not leak a previous campaign into a new connection', async () => {
      const { d, pending } = pollingSetup(3)
      d.reset()
      d.connect(4)
      pending[0].resolve(snap(100, 100, { state: 'completed', live: false, campaign_id: 3 }))
      await vi.advanceTimersByTimeAsync(0)
      expect(d.stats.value).toBeNull()
      expect(d.connection.value).toBe('connecting')
    })

    it('ignores answers that arrive after disconnect', async () => {
      const { d, pending } = pollingSetup()
      d.disconnect()
      pending[0].resolve(snap(10, 10))
      await vi.advanceTimersByTimeAsync(0)
      expect(d.stats.value).toBeNull()
      expect(d.connection.value).toBe('idle')
    })
  })

  // ── D4 / D5: polling that knows when to stop and when to slow down ──────
  describe('polling fallback', () => {
    afterEach(() => vi.useRealTimers())

    function polling(fetchStats, opts = {}) {
      vi.useFakeTimers()
      const ctx = setup({ fetchStats, pollMs: 1000, ...opts })
      ctx.d.connect(9)
      ctx.es().readyState = 2
      ctx.es().emit('error') // EventSource gave up → polling
      return ctx
    }

    it('stops for good on a permanent error (404) instead of polling forever', async () => {
      const fetchStats = vi.fn(async () => {
        throw new ApiError({ status: 404, code: 'campaign_not_found' })
      })
      const { d } = polling(fetchStats)
      await vi.advanceTimersByTimeAsync(30_000)
      expect(fetchStats).toHaveBeenCalledTimes(1)
      expect(d.connection.value).toBe('failed')
      expect(d.pollError.value?.code).toBe('campaign_not_found')
    })

    it('backs off exponentially on 503 and resets after a success', async () => {
      let fail = 3
      const calls = []
      const fetchStats = vi.fn(async () => {
        calls.push(Date.now())
        if (fail-- > 0) throw new ApiError({ status: 503, code: 'unavailable' })
        return snap(10, 10)
      })
      polling(fetchStats)
      const t0 = Date.now()
      await vi.advanceTimersByTimeAsync(20_000)
      const gaps = calls.slice(0, 5).map((t, i, a) => (i ? t - a[i - 1] : t - t0))
      // 0 (immediate), then 2 s, 4 s, 8 s after the failures, then back to 1 s.
      expect(gaps).toEqual([0, 2000, 4000, 8000, 1000])
    })

    it('waits at least Retry-After on 429', async () => {
      let first = true
      const fetchStats = vi.fn(async () => {
        if (first) {
          first = false
          throw new ApiError({ status: 429, code: 'rate_limited', retryAfterMs: 5000 })
        }
        return snap(1, 1)
      })
      polling(fetchStats)
      await vi.advanceTimersByTimeAsync(4900)
      expect(fetchStats).toHaveBeenCalledTimes(1)
      await vi.advanceTimersByTimeAsync(200)
      expect(fetchStats).toHaveBeenCalledTimes(2)
    })

    it('treats network failures as transient and caps the delay', async () => {
      const fetchStats = vi.fn(async () => {
        throw new ApiError({ code: 'network' })
      })
      const { d } = polling(fetchStats, { maxPollMs: 4000 })
      await vi.advanceTimersByTimeAsync(20_000)
      // Delays 2 s, then 4 s (the cap) each: calls at 0, 2, 6, 10, 14, 18 s.
      // A network blip is never a reason to give up.
      expect(fetchStats).toHaveBeenCalledTimes(6)
      expect(d.connection.value).toBe('polling')
    })

    it('switches to polling when the server sends "event: error"', async () => {
      vi.useFakeTimers()
      const fetchStats = vi.fn(async () => snap(5, 5))
      const { d, es } = setup({ fetchStats, pollMs: 1000 })
      d.connect(4)
      const source = es()
      source.open()
      source.emit('error', { error: 'stats unavailable' })
      expect(source.closed).toBe(true)
      expect(d.connection.value).toBe('polling')
      await vi.advanceTimersByTimeAsync(0)
      expect(fetchStats).toHaveBeenCalledWith(4)
      expect(d.stats.value.processed).toBe(10)
    })

    it('cleans up on unmount: no more polls, no more frames', async () => {
      const fetchStats = vi.fn(async () => snap(1, 1))
      const { d, scope } = polling(fetchStats)
      await vi.advanceTimersByTimeAsync(2500)
      const calls = fetchStats.mock.calls.length
      scope.stop()
      await vi.advanceTimersByTimeAsync(10_000)
      expect(fetchStats.mock.calls.length).toBe(calls)
      expect(d.connection.value).toBe('idle')
    })

    it('closes the EventSource on unmount while streaming', () => {
      const { d, es, scope } = setup()
      d.connect(1)
      es().open()
      scope.stop()
      expect(es().closed).toBe(true)
    })
  })
})
