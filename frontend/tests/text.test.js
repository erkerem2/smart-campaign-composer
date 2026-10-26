import { describe, it, expect } from 'vitest'
import { cleanBrief, measure, runeLength, scanPlaceholders, renderPreview, splitTemplate, normalize, LIMITS } from '../src/lib/text.js'

describe('runeLength', () => {
  it('counts code points like Go, not UTF-16 units or bytes', () => {
    expect(runeLength('İstanbul')).toBe(8)
    expect(runeLength('🎉')).toBe(1) // 2 UTF-16 units, 4 bytes
    expect(runeLength('çğıöşü')).toBe(6)
  })
})

describe('measure', () => {
  it('flags an SMS over 160 characters', () => {
    expect(measure('a'.repeat(160), LIMITS.sms_content).over).toBe(false)
    const m = measure('a'.repeat(161), LIMITS.sms_content)
    expect(m.over).toBe(true)
    expect(m.remaining).toBe(-1)
  })

  it('flags a subject over 80 characters, counting Turkish letters once', () => {
    expect(measure('ş'.repeat(80), LIMITS.email_subject).over).toBe(false)
    expect(measure('ş'.repeat(81), LIMITS.email_subject).over).toBe(true)
  })

  // Mirrors composer.Variable.Width: a user without a first name gets the
  // fallback "Değerli müşterimiz" (18 characters), so {{first_name}} is
  // budgeted at max(10, 18) = 18, exactly like the server's validator.
  it('budgets {{first_name}} at the width of its fallback, like the validator', () => {
    const m = measure('{{first_name}}, sepetin seni bekliyor', 160)
    expect(m.runes).toBe(37)
    expect(m.estimated).toBe(37 - 14 + 18)
    expect(m.variables).toEqual(['first_name'])
  })

  it('flags an SMS that only a user without a name would push over 160', () => {
    expect(measure('{{first_name}}' + 'a'.repeat(142), 160).over).toBe(false) // 156 raw, 160 estimated
    expect(measure('{{first_name}}' + 'a'.repeat(143), 160).over).toBe(true) // 161 for the fallback
    expect(measure('{{first_name}}' + 'a'.repeat(62), 80).over).toBe(false)
    expect(measure('{{first_name}}' + 'a'.repeat(63), 80).over).toBe(true)
  })

  it('trims like composer.Normalize before counting', () => {
    expect(measure('  merhaba \r\n', 160).runes).toBe(7)
    expect(normalize('a\r\nb\rc')).toBe('a\nb\nc')
  })

  it('reports unknown and malformed placeholders', () => {
    const m = measure('{{email}} {{first_name} {{ First }}', 160)
    expect(m.unknown).toEqual(['email'])
    expect(m.malformed.length).toBeGreaterThan(0)
  })
})

describe('scanPlaceholders', () => {
  it('accepts inner spaces and rejects triple braces', () => {
    expect(scanPlaceholders('{{ first_name }}').found[0].name).toBe('first_name')
    expect(scanPlaceholders('{{{first_name}}}').malformed).toHaveLength(1)
    expect(scanPlaceholders('tek { parantez }').malformed).toHaveLength(0)
  })
})

describe('renderPreview', () => {
  it('fills known variables and leaves unknown ones visible', () => {
    expect(renderPreview('Merhaba {{first_name}} {{coupon}}')).toBe('Merhaba Ayşe {{coupon}}')
    expect(renderPreview('Merhaba {{ first_name }}', { first_name: 'Mert' })).toBe('Merhaba Mert')
  })

  it('uses the fallback for an empty first name', () => {
    expect(renderPreview('{{first_name}}, selam', { first_name: '' })).toBe('Değerli müşterimiz, selam')
  })
})

describe('splitTemplate', () => {
  it('marks variable tokens for highlighting', () => {
    expect(splitTemplate('Hi {{first_name}}!')).toEqual([
      { text: 'Hi ' },
      { text: '{{first_name}}', variable: 'first_name', known: true },
      { text: '!' },
    ])
  })
})

// Parity with the server (backend/internal/composer/boundary_test.go): emoji
// are counted in code points, so the counter turns red at exactly the same
// text the validator rejects.
describe('emoji at the limit (A10 parity)', () => {
  const zwj = String.fromCodePoint(0x200d)
  const family = '👨' + zwj + '👩' + zwj + '👧'
  const flag = '🇹🇷'

  it('counts a ZWJ family as 5 and a flag as 2, like Go', () => {
    expect(runeLength(family)).toBe(5)
    expect(runeLength(flag)).toBe(2)
    expect(runeLength('❤️')).toBe(2)
  })

  it('accepts exactly 160 and flags 161', () => {
    for (const ok of ['🎉'.repeat(160), family.repeat(32), flag.repeat(80), '❤️'.repeat(80)]) {
      expect(measure(ok, LIMITS.sms_content).over).toBe(false)
    }
    for (const bad of ['🎉'.repeat(161), family.repeat(32) + 'a', flag.repeat(81)]) {
      expect(measure(bad, LIMITS.sms_content).over).toBe(true)
    }
    expect(measure(flag.repeat(40), LIMITS.email_subject).over).toBe(false)
    expect(measure(flag.repeat(40) + 'a', LIMITS.email_subject).over).toBe(true)
  })

  it('budgets {{first_name}} at 18 with Turkish / emoji padding too', () => {
    for (const pad of ['ş', 'ğ', '🎉']) {
      expect(measure('{{first_name}}' + pad.repeat(142), 160).over).toBe(false)
      expect(measure('{{first_name}}' + pad.repeat(143), 160).over).toBe(true)
    }
  })
})

// Mirrors textguard.CleanBrief: invisible format characters (Unicode Cf,
// except the emoji joiners U+200C / U+200D) are removed and the result is
// trimmed. A brief made only of them is empty, exactly as the server sees it.
describe('cleanBrief (C3 parity)', () => {
  const cp = (...c) => String.fromCodePoint(...c)

  it('removes invisible and bidi characters and trims', () => {
    expect(cleanBrief(cp(0x200b, 0x200b))).toBe('')
    expect(cleanBrief(cp(0xfeff))).toBe('')
    expect(cleanBrief(' ' + cp(0x2060) + ' \n' + cp(0x200b) + '\t')).toBe('')
    expect(cleanBrief(cp(0x202e) + 'kampanya')).toBe('kampanya')
    expect(cleanBrief('İndi' + cp(0x200b) + 'rim %30')).toBe('İndirim %30')
    expect(cleanBrief('brief' + cp(0xe0049, 0xe0047))).toBe('brief')
  })

  it('keeps emoji joiners and selectors', () => {
    const family = '👨' + cp(0x200d) + '👩'
    expect(cleanBrief('Aile ' + family)).toBe('Aile ' + family)
    expect(cleanBrief('❤️')).toBe('❤️')
  })

  it('does not count invisible padding toward the 2000-character limit', () => {
    const b = 'ş'.repeat(2000) + cp(0x200b).repeat(10)
    expect(runeLength(cleanBrief(b))).toBe(2000)
  })
})
