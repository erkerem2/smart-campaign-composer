import { describe, it, expect, vi } from 'vitest'
import { api, request, ApiError } from '../src/api/client.js'
import { jsonResponse } from './helpers.js'

describe('request', () => {
  it('sends JSON and returns the parsed body', async () => {
    const fetchImpl = vi.fn(async () => jsonResponse({ ok: true }))
    await expect(request('POST', '/api/x', { a: 1 }, { fetchImpl })).resolves.toEqual({ ok: true })
    const [url, init] = fetchImpl.mock.calls[0]
    expect(url).toBe('/api/x')
    expect(init.headers['Content-Type']).toBe('application/json')
    expect(init.body).toBe('{"a":1}')
  })

  it('turns the API error envelope into an ApiError with code and issues', async () => {
    const issues = [{ field: 'sms_content', code: 'too_long', message: 'x' }]
    const fetchImpl = async () =>
      jsonResponse({ error: { code: 'invalid_content', message: 'content breaks channel rules', issues } }, 422)
    const err = await request('POST', '/api/campaigns', {}, { fetchImpl }).catch((e) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect(err.status).toBe(422)
    expect(err.code).toBe('invalid_content')
    expect(err.issues).toEqual(issues)
    expect(err.userMessage).toMatch(/kanal kurallarına/)
  })

  it('reports a network failure as code "network"', async () => {
    const fetchImpl = async () => {
      throw new TypeError('Failed to fetch')
    }
    const err = await request('GET', '/healthz', undefined, { fetchImpl }).catch((e) => e)
    expect(err.code).toBe('network')
  })

  it('appends the rule path to invalid_rule messages', () => {
    const e = new ApiError({ code: 'invalid_rule', message: 'unknown field "password"', path: 'rules[0].field' })
    expect(e.userMessage).toContain('rules[0].field')
  })
})

describe('api', () => {
  it('sends the edited rule without asking the LLM again', async () => {
    const fetchImpl = vi.fn(async () => jsonResponse({}))
    await api.previewSegment({ brief: 'b', rule: { type: 'group' } }, { fetchImpl })
    expect(JSON.parse(fetchImpl.mock.calls[0][1].body)).toEqual({ brief: 'b', rule: { type: 'group' } })
  })

  it('maps createCampaign to the API field names', async () => {
    const fetchImpl = vi.fn(async () => jsonResponse({}, 201))
    await api.createCampaign({ brief: 'b', segmentRule: { r: 1 }, messages: { sms_content: 's' } }, { fetchImpl })
    expect(JSON.parse(fetchImpl.mock.calls[0][1].body)).toEqual({
      brief: 'b',
      segment_rule: { r: 1 },
      messages: { sms_content: 's' },
    })
  })

  it('treats a 503 health answer as data, not as an exception', async () => {
    const fetchImpl = async () => jsonResponse({ status: 'unhealthy', checks: {} }, 503)
    const res = await api.health({ fetchImpl })
    expect(res.httpStatus).toBe(503)
    expect(res.body.status).toBe('unhealthy')
  })
})
