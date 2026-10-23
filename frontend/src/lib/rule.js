// Human-readable view of a validated segment rule tree
// (backend/internal/segment/ast.go). The UI shows what the backend
// understood — the canonical rule returned by preview-segment — so the
// marketer can check the interpretation before approving.

export const FIELD_LABELS = Object.freeze({
  city: 'Şehir',
  age: 'Yaş',
  total_spend: 'Toplam harcama',
  cart_last_active_days: 'Son sepet aktivitesi',
  has_purchased_since_cart: 'Sepetten sonra alışveriş',
})

export const OPERATOR_SYMBOLS = Object.freeze({
  EQUALS: '=',
  NOT_EQUALS: '≠',
  GREATER_THAN: '>',
  GREATER_THAN_OR_EQUAL: '≥',
  LESS_THAN: '<',
  LESS_THAN_OR_EQUAL: '≤',
  IN: 'içinde',
  NOT_IN: 'dışında',
  BETWEEN: 'arasında',
})

export const LOGIC_LABELS = Object.freeze({
  AND: 'Şunların hepsi',
  OR: 'Şunlardan en az biri',
  NOT: 'Şu doğru değil',
})

const tl = new Intl.NumberFormat('tr-TR', { style: 'currency', currency: 'TRY', maximumFractionDigits: 0 })
const num = new Intl.NumberFormat('tr-TR')

function fmtValue(field, v) {
  if (typeof v === 'boolean') return v ? 'evet' : 'hayır'
  if (typeof v === 'number') return field === 'total_spend' ? tl.format(v) : num.format(v)
  return String(v)
}

function list(field, values) {
  return (Array.isArray(values) ? values : [values]).map((v) => fmtValue(field, v)).join(', ')
}

/**
 * describeCondition returns {label, text} for a condition node: the field's
 * name and a plain-language reading of the comparison.
 */
export function describeCondition(c) {
  const { field, operator: op, value } = c
  const label = FIELD_LABELS[field] || field

  if (field === 'has_purchased_since_cart' && (op === 'EQUALS' || op === 'NOT_EQUALS')) {
    const yes = (op === 'EQUALS') === (value === true)
    return { label, text: yes ? 'Son sepetinden sonra alışveriş yaptı' : 'Son sepetinden sonra alışveriş yapmadı' }
  }
  if (field === 'cart_last_active_days') {
    if (op === 'LESS_THAN_OR_EQUAL') return { label, text: `Son ${num.format(value)} gün içinde sepete ürün ekledi` }
    if (op === 'LESS_THAN') return { label, text: `Son ${num.format(value - 1)} gün içinde sepete ürün ekledi` }
    if (op === 'GREATER_THAN') return { label, text: `${num.format(value)} günden uzun süredir sepete dokunmadı` }
    if (op === 'GREATER_THAN_OR_EQUAL') return { label, text: `En az ${num.format(value)} gündür sepete dokunmadı` }
    if (op === 'BETWEEN' && Array.isArray(value)) {
      return { label, text: `${num.format(value[0])}–${num.format(value[1])} gün önce sepete ürün ekledi` }
    }
  }
  if (op === 'BETWEEN' && Array.isArray(value)) {
    return { label, text: `${fmtValue(field, value[0])} – ${fmtValue(field, value[1])}` }
  }
  if (op === 'IN') return { label, text: list(field, value) }
  if (op === 'NOT_IN') return { label, text: `${list(field, value)} dışında` }
  if (op === 'EQUALS') return { label, text: fmtValue(field, value) }
  const sym = OPERATOR_SYMBOLS[op] || op
  return { label, text: `${sym} ${fmtValue(field, value)}` }
}

/** countConditions counts the condition leaves of a rule tree. */
export function countConditions(node) {
  if (!node || typeof node !== 'object') return 0
  if (node.type === 'condition') return 1
  return (node.rules || []).reduce((n, r) => n + countConditions(r), 0)
}

/** prettyRule formats a rule for the JSON editor. */
export function prettyRule(rule) {
  return JSON.stringify(rule, null, 2)
}

/**
 * parseRuleInput parses the JSON editor's text. It only checks that the text
 * is a JSON object; the server's whitelist parser does the real validation.
 */
export function parseRuleInput(text) {
  let v
  try {
    v = JSON.parse(text)
  } catch (e) {
    return { ok: false, error: `Geçerli JSON değil: ${e.message}` }
  }
  if (!v || typeof v !== 'object' || Array.isArray(v)) return { ok: false, error: 'Kural bir JSON nesnesi olmalı.' }
  return { ok: true, rule: v }
}
