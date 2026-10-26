// Client-side mirror of the composer validator's counting rules
// (backend/internal/composer/validator.go, variables.go). It drives the live
// counters only — the server validates again on approval and is the
// authority. Keep the numbers in sync with internal/domain:
//   SMS ≤ 160 code points, e-mail subject ≤ 80 code points,
//   {{first_name}} is budgeted at max(estimate, fallback) = 18 characters
//   (composer.Variable.Width): a user without a name gets the fallback.

export const LIMITS = Object.freeze({ sms_content: 160, email_subject: 80 })

export const VARIABLES = Object.freeze([
  { name: 'first_name', label: 'Ad', sample: 'Ayşe', estimate: 10, fallback: 'Değerli müşterimiz' },
  { name: 'last_name', label: 'Soyad', sample: 'Yılmaz', estimate: 10, fallback: '' },
])

/** token formats a placeholder: token('first_name') → "{{first_name}}". */
export const token = (name) => `{{${name}}}`

const byName = new Map(VARIABLES.map((v) => [v.name, v]))

/**
 * width mirrors composer.Variable.Width: the characters budgeted for a
 * variable are its estimate or its fallback's length, whichever is larger.
 */
export function width(v) {
  return Math.max(v.estimate, runeLength(v.fallback))
}

// Go's strings.TrimSpace trims Unicode White_Space (unicode.IsSpace), which is
// not what String.prototype.trim does: trim() also removes U+FEFF and keeps
// U+0085 (NEL). The server is the authority, so the client trims its way.
const EDGE_SPACE = /^\p{White_Space}+|\p{White_Space}+$/gu

/** goTrim is strings.TrimSpace. */
export function goTrim(s) {
  return String(s ?? '').replace(EDGE_SPACE, '')
}

/** normalize matches composer.Normalize: CRLF/CR → LF, trimmed like Go. */
export function normalize(s) {
  return goTrim(String(s ?? '').replace(/\r\n?/g, '\n'))
}

// Invisible format characters (Unicode category Cf). The zero-width joiner
// and non-joiner are kept: emoji sequences such as 👨‍👩‍👧 need them.
const INVISIBLE = /\p{Cf}/gu
const KEEP = new Set([0x200c, 0x200d])

/**
 * cleanBrief mirrors textguard.CleanBrief on the server: invisible format
 * characters (zero-width space, BOM, word joiner, bidi controls, Unicode tag
 * characters, …) are removed and the result is trimmed. The brief counter and
 * the 2000-character limit work on this value, so a brief made only of
 * invisible characters is empty here exactly as it is on the server.
 */
export function cleanBrief(s) {
  return goTrim(String(s ?? '').replace(INVISIBLE, (m) => (KEEP.has(m.codePointAt(0)) ? m : '')))
}

const CONTROL = /\p{Cc}/u
const FORMAT = /\p{Cf}/u

function isBidiControl(cp) {
  return cp === 0x061c || cp === 0x200e || cp === 0x200f || (cp >= 0x202a && cp <= 0x202e) || (cp >= 0x2066 && cp <= 0x2069)
}

/**
 * forbiddenCharacter mirrors composer.checkCharacters and returns a message
 * for the first character the server would reject, or ''. Newlines are
 * allowed except in the subject; a tab only in the e-mail body.
 */
export function forbiddenCharacter(field, value) {
  const allowNewline = field !== 'email_subject'
  for (const ch of String(value ?? '')) {
    const cp = ch.codePointAt(0)
    if (cp === 0x0a && allowNewline) continue
    if (cp === 0x09 && field === 'email_body') continue
    if ((cp === 0x0a || cp === 0x0d) && !allowNewline) return 'Tek satır olmalı.'
    if (cp === 0x2028 || cp === 0x2029) return 'Satır/paragraf ayırıcı karakter (U+2028/2029) kullanılamaz.'
    if (CONTROL.test(ch) || isBidiControl(cp)) return `Kontrol karakteri içeriyor (U+${hex(cp)}).`
    if (FORMAT.test(ch) && !KEEP.has(cp)) return `Görünmez karakter içeriyor (U+${hex(cp)}).`
  }
  return ''
}

const hex = (cp) => cp.toString(16).toUpperCase().padStart(4, '0')

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
 *   estimated  length with each known variable replaced by its width
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
    estimated += width(v) - runeLength(p.token)
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

/**
 * fieldIssues returns the issues the server's validator would report for one
 * field, with the same stable codes (composer issue codes): empty,
 * forbidden_character, unknown_variable, malformed_placeholder, too_long,
 * estimated_too_long. Markup checks of the e-mail body stay server-side.
 * Shared vectors (tests/fixtures/text_vectors.json) keep the two in sync.
 */
export function fieldIssues(field, value) {
  const limit = LIMITS[field] || 0
  const text = normalize(value)
  const c = measure(text, limit)
  const out = []
  const add = (code, message) => out.push({ field, code, message })
  if (c.empty) {
    add('empty', 'Boş bırakılamaz.')
    return out
  }
  const bad = forbiddenCharacter(field, text)
  if (bad) add('forbidden_character', bad)
  for (const u of c.unknown) add('unknown_variable', `{{${u}}} desteklenmiyor.`)
  for (const m of c.malformed) add('malformed_placeholder', m)
  if (limit > 0 && c.runes > limit) add('too_long', `En fazla ${limit} karakter.`)
  else if (limit > 0 && c.estimated > limit) {
    add('estimated_too_long', `Kişiselleştirmeden sonra yaklaşık ${c.estimated} karakter olur; en fazla ${limit}.`)
  }
  return out
}
