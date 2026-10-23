<script setup>
import { ref, computed, nextTick } from 'vue'
import Icon from './Icon.vue'
import CharCounter from './CharCounter.vue'
import { VARIABLES, token } from '../../lib/text.js'

// One editable content field of the approval step: label, "edited" marker
// with a way back to Claude's suggestion, live counter, issues and
// variable shortcuts. State lives in the campaign flow.
const props = defineProps({
  flow: { type: Object, required: true },
  field: { type: String, required: true }, // sms_content | email_subject | email_body
  label: { type: String, required: true },
  channel: { type: String, required: true }, // sms | email
  multiline: { type: Boolean, default: true },
  rows: { type: Number, default: 4 },
  counter: { type: Boolean, default: true },
  variables: { type: Boolean, default: true },
  hint: { type: String, default: '' },
})

const f = props.flow
const el = ref(null)
const id = computed(() => `edit-${props.field}`)
const value = computed(() => f.edits[props.field])
const edited = computed(() => f.edited.value.includes(props.field))
const check = computed(() => f.checks.value[props.field])

// Length problems are already shown by the counter; list the rest.
const issues = computed(() => [
  ...f.clientIssues.value.filter((i) => i.field === props.field && i.code !== 'empty' && i.code !== 'too_long'),
  ...f.serverIssues.value.filter((i) => i.field === props.field),
])
const invalid = computed(
  () => f.clientIssues.value.some((i) => i.field === props.field) || f.serverIssues.value.some((i) => i.field === props.field),
)
const describedBy = computed(() => [props.counter && `${id.value}-count`, issues.value.length && `${id.value}-issues`, props.hint && `${id.value}-hint`].filter(Boolean).join(' '))

function onInput(e) {
  f.setField(props.field, e.target.value)
}

async function insertVariable(name) {
  const t = token(name)
  const v = value.value
  const start = el.value?.selectionStart ?? v.length
  const end = el.value?.selectionEnd ?? v.length
  f.setField(props.field, v.slice(0, start) + t + v.slice(end))
  await nextTick()
  el.value?.focus()
  el.value?.setSelectionRange(start + t.length, start + t.length)
}
</script>

<template>
  <div :data-field="field">
    <div class="mb-2 flex flex-wrap items-center gap-2">
      <span class="grid h-6 w-6 place-items-center rounded-full text-white" :class="channel === 'sms' ? 'bg-sms' : 'bg-email'">
        <Icon :name="channel === 'sms' ? 'sms' : 'mail'" :size="13" />
      </span>
      <label :for="id" class="font-semibold text-ink">{{ label }}</label>
      <span v-if="edited" class="rounded-full bg-accent-soft px-2 py-0.5 text-xs font-semibold text-accent-strong">Düzenlendi</span>
      <button
        v-if="edited && !f.locked.value"
        type="button"
        class="ml-auto inline-flex items-center gap-1 text-sm font-medium text-ink-2 hover:text-ink"
        @click="f.restoreField(field)"
      >
        <Icon name="undo" :size="15" /> Claude'un önerisine dön
      </button>
    </div>

    <textarea
      v-if="multiline"
      :id="id"
      ref="el"
      :value="value"
      :rows="rows"
      class="field resize-y"
      :readonly="f.locked.value"
      :aria-invalid="invalid"
      :aria-describedby="describedBy"
      :data-test="`edit-${field}`"
      @input="onInput"
    />
    <input
      v-else
      :id="id"
      ref="el"
      :value="value"
      type="text"
      class="field"
      :readonly="f.locked.value"
      :aria-invalid="invalid"
      :aria-describedby="describedBy"
      :data-test="`edit-${field}`"
      @input="onInput"
    />

    <CharCounter v-if="counter" :id="`${id}-count`" :check="check" class="mt-1.5" :data-test="`counter-${field}`" />
    <p v-if="hint" :id="`${id}-hint`" class="mt-1.5 text-sm text-ink-3">{{ hint }}</p>
    <ul v-if="issues.length" :id="`${id}-issues`" class="mt-1.5 space-y-0.5 text-sm text-bad" :data-test="`issues-${field}`">
      <li v-for="(i, k) in issues" :key="k">{{ i.message }}</li>
    </ul>

    <div v-if="variables && !f.locked.value" class="mt-2 flex flex-wrap gap-2">
      <button
        v-for="v in VARIABLES"
        :key="v.name"
        type="button"
        class="inline-flex items-center gap-1 rounded-full border border-line-strong px-2.5 py-1 font-mono text-xs text-ink-2 hover:border-accent hover:text-accent"
        :title="`${v.label} ekle`"
        @click="insertVariable(v.name)"
      >
        <Icon name="plus" :size="12" /><span v-text="token(v.name)" />
      </button>
    </div>
  </div>
</template>
