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

// D2. Errors that never reach the Go API — nginx answering for an upstream
// that is restarting (502), timing out (504), its own body limit (413), a
// rate limiter in front (429) — come back as HTML. They must still become a
// stable code and a Turkish sentence, never "HTTP 502" or raw HTML.
describe('errors from proxies and upstreams', () => {
  const html = (status, headers = {}) =>
    new Response(`<html><head><title>${status}</title></head><body><center><h1>${status}</h1></center><hr><center>nginx</center></body></html>`, {
      status,
      headers: { 'Content-Type': 'text/html', ...headers },
    })
  const fail = (res) => request('POST', '/api/campaigns', {}, { fetchImpl: async () => res }).catch((e) => e)

  it.each([
    [429, 'rate_limited', /çok fazla istek/i],
    [502, 'bad_gateway', /sunucuya şu an ulaşılamıyor/i],
    [503, 'unavailable', /geçici olarak hizmet veremiyor/i],
    [504, 'gateway_timeout', /zamanında yanıt vermedi/i],
    [413, 'body_too_large', /çok büyük/i],
  ])('HTML %i becomes %s with a readable message', async (status, code, message) => {
    const err = await fail(html(status))
    expect(err).toBeInstanceOf(ApiError)
    expect(err.status).toBe(status)
    expect(err.code).toBe(code)
    expect(err.userMessage).toMatch(message)
    expect(err.userMessage).not.toMatch(/<|nginx|HTTP \d/)
  })

  it('keeps the JSON code from the Go API when there is one', async () => {
    const err = await fail(jsonResponse({ error: { code: 'body_too_large', message: 'request body exceeds 262144 bytes' } }, 413))
    expect(err.code).toBe('body_too_large')
    expect(err.userMessage).toMatch(/çok büyük/i)
  })

  it('reads Retry-After (seconds or HTTP date) into retryAfterMs', async () => {
    expect((await fail(html(429, { 'Retry-After': '7' }))).retryAfterMs).toBe(7000)
    const date = new Date(Date.now() + 30_000).toUTCString()
    const ms = (await fail(html(503, { 'Retry-After': date }))).retryAfterMs
    expect(ms).toBeGreaterThan(20_000)
    expect(ms).toBeLessThanOrEqual(30_000)
    expect((await fail(html(502))).retryAfterMs).toBe(0)
  })

  it.each([
    ['origin_not_allowed', 403],
    ['not_found', 404],
    ['invalid_id', 400],
    ['unsupported_media_type', 415],
  ])('has a Turkish message for the API code %s', async (code, status) => {
    const err = await fail(jsonResponse({ error: { code, message: 'english detail' } }, status))
    expect(err.userMessage).not.toBe('english detail')
    expect(err.userMessage.length).toBeGreaterThan(10)
  })

  it('maps unknown non-JSON 4xx/5xx to a generic sentence, not the status line', async () => {
    const err = await fail(html(418))
    expect(err.code).toBe('http_error')
    expect(err.userMessage).not.toMatch(/HTTP 418/)
    const err5 = await fail(html(500))
    expect(err5.code).toBe('internal')
  })
})
