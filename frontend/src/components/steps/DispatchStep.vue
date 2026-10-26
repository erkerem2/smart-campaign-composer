<script setup>
import { computed } from 'vue'
import Icon from '../ui/Icon.vue'
import Notice from '../ui/Notice.vue'
import ThroughputChart from '../ui/ThroughputChart.vue'
import { fmtInt, fmtRate, fmtPercent, fmtDuration } from '../../lib/format.js'

const props = defineProps({ flow: { type: Object, required: true } })
const f = props.flow
const live = f.live

const RATE_LIMIT = 100 // msg/s per channel (CLAUDE.md, ADR 007)

const s = computed(() => live.stats.value)
const id = computed(() => f.campaign.value?.campaign?.id)

const badge = computed(() => {
  const st = live.state.value
  if (!s.value) return { label: 'Başlatılıyor', icon: 'refresh', cls: 'bg-accent-soft text-accent-strong', spin: true }
  switch (st) {
    case 'planning':
      return { label: 'Alıcılar hazırlanıyor', icon: 'refresh', cls: 'bg-accent-soft text-accent-strong', spin: true }
    case 'sending':
      return { label: 'Gönderiliyor', icon: 'send', cls: 'bg-accent-soft text-accent-strong' }
    case 'completed':
      return { label: 'Tamamlandı', icon: 'check', cls: 'bg-good-soft text-good' }
    case 'failed':
      return { label: 'Başarısız', icon: 'x', cls: 'bg-bad-soft text-bad' }
    case 'interrupted':
      return { label: 'Yarıda kaldı', icon: 'alert', cls: 'bg-warn-soft text-warn' }
    default:
      return { label: 'Beklemede', icon: 'info', cls: 'bg-paper text-ink-2' }
  }
})

const percent = computed(() => Math.floor(live.progress.value * 100))
const tiles = computed(() => [
  { key: 'sent', label: 'Gönderildi', value: s.value?.sent ?? 0, icon: 'check', tone: 'text-good' },
  { key: 'failed', label: 'Hatalı', value: s.value?.failed ?? 0, icon: 'x', tone: (s.value?.failed ?? 0) > 0 ? 'text-bad' : 'text-ink-3' },
  { key: 'retried', label: 'Yeniden denendi', value: s.value?.retried ?? 0, icon: 'refresh', tone: 'text-warn' },
  { key: 'pending', label: 'Sırada', value: s.value?.pending ?? 0, icon: 'pulse', tone: 'text-ink-3' },
])

const CHANNELS = [
  { key: 'sms', label: 'SMS', bar: 'bg-sms', dot: 'bg-sms' },
  { key: 'email', label: 'E-posta', bar: 'bg-email', dot: 'bg-email' },
]
const channelRows = computed(() =>
  CHANNELS.map((c) => {
    const cs = s.value?.channels?.[c.key] || {}
    // While sending: the live rate; once finished: the channel's average.
    const rate = live.final.value ? cs.speed || 0 : live.instant.value[c.key] || 0
    return { ...c, ...cs, rate, meter: Math.min(1, rate / RATE_LIMIT) }
  }),
)

const isLive = computed(() => !!s.value?.live && !live.final.value)
const connectionLabel = computed(
  () =>
    ({
      connecting: 'Bağlanıyor',
      live: 'Canlı bağlantı',
      reconnecting: 'Yeniden bağlanıyor',
      polling: 'Her saniye yenileniyor',
      closed: 'Akış kapandı',
      failed: 'Durum alınamıyor',
    })[live.connection.value] || '',
)
</script>

