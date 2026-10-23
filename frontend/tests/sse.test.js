import { describe, it, expect } from 'vitest'
import { createSSEParser, readSSE } from '../src/lib/sse.js'
import { sseResponse } from './helpers.js'

function parseAll(chunks) {
  const out = []
  const p = createSSEParser((e) => out.push(e))
  chunks.forEach((c) => p.feed(c))
  p.end()
  return out
}

describe('createSSEParser', () => {
  const stream = 'event: delta\ndata: {"a":1}\n\nevent: done\ndata: {"b":2}\n\n'

  it('parses events and defaults the name to message', () => {
    expect(parseAll(['data: x\n\n', stream])).toEqual([
      { event: 'message', data: 'x', id: '' },
      { event: 'delta', data: '{"a":1}', id: '' },
      { event: 'done', data: '{"b":2}', id: '' },
    ])
  })

  it('gives the same result for every split point', () => {
    const whole = parseAll([stream])
    for (let i = 1; i < stream.length; i++) {
      expect(parseAll([stream.slice(0, i), stream.slice(i)])).toEqual(whole)
    }
  })

  it('handles CRLF and CR line endings, also when CRLF is split across chunks', () => {
    const crlf = stream.replace(/\n/g, '\r\n')
    const whole = parseAll([stream])
    expect(parseAll([crlf])).toEqual(whole)
    expect(parseAll([stream.replace(/\n/g, '\r')])).toEqual(whole)
    for (let i = 1; i < crlf.length; i++) {
      expect(parseAll([crlf.slice(0, i), crlf.slice(i)])).toEqual(whole)
    }
  })

  it('joins multi-line data, ignores comments, strips only one leading space', () => {
    expect(parseAll([': keep-alive\ndata:  a\ndata:b\n\n'])).toEqual([{ event: 'message', data: ' a\nb', id: '' }])
  })

  it('discards an unterminated event at the end of the stream', () => {
    expect(parseAll(['data: complete\n\ndata: partial'])).toHaveLength(1)
  })
})

describe('readSSE', () => {
  it('decodes UTF-8 split inside a multi-byte character', async () => {
    const text = 'event: delta\ndata: {"delta":"İstanbul’da 🎉"}\n\n'
    const bytes = new TextEncoder().encode(text)
    const parts = [bytes.slice(0, 27), bytes.slice(27, 29), bytes.slice(29)]
    let i = 0
    const res = new Response(
      new ReadableStream({ pull: (c) => (i < parts.length ? c.enqueue(parts[i++]) : c.close()) }),
    )
    const events = []
    for await (const e of readSSE(res)) events.push(e)
    expect(JSON.parse(events[0].data).delta).toBe('İstanbul’da 🎉')
  })

  it('stops cleanly when the consumer breaks early', async () => {
    const res = sseResponse(['data: 1\n\n', 'data: 2\n\n', 'data: 3\n\n'])
    const seen = []
    for await (const e of readSSE(res)) {
      seen.push(e.data)
      break
    }
    expect(seen).toEqual(['1'])
  })
})
