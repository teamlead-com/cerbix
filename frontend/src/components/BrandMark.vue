<script setup lang="ts">
// Single source of the cerbix brand mark: a custom `branding.logoUrl` when the
// instance sets one, otherwise the default shield-with-check glyph on an accent
// tile. Kept inline (stroke=currentColor on a `bg-accent` tile) so it follows the
// live theme + configurable accent — a static logo.svg could not. The static
// `public/favicon.svg` carries the same mark for the browser tab.
import { useBranding } from "@/stores/branding";

const props = withDefaults(defineProps<{ tile?: number; glyph?: number }>(), {
  tile: 26,
  glyph: 15,
});
const branding = useBranding();
</script>

<template>
  <img
    v-if="branding.logoUrl"
    :src="branding.logoUrl"
    alt=""
    class="rounded-sm object-contain"
    :style="{ height: `${props.tile}px`, width: `${props.tile}px` }"
  />
  <span
    v-else
    class="grid place-items-center rounded-sm bg-accent text-accent-ink"
    :style="{ height: `${props.tile}px`, width: `${props.tile}px` }"
  >
    <svg
      viewBox="0 0 24 24"
      :style="{ height: `${props.glyph}px`, width: `${props.glyph}px` }"
      fill="none"
      stroke="currentColor"
      stroke-width="2.2"
      stroke-linecap="round"
      stroke-linejoin="round"
    >
      <path d="M12 3l7 3v5c0 4.5-3 7.5-7 9-4-1.5-7-4.5-7-9V6z" />
      <path d="M8.5 12l2 2 4.5-4.5" />
    </svg>
  </span>
</template>
