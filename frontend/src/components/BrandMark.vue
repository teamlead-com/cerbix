<script setup lang="ts">
// Single source of the cerbix brand mark: a custom `branding.logoUrl` when the
// instance sets one, otherwise the shared Sealed C glyph on an accent tile.
import BrandGlyph from "@/components/BrandGlyph.vue";
import { useBranding } from "@/stores/branding";

const props = withDefaults(defineProps<{ tile?: number; glyph?: number }>(), {
  tile: 26,
  glyph: 15,
});
const branding = useBranding();
</script>

<template>
  <span v-if="branding.logoUrl" data-testid="brand-mark">
    <img
      :src="branding.logoUrl"
      alt=""
      class="rounded-sm object-contain"
      :style="{ height: `${props.tile}px`, width: `${props.tile}px` }"
    />
  </span>
  <span
    v-else
    data-testid="brand-mark"
    class="grid place-items-center rounded-sm bg-accent text-accent-ink"
    :style="{ height: `${props.tile}px`, width: `${props.tile}px` }"
  >
    <BrandGlyph :size="props.glyph" />
  </span>
</template>
