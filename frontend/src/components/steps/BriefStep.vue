<script setup>
import { computed } from 'vue'
import Icon from '../ui/Icon.vue'
import Notice from '../ui/Notice.vue'
import { SAMPLE_BRIEF, MAX_BRIEF } from '../../composables/useCampaignFlow.js'
import { runeLength } from '../../lib/text.js'

const props = defineProps({ flow: { type: Object, required: true } })
const f = props.flow

const length = computed(() => runeLength(f.brief.value))
const tooLong = computed(() => length.value > MAX_BRIEF)
const canRun = computed(() => f.trimmedBrief.value.length > 0 && !tooLong.value && !f.previewing.value && !f.locked.value)

function useSample() {
  f.brief.value = SAMPLE_BRIEF
}
function onKey(e) {
  if ((e.metaKey || e.ctrlKey) && e.key === 'Enter' && canRun.value) f.runPreview()
}
</script>

<template>
  <section aria-labelledby="brief-title" class="max-w-3xl">
    <h1 tabindex="-1" id="brief-title" class="text-[2rem] font-bold leading-tight tracking-[-0.015em] text-ink">
      Kampanyayı kendi cümlelerinizle anlatın
    </h1>
    <p class="mt-3 max-w-[62ch] text-[1.0625rem] leading-relaxed text-ink-2">
      Kime ulaşmak istediğinizi, ne teklif ettiğinizi ve nasıl bir ton istediğinizi yazın. Claude bundan bir kitle
      kuralı ve SMS ile e-posta metinleri önerir; hiçbir şey siz onaylamadan gönderilmez.
    </p>

    <div class="mt-8">
      <div class="mb-2 flex items-end justify-between gap-4">
        <label for="brief" class="font-semibold text-ink">Kampanya isteği</label>
        <button type="button" class="text-sm font-semibold text-accent hover:underline disabled:text-ink-3 disabled:no-underline" :disabled="f.locked.value" @click="useSample">
          Örnek brief'i kullan
        </button>
      </div>
      <textarea
        id="brief"
        v-model="f.brief.value"
        rows="6"
        class="field resize-y text-[1.0625rem]"
        :readonly="f.locked.value"
        :aria-invalid="tooLong"
        aria-describedby="brief-help"
        placeholder="Örn. Son 30 günde sepetine ürün ekleyip alışveriş yapmayan İstanbul'daki kullanıcılara %30 indirim hatırlatması gönder. Samimi ama kısa olsun."
        data-test="brief-input"
        @keydown="onKey"
      />
      <p id="brief-help" class="mt-2 flex justify-between gap-4 text-sm text-ink-3">
        <span>Şehir, sepet aktivitesi, alışveriş, yaş ve toplam harcamaya göre hedefleme yapılabilir.</span>
        <span class="tnum shrink-0" :class="tooLong ? 'font-semibold text-bad' : ''">{{ length }} / {{ MAX_BRIEF }}</span>
      </p>
    </div>

    <Notice v-if="f.previewError.value" tone="bad" title="Kitle çıkarılamadı" class="mt-6">
      {{ f.previewError.value.userMessage }}
    </Notice>

    <div class="mt-8 flex flex-wrap items-center gap-3">
      <button type="button" class="btn btn-primary" :disabled="!canRun" data-test="run-preview" @click="f.runPreview()">
        <Icon :name="f.previewing.value ? 'refresh' : 'users'" :class="{ 'animate-spin': f.previewing.value }" />
        {{ f.previewing.value ? 'Kitle çıkarılıyor…' : 'Kitleyi çıkar' }}
      </button>
      <button
        v-if="f.preview.value && !f.briefDirty.value"
        type="button"
        class="btn btn-quiet"
        @click="f.goTo(2)"
      >
        Mevcut kitleye dön
      </button>
      <span class="hidden text-sm text-ink-3 sm:inline">Ctrl + Enter</span>
    </div>
  </section>
</template>
