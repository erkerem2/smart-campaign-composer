import { ref, reactive, computed } from 'vue'
import { api, ApiError } from '../api/client.js'
import { useComposeStream, FIELDS } from './useComposeStream.js'
import { useDispatchStream } from './useDispatchStream.js'
import { measure, normalize, LIMITS } from '../lib/text.js'

/** The reference brief from CASE_SPEC.md ("Örnek brief"). */
export const SAMPLE_BRIEF =
  "Son 30 günde sepetine ürün ekleyip alışveriş yapmayan İstanbul'daki kullanıcılara %30 indirim hatırlatması gönder. Samimi ama kısa olsun."

export const MAX_BRIEF = 2000

export const STEPS = Object.freeze([
  { id: 1, key: 'brief', title: 'Brief' },
  { id: 2, key: 'audience', title: 'Kitle' },
  { id: 3, key: 'compose', title: 'Mesajlar' },
  { id: 4, key: 'review', title: 'Onay' },
  { id: 5, key: 'dispatch', title: 'Gönderim' },
])

/**
 * useCampaignFlow is the wizard's single source of truth:
 *
 *   brief ─► preview (rule + audience) ─► compose stream ─► human edits ─►
 *   approve (POST /api/campaigns) ─► dispatch (POST …/dispatch) ─► live stats
 *
 * Every later step is derived from the earlier ones: changing the brief
 * invalidates the audience and the content; once a campaign is approved the
 * earlier steps are read-only (a new campaign starts the flow over).
 */