<template>
  <section aria-labelledby="dispatch-title">
    <div class="flex flex-wrap items-center gap-3">
      <h1 tabindex="-1" id="dispatch-title" class="text-[2rem] font-bold leading-tight tracking-[-0.015em] text-ink">
        Kampanya #{{ id }} gönderimi
      </h1>
      <span class="inline-flex items-center gap-1.5 rounded-full px-3 py-1 text-sm font-semibold" :class="badge.cls" data-test="dispatch-state" :data-state="live.state.value">
        <Icon :name="badge.icon" :size="15" :class="{ 'animate-spin': badge.spin }" /> {{ badge.label }}
      </span>
    </div>
    <p class="mt-3 max-w-[64ch] text-[1.0625rem] leading-relaxed text-ink-2">
      Mesajlar kuyruktan kanal başına saniyede en fazla {{ RATE_LIMIT }} mesajla gönderiliyor. Sağlayıcı hata ya da 429
      dönerse mesaj aynı tekilleştirme anahtarıyla, artan bekleme süreleriyle yeniden denenir.
    </p>

    <Notice v-if="f.dispatchError.value" tone="bad" title="Gönderim başlatılamadı" class="mt-6">
      {{ f.dispatchError.value.userMessage }}
      <button type="button" class="ml-1 font-semibold text-accent hover:underline" @click="f.startDispatch()">Tekrar dene</button>
    </Notice>
    <Notice v-if="live.state.value === 'interrupted'" tone="warn" title="Gönderim yarıda kaldı" class="mt-6">
      Sunucu gönderim sırasında yeniden başlatıldı. Devam ettirdiğinizde yalnızca gönderilmemiş mesajlar gider; kimseye
      ikinci kez mesaj gitmez.
      <button type="button" class="ml-1 font-semibold text-accent hover:underline" :disabled="f.dispatching.value" @click="f.startDispatch()">
        Gönderime devam et
      </button>
    </Notice>
    <Notice v-if="s?.error" tone="bad" title="Gönderim hatası" class="mt-6">{{ s.error }}</Notice>
    <Notice v-if="live.connection.value === 'failed'" tone="bad" title="Gönderim durumu alınamıyor" class="mt-6" data-test="poll-failed">
      {{ live.pollError.value?.userMessage || 'Sunucu bu kampanyanın durumunu vermiyor.' }} Gönderim sunucuda sürüyorsa
      etkilenmez; sayfayı yenileyerek tekrar bakabilirsiniz.
    </Notice>

    <!-- progress -->
    <div class="panel mt-8 p-5">
      <div class="flex flex-wrap items-baseline justify-between gap-2">
        <p class="text-ink-2">
          <span class="tnum text-[2rem] font-bold leading-none tracking-tight text-ink" data-test="progress-percent">%{{ percent }}</span>
          <span class="tnum ml-2">{{ fmtInt(s?.processed) }} / {{ fmtInt(s?.total) }} mesaj işlendi</span>
        </p>
        <p class="flex items-center gap-2 text-sm text-ink-3">
          <span class="h-2 w-2 rounded-full" :class="isLive ? 'bg-good' : 'bg-line-strong'" aria-hidden="true" />
          {{ connectionLabel }}<template v-if="s?.elapsed_seconds">, {{ fmtDuration(s.elapsed_seconds) }}</template>
        </p>
      </div>
      <div
        class="mt-4 h-3 overflow-hidden rounded-full bg-paper"
        role="progressbar"
        aria-label="Gönderim ilerlemesi"
        :aria-valuenow="percent"
        aria-valuemin="0"
        aria-valuemax="100"
      >
        <div class="flex h-full gap-[2px]">
          <div class="h-full rounded-l-full bg-good transition-[width] duration-500" :style="{ width: s?.total ? (s.sent / s.total) * 100 + '%' : '0%' }" />
          <div v-if="s?.failed" class="h-full bg-bad transition-[width] duration-500" :style="{ width: (s.failed / s.total) * 100 + '%' }" />
        </div>
      </div>
      <p class="mt-2 flex gap-4 text-sm text-ink-3">
        <span class="flex items-center gap-1.5"><span class="h-2 w-2 rounded-full bg-good" />Gönderildi</span>
        <span class="flex items-center gap-1.5"><span class="h-2 w-2 rounded-full bg-bad" />Hatalı</span>
        <span class="flex items-center gap-1.5"><span class="h-2 w-2 rounded-full border border-line-strong bg-paper" />Sırada</span>
      </p>

      <dl class="mt-6 grid grid-cols-2 gap-x-6 gap-y-5 border-t border-line pt-5 sm:grid-cols-4" data-test="dispatch-tiles">
        <div v-for="t in tiles" :key="t.key" :data-key="t.key">
          <dt class="flex items-center gap-1.5 text-sm font-medium text-ink-2"><Icon :name="t.icon" :size="15" :class="t.tone" />{{ t.label }}</dt>
          <dd class="tnum mt-1 text-2xl font-bold text-ink">{{ fmtInt(t.value) }}</dd>
        </div>
      </dl>
    </div>

    <!-- speed -->
    <div class="mt-6 grid gap-6 lg:grid-cols-[18rem_minmax(0,1fr)]">
      <div class="panel p-5">
        <h2 class="font-semibold text-ink">{{ live.final.value ? 'Ortalama hız' : 'Anlık hız' }}</h2>
        <p class="mt-3">
          <span class="tnum text-[2.75rem] font-bold leading-none tracking-tight text-ink" data-test="speed">{{
            fmtRate(live.final.value ? s?.speed : live.instant.value.total)
          }}</span>
          <span class="ml-1 text-ink-2">msg/sn</span>
        </p>
        <p class="tnum mt-1 text-sm text-ink-3">
          <template v-if="live.final.value">Gönderim {{ fmtDuration(s?.elapsed_seconds) }} sürdü</template>
          <template v-else>Ortalama {{ fmtRate(s?.speed) }} msg/sn</template>
        </p>

        <ul class="mt-5 space-y-4">
          <li v-for="c in channelRows" :key="c.key">
            <div class="flex items-baseline justify-between text-sm">
              <span class="flex items-center gap-2 font-medium text-ink"><span class="h-2 w-2 rounded-full" :class="c.dot" />{{ c.label }}</span>
              <span class="tnum text-ink-2"><span class="font-semibold text-ink">{{ fmtRate(c.rate) }}</span> / {{ RATE_LIMIT }}</span>
            </div>
            <div class="relative mt-1.5 h-2 rounded-full bg-paper" role="meter" :aria-label="`${c.label} hızı`" :aria-valuenow="Math.round(c.rate)" aria-valuemin="0" :aria-valuemax="RATE_LIMIT">
              <div class="h-full rounded-full transition-[width] duration-500" :class="c.bar" :style="{ width: c.meter * 100 + '%' }" />
            </div>
          </li>
        </ul>
      </div>

      <div class="panel min-w-0 p-5">
        <h2 class="font-semibold text-ink">Kanal başına hız</h2>
        <p class="mb-3 text-sm text-ink-3">Son 2 saniyedeki işlenen mesaj sayısından hesaplanır.</p>
        <ThroughputChart v-if="live.history.value.length > 1" :samples="live.history.value" :limit="RATE_LIMIT" />
        <p v-else class="grid h-[200px] place-items-center text-sm text-ink-3">Gönderim başlayınca grafik burada akar.</p>
      </div>
    </div>

    <!-- per channel -->
    <div class="panel mt-6 overflow-x-auto">
      <table class="w-full min-w-[40rem] whitespace-nowrap text-left text-sm">
        <caption class="sr-only">Kanal bazında gönderim</caption>
        <thead class="text-ink-3">
          <tr class="border-b border-line">
            <th scope="col" class="px-5 py-3 font-medium">Kanal</th>
            <th scope="col" class="px-3 py-3 text-right font-medium">Alıcı</th>
            <th scope="col" class="px-3 py-3 text-right font-medium">Gönderildi</th>
            <th scope="col" class="px-3 py-3 text-right font-medium">Hatalı</th>
            <th scope="col" class="px-3 py-3 text-right font-medium">Yeniden deneme</th>
            <th scope="col" class="px-3 py-3 text-right font-medium">İzin yok</th>
            <th scope="col" class="px-5 py-3 text-right font-medium">İlerleme</th>
          </tr>
        </thead>
        <tbody class="tnum text-ink">
          <tr v-for="c in channelRows" :key="c.key" class="border-b border-line last:border-0">
            <th scope="row" class="px-5 py-3 font-semibold"><span class="mr-2 inline-block h-2 w-2 rounded-full" :class="c.dot" />{{ c.label }}</th>
            <td class="px-3 py-3 text-right">{{ fmtInt(c.total) }}</td>
            <td class="px-3 py-3 text-right">{{ fmtInt(c.sent) }}</td>
            <td class="px-3 py-3 text-right" :class="c.failed ? 'text-bad' : ''">{{ fmtInt(c.failed) }}</td>
            <td class="px-3 py-3 text-right">{{ fmtInt(c.retried) }}</td>
            <td class="px-3 py-3 text-right text-ink-2">{{ fmtInt(c.opted_out) }}</td>
            <td class="px-5 py-3 text-right">%{{ fmtPercent(c.processed, c.total) }}</td>
          </tr>
        </tbody>
      </table>
    </div>

    <div class="mt-8 flex flex-wrap items-center gap-3">
      <button type="button" class="btn" :class="live.final.value ? 'btn-primary' : 'btn-quiet'" data-test="new-campaign" @click="f.newCampaign()">
        <Icon name="plus" /> Yeni kampanya
      </button>
      <p v-if="!live.final.value" class="text-sm text-ink-3">Gönderim arka planda sürer; sayfadan ayrılsanız da durmaz.</p>
    </div>
  </section>
</template>
