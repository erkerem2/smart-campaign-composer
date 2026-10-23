import { describe, it, expect, beforeEach, vi } from 'vitest'
import { effectScope } from 'vue'
import { useDispatchStream } from '../src/composables/useDispatchStream.js'
import { FakeEventSource, flush } from './helpers.js'

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
})
