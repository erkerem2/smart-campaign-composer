import { describe, it, expect } from 'vitest'
import { effectScope, watch } from 'vue'
import { useComposeStream } from '../src/composables/useComposeStream.js'
import { ApiError } from '../src/api/client.js'
import { sseResponse, frame } from './helpers.js'

const message = {
  sms_content: '{{first_name}}, sepetin seni bekliyor! %30 indirim bugün senin.',
  email_subject: '{{first_name}}, sepetindekiler için %30 indirim',
  email_body: 'Merhaba {{first_name}},\n\nSepetin hâlâ seni bekliyor.',
}

function run(open) {
  const scope = effectScope()
  const c = scope.run(() => useComposeStream({ open }))
  return { c, scope }
}

describe('useComposeStream', () => {
  it('types deltas field by field, then replaces them with the validated draft', async () => {
    const { c, scope } = run(async () =>
      sseResponse([
        frame('delta', { type: 'delta', attempt: 1, field: 'sms_content', delta: '{{first_name}}, sep' }),
        frame('delta', { type: 'delta', attempt: 1, field: 'sms_content', delta: 'etin' }),
        frame('delta', { type: 'delta', attempt: 1, field: 'email_subject', delta: 'Konu' }),
        frame('done', { type: 'done', attempt: 1, draft: { message, report: {}, attempts: 1 } }),
      ]),
    )
    const typed = []
    scope.run(() => watch(() => c.fields.sms_content, (v) => typed.push(v), { flush: 'sync' }))
    const p = c.start('brief')
    expect(c.status.value).toBe('streaming')
    await p
    expect(typed.slice(0, 2)).toEqual(['{{first_name}}, sep', '{{first_name}}, sepetin'])
    expect(c.status.value).toBe('done')
    expect(c.fields.sms_content).toBe(message.sms_content) // authoritative, not the typed text
    expect(c.activeField.value).toBeNull()
  })

  it('clears the fields on retry and reports the validator issues', async () => {
    const { c, scope } = run(async () =>
      sseResponse([
        frame('delta', { type: 'delta', attempt: 1, field: 'sms_content', delta: 'x'.repeat(200) }),
        frame('retry', { type: 'retry', attempt: 1, issues: [{ field: 'sms_content', code: 'too_long', message: 'm' }] }),
        frame('delta', { type: 'delta', attempt: 2, field: 'sms_content', delta: 'kısa' }),
        frame('done', { type: 'done', attempt: 2, draft: { message, report: {}, attempts: 2 } }),
      ]),
    )
    const typed = []
    scope.run(() => watch(() => c.fields.sms_content, (v) => typed.push(v), { flush: 'sync' }))
    await c.start('brief')
    expect(typed.slice(0, 3)).toEqual(['x'.repeat(200), '', 'kısa'])
    expect(c.retryIssues.value[0].code).toBe('too_long')
    expect(c.attempt.value).toBe(2)
    expect(c.draft.value.attempts).toBe(2)
  })

  it('keeps the rejected attempt so the marketer can fix it by hand', async () => {
    const rejected = { ...message, sms_content: 'x'.repeat(170) }
    const { c } = run(async () =>
      sseResponse([
        frame('error', {
          type: 'error',
          error: 'llm output rejected after 2 attempt(s)',
          issues: [{ field: 'sms_content', code: 'too_long', message: 'm' }],
          rejected,
        }),
      ]),
    )
    await c.start('brief')
    expect(c.status.value).toBe('error')
    expect(c.rejected.value).toEqual(rejected)
    expect(c.issues.value).toHaveLength(1)
  })

  it('reports a stream that ends without done or error', async () => {
    const { c } = run(async () => sseResponse([frame('delta', { type: 'delta', attempt: 1, field: 'sms_content', delta: 'a' })]))
    await c.start('brief')
    expect(c.status.value).toBe('error')
    expect(c.error.value).toMatch(/kesildi/)
  })

  it('shows the API error when the stream cannot start', async () => {
    const { c } = run(async () => {
      throw new ApiError({ status: 400, code: 'invalid_brief', message: 'brief is required' })
    })
    await c.start('')
    expect(c.status.value).toBe('error')
    expect(c.error.value).toMatch(/Brief/)
  })

  it('aborts the request when the scope is disposed', async () => {
    let signal
    const { c, scope } = run((brief, opts) => {
      signal = opts.signal
      return new Promise(() => {})
    })
    c.start('brief')
    scope.stop()
    expect(signal.aborted).toBe(true)
  })

  // ── D1: a stream that breaks ────────────────────────────────────────────
  // Whatever goes wrong mid-stream, the composable ends in a clean error
  // state: no half-typed (never validated) text left in the fields, no
  // "typing" caret, a Turkish message, and start() works again.

  /** brokenStream yields the given frames, then fails like a dropped connection. */
  function brokenStream(frames) {
    const enc = new TextEncoder()
    let i = 0
    const body = new ReadableStream({
      pull(controller) {
        if (i < frames.length) controller.enqueue(enc.encode(frames[i++]))
        else controller.error(new TypeError('network error'))
      },
    })
    return new Response(body, { status: 200, headers: { 'Content-Type': 'text/event-stream' } })
  }

  const delta = frame('delta', { type: 'delta', attempt: 1, field: 'sms_content', delta: 'yarım kalan met' })

  function expectCleanError(c, pattern) {
    expect(c.status.value).toBe('error')
    expect(c.error.value).toMatch(pattern)
    expect(c.activeField.value).toBeNull()
    expect(c.draft.value).toBeNull()
    expect(c.rejected.value).toBeNull()
    for (const f of ['sms_content', 'email_subject', 'email_body']) expect(c.fields[f]).toBe('')
  }

  it('ends cleanly when the connection drops mid-stream (reader.read() rejects)', async () => {
    const { c } = run(async () => brokenStream([delta]))
    await c.start('brief')
    expectCleanError(c, /bağlantı/i)
    expect(c.error.value).not.toMatch(/network error|TypeError/)
  })

  it('ends cleanly when the stream stops without a final event', async () => {
    const { c } = run(async () => sseResponse([delta]))
    await c.start('brief')
    expectCleanError(c, /kesildi/)
  })

  it('treats a malformed done frame as a broken stream', async () => {
    const { c } = run(async () => sseResponse([delta, 'event: done\ndata: {"type":"done","draft":{"message"\n\n']))
    await c.start('brief')
    expectCleanError(c, /kesildi|geçersiz/)
  })

  it.each([
    ['without a draft', { type: 'done', attempt: 1 }],
    ['with a null draft', { type: 'done', attempt: 1, draft: null }],
    ['without the message', { type: 'done', attempt: 1, draft: { report: {}, attempts: 1 } }],
    ['with a missing field', { type: 'done', attempt: 1, draft: { message: { sms_content: 'x', email_subject: 'y' } } }],
    ['with a non-string field', { type: 'done', attempt: 1, draft: { message: { ...message, email_body: 42 } } }],
  ])('rejects a done event %s instead of showing empty content as validated', async (_, done) => {
    const { c } = run(async () => sseResponse([delta, frame('done', done)]))
    await c.start('brief')
    expectCleanError(c, /geçersiz/)
  })

  it('"Tekrar dene" starts over from a clean state and can succeed', async () => {
    let calls = 0
    const { c } = run(async () =>
      ++calls === 1
        ? brokenStream([delta])
        : sseResponse([frame('done', { type: 'done', attempt: 1, draft: { message, report: {}, attempts: 1 } })]),
    )
    await c.start('brief')
    expect(c.status.value).toBe('error')
    const second = c.start('brief')
    expect(c.status.value).toBe('streaming')
    expect(c.error.value).toBe('')
    await second
    expect(c.status.value).toBe('done')
    expect(c.fields.sms_content).toBe(message.sms_content)
  })
})
