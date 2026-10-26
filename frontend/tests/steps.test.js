import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { effectScope, nextTick } from 'vue'
import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join } from 'node:path'
import BriefStep from '../src/components/steps/BriefStep.vue'
import ComposeStep from '../src/components/steps/ComposeStep.vue'
import DispatchStep from '../src/components/steps/DispatchStep.vue'
import ReviewStep from '../src/components/steps/ReviewStep.vue'
import ThroughputChart from '../src/components/ui/ThroughputChart.vue'
import SmsPreview from '../src/components/ui/SmsPreview.vue'
import EmailPreview from '../src/components/ui/EmailPreview.vue'
import TemplateText from '../src/components/ui/TemplateText.vue'
import { useCampaignFlow, SAMPLE_BRIEF } from '../src/composables/useCampaignFlow.js'
import { ApiError } from '../src/api/client.js'
import { sseResponse, frame, FakeEventSource } from './helpers.js'

const rule = { type: 'group', logic: 'AND', rules: [{ type: 'condition', field: 'city', operator: 'EQUALS', value: 'İstanbul' }] }
const message = {
  sms_content: '{{first_name}}, sepetin seni bekliyor!',
  email_subject: '{{first_name}}, %30 indirim',
  email_body: 'Merhaba {{first_name}}',
}
const doneFrame = (m = message) => frame('done', { type: 'done', attempt: 1, draft: { message: m, report: {}, attempts: 1 } })

function fakeClient(overrides = {}) {
  return {
    previewSegment: vi.fn(async ({ brief }) => ({ brief, rule, audience: { total: 10, sms_opt_in: 8, email_opt_in: 9, reachable: 10 } })),
    composeStream: vi.fn(async () => sseResponse([doneFrame()])),
    createCampaign: vi.fn(async () => ({ campaign: { id: 42, status: 'approved' }, messages: [], audience: {} })),
    dispatch: vi.fn(async () => ({ campaign_id: 42, state: 'planning', stream_url: '/api/campaigns/42/dispatch/stream' })),
    dispatchStats: vi.fn(),
    ...overrides,
  }
}

function newFlow(client = fakeClient()) {
  const scope = effectScope()
  const flow = scope.run(() => useCampaignFlow({ client, dispatch: { createSource: (u) => new FakeEventSource(u) } }))
  return { flow, client, scope }
}

async function dispatched(client = fakeClient()) {
  const ctx = newFlow(client)
  ctx.flow.brief.value = SAMPLE_BRIEF
  await ctx.flow.runPreview()
  await ctx.flow.startCompose()
  await ctx.flow.approveAndSend()
  return { ...ctx, es: () => FakeEventSource.instances.at(-1) }
}

const zwsp = String.fromCodePoint(0x200b)

beforeEach(() => (FakeEventSource.instances = []))

describe('BriefStep', () => {
  it('counts the brief like the server: cleaned, in code points', async () => {
    const { flow } = newFlow()
    const w = mount(BriefStep, { props: { flow } })
    await w.find('[data-test="brief-input"]').setValue('  🎉🎉🎉' + zwsp + '  ')
    expect(w.text()).toContain('3 / 2000')
    expect(w.find('[data-test="run-preview"]').attributes('disabled')).toBeUndefined()
  })

  it('disables the button for an empty, invisible-only or too long brief', async () => {
    const { flow } = newFlow()
    const w = mount(BriefStep, { props: { flow } })
    const input = w.find('[data-test="brief-input"]')
    const button = () => w.find('[data-test="run-preview"]')
    for (const v of ['', '   ', zwsp + zwsp, '🎉'.repeat(2001)]) {
      await input.setValue(v)
      expect(button().attributes('disabled')).toBeDefined()
    }
    expect(input.attributes('aria-invalid')).toBe('true')
    await input.setValue('🎉'.repeat(2000))
    expect(button().attributes('disabled')).toBeUndefined()
  })

  it('shows a preview error in Turkish', async () => {
    const client = fakeClient({
      previewSegment: vi.fn(async () => {
        throw new ApiError({ status: 502, code: 'bad_gateway' })
      }),
    })
    const { flow } = newFlow(client)
    const w = mount(BriefStep, { props: { flow } })
    await w.find('[data-test="brief-input"]').setValue(SAMPLE_BRIEF)
    await w.find('[data-test="run-preview"]').trigger('click')
    await flushPromises()
    expect(w.text()).toContain('Kitle çıkarılamadı')
    expect(w.text()).toMatch(/ulaşılamıyor/)
  })
})

