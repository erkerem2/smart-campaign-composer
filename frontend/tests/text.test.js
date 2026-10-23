import { describe, it, expect } from 'vitest'
import { measure, runeLength, scanPlaceholders, renderPreview, splitTemplate, normalize, LIMITS } from '../src/lib/text.js'

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

  it('estimates {{first_name}} as 10 characters, like the validator', () => {
    const m = measure('{{first_name}}, sepetin seni bekliyor', 160)
    expect(m.runes).toBe(37)
    expect(m.estimated).toBe(37 - 14 + 10)
    expect(m.variables).toEqual(['first_name'])
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
