<script setup>
import { computed } from 'vue'
import { describeCondition, LOGIC_LABELS } from '../../lib/rule.js'

defineOptions({ name: 'RuleNode' })
const props = defineProps({
  node: { type: Object, required: true },
  depth: { type: Number, default: 0 },
})
const cond = computed(() => (props.node.type === 'condition' ? describeCondition(props.node) : null))
const raw = computed(() => {
  const n = props.node
  return `${n.field} ${n.operator} ${JSON.stringify(n.value)}`
})
</script>

<template>
  <div v-if="node.type === 'group'" :class="depth > 0 ? 'rounded-[var(--radius-control)] border border-line p-3' : ''">
    <p class="mb-2 text-sm font-medium text-ink-2">{{ LOGIC_LABELS[node.logic] || node.logic }}</p>
    <ul class="space-y-2">
      <li v-for="(r, i) in node.rules" :key="i">
        <RuleNode :node="r" :depth="depth + 1" />
      </li>
    </ul>
  </div>
  <div v-else class="flex flex-wrap items-baseline gap-x-3 gap-y-0.5 rounded-[var(--radius-control)] bg-paper px-3 py-2">
    <span class="min-w-[9.5rem] text-sm text-ink-3">{{ cond.label }}</span>
    <span class="font-medium text-ink">{{ cond.text }}</span>
    <code class="ml-auto font-mono text-xs text-ink-3">{{ raw }}</code>
  </div>
</template>
