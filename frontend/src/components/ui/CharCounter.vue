<script setup>
import { computed } from 'vue'

// Live counter for a channel limit. `check` is the result of lib/text.measure.
const props = defineProps({
  check: { type: Object, required: true },
  id: { type: String, default: undefined },
})

const shown = computed(() => Math.max(props.check.runes, props.check.estimated))
const near = computed(() => !props.check.over && props.check.limit && shown.value > props.check.limit * 0.9)
const personalised = computed(() => props.check.variables.length > 0 && props.check.estimated !== props.check.runes)
</script>

<template>
  <p
    :id="id"
    class="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1 text-sm"
    aria-live="polite"
  >
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
</template>
