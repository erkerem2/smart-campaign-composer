<script setup>
import { ref, computed } from 'vue'
import Icon from '../ui/Icon.vue'
import Notice from '../ui/Notice.vue'
import FieldEditor from '../ui/FieldEditor.vue'
import SmsPreview from '../ui/SmsPreview.vue'
import EmailPreview from '../ui/EmailPreview.vue'
import { renderPreview } from '../../lib/text.js'
import { fmtInt } from '../../lib/format.js'

const props = defineProps({ flow: { type: Object, required: true } })
const f = props.flow

const sampleName = ref('Ayşe')
const sample = computed(() => ({ first_name: sampleName.value.trim(), last_name: 'Yılmaz' }))
const preview = computed(() => ({
  sms: renderPreview(f.edits.sms_content, sample.value),
  subject: renderPreview(f.edits.email_subject, sample.value),
  body: renderPreview(f.edits.email_body, sample.value),
}))
const audience = computed(() => f.preview.value?.audience || {})

const KNOWN = ['sms_content', 'email_subject', 'email_body']
const otherServerIssues = computed(() => f.serverIssues.value.filter((i) => !KNOWN.includes(i.field)))
</script>

<template>
  <section aria-labelledby="review-title">
    <h1 tabindex="-1" id="review-title" class="text-[2rem] font-bold leading-tight tracking-[-0.015em] text-ink">Göndermeden önce gözden geçirin</h1>
    <p class="mt-3 max-w-[62ch] text-[1.0625rem] leading-relaxed text-ink-2">
      Metinleri olduğu gibi onaylayabilir ya da düzenleyebilirsiniz. Onayladığınızda sunucu her şeyi yeniden doğrular;
      kurala uymayan bir içerik kaydedilmez.
    </p>

    <div class="mt-8 grid items-start gap-8 lg:grid-cols-[minmax(0,1fr)_22rem]">
      <!-- editors -->
      <div class="space-y-7">
        <FieldEditor :flow="f" field="sms_content" label="SMS metni" channel="sms" :rows="4" />
        <FieldEditor :flow="f" field="email_subject" label="E-posta konusu" channel="email" :multiline="false" />
        <FieldEditor
          :flow="f"
          field="email_body"
          label="E-posta gövdesi"
          channel="email"
          :rows="10"
          :counter="false"
          hint="Markdown ya da basit HTML (p, strong, em, ul, li, a). Script, stil ve görsel kabul edilmez."
        />

        <div class="rounded-[var(--radius-control)] border border-line bg-surface p-4 text-sm leading-relaxed text-ink-2">
          <p class="font-semibold text-ink">Kişiselleştirme değişkenleri</p>
          <ul class="mt-1.5 space-y-1">
            <li><code class="font-mono text-accent-strong" v-text="'{{first_name}}'" /> alıcının adı. Ad boşsa "Değerli müşterimiz" yazılır. Sayaç bu değişkeni ortalama 10 karakter sayar.</li>
            <li><code class="font-mono text-accent-strong" v-text="'{{last_name}}'" /> alıcının soyadı.</li>
            <li>Başka bir değişken (ör. <code class="font-mono" v-text="'{{email}}'" />) kabul edilmez ve kırmızı işaretlenir.</li>
          </ul>
        </div>
      </div>

      <!-- live preview -->
      <aside class="space-y-4 lg:sticky lg:top-6" aria-label="Alıcının göreceği hali">
        <div class="flex items-center gap-2">
          <label for="sample-name" class="text-sm font-medium text-ink-2">Önizleme adı</label>
          <input id="sample-name" v-model="sampleName" type="text" class="field w-36 py-1.5 text-sm" placeholder="boş: yedek metin" />
        </div>
        <SmsPreview :text="preview.sms" rendered />
        <EmailPreview :subject="preview.subject" :body="preview.body" rendered />
      </aside>
    </div>

    <!-- approval -->
    <div class="panel mt-10 flex flex-col gap-4 p-5 sm:flex-row sm:items-center sm:justify-between">
      <p class="max-w-[60ch] text-[0.9375rem] leading-relaxed text-ink-2">
        Onaylarsanız <strong class="tnum text-ink">{{ fmtInt(audience.sms_opt_in) }}</strong> kişiye SMS,
        <strong class="tnum text-ink">{{ fmtInt(audience.email_opt_in) }}</strong> kişiye e-posta gönderilir. İzni olmayan kişiye
        o kanaldan mesaj gitmez; aynı kişiye aynı mesaj iki kez gönderilmez.
      </p>
      <div class="flex shrink-0 gap-3">
        <button type="button" class="btn btn-quiet" @click="f.goTo(3)"><Icon name="back" /> Geri</button>
        <button type="button" class="btn btn-primary" :disabled="!f.canApprove.value" data-test="approve" @click="f.approveAndSend()">
          <Icon :name="f.approving.value ? 'refresh' : 'send'" :class="{ 'animate-spin': f.approving.value }" />
          {{ f.approving.value ? 'Onaylanıyor…' : 'Onayla ve gönder' }}
        </button>
      </div>
    </div>

    <Notice v-if="f.approveError.value" tone="bad" title="Kampanya onaylanmadı" class="mt-4" data-test="approve-error">
      {{ f.approveError.value.userMessage }}
      <ul v-if="otherServerIssues.length" class="mt-1 list-disc pl-5">
        <li v-for="(i, k) in otherServerIssues" :key="k">{{ i.message }}</li>
      </ul>
    </Notice>
    <p v-else-if="!f.canApprove.value && f.clientIssues.value.length" class="mt-3 text-sm text-bad">
      Onaylamak için işaretli alanları düzeltin.
    </p>
  </section>
</template>