describe('ComposeStep', () => {
  it('shows the streamed text while typing, then the validated draft', async () => {
    let finish
    const gate = new Promise((r) => (finish = r))
    const enc = new TextEncoder()
    const client = fakeClient({
      composeStream: vi.fn(async () => {
        let step = 0
        return new Response(
          new ReadableStream({
            async pull(c) {
              if (step === 0) c.enqueue(enc.encode(frame('delta', { type: 'delta', attempt: 1, field: 'sms_content', delta: 'Yazılıyor' })))
              else if (step === 1) {
                await gate
                c.enqueue(enc.encode(doneFrame()))
              } else c.close()
              step++
            },
          }),
          { headers: { 'Content-Type': 'text/event-stream' } },
        )
      }),
    })
    const { flow } = newFlow(client)
    flow.brief.value = SAMPLE_BRIEF
    await flow.runPreview()
    const w = mount(ComposeStep, { props: { flow } })
    const p = flow.startCompose()
    await flushPromises()
    expect(w.text()).toContain('Claude mesajları yazıyor')
    expect(w.find('[data-test="stream-sms"]').text()).toContain('Yazılıyor')
    finish()
    await p
    await flushPromises()
    expect(w.text()).toContain('Taslak hazır ve doğrulandı')
    expect(w.find('[data-test="stream-sms"]').text()).toContain('sepetin seni bekliyor')
    expect(w.find('[data-test="go-review"]').exists()).toBe(true)
  })

  it('offers "Tekrar dene" after a broken stream and starts over', async () => {
    let calls = 0
    const client = fakeClient({
      composeStream: vi.fn(async () => {
        if (++calls === 1) {
          return new Response(
            new ReadableStream({
              pull(c) {
                c.error(new TypeError('network error'))
              },
            }),
            { headers: { 'Content-Type': 'text/event-stream' } },
          )
        }
        return sseResponse([doneFrame()])
      }),
    })
    const { flow } = newFlow(client)
    flow.brief.value = SAMPLE_BRIEF
    await flow.runPreview()
    const w = mount(ComposeStep, { props: { flow } })
    await flow.startCompose()
    await flushPromises()
    expect(w.text()).toContain('Akış tamamlanmadı')
    expect(w.text()).toMatch(/bağlantı koptu/i)
    expect(w.find('[data-test="go-review"]').exists()).toBe(false)

    await w.find('[data-test="compose-retry"]').trigger('click')
    await flushPromises()
    expect(client.composeStream).toHaveBeenCalledTimes(2)
    expect(w.text()).toContain('Taslak hazır ve doğrulandı')
  })
})

