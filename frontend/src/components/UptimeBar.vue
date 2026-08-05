<script setup lang="ts">
type Seg = "up" | "degraded" | "down" | "maint" | "none";
withDefaults(defineProps<{ segments: Seg[]; height?: number }>(), { height: 26 });

// SVG (not flex) so every tick scales by the same factor under
// preserveAspectRatio="none" — this avoids the flexbox sub-pixel rounding that
// made the gaps look uneven (~every 4th tick drifting a pixel). Colors are theme
// CSS vars, so they follow light/dark automatically.
const fill: Record<Seg, string> = {
  up: "var(--up)",
  degraded: "var(--degraded)",
  down: "var(--down)",
  maint: "var(--maint)",
  none: "var(--inset)",
};
</script>

<template>
  <svg
    :viewBox="`0 0 ${segments.length} 10`"
    preserveAspectRatio="none"
    :style="{ height: height + 'px', width: '100%', display: 'block' }"
    role="img"
    aria-label="availability"
  >
    <rect
      v-for="(s, i) in segments"
      :key="i"
      :x="i + 0.13"
      y="0"
      :width="0.74"
      height="10"
      rx="0.2"
      ry="0.7"
      :style="{ fill: fill[s] }"
    />
  </svg>
</template>
