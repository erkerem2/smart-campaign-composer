import { describe, it, expect, vi, beforeEach } from 'vitest'
import { effectScope } from 'vue'
import { useCampaignFlow, SAMPLE_BRIEF } from '../src/composables/useCampaignFlow.js'
import { ApiError } from '../src/api/client.js'
import { sseResponse, frame, FakeEventSource } from './helpers.js'

const rule = { type: 'group', logic: 'AND', rules: [{ type: 'condition', field: 'city', operator: 'EQUALS', value: 'İstanbul' }] }
const message = {
  sms_content: '{{first_name}}, sepetin seni bekliyor! %30 indirim.',
  email_subject: '{{first_name}}, %30 indirim seni bekliyor',
  email_body: 'Merhaba {{first_name}},\n\nSepetin seni bekliyor.',
}

function fakeClient(overrides = {}) {
  return {
    previewSegment: vi.fn(async ({ brief }) => ({ brief, rule, audience: { total: 795, sms_opt_in: 556, email_opt_in: 586, reachable: 700 } })),
    composeStream: vi.fn(async () =>
      sseResponse([
        frame('delta', { type: 'delta', attempt: 1, field: 'sms_content', delta: 'x' }),
        frame('done', { type: 'done', attempt: 1, draft: { message, report: {}, attempts: 1 } }),
      ]),
    ),
    createCampaign: vi.fn(async () => ({ campaign: { id: 42, status: 'approved' }, messages: [], audience: {} })),
    dispatch: vi.fn(async () => ({ campaign_id: 42, state: 'planning', stream_url: '/api/campaigns/42/dispatch/stream' })),
    dispatchStats: vi.fn(),
    ...overrides,
  }
}

function setup(client) {
  const scope = effectScope()
  const flow = scope.run(() =>
    useCampaignFlow({ client, dispatch: { createSource: (u) => new FakeEventSource(u) } }),
  )
  return flow
}

async function toReview(flow) {
  flow.brief.value = SAMPLE_BRIEF
  await flow.runPreview()
  await flow.startCompose()
  flow.goTo(4)
}

