<script setup>
import { ref, computed, onMounted, onBeforeUnmount } from 'vue'
import { useHealth } from '../../composables/useHealth.js'
import { fmtDuration } from '../../lib/format.js'

const props = defineProps({ intervalMs: { type: Number, default: 10_000 } })
const health = useHealth({ intervalMs: props.intervalMs })
const open = ref(false)
const root = ref(null)

const view = computed(() => {
  switch (health.status.value) {
    case 'ok':
      return { label: 'Sistem çalışıyor', dot: 'bg-good', ring: 'ring-good/25', text: 'text-good' }
    case 'degraded':
      return { label: 'Kısmi sorun', dot: 'bg-warn', ring: 'ring-warn/25', text: 'text-warn' }
    case 'unhealthy':
      return { label: 'Sistem hatası', dot: 'bg-bad', ring: 'ring-bad/25', text: 'text-bad' }
    case 'unreachable':
      return { label: 'API erişilemiyor', dot: 'bg-bad', ring: 'ring-bad/25', text: 'text-bad' }
    default:
      return { label: 'Kontrol ediliyor', dot: 'bg-ink-3', ring: 'ring-line', text: 'text-ink-2' }
  }
})

const CHECK_NAMES = { database: 'Veritabanı', provider: 'SMS / e-posta sağlayıcı' }
const llm = computed(() => health.system.value?.llm_mode)

function onDocClick(e) {
  if (open.value && root.value && !root.value.contains(e.target)) open.value = false
}
function onKey(e) {
  if (e.key === 'Escape') open.value = false
}
onMounted(() => {
  document.addEventListener('click', onDocClick)
  document.addEventListener('keydown', onKey)
})
onBeforeUnmount(() => {
  document.removeEventListener('click', onDocClick)
  document.removeEventListener('keydown', onKey)
})
</script>

<template>
  <div ref="root" class="relative flex items-center gap-2">
    <span
      v-if="llm"
      class="hidden rounded-full border border-line px-2.5 py-1 text-xs font-medium text-ink-2 sm:inline"
      :title="llm === 'mock' ? 'ANTHROPIC_API_KEY tanımlı değil: sabit yanıt veren mock LLM' : 'Claude API'"
    >
      {{ llm === 'mock' ? 'Mock LLM' : 'Claude API' }}
    </span>
    <button
      type="button"
      class="flex items-center gap-2 rounded-full border border-line bg-surface py-1 pl-2.5 pr-3 text-sm font-medium hover:border-line-strong"
      :aria-expanded="open"
      aria-controls="health-panel"
      data-test="health-badge"
      :data-status="health.status.value"
      @click="open = !open"
    >
      <span class="relative flex h-2.5 w-2.5">
        <span
          v-if="health.status.value === 'ok'"
          class="absolute inline-flex h-full w-full animate-ping rounded-full bg-good opacity-40 motion-reduce:hidden"
        />
        <span class="relative inline-flex h-2.5 w-2.5 rounded-full ring-4" :class="[view.dot, view.ring]" />
      </span>
      <span class="whitespace-nowrap" :class="view.text">{{ view.label }}</span>
    </button>

    <div
      v-if="open"
      id="health-panel"
      class="panel absolute right-0 top-full z-20 mt-2 w-72 p-4 text-sm shadow-[0_16px_36px_-20px_rgba(23,32,54,0.45)]"
    >
      <p class="font-semibold text-ink">/healthz</p>
      <p v-if="health.status.value === 'unreachable'" class="mt-1 text-ink-2">
        Backend yanıt vermiyor. <code class="font-mono text-xs">make run</code> ile :8080'de başlatın.
      </p>
      <ul v-else class="mt-2 space-y-1.5">
        <li v-for="c in health.checks.value" :key="c.name" class="flex items-center justify-between gap-3">
          <span class="text-ink-2">{{ CHECK_NAMES[c.name] || c.name }}</span>
          <span :class="c.status === 'ok' ? 'text-good' : c.critical ? 'text-bad' : 'text-warn'" class="font-medium">
            {{ c.status === 'ok' ? `çalışıyor, ${Math.round(c.latency_ms)} ms` : 'yanıt yok' }}
          </span>
        </li>
      </ul>
      <dl v-if="health.system.value" class="mt-3 grid grid-cols-2 gap-x-3 gap-y-1 border-t border-line pt-3 text-ink-2">
        <dt>Çalışma süresi</dt>
        <dd class="tnum text-right text-ink">{{ fmtDuration(health.system.value.uptime_seconds) }}</dd>
        <dt>Aktif gönderim</dt>
        <dd class="tnum text-right text-ink">{{ health.system.value.active_dispatches }}</dd>
        <dt>Sürüm</dt>
        <dd class="text-right text-ink">{{ health.system.value.version || '–' }}</dd>
      </dl>
      <p class="mt-3 text-xs text-ink-3">Her {{ Math.round(intervalMs / 1000) }} saniyede bir kontrol edilir.</p>
    </div>
  </div>
</template>
