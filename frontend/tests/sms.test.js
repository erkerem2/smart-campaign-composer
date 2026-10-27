import { describe, it, expect } from 'vitest'
import { gsmSeptets, isGsm7, nonGsmCharacters, smsSegments, smsInfo, smsLabel, GSM7, UCS2 } from '../src/lib/sms.js'
import { measure, LIMITS } from '../src/lib/text.js'

describe('GSM-7 alphabet', () => {
  it('keeps ü ö Ü Ö Ç in GSM-7 but not ç ş ğ ı İ Ş Ğ', () => {
    for (const ch of ['ü', 'ö', 'Ü', 'Ö', 'Ç', 'a', 'Z', '0', ' ', '\n', '@', '£', '§', 'é']) {
      expect(gsmSeptets(ch), ch).toBe(1)
    }
    for (const ch of ['ç', 'ş', 'ğ', 'ı', 'İ', 'Ş', 'Ğ', '😀', '’', '“', '₺']) {
      expect(gsmSeptets(ch), ch).toBe(0)
    }
  })

  it('charges 2 septets for the extension table', () => {
    for (const ch of ['€', '{', '}', '[', ']', '~', '|', '^', '\\']) expect(gsmSeptets(ch), ch).toBe(2)
  })

  it('detects the characters that force UCS-2, once each and in order', () => {
    expect(isGsm7('Sepetin seni bekliyor!')).toBe(true)
    expect(isGsm7('Ürün güzel, Çok özel')).toBe(true)
    expect(isGsm7('Kısa ve öz')).toBe(false)
    expect(nonGsmCharacters('Şimdi alışverişe başla, çok şık!')).toEqual(['Ş', 'ı', 'ş', 'ç'])
    expect(nonGsmCharacters('plain text')).toEqual([])
  })
})

describe('smsSegments', () => {
  it('GSM-7: 160 in one SMS, then 153 per part', () => {
    expect(smsSegments('')).toMatchObject({ encoding: 'GSM-7', units: 0, segments: 0 })
    expect(smsSegments('a'.repeat(160))).toMatchObject({ encoding: 'GSM-7', limit: 160, units: 160, segments: 1 })
    expect(smsSegments('a'.repeat(161)).segments).toBe(2)
    expect(smsSegments('a'.repeat(306)).segments).toBe(2)
    expect(smsSegments('a'.repeat(307)).segments).toBe(3)
  })

  it('GSM-7: extension characters count double and are not split across parts', () => {
    expect(smsSegments('€'.repeat(80))).toMatchObject({ encoding: 'GSM-7', units: 160, segments: 1 })
    expect(smsSegments('€'.repeat(81))).toMatchObject({ units: 162, segments: 2 })
    // 152 + 2 + 152 = 306 septets would fit 2 × 153, but the escape pair
    // cannot straddle the boundary, so a third part is needed.
    expect(smsSegments('a'.repeat(152) + '€' + 'a'.repeat(152))).toMatchObject({ units: 306, segments: 3 })
  })

  it('UCS-2: one Turkish letter switches the whole message to 70 / 67', () => {
    expect(smsSegments('ç'.repeat(70))).toMatchObject({ encoding: 'UCS-2', limit: 70, perPart: 67, units: 70, segments: 1 })
    expect(smsSegments('ç'.repeat(71)).segments).toBe(2)
    expect(smsSegments('ç'.repeat(134)).segments).toBe(2)
    expect(smsSegments('ç'.repeat(135)).segments).toBe(3)
    expect(smsSegments('a'.repeat(69) + 'ı')).toMatchObject({ encoding: 'UCS-2', segments: 1, nonGsm: ['ı'] })
    expect(smsSegments('a'.repeat(70) + 'ı').segments).toBe(2)
  })

  it('UCS-2: emoji outside the BMP take 2 units and are not split', () => {
    expect(smsSegments('😀'.repeat(35))).toMatchObject({ encoding: 'UCS-2', units: 70, segments: 1 })
    // 66 + 2 + 66 = 134 units would fit 2 × 67, but the surrogate pair
    // cannot be split, so a third part is needed.
    expect(smsSegments('a'.repeat(66) + '😀' + 'a'.repeat(66))).toMatchObject({ units: 134, segments: 3 })
  })
})

describe('smsInfo', () => {
  it('budgets {{first_name}} like the length counter (18 characters)', () => {
    const tpl = '{{first_name}}, sepetin seni bekliyor!'
    const info = smsInfo(tpl)
    expect(info).toMatchObject({ encoding: 'GSM-7', units: measure(tpl, 160).estimated, segments: 1, personalised: true })
    expect(info.units).toBe(18 + ', sepetin seni bekliyor!'.length)
  })

  it('warns that a Turkish name can turn a GSM-7 template into UCS-2', () => {
    const info = smsInfo('{{first_name}}' + 'a'.repeat(60)) // 78 → 1 SMS in GSM-7, 2 in UCS-2
    expect(info).toMatchObject({ encoding: 'GSM-7', segments: 1 })
    expect(info.worst).toEqual({ units: 78, segments: 2 })
    expect(smsInfo('a'.repeat(60)).worst).toBeNull() // no variables, nothing can change
    expect(smsInfo('{{first_name}} ş').worst).toBeNull() // already UCS-2
  })

  it('normalizes like the validator before counting', () => {
    expect(smsInfo('  merhaba \r\n')).toMatchObject({ units: 7, segments: 1 })
    expect(smsInfo('a\r\nb').units).toBe(3) // CRLF → LF, one septet
  })

  it('leaves unknown placeholders literal', () => {
    // 6 letters + 4 braces; braces are GSM-7 extension characters (2 septets).
    expect(smsInfo('{{coupon}}')).toMatchObject({ encoding: 'GSM-7', units: 14, personalised: false, worst: null })
  })

  it('does not change the 160-character ceiling: 160 Turkish letters fit but take 3 SMS', () => {
    const text = 'ş'.repeat(LIMITS.sms_content)
    expect(measure(text, LIMITS.sms_content).over).toBe(false)
    expect(smsInfo(text)).toMatchObject({ encoding: 'UCS-2', segments: 3 })
    expect(measure(text + 'ş', LIMITS.sms_content).over).toBe(true)
  })
})

describe('smsLabel', () => {
  it('formats encoding, single-SMS limit and part count', () => {
    expect(smsLabel(smsInfo('Merhaba'))).toBe('GSM-7 (160 limit) - 1 SMS')
    expect(smsLabel(smsInfo('ç'.repeat(71)))).toBe('UCS-2 (70 limit) - 2 SMS')
    expect([GSM7.single, GSM7.multi, UCS2.single, UCS2.multi]).toEqual([160, 153, 70, 67])
  })
})