export function useCampaignFlow({ client = api, compose: composeOpts, dispatch: dispatchOpts } = {}) {
  const step = ref(1)
  const brief = ref('')

  // Step 2
  const preview = ref(null) // {brief, rule, audience}
  const previewing = ref(false)
  const previewError = ref(null) // ApiError

  // Step 3
  const compose = useComposeStream({ open: client.composeStream, ...composeOpts })
  const composedFor = ref('') // brief the current content was generated from

  // Step 4
  const edits = reactive({ sms_content: '', email_subject: '', email_body: '' })
  const original = reactive({ sms_content: '', email_subject: '', email_body: '' })
  const serverIssues = ref([]) // 422 invalid_content issues from approval
  const approving = ref(false)
  const approveError = ref(null)

  // Step 5
  const campaign = ref(null) // POST /api/campaigns response
  const dispatchInfo = ref(null) // POST /dispatch response
  const dispatchError = ref(null)
  const dispatching = ref(false)
  const live = useDispatchStream({ fetchStats: client.dispatchStats, ...dispatchOpts })

  const trimmedBrief = computed(() => brief.value.trim())
  const briefDirty = computed(() => !preview.value || trimmedBrief.value !== preview.value.brief)
  const locked = computed(() => campaign.value !== null)

  const reached = computed(() => {
    if (campaign.value) return 5
    if (briefDirty.value) return 1
    const st = compose.status.value
    if (st === 'done' || (st === 'error' && compose.rejected.value)) return 4
    if (st !== 'idle') return 3
    return 2
  })

  function canVisit(id) {
    if (locked.value) return id === 5
    return id <= reached.value
  }

  function goTo(id) {
    if (canVisit(id)) step.value = id
  }

  // ── Step 1 → 2 ────────────────────────────────────────────────────────
  async function runPreview() {
    const b = trimmedBrief.value
    if (!b || b.length > MAX_BRIEF || locked.value) return
    previewing.value = true
    previewError.value = null
    try {
      const p = await client.previewSegment({ brief: b })
      preview.value = { ...p, brief: b }
      if (composedFor.value !== b) resetContent()
      step.value = 2
    } catch (err) {
      previewError.value = toError(err)
    } finally {
      previewing.value = false
    }
  }

  /** Re-validates a rule edited as JSON in the audience card (no LLM call). */
  async function revalidateRule(rule) {
    if (!preview.value || locked.value) return false
    previewing.value = true
    previewError.value = null
    try {
      const p = await client.previewSegment({ brief: preview.value.brief, rule })
      preview.value = { ...p, brief: preview.value.brief }
      return true
    } catch (err) {
      previewError.value = toError(err)
      return false
    } finally {
      previewing.value = false
    }
  }

  // ── Step 2 → 3 ────────────────────────────────────────────────────────
  async function startCompose() {
    if (!preview.value || locked.value) return
    step.value = 3
    composedFor.value = preview.value.brief
    serverIssues.value = []
    await compose.start(preview.value.brief)
    const src = compose.draft.value?.message || compose.rejected.value
    if (src) {
      for (const f of FIELDS) {
        original[f] = src[f] ?? ''
        edits[f] = src[f] ?? ''
      }
    }
  }

  function resetContent() {
    compose.reset()
    composedFor.value = ''
    for (const f of FIELDS) {
      edits[f] = ''
      original[f] = ''
    }
    serverIssues.value = []
  }

  // ── Step 4 ────────────────────────────────────────────────────────────
  const checks = computed(() => ({
    sms_content: measure(edits.sms_content, LIMITS.sms_content),
    email_subject: measure(edits.email_subject, LIMITS.email_subject),
    email_body: measure(edits.email_body, 0),
  }))

  const clientIssues = computed(() => {
    const out = []
    for (const f of FIELDS) {
      const c = checks.value[f]
      if (c.empty) out.push({ field: f, code: 'empty', message: 'Boş bırakılamaz.' })
      if (c.over) out.push({ field: f, code: 'too_long', message: `En fazla ${c.limit} karakter.` })
      for (const u of c.unknown) out.push({ field: f, code: 'unknown_variable', message: `{{${u}}} desteklenmiyor.` })
      for (const m of c.malformed) out.push({ field: f, code: 'malformed_placeholder', message: m })
      if (f === 'email_subject' && /\n/.test(normalize(edits[f]))) {
        out.push({ field: f, code: 'forbidden_character', message: 'Konu tek satır olmalı.' })
      }
    }
    return out
  })

  const canApprove = computed(
    () => !locked.value && !!preview.value && !briefDirty.value && clientIssues.value.length === 0 && !approving.value,
  )

  const edited = computed(() => FIELDS.filter((f) => normalize(edits[f]) !== normalize(original[f])))

  function setField(f, v) {
    edits[f] = v
    serverIssues.value = serverIssues.value.filter((i) => i.field !== f)
  }

  function restoreField(f) {
    setField(f, original[f])
  }

  // ── Step 4 → 5 ────────────────────────────────────────────────────────
  async function approveAndSend() {
    if (!canApprove.value) return
    approving.value = true
    approveError.value = null
    serverIssues.value = []
    try {
      const messages = {}
      for (const f of FIELDS) messages[f] = normalize(edits[f])
      campaign.value = await client.createCampaign({
        brief: preview.value.brief,
        segmentRule: preview.value.rule,
        messages,
      })
    } catch (err) {
      const e = toError(err)
      if (e.code === 'invalid_content') serverIssues.value = e.issues
      approveError.value = e
      approving.value = false
      return
    }
    approving.value = false
    step.value = 5
    await startDispatch()
  }

  /** Starts (or resumes, for an interrupted run) delivery and follows it live. */
  async function startDispatch() {
    const id = campaign.value?.campaign?.id
    if (!id || dispatching.value) return
    dispatching.value = true
    dispatchError.value = null
    try {
      dispatchInfo.value = await client.dispatch(id)
      live.connect(id, dispatchInfo.value.stream_url)
    } catch (err) {
      const e = toError(err)
      if (e.code === 'already_running') {
        live.connect(id) // another tab or a double click: just follow it
      } else {
        dispatchError.value = e
      }
    } finally {
      dispatching.value = false
    }
  }

  function newCampaign() {
    live.reset()
    resetContent()
    preview.value = null
    previewError.value = null
    approveError.value = null
    campaign.value = null
    dispatchInfo.value = null
    dispatchError.value = null
    brief.value = ''
    step.value = 1
  }

  return {
    step, brief, trimmedBrief, briefDirty, locked, reached, canVisit, goTo,
    preview, previewing, previewError, runPreview, revalidateRule,
    compose, startCompose,
    edits, original, checks, clientIssues, serverIssues, edited, canApprove, setField, restoreField,
    approving, approveError, approveAndSend,
    campaign, dispatchInfo, dispatchError, dispatching, startDispatch, live,
    newCampaign,
  }
}

function toError(err) {
  if (err instanceof ApiError) return err
  return new ApiError({ code: 'unknown', message: String(err?.message || err) })
}
