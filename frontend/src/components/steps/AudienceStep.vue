<script setup>
import { ref, computed } from 'vue'
import Icon from '../ui/Icon.vue'
import Notice from '../ui/Notice.vue'
import RuleNode from '../ui/RuleNode.vue'
import { prettyRule, parseRuleInput, countConditions } from '../../lib/rule.js'
import { fmtInt, fmtPercent } from '../../lib/format.js'

const props = defineProps({ flow: { type: Object, required: true } })
const f = props.flow

const p = computed(() => f.preview.value)
const a = computed(() => p.value?.audience || { total: 0, sms_opt_in: 0, email_opt_in: 0, reachable: 0 })
const empty = computed(() => a.value.reachable === 0)

const editing = ref(false)
const json = ref('')
const parseError = ref('')

function openEditor() {
  json.value = prettyRule(p.value.rule)
  parseError.value = ''
  editing.value = true
}
async function applyRule() {
  const r = parseRuleInput(json.value)
  if (!r.ok) {
    parseError.value = r.error
    return
  }
  parseError.value = ''
  if (await f.revalidateRule(r.rule)) editing.value = false
}

const tiles = computed(() => [
  { key: 'total', label: 'Toplam kitle', value: a.value.total, note: 'Kurala uyan kullanıcı', icon: 'users', tone: 'ink' },
  { key: 'sms', label: 'SMS alabilir', value: a.value.sms_opt_in, note: `Kitlenin %${fmtPercent(a.value.sms_opt_in, a.value.total)} kadarı izinli`, icon: 'sms', tone: 'sms' },
  { key: 'email', label: 'E-posta alabilir', value: a.value.email_opt_in, note: `Kitlenin %${fmtPercent(a.value.email_opt_in, a.value.total)} kadarı izinli`, icon: 'mail', tone: 'email' },
  { key: 'reachable', label: 'Ulaşılabilir', value: a.value.reachable, note: 'En az bir kanaldan izinli', icon: 'check', tone: 'good' },
])
const toneClass = {
  ink: 'bg-paper text-ink-2',
  sms: 'bg-sms-soft text-sms',
  email: 'bg-email-soft text-email',
  good: 'bg-good-soft text-good',
}
</script>

<template>
  <section v-if="p" aria-labelledby="aud-title">
    <h1 tabindex="-1" id="aud-title" class="text-[2rem] font-bold leading-tight tracking-[-0.015em] text-ink">Bu kampanya kime gidecek?</h1>
    <p class="mt-3 max-w-[62ch] text-[1.0625rem] leading-relaxed text-ink-2">
      Claude brief'inizi aşağıdaki kurala çevirdi. Kural sunucuda izinli alanlar listesiyle doğrulandı ve sayımlar
      gerçek kullanıcı verisinden geliyor.
    </p>

    <dl class="mt-8 grid grid-cols-2 gap-3 lg:grid-cols-4" data-test="audience-counts">
      <div v-for="t in tiles" :key="t.key" class="panel p-4" :data-key="t.key">
        <dt class="flex items-center gap-2 text-sm font-medium text-ink-2">
          <span class="grid h-7 w-7 place-items-center rounded-full" :class="toneClass[t.tone]"><Icon :name="t.icon" :size="15" /></span>
          {{ t.label }}
        </dt>
        <dd class="tnum mt-3 text-[2rem] font-bold leading-none tracking-tight text-ink">{{ fmtInt(t.value) }}</dd>
        <dd class="mt-1.5 text-sm text-ink-3">{{ t.note }}</dd>
      </div>
    </dl>

    <Notice v-if="empty" tone="warn" title="Ulaşılabilir kimse yok" class="mt-4">
      Bu kurala uyan kullanıcıların hiçbiri SMS ya da e-posta izni vermemiş. Brief'i genişletin ya da kuralı düzenleyin.
    </Notice>

    <div class="panel mt-6 p-5">
      <div class="flex flex-wrap items-center justify-between gap-3">
        <h2 class="text-lg font-semibold text-ink">
          Segment kuralı
          <span class="ml-1 text-sm font-normal text-ink-3">{{ countConditions(p.rule) }} koşul</span>
        </h2>
        <button v-if="!editing && !f.locked.value" type="button" class="btn btn-quiet py-1.5 text-sm" @click="openEditor">
          <Icon name="code" :size="16" /> JSON olarak düzenle
        </button>
      </div>

      <div v-if="!editing" class="mt-4" data-test="rule-card">
        <RuleNode :node="p.rule" />
      </div>

      <div v-else class="mt-4">
        <label for="rule-json" class="sr-only">Segment kuralı (JSON)</label>
        <textarea
          id="rule-json"
          v-model="json"
          rows="12"
          spellcheck="false"
          class="field font-mono text-sm"
          :aria-invalid="!!parseError"
        />
        <p class="mt-2 text-sm text-ink-3">
          Kural SQL olarak çalıştırılmaz: sunucu yalnızca izinli alanları ve operatörleri kabul eder, sonra sayımı yeniler.
        </p>
        <Notice v-if="parseError" tone="bad" class="mt-3">{{ parseError }}</Notice>
        <div class="mt-4 flex gap-3">
          <button type="button" class="btn btn-primary" :disabled="f.previewing.value" @click="applyRule">
            <Icon name="refresh" :class="{ 'animate-spin': f.previewing.value }" /> Doğrula ve yeniden say
          </button>
          <button type="button" class="btn btn-quiet" @click="editing = false">Vazgeç</button>
        </div>
      </div>
    </div>

    <Notice v-if="f.previewError.value" tone="bad" title="Kural kabul edilmedi" class="mt-4">
      {{ f.previewError.value.userMessage }}
    </Notice>

    <div class="mt-8 flex flex-wrap gap-3">
      <button type="button" class="btn btn-quiet" @click="f.goTo(1)"><Icon name="back" /> Brief'i düzenle</button>
      <button
        type="button"
        class="btn btn-primary"
        :disabled="empty || editing || f.locked.value"
        data-test="start-compose"
        @click="f.startCompose()"
      >
        <Icon name="sparkle" /> {{ f.compose.status.value === 'idle' ? 'Mesajları üret' : 'Mesajları yeniden üret' }}
      </button>
      <button
        v-if="f.reached.value >= 4"
        type="button"
        class="btn btn-quiet"
        @click="f.goTo(4)"
      >
        Taslağa dön
      </button>
    </div>
  </section>
</template>
