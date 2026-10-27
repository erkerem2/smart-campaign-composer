import { describe, it, expect, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { effectScope } from 'vue'
import CharCounter from '../src/components/ui/CharCounter.vue'
import HealthBadge from '../src/components/ui/HealthBadge.vue'
import StepNav from '../src/components/ui/StepNav.vue'
import ReviewStep from '../src/components/steps/ReviewStep.vue'
import ComposeStep from '../src/components/steps/ComposeStep.vue'
import AudienceStep from '../src/components/steps/AudienceStep.vue'
import { measure } from '../src/lib/text.js'
import { smsInfo } from '../src/lib/sms.js'
import { api } from '../src/api/client.js'
import { useCampaignFlow, SAMPLE_BRIEF, STEPS } from '../src/composables/useCampaignFlow.js'
import { sseResponse, frame } from './helpers.js'

const message = {
  sms_content: '{{first_name}}, sepetin seni bekliyor!',
  email_subject: '{{first_name}}, %30 indirim',
  email_body: 'Merhaba {{first_name}}',
}
const rule = { type: 'group', logic: 'AND', rules: [{ type: 'condition', field: 'city', operator: 'EQUALS', value: 'İstanbul' }] }

async function reviewFlow(audience = { total: 795, sms_opt_in: 556, email_opt_in: 586, reachable: 700 }) {
  const client = {
    previewSegment: async ({ brief }) => ({ brief, rule, audience }),
    composeStream: async () => sseResponse([frame('done', { type: 'done', attempt: 1, draft: { message, report: {}, attempts: 1 } })]),
    createCampaign: vi.fn(),
    dispatch: vi.fn(),
    dispatchStats: vi.fn(),
  }
  const flow = effectScope().run(() => useCampaignFlow({ client }))
  flow.brief.value = SAMPLE_BRIEF
  await flow.runPreview()
  await flow.startCompose()
  flow.goTo(4)
  return flow
}

describe('CharCounter', () => {
  it('turns red and says how much is over the limit', () => {
    const w = mount(CharCounter, { props: { check: measure('a'.repeat(163), 160) } })
    const n = w.find('[data-state]')
    expect(n.attributes('data-state')).toBe('over')
    expect(n.classes()).toContain('text-bad')
    expect(n.text()).toContain('163 / 160')
    expect(n.text()).toContain('3 fazla')
  })

  it('warns near the limit', () => {
    const w = mount(CharCounter, { props: { check: measure('a'.repeat(150), 160) } })
    expect(w.find('[data-state]').attributes('data-state')).toBe('near')
  })
  it('shows no encoding line without the sms prop (e-mail subject)', () => {
    const w = mount(CharCounter, { props: { check: measure('Konu', 80) } })
    expect(w.find('[data-test=sms-encoding]').exists()).toBe(false)
  })

  it('shows GSM-7 and the SMS count next to the character counter', () => {
    const text = 'Sepetin seni bekliyor!'
    const w = mount(CharCounter, { props: { check: measure(text, 160), sms: smsInfo(text) } })
    const line = w.find('[data-test=sms-encoding]')
    expect(line.attributes('data-encoding')).toBe('GSM-7')
    expect(line.text()).toContain('GSM-7 (160 limit) - 1 SMS')
    expect(w.find('[data-test=sms-non-gsm]').exists()).toBe(false)
    expect(w.find('[data-state]').text()).toContain('22 / 160')
  })

  it('switches to UCS-2, names the Turkish characters and counts parts', () => {
    const text = 'Şimdi alışverişe başla, sepetindeki ürünler seni bekliyor, kaçırma fırsatı!' // 75 chars
    const w = mount(CharCounter, { props: { check: measure(text, 160), sms: smsInfo(text) } })
    const line = w.find('[data-test=sms-encoding]')
    expect(line.attributes('data-encoding')).toBe('UCS-2')
    expect(line.attributes('data-segments')).toBe('2')
    expect(line.text()).toContain('UCS-2 (70 limit) - 2 SMS')
    expect(w.find('[data-test=sms-non-gsm]').text()).toContain('Ş ı ş ç')
    // The 160 ceiling is unchanged: 75 characters are fine.
    expect(w.find('[data-state]').attributes('data-state')).toBe('ok')
  })

  it('warns when a Turkish name would need more parts', () => {
    const text = '{{first_name}}' + 'a'.repeat(60)
    const w = mount(CharCounter, { props: { check: measure(text, 160), sms: smsInfo(text) } })
    expect(w.find('[data-test=sms-encoding]').text()).toContain('GSM-7 (160 limit) - 1 SMS')
    expect(w.find('[data-test=sms-worst]').text()).toContain('UCS-2 - 2 SMS')
  })
})

describe('SMS encoding in the steps', () => {
  it('shows encoding and SMS parts under the streamed SMS', async () => {
    const flow = await reviewFlow()
    const w = mount(ComposeStep, { props: { flow } })
    const line = w.find('[data-test=compose-sms-counter] [data-test=sms-encoding]')
    expect(line.text()).toContain('GSM-7 (160 limit) - 1 SMS')
  })

  it('updates live while the marketer edits, without changing the 160 limit', async () => {
    const flow = await reviewFlow()
    const w = mount(ReviewStep, { props: { flow } })
    const sms = w.find('[data-test=edit-sms_content]')
    const line = () => w.find('[data-test=counter-sms_content] [data-test=sms-encoding]')

    await sms.setValue('Kisa ve oz')
    expect(line().attributes('data-encoding')).toBe('GSM-7')
    expect(line().text()).toContain('GSM-7 (160 limit) - 1 SMS')

    await sms.setValue('Kısa ve öz')
    expect(line().attributes('data-encoding')).toBe('UCS-2')
    expect(line().text()).toContain('UCS-2 (70 limit) - 1 SMS')

    // 150 Turkish letters: 3 SMS parts, but still within the 160 ceiling.
    await sms.setValue('ç'.repeat(150))
    expect(line().text()).toContain('UCS-2 (70 limit) - 3 SMS')
    expect(w.find('[data-test=counter-sms_content] [data-state]').attributes('data-state')).not.toBe('over')
    expect(w.find('[data-test=approve]').attributes('disabled')).toBeUndefined()

    // The e-mail subject counter has no SMS encoding line.
    expect(w.find('[data-test=counter-email_subject] [data-test=sms-encoding]').exists()).toBe(false)
  })
})

describe('ReviewStep', () => {
  it('blocks approval while the SMS is too long and re-enables it after the fix', async () => {
    const flow = await reviewFlow()
    const w = mount(ReviewStep, { props: { flow } })
    const sms = w.find('[data-test=edit-sms_content]')
    await sms.setValue('ç'.repeat(161))
    expect(w.find('[data-test=counter-sms_content] [data-state]').attributes('data-state')).toBe('over')
    expect(w.find('[data-test=approve]').attributes('disabled')).toBeDefined()
    expect(w.text()).toContain('Düzenlendi')
    await sms.setValue('Kısa ve öz')
    expect(w.find('[data-test=approve]').attributes('disabled')).toBeUndefined()
  })

  it('inserts a variable at the cursor and shows the personalised preview', async () => {
    const flow = await reviewFlow()
    const w = mount(ReviewStep, { props: { flow }, attachTo: document.body })
    const subject = w.find('[data-test=edit-email_subject]')
    await subject.setValue('Selam ')
    subject.element.setSelectionRange(6, 6)
    const chip = w.find('[data-field=email_subject]').findAll('button').find((b) => b.text().includes('{{first_name}}'))
    await chip.trigger('click')
    await flushPromises()
    expect(flow.edits.email_subject).toBe('Selam {{first_name}}')
    expect(w.text()).toContain('Selam Ayşe')
    w.unmount()
  })

  it('marks an unknown variable as an issue', async () => {
    const flow = await reviewFlow()
    const w = mount(ReviewStep, { props: { flow } })
    await w.find('[data-test=edit-email_subject]').setValue('{{email}} için fırsat')
    expect(w.find('[data-test=issues-email_subject]').text()).toContain('{{email}} desteklenmiyor')
    expect(w.find('[data-test=approve]').attributes('disabled')).toBeDefined()
  })
})

describe('AudienceStep', () => {
  it('shows the four audience badges and the readable rule', async () => {
    const flow = await reviewFlow()
    flow.goTo(2)
    const w = mount(AudienceStep, { props: { flow } })
    const counts = w.find('[data-test=audience-counts]').text()
    expect(counts).toContain('795')
    expect(counts).toContain('556')
    expect(counts).toContain('586')
    expect(counts).toContain('700')
    expect(w.find('[data-test=rule-card]').text()).toContain('İstanbul')
  })

  it('blocks content generation when nobody is reachable', async () => {
    const flow = await reviewFlow({ total: 4, sms_opt_in: 0, email_opt_in: 0, reachable: 0 })
    const w = mount(AudienceStep, { props: { flow } })
    expect(w.text()).toContain('Ulaşılabilir kimse yok')
    expect(w.find('[data-test=start-compose]').attributes('disabled')).toBeDefined()
  })
})

describe('HealthBadge', () => {
  it('goes green when /healthz is ok and red when the API is unreachable', async () => {
    vi.useFakeTimers()
    const spy = vi.spyOn(api, 'health').mockResolvedValue({ httpStatus: 200, body: { status: 'ok', checks: {}, system: { llm_mode: 'mock' } } })
    const w = mount(HealthBadge, { props: { intervalMs: 1000 } })
    await vi.advanceTimersByTimeAsync(0)
    expect(w.find('[data-test=health-badge]').attributes('data-status')).toBe('ok')
    expect(w.text()).toContain('Mock LLM')
    spy.mockRejectedValue(new Error('offline'))
    await vi.advanceTimersByTimeAsync(1000)
    expect(w.find('[data-test=health-badge]').attributes('data-status')).toBe('unreachable')
    expect(w.text()).toContain('API erişilemiyor')
    w.unmount()
    vi.useRealTimers()
  })
})

describe('StepNav', () => {
  it('disables steps that are not reachable yet', () => {
    const w = mount(StepNav, { props: { steps: STEPS, current: 1, reached: 2, canVisit: (id) => id <= 2 } })
    const buttons = w.findAll('button')
    expect(buttons[0].attributes('aria-current')).toBe('step')
    expect(buttons[1].attributes('disabled')).toBeUndefined()
    expect(buttons[2].attributes('disabled')).toBeDefined()
  })
})
