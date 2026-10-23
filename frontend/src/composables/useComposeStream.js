import { ref, reactive, getCurrentScope, onScopeDispose } from 'vue'
import { api, ApiError } from '../api/client.js'
import { readSSE } from '../lib/sse.js'

export const FIELDS = Object.freeze(['sms_content', 'email_subject', 'email_body'])

/**
 * useComposeStream consumes POST /api/campaigns/compose/stream.
 *
 * Deltas are display-only (the "typing" effect). The server's `done` event
 * carries the validated draft, which replaces the streamed text — the UI
 * reviews and approves what the validator accepted, not what was typed.
 *
 *   status: idle | streaming | done | error
 *   retry: the model's first answer failed validation; fields are cleared and
 *          attempt 2 streams with the validator's feedback.
 *   error: the last attempt was rejected too; `rejected` holds it so the
 *          marketer can fix it by hand.
 */
export function useComposeStream({ open = api.composeStream } = {}) {
  const fields = reactive({ sms_content: '', email_subject: '', email_body: '' })
  const status = ref('idle')
  const attempt = ref(0)
  const activeField = ref(null)
  const draft = ref(null)
  const retryIssues = ref([])
  const issues = ref([])
  const error = ref('')
  const rejected = ref(null)
  let controller = null

  function clearFields() {
    for (const f of FIELDS) fields[f] = ''
  }

  function reset() {
    stop()
    clearFields()
    status.value = 'idle'
    attempt.value = 0
    activeField.value = null
    draft.value = null
    retryIssues.value = []
    issues.value = []
    error.value = ''
    rejected.value = null
  }

  function handle(ev) {
    let data
    try {
      data = JSON.parse(ev.data)
    } catch {
      return
    }
    const type = data.type || ev.event
    switch (type) {
      case 'delta':
        if (!FIELDS.includes(data.field)) return
        attempt.value = data.attempt || attempt.value || 1
        activeField.value = data.field
        fields[data.field] += data.delta || ''
        break
      case 'retry':
        retryIssues.value = data.issues || []
        attempt.value = (data.attempt || attempt.value || 1) + 1
        activeField.value = null
        clearFields()
        break
      case 'done': {
        const d = data.draft
        draft.value = d
        for (const f of FIELDS) fields[f] = d?.message?.[f] ?? ''
        activeField.value = null
        status.value = 'done'
        break
      }
      case 'error':
        error.value = data.error || 'İçerik üretilemedi.'
        issues.value = data.issues || []
        rejected.value = data.rejected || null
        activeField.value = null
        status.value = 'error'
        break
    }
  }

  async function start(brief) {
    reset()
    controller = new AbortController()
    const { signal } = controller
    status.value = 'streaming'
    attempt.value = 1
    try {
      const res = await open(brief, { signal })
      for await (const ev of readSSE(res, signal)) {
        handle(ev)
        if (status.value !== 'streaming') break
      }
      if (status.value === 'streaming' && !signal.aborted) {
        status.value = 'error'
        error.value = 'Akış tamamlanmadan kesildi. Tekrar deneyin.'
      }
    } catch (err) {
      if (signal.aborted || err?.name === 'AbortError') return
      status.value = 'error'
      error.value = err instanceof ApiError ? err.userMessage : String(err?.message || err)
    } finally {
      if (controller?.signal === signal) controller = null
    }
  }

  function stop() {
    controller?.abort()
    controller = null
  }

  if (getCurrentScope()) onScopeDispose(stop)

  return { fields, status, attempt, activeField, draft, retryIssues, issues, error, rejected, start, stop, reset }
}
