// Server-Sent Events parsing for streams that EventSource cannot open
// (the compose stream is a POST). Implements the WHATWG event-stream rules
// that matter here: LF / CR / CRLF line endings (also split across chunks),
// multi-line data, comments, the optional space after the colon, and
// dispatch on a blank line.

/**
 * createSSEParser returns {feed(text), end()}; onEvent({event, data, id}) is
 * called for every complete event. event defaults to "message".
 */
export function createSSEParser(onEvent) {
  let buffer = ''
  let data = []
  let event = ''
  let lastId = ''

  function dispatch() {
    if (data.length === 0) {
      event = ''
      return
    }
    onEvent({ event: event || 'message', data: data.join('\n'), id: lastId })
    data = []
    event = ''
  }

  function line(l) {
    if (l === '') return dispatch()
    if (l.startsWith(':')) return // comment / keep-alive
    const colon = l.indexOf(':')
    const field = colon === -1 ? l : l.slice(0, colon)
    let value = colon === -1 ? '' : l.slice(colon + 1)
    if (value.startsWith(' ')) value = value.slice(1)
    switch (field) {
      case 'event':
        event = value
        break
      case 'data':
        data.push(value)
        break
      case 'id':
        if (!value.includes('\0')) lastId = value
        break
      default: // retry and unknown fields are ignored
    }
  }

  return {
    feed(text) {
      buffer += text
      // A trailing CR may be the first half of CRLF: wait for the next chunk.
      let keep = ''
      if (buffer.endsWith('\r')) {
        keep = '\r'
        buffer = buffer.slice(0, -1)
      }
      const lines = buffer.split(/\r\n|\r|\n/)
      buffer = lines.pop() + keep
      for (const l of lines) line(l)
    },
    end() {
      // A held-back trailing CR was a line ending after all.
      if (buffer.endsWith('\r')) {
        for (const l of buffer.slice(0, -1).split(/\r\n|\r|\n/)) line(l)
      }
      // An event without its terminating blank line is discarded (spec).
      buffer = ''
      data = []
      event = ''
    },
  }
}

/**
 * readSSE iterates the events of a fetch Response body.
 *
 *   for await (const ev of readSSE(res)) { ... }
 */
export async function* readSSE(response, signal) {
  if (!response.body) throw new Error('stream: response has no body')
  const reader = response.body.getReader()
  const decoder = new TextDecoder('utf-8')
  const queue = []
  const parser = createSSEParser((ev) => queue.push(ev))
  const onAbort = () => reader.cancel().catch(() => {})
  signal?.addEventListener('abort', onAbort, { once: true })
  let finished = false
  try {
    for (;;) {
      const { value, done } = await reader.read()
      if (done) {
        finished = true
        parser.feed(decoder.decode())
        parser.end()
        while (queue.length) yield queue.shift()
        return
      }
      parser.feed(decoder.decode(value, { stream: true }))
      while (queue.length) yield queue.shift()
    }
  } finally {
    signal?.removeEventListener('abort', onAbort)
    // The consumer stopped early (break / return): close the connection.
    if (!finished) reader.cancel().catch(() => {})
  }
}
