// SMS encoding and segment (part) counter. Informational only: the hard
// ceiling stays the validator's 160 code points (lib/text.js LIMITS, the
// server's composer validator). This tells the marketer how many SMS parts
// the operator will bill for that text.
//
//   GSM-7  (3GPP TS 23.038 default alphabet): 160 septets in one SMS, 153 per
//          part when concatenated (the UDH takes 7). Characters of the
//          extension table ( ^ { } \ [ ] ~ | € and form feed) cost 2 septets
//          (ESC + char) and are never split across parts.
//   UCS-2  any character outside GSM-7 switches the WHOLE message to UCS-2:
//          70 UTF-16 code units in one SMS, 67 per part. Characters outside
//          the BMP (most emoji) take 2 units and are never split.
//
// Turkish: ü ö Ü Ö Ç are in GSM-7; ç ş ğ ı İ Ş Ğ are not (the Turkish
// national shift tables are not assumed: most gateways do not support them).

import { normalize, scanPlaceholders, VARIABLES, width } from './text.js'

export const GSM7 = Object.freeze({ name: 'GSM-7', single: 160, multi: 153 })
export const UCS2 = Object.freeze({ name: 'UCS-2', single: 70, multi: 67 })

// Default alphabet (Unicode mapping of GSM 03.38, without ESC 0x1B).
const GSM_BASIC = new Set(
  '@£$¥èéùìòÇ\nØø\rÅåΔ_ΦΓΛΩΠΨΣΘΞÆæßÉ !"#¤%&\'()*+,-./0123456789:;<=>?' +
    '¡ABCDEFGHIJKLMNOPQRSTUVWXYZÄÖÑÜ§¿abcdefghijklmnopqrstuvwxyzäöñüà',
)
// Extension table: reachable through ESC, so 2 septets each.
const GSM_EXTENDED = new Set('\f^{}\\[~]|€')

/** gsmSeptets returns the septet cost of one character in GSM-7, or 0 if it has none. */
export function gsmSeptets(ch) {
  if (GSM_BASIC.has(ch)) return 1
  if (GSM_EXTENDED.has(ch)) return 2
  return 0
}

/** isGsm7 reports whether every character of s can be sent in GSM-7. */
export function isGsm7(s) {
  for (const ch of String(s ?? '')) if (!gsmSeptets(ch)) return false
  return true
}

/** nonGsmCharacters lists the distinct characters of s that force UCS-2, in order of appearance. */
export function nonGsmCharacters(s) {
  const out = []
  for (const ch of String(s ?? '')) if (!gsmSeptets(ch) && !out.includes(ch)) out.push(ch)
  return out
}

// countParts packs per-character costs into SMS parts: one part if the total
// fits `single`, otherwise greedy parts of at most `multi` units, never
// splitting a character (an escape pair or a surrogate pair).
function countParts(costs, enc) {
  const units = costs.reduce((a, b) => a + b, 0)
  if (units === 0) return { units, segments: 0 }
  if (units <= enc.single) return { units, segments: 1 }
  let segments = 1
  let used = 0
  for (const c of costs) {
    if (used + c > enc.multi) {
      segments++
      used = 0
    }
    used += c
  }
  return { units, segments }
}

const gsmCosts = (s) => Array.from(s, gsmSeptets)
const ucsCosts = (s) => Array.from(s, (ch) => ch.length) // UTF-16 code units: 1 or 2

/**
 * smsSegments encodes a final (already rendered) SMS text and counts its parts:
 *   { encoding: 'GSM-7'|'UCS-2', limit: 160|70, perPart: 153|67, units, segments, nonGsm }
 * `units` is septets for GSM-7 and UTF-16 code units for UCS-2.
 */
export function smsSegments(text) {
  const s = String(text ?? '')
  const nonGsm = nonGsmCharacters(s)
  const enc = nonGsm.length ? UCS2 : GSM7
  const { units, segments } = countParts(nonGsm.length ? ucsCosts(s) : gsmCosts(s), enc)
  return { encoding: enc.name, limit: enc.single, perPart: enc.multi, units, segments, nonGsm }
}

const byName = new Map(VARIABLES.map((v) => [v.name, v]))

/**
 * smsInfo is the counter state for an SMS template such as
 * "{{first_name}}, sepetin seni bekliyor". The template is normalized like
 * the validator does; each known variable is budgeted at the same width as
 * lib/text.measure (18 characters for {{first_name}}), assuming a GSM-7 name.
 *
 * A name (or the fallback "Değerli müşterimiz") can contain ş, ğ, ı… and turn
 * an otherwise GSM-7 message into UCS-2 for that recipient, so when the
 * template has variables and is GSM-7, `worst` holds the UCS-2 count of the
 * same length: { segments, units }. It is null otherwise.
 */
export function smsInfo(value) {
  const text = normalize(value)
  const { found } = scanPlaceholders(text)
  let budgeted = ''
  let last = 0
  let personalised = false
  for (const p of found) {
    const v = byName.get(p.name)
    if (!v) continue // unknown placeholders stay literal (the validator rejects them anyway)
    budgeted += text.slice(last, p.start) + 'x'.repeat(width(v))
    last = p.end
    personalised = true
  }
  budgeted += text.slice(last)

  const info = smsSegments(budgeted)
  let worst = null
  if (personalised && info.encoding === GSM7.name) {
    worst = countParts(ucsCosts(budgeted), UCS2)
  }
  return { ...info, personalised, worst }
}

/** smsLabel formats the counter line: "GSM-7 (160 limit) - 1 SMS". */
export function smsLabel(info) {
  return `${info.encoding} (${info.limit} limit) - ${info.segments} SMS`
}
