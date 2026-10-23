<script setup>
import TemplateText from './TemplateText.vue'
import Caret from './Caret.vue'
import Icon from './Icon.vue'

// A phone-sized SMS thread: the shape the recipient will actually see.
defineProps({
  text: { type: String, default: '' },
  typing: { type: Boolean, default: false },
  sender: { type: String, default: 'MAGAZA' },
  rendered: { type: Boolean, default: false }, // text is already personalised
})
</script>

<template>
  <figure class="mx-auto w-full max-w-[22rem]">
    <div class="rounded-[2rem] border border-line-strong bg-ink p-2.5 shadow-[0_18px_40px_-24px_rgba(23,32,54,0.55)]">
      <div class="flex min-h-[15rem] flex-col rounded-[1.6rem] bg-[#f6f8f7] px-4 pb-5 pt-3">
        <div class="mx-auto mb-4 h-1.5 w-16 rounded-full bg-line-strong" aria-hidden="true" />
        <div class="mb-4 flex items-center gap-2 border-b border-line pb-3">
          <span class="grid h-8 w-8 place-items-center rounded-full bg-sms text-white"><Icon name="sms" :size="16" /></span>
          <div class="leading-tight">
            <p class="text-sm font-semibold text-ink">{{ sender }}</p>
            <p class="text-xs text-ink-3">SMS</p>
          </div>
        </div>
        <div
          v-if="text || typing"
          class="max-w-[92%] self-start rounded-2xl rounded-bl-md bg-sms-soft px-3.5 py-2.5 text-[0.9375rem] leading-snug text-ink"
        >
          <span v-if="rendered" class="whitespace-pre-wrap break-words">{{ text }}</span>
          <TemplateText v-else :text="text" />
          <Caret v-if="typing" class="text-sms" />
        </div>
        <p v-else class="m-auto text-sm text-ink-3">SMS metni burada görünecek.</p>
      </div>
    </div>
    <figcaption class="sr-only">SMS önizlemesi</figcaption>
  </figure>
</template>
