import { describe, it, expect } from 'vitest'
import { describeCondition, countConditions, parseRuleInput } from '../src/lib/rule.js'

const reference = {
  type: 'group',
  logic: 'AND',
  rules: [
    { type: 'condition', field: 'city', operator: 'EQUALS', value: 'İstanbul' },
    { type: 'condition', field: 'cart_last_active_days', operator: 'LESS_THAN_OR_EQUAL', value: 30 },
    { type: 'condition', field: 'has_purchased_since_cart', operator: 'EQUALS', value: false },
  ],
}

describe('describeCondition', () => {
  it('reads the reference rule in plain Turkish', () => {
    expect(reference.rules.map((r) => describeCondition(r).text)).toEqual([
      'İstanbul',
      'Son 30 gün içinde sepete ürün ekledi',
      'Son sepetinden sonra alışveriş yapmadı',
    ])
  })

  it('handles negation, lists and ranges', () => {
    expect(describeCondition({ field: 'has_purchased_since_cart', operator: 'NOT_EQUALS', value: false }).text).toBe(
      'Son sepetinden sonra alışveriş yaptı',
    )
    expect(describeCondition({ field: 'city', operator: 'NOT_IN', value: ['Ankara', 'İzmir'] }).text).toBe(
      'Ankara, İzmir dışında',
    )
    expect(describeCondition({ field: 'age', operator: 'BETWEEN', value: [25, 34] }).text).toBe('25 – 34')
    expect(describeCondition({ field: 'age', operator: 'GREATER_THAN_OR_EQUAL', value: 18 }).text).toBe('≥ 18')
  })

  it('falls back to the raw field name for fields it does not know', () => {
    expect(describeCondition({ field: 'loyalty_tier', operator: 'EQUALS', value: 'gold' })).toEqual({
      label: 'loyalty_tier',
      text: 'gold',
    })
  })
})

describe('countConditions / parseRuleInput', () => {
  it('counts leaves in nested groups', () => {
    expect(countConditions({ type: 'group', logic: 'OR', rules: [reference, reference.rules[0]] })).toBe(4)
  })

  it('accepts only JSON objects', () => {
    expect(parseRuleInput('{"type":"group"}').ok).toBe(true)
    expect(parseRuleInput('[1]').ok).toBe(false)
    expect(parseRuleInput('SELECT * FROM users').ok).toBe(false)
  })
})
