// Thin fetch wrapper for the Go API (backend/internal/api).
//
// Every error the API returns has the shape
//   {"error": {"code", "message", "path"?, "issues"?}}
// and becomes an ApiError carrying the stable code, so the UI can branch on
// it (invalid_content → show issues next to the fields, already_running → just
// attach to the stream, …) instead of on HTTP status alone.

const BASE = String(import.meta.env?.VITE_API_BASE ?? '').replace(/\/+$/, '')

/** apiUrl prefixes a server path ("/api/...") with the optional API base. */
export function apiUrl(path) {
  if (/^https?:\/\//i.test(path)) return path
  return BASE + path
}

const USER_MESSAGES = {
  network: 'Sunucuya ulaşılamadı. Backend çalışıyor mu? (make run, :8080)',
  invalid_json: 'İstek sunucu tarafından okunamadı.',
  invalid_brief: 'Brief boş ya da 2000 karakterden uzun.',
  invalid_rule: 'Segment kuralı doğrulanamadı.',
  missing_rule: 'Onaylanacak bir segment kuralı yok.',
  missing_messages: 'Onaylanacak mesaj içeriği yok.',
  llm_unavailable: 'Model şu an yanıt vermiyor. Tekrar deneyin ya da kuralı JSON olarak düzenleyin.',
  empty_audience: 'Bu kurala uyan ve SMS ya da e-posta izni veren kimse yok.',
  invalid_content: 'İçerik kanal kurallarına uymuyor. İşaretli alanları düzeltin.',
  already_running: 'Bu kampanya zaten gönderiliyor.',
  invalid_state: 'Kampanya bu durumda gönderilemez.',
  no_approved_messages: 'Kampanyada onaylı mesaj yok.',
  campaign_not_found: 'Kampanya bulunamadı.',
  unsupported_media_type: 'İstek JSON olarak gönderilmedi.',
  internal: 'Sunucuda beklenmeyen bir hata oluştu. Ayrıntılar backend loglarında.',
}

export class ApiError extends Error {
  constructor({ status = 0, code = 'unknown', message = '', path, issues } = {}) {
    super(message || code)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.path = path
    this.issues = Array.isArray(issues) ? issues : []
  }

  /** Turkish, user-facing sentence; server detail is appended where it helps. */
  get userMessage() {
    const base = USER_MESSAGES[this.code]
    if (!base) return this.message || 'Beklenmeyen bir hata oluştu.'
    if (this.code === 'invalid_rule' && this.message) {
      return `${base} ${this.message}${this.path ? ` (${this.path})` : ''}`
    }
    if (this.code === 'invalid_state' && this.message) return `${base} (${this.message})`
    return base
  }
}

async function readBody(res) {
  const type = res.headers.get('content-type') || ''
  if (!type.includes('application/json')) return null
  try {
    return await res.json()
  } catch {
    return null
  }
}

/** toApiError converts a non-2xx response into an ApiError. */
export async function toApiError(res) {
  const body = await readBody(res)
  const e = body && body.error
  if (e && typeof e === 'object') return new ApiError({ status: res.status, ...e })
  return new ApiError({ status: res.status, code: res.status >= 500 ? 'internal' : 'http_error', message: `HTTP ${res.status}` })
}

/**
 * request sends one JSON request. Options: signal, fetchImpl (tests).
 * Resolves with the parsed JSON body, rejects with ApiError.
 */
export async function request(method, path, body, { signal, fetchImpl } = {}) {
  const doFetch = fetchImpl || globalThis.fetch
  const headers = { Accept: 'application/json' }
  const init = { method, headers, signal }
  if (body !== undefined) {
    headers['Content-Type'] = 'application/json'
    init.body = JSON.stringify(body)
  }
  let res
  try {
    res = await doFetch(apiUrl(path), init)
  } catch (err) {
    if (err?.name === 'AbortError') throw err
    throw new ApiError({ code: 'network', message: String(err?.message || err) })
  }
  if (!res.ok) throw await toApiError(res)
  return readBody(res)
}

export const api = {
  /** {brief} → LLM rule; {brief, rule} → validate an edited rule (no LLM). */
  previewSegment({ brief, rule }, opts) {
    const body = rule ? { brief, rule } : { brief }
    return request('POST', '/api/campaigns/preview-segment', body, opts)
  },

  /** Opens the compose SSE stream (POST, so fetch rather than EventSource). */
  async composeStream(brief, { signal, fetchImpl } = {}) {
    const doFetch = fetchImpl || globalThis.fetch
    let res
    try {
      res = await doFetch(apiUrl('/api/campaigns/compose/stream'), {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', Accept: 'text/event-stream' },
        body: JSON.stringify({ brief }),
        signal,
      })
    } catch (err) {
      if (err?.name === 'AbortError') throw err
      throw new ApiError({ code: 'network', message: String(err?.message || err) })
    }
    if (!res.ok) throw await toApiError(res)
    return res
  },

  /** The marketer's approval: rule + (possibly edited) content, validated again server-side. */
  createCampaign({ brief, segmentRule, messages }, opts) {
    return request('POST', '/api/campaigns', { brief, segment_rule: segmentRule, messages }, opts)
  },

  getCampaign(id, opts) {
    return request('GET', `/api/campaigns/${encodeURIComponent(id)}`, undefined, opts)
  },

  dispatch(id, opts) {
    return request('POST', `/api/campaigns/${encodeURIComponent(id)}/dispatch`, {}, opts)
  },

  dispatchStats(id, opts) {
    return request('GET', `/api/campaigns/${encodeURIComponent(id)}/dispatch`, undefined, opts)
  },

  dispatchStreamUrl(id) {
    return apiUrl(`/api/campaigns/${encodeURIComponent(id)}/dispatch/stream`)
  },

  /**
   * /healthz answers 503 with a normal body when the database is down, so
   * both 200 and 503 are data here; only a network failure throws.
   */
  async health({ signal, fetchImpl } = {}) {
    const doFetch = fetchImpl || globalThis.fetch
    const started = (globalThis.performance || Date).now()
    let res
    try {
      res = await doFetch(apiUrl('/healthz'), { headers: { Accept: 'application/json' }, signal, cache: 'no-store' })
    } catch (err) {
      if (err?.name === 'AbortError') throw err
      throw new ApiError({ code: 'network', message: String(err?.message || err) })
    }
    const body = await readBody(res)
    const latencyMs = (globalThis.performance || Date).now() - started
    return { httpStatus: res.status, body, latencyMs }
  },
}