describe('DispatchStep', () => {
  const snap = (extra = {}) => ({
    state: 'sending', live: true, total: 200, processed: 120, sent: 110, failed: 10, retried: 7, pending: 80,
    speed: 95.5, elapsed_seconds: 3, channels: { sms: { total: 100, processed: 60, sent: 55, failed: 5, retried: 3, opted_out: 2, speed: 50 }, email: { total: 100, processed: 60, sent: 55, failed: 5, retried: 4, opted_out: 1, speed: 45.5 } },
    ...extra,
  })

  it('shows live progress and counters from the stream', async () => {
    const { flow, es } = await dispatched()
    const w = mount(DispatchStep, { props: { flow } })
    es().open()
    es().emit('stats', snap())
    await nextTick()
    expect(w.find('[data-test="dispatch-state"]').attributes('data-state')).toBe('sending')
    expect(w.find('[data-test="progress-percent"]').text()).toBe('%60')
    const tile = (k) => w.find(`[data-test="dispatch-tiles"] [data-key="${k}"] dd`).text()
    expect([tile('sent'), tile('failed'), tile('retried'), tile('pending')]).toEqual(['110', '10', '7', '80'])
    expect(w.text()).toContain('Kampanya #42 gönderimi')
  })

  it('resumes an interrupted run with "Gönderime devam et"', async () => {
    const { flow, client, es } = await dispatched()
    const w = mount(DispatchStep, { props: { flow } })
    es().emit('stats', snap({ state: 'interrupted', live: false }))
    await nextTick()
    const resume = w.findAll('button').find((b) => b.text().includes('Gönderime devam et'))
    expect(resume).toBeTruthy()
    await resume.trigger('click')
    await flushPromises()
    expect(client.dispatch).toHaveBeenCalledTimes(2)
    expect(client.createCampaign).toHaveBeenCalledTimes(1)
  })

  it('retries a failed dispatch call with "Tekrar dene"', async () => {
    let n = 0
    const client = fakeClient({
      dispatch: vi.fn(async () => {
        if (++n === 1) throw new ApiError({ status: 503, code: 'unavailable' })
        return { campaign_id: 42, state: 'planning', stream_url: '/api/campaigns/42/dispatch/stream' }
      }),
    })
    const { flow } = await dispatched(client)
    const w = mount(DispatchStep, { props: { flow } })
    expect(w.text()).toContain('Gönderim başlatılamadı')
    expect(w.text()).toMatch(/geçici olarak hizmet veremiyor/)
    await w.findAll('button').find((b) => b.text() === 'Tekrar dene').trigger('click')
    await flushPromises()
    expect(client.dispatch).toHaveBeenCalledTimes(2)
    expect(w.text()).not.toContain('Gönderim başlatılamadı')
  })

  it('says so when status polling has stopped for good', async () => {
    vi.useFakeTimers()
    try {
      const client = fakeClient({
        dispatchStats: vi.fn(async () => {
          throw new ApiError({ status: 404, code: 'campaign_not_found' })
        }),
      })
      const { flow, es } = await dispatched(client)
      const w = mount(DispatchStep, { props: { flow } })
      es().readyState = 2
      es().emit('error')
      await vi.advanceTimersByTimeAsync(0)
      await nextTick()
      expect(flow.live.connection.value).toBe('failed')
      expect(w.find('[data-test="poll-failed"]').text()).toMatch(/Kampanya bulunamadı/)
    } finally {
      vi.useRealTimers()
    }
  })
})

describe('ThroughputChart', () => {
  const paths = (w) => w.findAll('path').map((p) => p.attributes('d'))

  it('draws one line per channel and the limit', () => {
    const samples = [
      { t: 0, sms: 0, email: 0 },
      { t: 1, sms: 80, email: 60 },
      { t: 2, sms: 95, email: 99 },
    ]
    const w = mount(ThroughputChart, { props: { samples, limit: 100 } })
    const ds = paths(w)
    expect(ds).toHaveLength(2)
    for (const d of ds) {
      expect(d).toMatch(/^M[\d.]+,[\d.]+(L[\d.]+,[\d.]+){2}$/)
    }
    expect(w.text()).toContain('Kanal limiti 100/sn')
    expect(w.text()).toContain('SMS')
    expect(w.text()).toContain('E-posta')
  })

  it.each([
    ['a single sample', [{ t: 0, sms: 0, email: 0 }]],
    ['all zeros over a long run', Array.from({ length: 240 }, (_, i) => ({ t: i / 2, sms: 0, email: 0 }))],
    ['values far above the limit', [{ t: 0, sms: 0, email: 0 }, { t: 1, sms: 5000, email: 1e6 }]],
    ['missing values', [{ t: 0 }, { t: 1, sms: 10 }]],
  ])('never renders NaN or Infinity with %s', (_, samples) => {
    const w = mount(ThroughputChart, { props: { samples, limit: 100 } })
    expect(w.html()).not.toMatch(/NaN|Infinity/)
  })
})

