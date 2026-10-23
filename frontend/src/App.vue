<script setup>
import { watch, nextTick, ref } from 'vue'
import HealthBadge from './components/ui/HealthBadge.vue'
import StepNav from './components/ui/StepNav.vue'
import BriefStep from './components/steps/BriefStep.vue'
import AudienceStep from './components/steps/AudienceStep.vue'
import ComposeStep from './components/steps/ComposeStep.vue'
import ReviewStep from './components/steps/ReviewStep.vue'
import DispatchStep from './components/steps/DispatchStep.vue'
import { useCampaignFlow, STEPS } from './composables/useCampaignFlow.js'

const flow = useCampaignFlow()
const main = ref(null)

// Move focus to the new step's heading so keyboard and screen-reader users
// land where the content changed.
watch(flow.step, async () => {
  await nextTick()
  window.scrollTo({ top: 0, behavior: 'smooth' })
  main.value?.querySelector('h1')?.focus({ preventScroll: true })
})
</script>

<template>
  <div class="min-h-dvh">
    <header class="border-b border-line bg-surface">
      <div class="mx-auto flex max-w-6xl items-center justify-between gap-4 px-4 py-3 sm:px-6">
        <div class="flex items-center gap-3">
          <img src="/favicon.svg" alt="" class="h-8 w-8" />
          <div class="leading-tight">
            <p class="text-[1.0625rem] font-bold tracking-[-0.01em] text-ink">Campaign Composer</p>
            <p class="hidden text-xs text-ink-3 sm:block">SMS ve e-posta kampanyaları</p>
          </div>
        </div>
        <HealthBadge />
      </div>
      <div class="mx-auto max-w-6xl px-4 pb-3 sm:px-6">
        <StepNav :steps="STEPS" :current="flow.step.value" :reached="flow.reached.value" :can-visit="flow.canVisit" @go="flow.goTo" />
      </div>
    </header>

    <main ref="main" class="mx-auto max-w-6xl px-4 pb-24 pt-10 sm:px-6 [&_h1]:outline-none" tabindex="-1">
      <BriefStep v-if="flow.step.value === 1" :flow="flow" />
      <AudienceStep v-else-if="flow.step.value === 2" :flow="flow" />
      <ComposeStep v-else-if="flow.step.value === 3" :flow="flow" />
      <ReviewStep v-else-if="flow.step.value === 4" :flow="flow" />
      <DispatchStep v-else :flow="flow" />
    </main>
  </div>
</template>