describe('useCampaignFlow', () => {
  beforeEach(() => (FakeEventSource.instances = []))

  it('walks brief → audience → content → approval → live dispatch', async () => {
    const client = fakeClient()
    const flow = setup(client)
    await toReview(flow)
    expect(flow.step.value).toBe(4)
    expect(flow.edits.sms_content).toBe(message.sms_content)
    expect(flow.canApprove.value).toBe(true)

    await flow.approveAndSend()
    expect(client.createCampaign).toHaveBeenCalledWith({ brief: SAMPLE_BRIEF, segmentRule: rule, messages: message })
    expect(client.dispatch).toHaveBeenCalledWith(42)
    expect(flow.step.value).toBe(5)
    expect(FakeEventSource.instances[0].url).toBe('/api/campaigns/42/dispatch/stream')
    expect(flow.locked.value).toBe(true)
    expect(flow.canVisit(2)).toBe(false)
  })

  it('blocks approval while the SMS is over 160 characters', async () => {
    const flow = setup(fakeClient())
    await toReview(flow)
    flow.setField('sms_content', 'a'.repeat(161))
    expect(flow.canApprove.value).toBe(false)
    expect(flow.clientIssues.value[0]).toMatchObject({ field: 'sms_content', code: 'too_long' })
    flow.restoreField('sms_content')
    expect(flow.canApprove.value).toBe(true)
  })

  it('sends the marketer’s edits, trimmed, and tracks which fields changed', async () => {
    const client = fakeClient()
    const flow = setup(client)
    await toReview(flow)
    flow.setField('email_subject', '  Elle yazılmış konu  ')
    expect(flow.edited.value).toEqual(['email_subject'])
    await flow.approveAndSend()
    expect(client.createCampaign.mock.calls[0][0].messages.email_subject).toBe('Elle yazılmış konu')
  })

  it('shows server-side issues next to the field and stays on the review step', async () => {
    const issues = [{ field: 'email_body', code: 'unsafe_markup', message: 'tag <script> is not allowed' }]
    const client = fakeClient({
      createCampaign: vi.fn(async () => {
        throw new ApiError({ status: 422, code: 'invalid_content', issues })
      }),
    })
    const flow = setup(client)
    await toReview(flow)
    await flow.approveAndSend()
    expect(flow.step.value).toBe(4)
    expect(flow.serverIssues.value).toEqual(issues)
    expect(client.dispatch).not.toHaveBeenCalled()
    flow.setField('email_body', 'Düz metin')
    expect(flow.serverIssues.value).toEqual([])
  })

  it('invalidates later steps when the brief changes', async () => {
    const flow = setup(fakeClient())
    await toReview(flow)
    flow.brief.value = 'Ankara’daki kullanıcılara yeni sezon duyurusu'
    expect(flow.reached.value).toBe(1)
    expect(flow.canVisit(4)).toBe(false)
    expect(flow.canApprove.value).toBe(false)
  })

  it('opens the review with the rejected attempt when the model could not fix it', async () => {
    const rejected = { ...message, sms_content: 'y'.repeat(170) }
    const flow = setup(
      fakeClient({ composeStream: vi.fn(async () => sseResponse([frame('error', { type: 'error', error: 'rejected', rejected })])) }),
    )
    flow.brief.value = SAMPLE_BRIEF
    await flow.runPreview()
    await flow.startCompose()
    expect(flow.reached.value).toBe(4)
    expect(flow.edits.sms_content).toBe(rejected.sms_content)
    expect(flow.canApprove.value).toBe(false)
  })

  it('follows an already running dispatch instead of failing', async () => {
    const client = fakeClient({
      dispatch: vi.fn(async () => {
        throw new ApiError({ status: 409, code: 'already_running' })
      }),
    })
    const flow = setup(client)
    await toReview(flow)
    await flow.approveAndSend()
    expect(flow.dispatchError.value).toBeNull()
    expect(FakeEventSource.instances).toHaveLength(1)
  })

  it('keeps the preview error for the audience step', async () => {
    const flow = setup(
      fakeClient({ previewSegment: vi.fn(async () => { throw new ApiError({ status: 502, code: 'llm_unavailable' }) }) }),
    )
    flow.brief.value = SAMPLE_BRIEF
    await flow.runPreview()
    expect(flow.step.value).toBe(1)
    expect(flow.previewError.value.code).toBe('llm_unavailable')
  })

  // D6: the brief limit is 2000 characters as the server counts them (Unicode
  // code points, like Go's utf8.RuneCountInString), not UTF-16 units: an
  // emoji is one character but two UTF-16 units. The counter on the brief
  // step already counts code points, so the request must not silently be
  // dropped while the counter says the brief fits.
  it('applies the 2000-character brief limit in code points, like the server', async () => {
    const client = fakeClient()
    const flow = setup(client)

    flow.brief.value = '🎉'.repeat(1500) // 1500 characters, 3000 UTF-16 units
    await flow.runPreview()
    expect(client.previewSegment).toHaveBeenCalledTimes(1)
    expect(flow.previewError.value).toBeNull()

    flow.brief.value = 'ş'.repeat(1000) + '🎉'.repeat(1000) // exactly 2000
    await flow.runPreview()
    expect(client.previewSegment).toHaveBeenCalledTimes(2)

    flow.brief.value = '🎉'.repeat(2001)
    await flow.runPreview()
    expect(client.previewSegment).toHaveBeenCalledTimes(2) // not sent
    expect(flow.previewError.value?.code).toBe('invalid_brief') // and not silent
  })

  // C3 parity: the brief is cleaned like the server does it, so a brief made
  // only of invisible characters is empty (no request), and invisible
  // padding does not count toward the limit.
  it('treats a brief of invisible characters as empty and ignores invisible padding', async () => {
    const client = fakeClient()
    const flow = setup(client)
    const zwsp = String.fromCodePoint(0x200b)

    flow.brief.value = zwsp + zwsp + ' ' + String.fromCodePoint(0xfeff)
    expect(flow.trimmedBrief.value).toBe('')
    await flow.runPreview()
    expect(client.previewSegment).not.toHaveBeenCalled()

    flow.brief.value = 'ş'.repeat(2000) + zwsp.repeat(5)
    await flow.runPreview()
    expect(client.previewSegment).toHaveBeenCalledWith({ brief: 'ş'.repeat(2000) }, expect.anything())
  })

  // ── D7: double clicks and answers that arrive out of order ─────────────
  function deferred() {
    let resolve, reject
    const promise = new Promise((res, rej) => ((resolve = res), (reject = rej)))
    return { promise, resolve, reject }
  }

  it('keeps the newest preview when an older request answers last, and aborts the older one', async () => {
    const pending = []
    const signals = []
    const client = fakeClient({
      previewSegment: vi.fn(({ brief }, opts) => {
        signals.push(opts?.signal)
        const d = deferred()
        pending.push({ brief, d })
        return d.promise
      }),
    })
    const flow = setup(client)
    flow.brief.value = 'eski brief'
    const first = flow.runPreview()
    flow.brief.value = 'yeni brief'
    const second = flow.runPreview()

    expect(signals[0]?.aborted).toBe(true) // the stale request is cancelled
    expect(signals[1]?.aborted).toBe(false)

    pending[1].d.resolve({ brief: 'yeni brief', rule, audience: { total: 2, reachable: 2 } })
    await second
    expect(flow.preview.value.brief).toBe('yeni brief')
    expect(flow.previewing.value).toBe(false)

    pending[0].d.resolve({ brief: 'eski brief', rule, audience: { total: 999, reachable: 999 } })
    await first
    expect(flow.preview.value.brief).toBe('yeni brief')
    expect(flow.preview.value.audience.total).toBe(2)
    expect(flow.briefDirty.value).toBe(false)
  })

  it('ignores an error from a stale preview request', async () => {
    const pending = []
    const client = fakeClient({
      previewSegment: vi.fn(() => {
        const d = deferred()
        pending.push(d)
        return d.promise
      }),
    })
    const flow = setup(client)
    flow.brief.value = 'bir'
    const first = flow.runPreview()
    flow.brief.value = 'iki'
    const second = flow.runPreview()
    pending[1].resolve({ brief: 'iki', rule, audience: { total: 1, reachable: 1 } })
    await second
    pending[0].reject(new ApiError({ status: 502, code: 'llm_unavailable' }))
    await first
    expect(flow.previewError.value).toBeNull()
    expect(flow.preview.value.brief).toBe('iki')
  })

  it('creates one campaign and starts one dispatch on a double click', async () => {
    const created = deferred()
    const client = fakeClient({ createCampaign: vi.fn(() => created.promise) })
    const flow = setup(client)
    await toReview(flow)
    const a = flow.approveAndSend()
    const b = flow.approveAndSend()
    created.resolve({ campaign: { id: 42, status: 'approved' }, messages: [], audience: {} })
    await Promise.all([a, b])
    expect(client.createCampaign).toHaveBeenCalledTimes(1)
    expect(client.dispatch).toHaveBeenCalledTimes(1)

    await Promise.all([flow.startDispatch(), flow.startDispatch()])
    expect(client.dispatch).toHaveBeenCalledTimes(2) // one per click burst, never two at once
  })

  // ── D8: the campaign exists, sending did not start ─────────────────────
  it('retries only the dispatch after a 500, never re-creating the campaign', async () => {
    let attempt = 0
    const client = fakeClient({
      dispatch: vi.fn(async () => {
        if (++attempt === 1) throw new ApiError({ status: 500, code: 'internal' })
        return { campaign_id: 42, state: 'planning', stream_url: '/api/campaigns/42/dispatch/stream' }
      }),
    })
    const flow = setup(client)
    await toReview(flow)
    await flow.approveAndSend()
    expect(flow.step.value).toBe(5)
    expect(flow.locked.value).toBe(true)
    expect(flow.dispatchError.value?.code).toBe('internal')
    expect(FakeEventSource.instances).toHaveLength(0)

    await flow.approveAndSend() // the approve button is gone; even if called, it must not create again
    expect(client.createCampaign).toHaveBeenCalledTimes(1)

    await flow.startDispatch() // "Tekrar dene"
    expect(client.dispatch).toHaveBeenCalledTimes(2)
    expect(client.dispatch).toHaveBeenLastCalledWith(42)
    expect(flow.dispatchError.value).toBeNull()
    expect(FakeEventSource.instances.at(-1).url).toBe('/api/campaigns/42/dispatch/stream')
  })

  it('explains a 409 invalid_state and keeps the campaign', async () => {
    const client = fakeClient({
      dispatch: vi.fn(async () => {
        throw new ApiError({ status: 409, code: 'invalid_state', message: 'campaign is completed; only approved campaigns can be dispatched' })
      }),
    })
    const flow = setup(client)
    await toReview(flow)
    await flow.approveAndSend()
    expect(flow.dispatchError.value.userMessage).toMatch(/bu durumda gönderilemez/)
    expect(flow.campaign.value.campaign.id).toBe(42)
    expect(client.createCampaign).toHaveBeenCalledTimes(1)
  })

  it('resumes an interrupted run through the same endpoint', async () => {
    const client = fakeClient()
    const flow = setup(client)
    await toReview(flow)
    await flow.approveAndSend()
    FakeEventSource.instances[0].emit('stats', { state: 'interrupted', live: false, total: 10, processed: 4, channels: {} })
    expect(flow.live.state.value).toBe('interrupted')
    await flow.startDispatch() // "Gönderime devam et"
    expect(client.dispatch).toHaveBeenCalledTimes(2)
    expect(client.createCampaign).toHaveBeenCalledTimes(1)
    expect(FakeEventSource.instances).toHaveLength(2)
  })
})
