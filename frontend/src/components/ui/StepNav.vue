<script setup>
import Icon from './Icon.vue'

// The five steps are a real sequence, so they are numbered.
const props = defineProps({
  steps: { type: Array, required: true },
  current: { type: Number, required: true },
  reached: { type: Number, required: true },
  canVisit: { type: Function, required: true },
})
const emit = defineEmits(['go'])

function state(id) {
  if (id === props.current) return 'current'
  if (id < props.reached) return 'done'
  return props.canVisit(id) ? 'available' : 'locked'
}
</script>

<template>
  <nav aria-label="Kampanya adımları">
    <ol class="flex items-center gap-1 overflow-x-auto sm:gap-2">
      <li v-for="(s, i) in steps" :key="s.id" class="flex shrink-0 items-center gap-1 sm:gap-2">
        <button
          type="button"
          class="group flex items-center gap-2 rounded-full py-1.5 pl-1.5 pr-3 text-sm font-medium transition-colors"
          :class="{
            'bg-accent text-white': state(s.id) === 'current',
            'text-ink hover:bg-accent-soft': state(s.id) === 'done' || state(s.id) === 'available',
            'text-ink-3': state(s.id) === 'locked',
          }"
          :disabled="!canVisit(s.id) || state(s.id) === 'current'"
          :aria-current="state(s.id) === 'current' ? 'step' : undefined"
          @click="emit('go', s.id)"
        >
          <span
            class="tnum grid h-6 w-6 place-items-center rounded-full text-xs font-semibold"
            :class="{
              'bg-white text-accent': state(s.id) === 'current',
              'bg-good text-white': state(s.id) === 'done',
              'border border-line-strong': state(s.id) === 'available' || state(s.id) === 'locked',
            }"
          >
            <Icon v-if="state(s.id) === 'done'" name="check" :size="14" />
            <template v-else>{{ s.id }}</template>
          </span>
          {{ s.title }}
        </button>
        <span v-if="i < steps.length - 1" class="h-px w-4 bg-line-strong sm:w-8" aria-hidden="true" />
      </li>
    </ol>
  </nav>
</template>
