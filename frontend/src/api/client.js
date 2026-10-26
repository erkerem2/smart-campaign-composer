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
  body_too_large: 'Gönderilen içerik çok büyük. Metinleri kısaltıp tekrar deneyin.',
  origin_not_allowed: 'Bu sayfanın adresinden API çağrısına izin verilmiyor (CORS ayarı).',
  not_found: 'İstenen adres sunucuda bulunamadı.',
  invalid_id: 'Kampanya numarası geçersiz.',
  // Answers from a proxy / gateway in front of the API (usually HTML).
  rate_limited: 'Çok fazla istek gönderildi. Birkaç saniye sonra tekrar deneyin.',
  bad_gateway: 'Sunucuya şu an ulaşılamıyor (yeniden başlıyor olabilir). Birazdan tekrar deneyin.',
  unavailable: 'Sunucu geçici olarak hizmet veremiyor. Birazdan tekrar deneyin.',
  gateway_timeout: 'Sunucu zamanında yanıt vermedi. Tekrar deneyin.',
  http_error: 'İstek sunucu tarafından reddedildi.',
}

// Status codes a proxy or gateway answers with when the request never reached
// the API (or the API did not answer in time), with the code the UI uses.
const STATUS_CODES = {
  413: 'body_too_large',
  429: 'rate_limited',
  502: 'bad_gateway',
  503: 'unavailable',
  504: 'gateway_timeout',
}

/**
 * parseRetryAfter reads a Retry-After header (delay-seconds or HTTP-date,
 * RFC 9110) into milliseconds; 0 when absent or invalid.
 */
export function parseRetryAfter(value, now = Date.now()) {
  const v = String(value ?? '').trim()
  if (!v) return 0
  if (/^\d+$/.test(v)) return Number(v) * 1000
  const at = Date.parse(v)
  return Number.isNaN(at) ? 0 : Math.max(0, at - now)
}

export class ApiError extends Error {
  constructor({ status = 0, code = 'unknown', message = '', path, issues, retryAfterMs = 0 } = {}) {
    super(message || code)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.path = path
    this.issues = Array.isArray(issues) ? issues : []
    this.retryAfterMs = retryAfterMs
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
  const retryAfterMs = parseRetryAfter(res.headers.get('retry-after'))
  const e = body && body.error
  if (e && typeof e === 'object') return new ApiError({ status: res.status, retryAfterMs, ...e })
  // Not the API's JSON envelope: a proxy or gateway answered (HTML, empty).
  const code = STATUS_CODES[res.status] || (res.status >= 500 ? 'internal' : 'http_error')
  return new ApiError({ status: res.status, code, message: `HTTP ${res.status}`, retryAfterMs })
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
