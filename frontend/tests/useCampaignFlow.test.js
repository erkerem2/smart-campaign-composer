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
})
