<script setup>
import { computed } from 'vue'
import { smsLabel } from '../../lib/sms.js'

// Live counter for a channel limit. `check` is the result of lib/text.measure.
// For SMS, `sms` (lib/sms.smsInfo) adds the encoding and the number of SMS
// parts. It is informational: the limit that blocks approval stays `check`.
const props = defineProps({
  check: { type: Object, required: true },
  sms: { type: Object, default: null },
  id: { type: String, default: undefined },
})

const shown = computed(() => Math.max(props.check.runes, props.check.estimated))
const near = computed(() => !props.check.over && props.check.limit && shown.value > props.check.limit * 0.9)
const personalised = computed(() => props.check.variables.length > 0 && props.check.estimated !== props.check.runes)

const smsText = computed(() => (props.sms ? smsLabel(props.sms) : ''))
const multipart = computed(() => props.sms && props.sms.segments > 1)
// Names (and the "Değerli müşterimiz" fallback) may contain ş, ğ, ı…: for
// those recipients a GSM-7 template is sent as UCS-2.
const worstSegments = computed(() => (props.sms?.worst && props.sms.worst.segments > props.sms.segments ? props.sms.worst.segments : 0))
const smsTitle = computed(() => {
  const s = props.sms
  if (!s) return undefined
  return `${s.encoding}: tek SMS'te ${s.limit}, çok parçalı mesajda parça başına ${s.perPart} karakter.`
})
</script>

<template>
  <div :id="id" aria-live="polite">
    <p class="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1 text-sm">
      <span class="text-ink-3">
        <template v-if="personalised">Kişiselleştirilmiş hali ~{{ check.estimated }} karakter</template>
      </span>
      <span
        class="tnum font-semibold"
        :class="check.over ? 'text-bad' : near ? 'text-warn' : 'text-ink-2'"
        :data-state="check.over ? 'over' : near ? 'near' : 'ok'"
      >
        {{ check.runes }} / {{ check.limit }}
        <span v-if="check.over" class="font-medium"> ({{ shown - check.limit }} fazla)</span>
      </span>
    </p>
    <p
      v-if="sms"
      class="mt-0.5 flex flex-wrap items-baseline justify-end gap-x-2 text-xs text-ink-3"
      data-test="sms-encoding"
      :data-encoding="sms.encoding"
      :data-segments="sms.segments"
      :title="smsTitle"
    >
      <span v-if="sms.nonGsm.length" data-test="sms-non-gsm">
        Türkçe/özel karakter: <span class="font-mono">{{ sms.nonGsm.slice(0, 6).join(' ') }}</span><template v-if="sms.nonGsm.length > 6"> …</template>
      </span>
      <span v-if="worstSegments" data-test="sms-worst">İsimde Türkçe karakter varsa UCS-2 - {{ worstSegments }} SMS</span>
      <span class="tnum font-semibold" :class="multipart ? 'text-warn' : 'text-ink-2'">{{ smsText }}</span>
    </p>
  </div>
</template>
