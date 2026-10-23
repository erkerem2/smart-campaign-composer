<script setup>
import { computed } from 'vue'
import Icon from '../ui/Icon.vue'
import Notice from '../ui/Notice.vue'
import SmsPreview from '../ui/SmsPreview.vue'
import EmailPreview from '../ui/EmailPreview.vue'
import CharCounter from '../ui/CharCounter.vue'
import { measure, LIMITS } from '../../lib/text.js'

const props = defineProps({ flow: { type: Object, required: true } })
const f = props.flow
const c = f.compose

const FIELD_NAMES = { sms_content: 'SMS', email_subject: 'E-posta konusu', email_body: 'E-posta gövdesi' }
const streaming = computed(() => c.status.value === 'streaming')
const smsCheck = computed(() => measure(c.fields.sms_content, LIMITS.sms_content))
const subjectCheck = computed(() => measure(c.fields.email_subject, LIMITS.email_subject))

const heading = computed(() => {
  if (streaming.value) return 'Claude mesajları yazıyor'
  if (c.status.value === 'done') return 'Taslak hazır ve doğrulandı'
  if (c.status.value === 'error' && c.rejected.value) return 'Taslak kurallara uymadı'
  return 'Mesajlar üretilemedi'
})
</script>

<template>
  <section aria-labelledby="compose-title">
    <h1 tabindex="-1" id="compose-title" class="text-[2rem] font-bold leading-tight tracking-[-0.015em] text-ink">{{ heading }}</h1>
    <p class="mt-3 max-w-[62ch] text-[1.0625rem] leading-relaxed text-ink-2" aria-live="polite">
      <template v-if="streaming">
        Metin model yazdıkça akıyor. Bitince sunucu karakter limitlerini, değişkenleri ve e-posta biçimini kontrol eder;
        incelemeye yalnızca doğrulanmış taslak gelir.
      </template>
      <template v-else-if="c.status.value === 'done'">
        <template v-if="c.draft.value?.attempts > 1">İlk taslak kurallara uymadığı için model hataları görerek bir kez daha yazdı. </template>
        SMS {{ smsCheck.runes }}/{{ LIMITS.sms_content }}, konu {{ subjectCheck.runes }}/{{ LIMITS.email_subject }} karakter.
        Göndermeden önce inceleyip düzenleyebilirsiniz.
      </template>
    </p>

    <Notice v-if="streaming && c.attempt.value > 1" tone="warn" title="İkinci deneme" class="mt-6">
      İlk taslak doğrulamadan geçemedi; model şu sorunları düzeltiyor:
      <ul class="mt-1 list-disc pl-5">
        <li v-for="(i, k) in c.retryIssues.value" :key="k">{{ FIELD_NAMES[i.field] || i.field }}: {{ i.message }}</li>
      </ul>
    </Notice>

    <Notice v-if="c.status.value === 'error' && c.rejected.value" tone="warn" title="Son denemeyi elle düzeltebilirsiniz" class="mt-6">
      Model iki denemede de kurallara uyan bir metin üretemedi:
      <ul class="mt-1 list-disc pl-5">
        <li v-for="(i, k) in c.issues.value" :key="k">{{ FIELD_NAMES[i.field] || i.field }}: {{ i.message }}</li>
      </ul>
    </Notice>
    <Notice v-else-if="c.status.value === 'error'" tone="bad" title="Akış tamamlanmadı" class="mt-6">
      {{ c.error.value }}
    </Notice>

    <div class="mt-8 grid items-start gap-6 lg:grid-cols-[22rem_1fr]">
      <div>
        <SmsPreview :text="c.fields.sms_content" :typing="c.activeField.value === 'sms_content'" data-test="stream-sms" />
        <CharCounter :check="smsCheck" class="mx-auto mt-3 max-w-[22rem]" />
      </div>
      <div>
        <EmailPreview
          :subject="c.fields.email_subject"
          :body="c.fields.email_body"
          :typing="['email_subject', 'email_body'].includes(c.activeField.value) ? c.activeField.value : ''"
          data-test="stream-email"
        />
        <CharCounter :check="subjectCheck" class="mt-3" />
      </div>
    </div>

    <div class="mt-8 flex flex-wrap gap-3">
      <button type="button" class="btn btn-quiet" :disabled="streaming" @click="f.goTo(2)"><Icon name="back" /> Kitleye dön</button>
      <button v-if="streaming" type="button" class="btn btn-quiet" @click="c.reset(); f.goTo(2)">Durdur</button>
      <button v-else type="button" class="btn btn-quiet" @click="f.startCompose()"><Icon name="refresh" /> Yeniden üret</button>
      <button
        v-if="f.reached.value >= 4"
        type="button"
        class="btn btn-primary"
        data-test="go-review"
        @click="f.goTo(4)"
      >
        <Icon name="edit" /> {{ c.status.value === 'done' ? 'İncele ve onayla' : 'Elle düzelt' }}
      </button>
    </div>
  </section>
</template>
