<script setup>
import { computed } from 'vue'
import Icon from './Icon.vue'

const props = defineProps({
  tone: { type: String, default: 'info' }, // info | good | warn | bad
  title: { type: String, default: '' },
})

const styles = {
  info: { box: 'bg-accent-soft border-accent/25', icon: 'text-accent', name: 'info' },
  good: { box: 'bg-good-soft border-good/25', icon: 'text-good', name: 'check' },
  warn: { box: 'bg-warn-soft border-warn/30', icon: 'text-warn', name: 'alert' },
  bad: { box: 'bg-bad-soft border-bad/25', icon: 'text-bad', name: 'alert' },
}
const s = computed(() => styles[props.tone] || styles.info)
</script>

<template>
  <div
    class="flex gap-3 rounded-[var(--radius-control)] border px-4 py-3 text-[0.9375rem] leading-relaxed"
    :class="s.box"
    :role="tone === 'bad' ? 'alert' : 'status'"
  >
    <Icon :name="s.name" :class="s.icon" class="mt-0.5" />
    <div class="min-w-0 flex-1">
      <p v-if="title" class="font-semibold text-ink">{{ title }}</p>
      <div class="text-ink-2"><slot /></div>
    </div>
  </div>
</template>
