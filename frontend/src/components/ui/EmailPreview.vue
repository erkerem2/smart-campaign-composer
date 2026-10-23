<script setup>
import TemplateText from './TemplateText.vue'
import Caret from './Caret.vue'
import Icon from './Icon.vue'

// An inbox-style e-mail card. The body is shown as plain text (Markdown /
// whitelisted HTML is validated server-side); nothing is rendered as HTML.
defineProps({
  subject: { type: String, default: '' },
  body: { type: String, default: '' },
  typing: { type: String, default: '' }, // '' | 'email_subject' | 'email_body'
  rendered: { type: Boolean, default: false },
  from: { type: String, default: 'Mağaza <kampanya@ornek-magaza.com>' },
})
</script>

<template>
  <figure class="panel overflow-hidden">
    <div class="flex items-center gap-3 border-b border-line bg-email-soft/60 px-5 py-3">
      <span class="grid h-8 w-8 place-items-center rounded-full bg-email text-white"><Icon name="mail" :size="16" /></span>
      <div class="min-w-0 text-sm leading-tight">
        <p class="truncate font-semibold text-ink">{{ from }}</p>
        <p class="text-ink-3">E-posta</p>
      </div>
    </div>
    <div class="px-5 py-4">
      <p class="text-lg font-semibold leading-snug text-ink" :class="{ 'text-ink-3 font-normal': !subject && !typing }">
        <template v-if="subject || typing">
          <span v-if="rendered" class="break-words">{{ subject }}</span>
          <TemplateText v-else :text="subject" />
          <Caret v-if="typing === 'email_subject'" class="text-email" />
        </template>
        <template v-else>Konu satırı</template>
      </p>
      <div class="mt-3 max-h-[22rem] overflow-y-auto text-[0.9375rem] leading-relaxed text-ink-2">
        <template v-if="body || typing === 'email_body'">
          <span v-if="rendered" class="whitespace-pre-wrap break-words">{{ body }}</span>
          <TemplateText v-else :text="body" />
          <Caret v-if="typing === 'email_body'" class="text-email" />
        </template>
        <p v-else class="text-ink-3">E-posta gövdesi burada görünecek.</p>
      </div>
    </div>
    <figcaption class="sr-only">E-posta önizlemesi</figcaption>
  </figure>
</template>
