<script setup>
import { computed } from 'vue'
import { splitTemplate } from '../../lib/text.js'

// Renders a template as plain text with {{variables}} highlighted. Never
// uses v-html: model output and marketer edits are untrusted text.
const props = defineProps({ text: { type: String, default: '' } })
const parts = computed(() => splitTemplate(props.text))
</script>

<template>
  <span class="whitespace-pre-wrap break-words"
    ><template v-for="(p, i) in parts" :key="i"
      ><mark
        v-if="p.variable"
        class="rounded px-0.5 font-mono text-[0.85em]"
        :class="p.known ? 'bg-accent-soft text-accent-strong' : 'bg-bad-soft text-bad'"
        >{{ p.text }}</mark
      ><template v-else>{{ p.text }}</template></template
    ><slot
  /></span>
</template>
