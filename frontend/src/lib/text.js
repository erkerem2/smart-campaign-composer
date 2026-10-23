// Client-side mirror of the composer validator's counting rules
// (backend/internal/composer/validator.go, variables.go). It drives the live
// counters only — the server validates again on approval and is the
// authority. Keep the numbers in sync with internal/domain:
//   SMS ≤ 160 code points, e-mail subject ≤ 80 code points,
//   {{first_name}} is estimated at 10 characters after personalisation.

export const LIMITS = Object.freeze({ sms_content: 160, email_subject: 80 })

export const VARIABLES = Object.freeze([
  { name: 'first_name', label: 'Ad', sample: 'Ayşe', estimate: 10, fallback: 'Değerli müşterimiz' },
  { name: 'last_name', label: 'Soyad', sample: 'Yılmaz', estimate: 10, fallback: '' },
])

/** token formats a placeholder: token('first_name') → "{{first_name}}". */
export const token = (name) => `{{${name}}}`

const byName = new Map(VARIABLES.map((v) => [v.name, v]))

/** normalize matches composer.Normalize: CRLF/CR → LF, trimmed. */
export function normalize(s) {
  return String(s ?? '').replace(/\r\n?/g, '\n').trim()
}

/** runeLength counts Unicode code points, like Go's utf8.RuneCountInString. */
export function runeLength(s) {
  let n = 0
  for (const _ of String(s ?? '')) n++ // eslint-disable-line no-unused-vars
  return n
}

const NAME_RE = /^[a-z][a-z0-9_]{0,31}$/

/**
 * scanPlaceholders mirrors composer.scanPlaceholders: well-formed
 * {{identifier}} tokens (inner spaces allowed) and every malformed brace pair.
 */
export function scanPlaceholders(s) {
  s = String(s ?? '')
  const found = []
  const malformed = []
  let i = 0
  while (i < s.length) {
    const open = s.indexOf('{{', i)
    const close = s.indexOf('}}', i)
    if (open < 0 && close < 0) break
    if (open < 0 || (close >= 0 && close < open)) {
      malformed.push('Eşi olmayan "}}"')
      i = close + 2
      continue
    }
    const end = s.indexOf('}}', open + 2)
    if (end < 0) {
      malformed.push('Kapanmamış "{{"')
      break
    }
    const inner = s.slice(open + 2, end).trim()
    let tokenEnd = end + 2
    if (!NAME_RE.test(inner)) {
      malformed.push(`Geçersiz değişken ${s.slice(open, tokenEnd)}`)
    } else if (s[tokenEnd] === '}') {
      malformed.push(`Fazla "}" (${s.slice(open, tokenEnd)})`)
      tokenEnd++
    } else {
      found.push({ name: inner, start: open, end: tokenEnd, token: s.slice(open, tokenEnd) })
    }
    i = tokenEnd
  }
  return { found, malformed }
}

/**
 * measure returns the counter state for one field:
 *   runes      raw template length (what the validator calls "runes")
 *   estimated  length with each known variable replaced by its estimate
 *   over       either number is above the limit (server: too_long / estimated_too_long)
 *   unknown    variables outside the whitelist (server: unknown_variable)
 */
export function measure(value, limit = 0) {
  const text = normalize(value)
  const runes = runeLength(text)
  const { found, malformed } = scanPlaceholders(text)
  let estimated = runes
  const unknown = []
  const used = []
  for (const p of found) {
    const v = byName.get(p.name)
    if (!v) {
      if (!unknown.includes(p.name)) unknown.push(p.name)
      continue
    }
    estimated += v.estimate - runeLength(p.token)
    if (!used.includes(p.name)) used.push(p.name)
  }
  const over = limit > 0 && (runes > limit || estimated > limit)
  return {
    runes,
    estimated,
    limit,
    over,
    remaining: limit > 0 ? limit - Math.max(runes, estimated) : null,
    variables: used,
    unknown,
    malformed,
    empty: runes === 0,
  }
}

/**
 * renderPreview fills known variables with sample values (or the fallback
 * when a value is empty). Unknown placeholders are left untouched so the
 * marketer can see them. The result is plain text: render it with
 * interpolation, never v-html.
 */
export function renderPreview(value, sample = {}) {
  const text = String(value ?? '')
  const { found } = scanPlaceholders(text)
  let out = ''
  let last = 0
  for (const p of found) {
    const v = byName.get(p.name)
    if (!v) continue
    const raw = sample[p.name] ?? v.sample
    out += text.slice(last, p.start) + (raw ? raw : v.fallback)
    last = p.end
  }
  return out + text.slice(last)
}

/** splitTemplate splits text into [{text, variable?}] parts for highlighting. */
export function splitTemplate(value) {
  const text = String(value ?? '')
  const { found } = scanPlaceholders(text)
  const parts = []
  let last = 0
  for (const p of found) {
    if (p.start > last) parts.push({ text: text.slice(last, p.start) })
    parts.push({ text: p.token, variable: p.name, known: byName.has(p.name) })
    last = p.end
  }
  if (last < text.length) parts.push({ text: text.slice(last) })
  return parts
}
