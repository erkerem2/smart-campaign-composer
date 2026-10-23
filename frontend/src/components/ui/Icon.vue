<script setup>
// Small inline icon set (24×24, stroke-based) so the app needs no icon font.
defineProps({
  name: { type: String, required: true },
  size: { type: [Number, String], default: 18 },
})

const paths = {
  check: 'M5 12.5l4.5 4.5L19 7.5',
  alert: 'M12 8v5M12 16.5v.01M10.3 3.9L2.6 17.5A2 2 0 004.3 20.5h15.4a2 2 0 001.7-3L13.7 3.9a2 2 0 00-3.4 0z',
  x: 'M6 6l12 12M18 6L6 18',
  info: 'M12 11v6M12 7.5v.01M12 21a9 9 0 100-18 9 9 0 000 18z',
  sms: 'M4 6.5A2.5 2.5 0 016.5 4h11A2.5 2.5 0 0120 6.5v8a2.5 2.5 0 01-2.5 2.5H10l-4.5 3.5V17A2.5 2.5 0 014 14.5v-8z',
  mail: 'M3.5 6.5A1.5 1.5 0 015 5h14a1.5 1.5 0 011.5 1.5v11A1.5 1.5 0 0119 19H5a1.5 1.5 0 01-1.5-1.5v-11zM4 6.5l8 6 8-6',
  users: 'M9 11a3.5 3.5 0 100-7 3.5 3.5 0 000 7zM2.5 20a6.5 6.5 0 0113 0M16 4.3a3.5 3.5 0 010 6.4M18.5 20a6.5 6.5 0 00-3-5.5',
  sparkle: 'M12 3l1.8 5.2L19 10l-5.2 1.8L12 17l-1.8-5.2L5 10l5.2-1.8L12 3zM19 16l.8 2.2L22 19l-2.2.8L19 22l-.8-2.2L16 19l2.2-.8L19 16z',
  refresh: 'M20 11a8 8 0 10-2.3 5.7M20 4v7h-7',
  back: 'M15 6l-6 6 6 6',
  send: 'M4 12l16-8-6 16-2.5-6.5L4 12z',
  play: 'M7 5l12 7-12 7V5z',
  edit: 'M4 20h4l10.5-10.5a2.1 2.1 0 00-3-3L5 17v3zM13.5 7.5l3 3',
  code: 'M9 8l-4 4 4 4M15 8l4 4-4 4',
  undo: 'M9 14L4 9l5-5M4 9h10.5a5.5 5.5 0 010 11H11',
  pulse: 'M3 12h4l3-7 4 14 3-7h4',
  plus: 'M12 5v14M5 12h14',
}
</script>

<template>
  <svg
    :width="size"
    :height="size"
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    stroke-width="1.9"
    stroke-linecap="round"
    stroke-linejoin="round"
    aria-hidden="true"
    class="shrink-0"
  >
    <path :d="paths[name]" />
  </svg>
</template>
