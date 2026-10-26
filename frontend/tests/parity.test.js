import { describe, it, expect } from 'vitest'
import vectors from './fixtures/text_vectors.json'
import { normalize, measure, fieldIssues, LIMITS } from '../src/lib/text.js'

// D9. The shared vectors are produced by the Go validator
// (backend/internal/composer/testdata/text_vectors.json; parity_test.go keeps
// the two copies identical). For every case the client must normalise, count
// and flag exactly like the server, so the counters and "Onayla" button never
// disagree with the 422 the server would answer.
describe('client text rules match the server validator', () => {
  it('has the shared vectors', () => {
    expect(vectors.length).toBeGreaterThanOrEqual(50)
  })

  for (const v of vectors) {
    it(`${v.field}: ${v.name}`, () => {
      expect(normalize(v.input)).toBe(v.normalized)
      const m = measure(v.input, LIMITS[v.field] || 0)
      expect([m.runes, m.estimated]).toEqual([v.runes, v.estimated])
      const codes = [...new Set(fieldIssues(v.field, v.input).map((i) => i.code))].sort()
      expect(codes).toEqual(v.codes)
    })
  }
})
