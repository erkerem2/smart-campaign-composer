<script setup>
import { ref, computed, onMounted, onBeforeUnmount } from 'vue'
import { fmtRate } from '../../lib/format.js'

// Per-channel throughput over the run (msg/s), against the 100 msg/s
// per-channel limit. Two series → legend + direct end labels; hover shows a
// crosshair with both values. Text uses ink tokens, marks carry the colour.
const props = defineProps({
  samples: { type: Array, required: true }, // [{t, sms, email}]
  limit: { type: Number, default: 100 },
  height: { type: Number, default: 200 },
})

const SERIES = [
  { key: 'sms', label: 'SMS', color: 'var(--color-sms)' },
  { key: 'email', label: 'E-posta', color: 'var(--color-email)' },
]

const el = ref(null)
const width = ref(640)
let ro = null
onMounted(() => {
  if (!el.value) return
  width.value = el.value.clientWidth || 640
  if ('ResizeObserver' in window) {
    ro = new ResizeObserver(([e]) => (width.value = Math.max(280, e.contentRect.width)))
    ro.observe(el.value)
  }
})
onBeforeUnmount(() => ro?.disconnect())

const pad = { top: 14, right: 64, bottom: 26, left: 36 }
const innerW = computed(() => Math.max(10, width.value - pad.left - pad.right))
const innerH = computed(() => props.height - pad.top - pad.bottom)

const tMax = computed(() => Math.max(10, ...props.samples.map((s) => s.t)))
const yMax = computed(() => {
  const peak = Math.max(0, ...props.samples.flatMap((s) => [s.sms, s.email]))
  return Math.max(props.limit * 1.2, Math.ceil((peak * 1.1) / 20) * 20)
})
const x = (t) => pad.left + (t / tMax.value) * innerW.value
const y = (v) => pad.top + innerH.value - (v / yMax.value) * innerH.value

const yTicks = computed(() => {
  const step = yMax.value > 150 ? 50 : 25
  const out = []
  for (let v = 0; v <= yMax.value; v += step) out.push(v)
  return out
})
const xTicks = computed(() => {
  const step = tMax.value > 120 ? 30 : tMax.value > 40 ? 10 : 5
  const out = []
  for (let t = 0; t <= tMax.value; t += step) out.push(t)
  return out
})

const lines = computed(() =>
  SERIES.map((s) => {
    const pts = props.samples.map((p) => `${x(p.t).toFixed(1)},${y(p[s.key]).toFixed(1)}`)
    const last = props.samples.at(-1)
    return { ...s, d: pts.length ? 'M' + pts.join('L') : '', last: last ? { x: x(last.t), y: y(last[s.key]), v: last[s.key] } : null }
  }),
)

// keep the two end labels from colliding
const endLabels = computed(() => {
  const ls = lines.value.filter((l) => l.last).map((l) => ({ ...l, ly: l.last.y }))
  if (ls.length === 2 && Math.abs(ls[0].ly - ls[1].ly) < 14) {
    const [a, b] = ls[0].ly <= ls[1].ly ? [ls[0], ls[1]] : [ls[1], ls[0]]
    const mid = (a.ly + b.ly) / 2
    a.ly = mid - 7
    b.ly = mid + 7
  }
  return ls
})

const hover = ref(null)
function onMove(e) {
  if (!props.samples.length) return
  const rect = e.currentTarget.getBoundingClientRect()
  const px = ((e.clientX - rect.left) / rect.width) * width.value
  const t = ((px - pad.left) / innerW.value) * tMax.value
  let best = props.samples[0]
  for (const s of props.samples) if (Math.abs(s.t - t) < Math.abs(best.t - t)) best = s
  hover.value = best
}
</script>

<template>
  <div ref="el" class="relative w-full">
    <svg
      :width="width"
      :height="height"
      :viewBox="`0 0 ${width} ${height}`"
      role="img"
      aria-label="Kanal başına anlık gönderim hızı, saniyede mesaj"
      class="block select-none"
      @pointermove="onMove"
      @pointerleave="hover = null"
    >
      <g class="text-[11px]" fill="var(--color-ink-3)">
        <g v-for="v in yTicks" :key="'y' + v">
          <line :x1="pad.left" :x2="pad.left + innerW" :y1="y(v)" :y2="y(v)" stroke="var(--color-line)" stroke-width="1" />
          <text :x="pad.left - 8" :y="y(v) + 4" text-anchor="end" class="tnum">{{ v }}</text>
        </g>
        <text v-for="t in xTicks" :key="'x' + t" :x="x(t)" :y="height - 8" text-anchor="middle" class="tnum">{{ t }} sn</text>
      </g>

      <!-- the per-channel cap -->
      <line
        :x1="pad.left"
        :x2="pad.left + innerW"
        :y1="y(limit)"
        :y2="y(limit)"
        stroke="var(--color-ink-2)"
        stroke-width="1.25"
        stroke-dasharray="5 4"
      />
      <text :x="pad.left + 6" :y="y(limit) - 6" class="text-[11px]" fill="var(--color-ink-2)" stroke="var(--color-surface)" stroke-width="4" paint-order="stroke">
        Kanal limiti {{ limit }}/sn
      </text>

      <path
        v-for="l in lines"
        :key="l.key"
        :d="l.d"
        fill="none"
        :stroke="l.color"
        stroke-width="2"
        stroke-linejoin="round"
        stroke-linecap="round"
      />
      <g v-for="l in endLabels" :key="'e' + l.key">
        <circle :cx="l.last.x" :cy="l.last.y" r="4" :fill="l.color" stroke="var(--color-surface)" stroke-width="2" />
        <text
          :x="l.last.x + 9"
          :y="l.ly + 4"
          class="tnum text-[12px] font-semibold"
          fill="var(--color-ink)"
          stroke="var(--color-surface)"
          stroke-width="4"
          paint-order="stroke"
        >
          {{ l.label }} {{ fmtRate(l.last.v) }}
        </text>
      </g>

      <g v-if="hover">
        <line :x1="x(hover.t)" :x2="x(hover.t)" :y1="pad.top" :y2="pad.top + innerH" stroke="var(--color-ink-3)" stroke-width="1" />
        <circle v-for="l in lines" :key="'h' + l.key" :cx="x(hover.t)" :cy="y(hover[l.key])" r="4" :fill="l.color" stroke="var(--color-surface)" stroke-width="2" />
      </g>
    </svg>

    <div
      v-if="hover"
      class="panel pointer-events-none absolute top-2 px-3 py-2 text-xs shadow-[0_10px_24px_-14px_rgba(23,32,54,0.5)]"
      :style="{ left: Math.min(x(hover.t) + 12, width - 150) + 'px' }"
    >
      <p class="tnum mb-1 text-ink-3">{{ fmtRate(hover.t) }}. saniye</p>
      <p v-for="s in SERIES" :key="s.key" class="tnum flex items-center gap-2 text-ink">
        <span class="h-2 w-2 rounded-full" :style="{ background: s.color }" />
        {{ s.label }} <span class="ml-auto font-semibold">{{ fmtRate(hover[s.key]) }} msg/sn</span>
      </p>
    </div>

    <ul class="mt-2 flex gap-4 text-sm text-ink-2" aria-label="Seriler">
      <li v-for="s in SERIES" :key="s.key" class="flex items-center gap-2">
        <span class="h-0.5 w-4 rounded" :style="{ background: s.color }" />{{ s.label }}
      </li>
      <li class="flex items-center gap-2">
        <span class="w-4 border-t-[1.5px] border-dashed border-ink-2" />Limit
      </li>
    </ul>
  </div>
</template>