// D10 · XSS regression. Model output, marketer edits and user data are
// untrusted. Whatever they contain must be shown as text: no element, no
// attribute, no script may come out of them.
describe('untrusted text is never rendered as HTML', () => {
  const payload =
    '<img src=x onerror="window.__xss=1"><script>window.__xss=2</script><svg onload="window.__xss=3"></svg><a href="javascript:window.__xss=4">x</a>{{first_name}}'

  afterEach(() => {
    delete window.__xss
  })

  function expectInert(w) {
    expect(w.find('img').exists()).toBe(false)
    expect(w.find('script').exists()).toBe(false)
    expect(w.find('a[href^="javascript"]').exists()).toBe(false)
    expect(w.findAll('svg').filter((s) => s.attributes('onload')).length).toBe(0)
    // No element carries an event-handler attribute (the escaped text may of
    // course contain the characters "onerror=", that is the point).
    for (const el of w.element.querySelectorAll('*')) {
      for (const a of el.attributes) expect(a.name.startsWith('on'), `${el.tagName} ${a.name}`).toBe(false)
    }
    expect(w.text()).toContain('<img src=x onerror=')
    expect(window.__xss).toBeUndefined()
  }

  it('SmsPreview (template and rendered)', () => {
    expectInert(mount(SmsPreview, { props: { text: payload }, attachTo: document.body }))
    expectInert(mount(SmsPreview, { props: { text: payload, rendered: true }, attachTo: document.body }))
  })

  it('EmailPreview subject and body (template and rendered)', () => {
    for (const rendered of [false, true]) {
      expectInert(mount(EmailPreview, { props: { subject: payload, body: payload, rendered }, attachTo: document.body }))
    }
  })

  it('TemplateText', () => {
    expectInert(mount(TemplateText, { props: { text: payload }, attachTo: document.body }))
  })

  it('ComposeStep while the model streams hostile text', async () => {
    const client = fakeClient({
      composeStream: vi.fn(async () =>
        sseResponse([
          frame('delta', { type: 'delta', attempt: 1, field: 'sms_content', delta: payload }),
          frame('delta', { type: 'delta', attempt: 1, field: 'email_body', delta: payload }),
          doneFrame({ sms_content: payload, email_subject: payload, email_body: payload }),
        ]),
      ),
    })
    const { flow } = newFlow(client)
    flow.brief.value = SAMPLE_BRIEF
    await flow.runPreview()
    const w = mount(ComposeStep, { props: { flow }, attachTo: document.body })
    await flow.startCompose()
    await flushPromises()
    expectInert(w)
  })

  it('ReviewStep with hostile edits', async () => {
    const { flow } = newFlow()
    flow.brief.value = SAMPLE_BRIEF
    await flow.runPreview()
    await flow.startCompose()
    flow.goTo(4)
    for (const f of ['sms_content', 'email_subject', 'email_body']) flow.setField(f, payload)
    const w = mount(ReviewStep, { props: { flow }, attachTo: document.body })
    await nextTick()
    expect(w.find('img').exists()).toBe(false)
    expect(w.find('script').exists()).toBe(false)
    expect(window.__xss).toBeUndefined()
  })

  it('no component uses v-html or innerHTML', () => {
    const root = join(process.cwd(), 'src') // vitest runs from the frontend root
    const files = []
    const walk = (dir) => {
      for (const name of readdirSync(dir)) {
        const p = join(dir, name)
        if (statSync(p).isDirectory()) walk(p)
        else if (/\.(vue|js)$/.test(name)) files.push(p)
      }
    }
    walk(root)
    expect(files.length).toBeGreaterThan(10)
    for (const f of files) {
      const code = readFileSync(f, 'utf8')
        .replace(/\/\*[\s\S]*?\*\//g, '')
        .replace(/\/\/.*$/gm, '')
        .replace(/<!--[\s\S]*?-->/g, '')
      expect(code, f).not.toMatch(/v-html|innerHTML|outerHTML|insertAdjacentHTML|document\.write/)
    }
  })
})
