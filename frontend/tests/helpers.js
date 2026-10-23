// Shared test doubles: an SSE response body and a scriptable EventSource.

/** sseResponse builds a fetch Response whose body yields the given chunks. */
export function sseResponse(chunks, { status = 200 } = {}) {
  const enc = new TextEncoder()
  let i = 0
  const body = new ReadableStream({
    pull(controller) {
      if (i < chunks.length) controller.enqueue(enc.encode(chunks[i++]))
      else controller.close()
    },
  })
  return new Response(body, { status, headers: { 'Content-Type': 'text/event-stream' } })
}

/** frame formats one SSE event the way the Go server writes it. */
export const frame = (event, data) => `event: ${event}\ndata: ${JSON.stringify(data)}\n\n`

export function jsonResponse(body, status = 200) {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

/** FakeEventSource records listeners so tests can push frames. */
export class FakeEventSource {
  static instances = []
  constructor(url) {
    this.url = url
    this.readyState = 0
    this.listeners = {}
    this.closed = false
    FakeEventSource.instances.push(this)
  }
  addEventListener(type, fn) {
    ;(this.listeners[type] ||= []).push(fn)
  }
  emit(type, data) {
    const ev = data === undefined ? { type } : { type, data: typeof data === 'string' ? data : JSON.stringify(data) }
    for (const fn of this.listeners[type] || []) fn(ev)
  }
  open() {
    this.readyState = 1
    this.onopen?.()
  }
  close() {
    this.readyState = 2
    this.closed = true
  }
}

export const flush = () => new Promise((r) => setTimeout(r, 0))
