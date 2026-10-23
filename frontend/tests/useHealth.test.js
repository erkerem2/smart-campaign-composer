import { describe, it, expect, vi, afterEach } from 'vitest'
import { effectScope } from 'vue'
import { useHealth } from '../src/composables/useHealth.js'

afterEach(() => vi.useRealTimers())

function setup(fetchHealth, opts = {}) {
  const scope = effectScope()
  const h = scope.run(() => useHealth({ fetchHealth, intervalMs: 5000, ...opts }))
  return { h, scope }
}

describe('useHealth', () => {
  it('reports ok / degraded / unhealthy from the body', async () => {
    vi.useFakeTimers()
    const fetchHealth = vi.fn()
      .mockResolvedValueOnce({ httpStatus: 200, body: { status: 'ok', checks: { database: { status: 'ok' } } }, latencyMs: 3 })
      .mockResolvedValueOnce({ httpStatus: 200, body: { status: 'degraded', checks: {} }, latencyMs: 3 })
      .mockResolvedValueOnce({ httpStatus: 503, body: { status: 'unhealthy', checks: {} }, latencyMs: 3 })
    const { h } = setup(fetchHealth)
    await vi.advanceTimersByTimeAsync(0)
    expect(h.status.value).toBe('ok')
    expect(h.checks.value[0].name).toBe('database')
    await vi.advanceTimersByTimeAsync(5000)
    expect(h.status.value).toBe('degraded')
    await vi.advanceTimersByTimeAsync(5000)
    expect(h.status.value).toBe('unhealthy')
  })

  it('reports unreachable when the request fails', async () => {
    const { h } = setup(async () => {
      throw new Error('offline')
    })
    await h.check()
    expect(h.status.value).toBe('unreachable')
  })

  it('stops polling when its scope is disposed', async () => {
    vi.useFakeTimers()
    const fetchHealth = vi.fn(async () => ({ httpStatus: 200, body: { status: 'ok' } }))
    const { scope } = setup(fetchHealth)
    await vi.advanceTimersByTimeAsync(0)
    scope.stop()
    await vi.advanceTimersByTimeAsync(20000)
    expect(fetchHealth).toHaveBeenCalledTimes(1)
  })
})
