<script setup lang="ts">
// The reliability timeline motif, per the approved mock (spec §12.2): the product's
// uptime-signal grid of discrete ticks, where UNKNOWN is a SHORT tick and PROVISIONAL is
// REDUCED OPACITY on the same grid — two encodings chosen so neither can be read as a
// status colour. A tick under an active REPAIR range is masked with the work encoding
// (§12.1: a repairing interval is rendered as such, never as data). Revision boundaries
// render as accent marks, epoch markers as thin lines.
//
// Geometry is TIME-WEIGHTED ([218] P1-3): the API deliberately emits several points inside
// one rollup step when an epoch, revision or sealed/provisional boundary splits it, so each
// tick's width is its bucket count — a 6-bucket boundary fragment must not look as long as
// a 24-bucket day.
import { computed } from "vue";

export type ReliabilityTick = {
  state: "good" | "bad" | "unknown" | "excluded" | "repairing" | "none";
  // weight is the tick's bucket count; widths are proportional to it.
  weight: number;
  provisional?: boolean;
  // boundary markers drawn BEFORE this tick
  revisionBoundary?: boolean;
  epochBoundary?: boolean;
};

const props = withDefaults(defineProps<{ ticks: ReliabilityTick[]; height?: number }>(), { height: 26 });

const fill: Record<ReliabilityTick["state"], string> = {
  good: "var(--up)",
  bad: "var(--down)",
  unknown: "var(--ink-3)",
  excluded: "var(--maint)",
  repairing: "var(--accent)",
  none: "var(--inset)",
};

const laid = computed(() => {
  let x = 0;
  return props.ticks.map((t) => {
    const w = Math.max(t.weight, 0.0001);
    const cell = { t, x, w };
    x += w;
    return cell;
  });
});
const total = computed(() => laid.value.reduce((sum, c) => sum + c.w, 0) || 1);
</script>

<template>
  <svg
    :viewBox="`0 0 ${total} 10`"
    preserveAspectRatio="none"
    :style="{ height: height + 'px', width: '100%', display: 'block' }"
    role="img"
    aria-label="reliability timeline"
  >
    <template v-for="(c, i) in laid" :key="i">
      <rect
        :x="c.x + c.w * 0.08"
        :y="c.t.state === 'unknown' ? 5 : 0"
        :width="c.w * 0.84"
        :height="c.t.state === 'unknown' ? 5 : 10"
        rx="0.2"
        ry="0.7"
        :style="{ fill: fill[c.t.state], opacity: c.t.provisional ? 0.38 : c.t.state === 'repairing' ? 0.55 : 1 }"
        :data-provisional="c.t.provisional ? 'true' : undefined"
        :data-state="c.t.state"
        :data-weight="c.t.weight"
      />
      <rect v-if="c.t.revisionBoundary" :x="c.x" y="-1" :width="total * 0.002 + 0.05" height="12" style="fill: var(--accent)" data-marker="revision" />
      <rect v-else-if="c.t.epochBoundary" :x="c.x" y="0" :width="total * 0.001 + 0.03" height="10" style="fill: var(--ink-3); opacity: 0.7" data-marker="epoch" />
    </template>
  </svg>
</template>
