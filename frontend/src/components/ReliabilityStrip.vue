<script setup lang="ts">
// The reliability timeline (func-truthful-rendering §5, FR-031, D-0235). This component DRAWS;
// every rule it draws by lives in `@/lib/reliabilitygeometry`, tested without a DOM, because the
// geometry is the part that was wrong before.
//
// THE AXIS IS CLOCK TIME. `axisFromMs`/`axisToMs` are the shared axis, and a cell's width is its
// real extent on it. Unstored time occupies width in the `not-stored` encoding — a hatch with a
// cell outline, because absence is not a verdict and may borrow neither a status hue nor opacity.
// Inter-tick padding is a FIXED width, never a fraction of the tick: the old 8%-of-tick padding
// was a hairline across ninety ticks and a canyon across three, reading as absence while meaning
// nothing.
//
// FIVE ENCODINGS, none confusable with another (ruled at party [143]): `good`/`down`/`excluded`
// keep their hues; `unknown` is a solid `--ink-3` slice, because it is a DECIDED verdict and must
// not read as a status colour; `not-stored` is the hatch; `provisional` is opacity and is its only
// user. The short-tick encoding for `unknown` is RETIRED — inside a stack height carries the
// slice's quantity, so it cannot also carry its identity (party [138]).
//
// WIDTH IS NEVER FLOORED. The same component draws a segment LANE on the shared axis, and a lane
// whose projected width falls below one device pixel gets a fixed-width anchored MARKER at its
// start rather than a widened lane: a lane's extent claims duration, so it keeps it. Colliding
// markers stack vertically. The slice floor and this rule are two mechanisms for two axes —
// height tolerates a floor, width does not (party [140]).
//
// FR-025 change marks ride on top exactly as before, kind-shaped and in the accent: a deploy is
// not good or bad, what followed it is, and the cells beneath say that in the hues the facts own.
import { computed, ref } from "vue";

import { kindClip, type StripMark } from "@/lib/changes";
import { stackSlices, type Cell, type MarkCluster } from "@/lib/reliabilitygeometry";

export type { StripMark };

const props = withDefaults(
  defineProps<{
    /** the shared axis; a lane's cells may cover only part of it */
    axisFromMs: number;
    axisToMs: number;
    cells: Cell[];
    height?: number;
    /** 'lane' is a segment's row on the same axis: shorter, and sub-pixel becomes a marker */
    variant?: "overview" | "lane";
    /** clustered definition-revision / epoch boundaries, anchored at their earliest member */
    clusters?: MarkCluster[];
    /** the window's right edge; everything past it is in no number (§11.3) */
    sealedThroughMs?: number;
    /** FR-025 change marks */
    marks?: StripMark[];
  }>(),
  { height: 30, variant: "overview", clusters: () => [], sealedThroughMs: undefined, marks: () => [] },
);

/** Fixed, in the strip's own user units — never a share of a cell (F2). */
const PAD = 1.2;
const MARK_W = 2.2;
/**
 * A cell's HIT TARGET may be widened to stay reachable; a cell's DRAWN WIDTH may not.
 *
 * Width carries duration, so a factual rect keeps its exact projected width even when that is
 * sub-pixel — a cell too short to see at this zoom is a cell whose duration is too short to see,
 * and widening it would overstate the time it covers. The earlier `Math.max(0.25, …)` floored
 * every cell's drawn width, which at a 30-day axis drew anything under ~10.8 minutes as ~10.8
 * minutes (reviewer P1). The interaction affordance is separate and explicitly non-geometric.
 */
const HIT_W = 1.5;

const span = computed(() => Math.max(1, props.axisToMs - props.axisFromMs));
/** The strip is drawn in a 1000-unit viewBox, so 1 unit is 0.1% of the axis. */
const W = 1000;
const x = (ms: number) => ((ms - props.axisFromMs) / span.value) * W;

const h = computed(() => props.height);
const laid = computed(() =>
  props.cells.map((c) => {
    const x0 = x(c.startMs);
    const w = x(c.endMs) - x0;
    const pad = w > PAD * 3 ? PAD : 0; // a fragment narrower than its own padding keeps its width
    // `w` is the FACTUAL width: exact, never floored, and drawn only when it is positive.
    // `hitX`/`hitW` are the interaction affordance, widened and centred so a sub-pixel cell stays
    // reachable by pointer and keyboard without its drawing claiming the extra time.
    const factual = Math.max(0, w - pad * 2);
    const hitW = Math.max(w, HIT_W);
    return {
      c,
      x: x0 + pad,
      w: factual,
      hitX: x0 + (w - hitW) / 2,
      hitW,
      slices: stackSlices(c, h.value),
    };
  }),
);

/** A lane whose whole extent is under a device pixel is MARKED, never widened. */
const laneExtent = computed(() => {
  if (!props.cells.length) return null;
  const from = props.cells[0].startMs;
  const to = props.cells[props.cells.length - 1].endMs;
  return { from, to, units: x(to) - x(from) };
});
// 1000 viewBox units render across the container; a lane under ~0.1 units is under a pixel on any
// realistic width, and that is the threshold for "the axis has no room for this".
const subPixel = computed(
  () => props.variant === "lane" && laneExtent.value !== null && laneExtent.value.units < 0.12,
);

const fill: Record<string, string> = {
  good: "var(--up)",
  bad: "var(--down)",
  unknown: "var(--ink-3)",
  excluded: "var(--maint)",
};

// §5.4: the strip had NO tooltip at all — not one. Every cell is now hoverable AND focusable, and
// the content is the parent's business (it owns the vocabulary), so it arrives through a scoped
// slot. The strip only says which cell and where.
const hovered = ref<{ cell: Cell; pct: number } | null>(null);
function enter(cell: Cell) {
  hovered.value = { cell, pct: ((x(cell.startMs) + (x(cell.endMs) - x(cell.startMs)) / 2) / W) * 100 };
}
function leave() {
  hovered.value = null;
}

const placedMarks = computed(() => {
  const out: { m: StripMark; pct: number }[] = [];
  for (const m of props.marks) {
    const at = Date.parse(m.at);
    if (Number.isNaN(at) || at < props.axisFromMs || at >= props.axisToMs) continue;
    out.push({ m, pct: (x(at) / W) * 100 });
  }
  return out;
});
</script>

<template>
  <div class="relative block" :class="placedMarks.length ? 'pt-[22px]' : ''" :data-marks="placedMarks.length || undefined">
    <svg
      :viewBox="`0 0 ${W} ${h + 6}`"
      preserveAspectRatio="none"
      :style="{ height: h + 6 + 'px', width: '100%', display: 'block' }"
      role="img"
      :aria-label="variant === 'lane' ? 'segment lane' : 'reliability timeline'"
      :data-variant="variant"
    >
      <defs>
        <!-- `not-stored`: a hatch WITH a cell outline. Not a hue (absence is not a verdict) and
             not opacity (that means `provisional`). -->
        <pattern id="rsHatch" width="5" height="5" patternUnits="userSpaceOnUse" patternTransform="rotate(45)">
          <!-- a PATH, not a rect: a stray <rect> in <defs> is the first rect in document order
               and every `find("rect")` in a test would land on it instead of on a cell. -->
          <path d="M0 0H5V5H0Z" fill="var(--inset)" />
          <line x1="0" y1="0" x2="0" y2="5" stroke="var(--border-strong)" stroke-width="1" />
        </pattern>
      </defs>

      <template v-if="!subPixel">
        <template v-for="(cell, i) in laid.filter((l) => l.w > 0)" :key="i">
          <!-- an active repair is rendered as WORK, never as data (§12.1) -->
          <rect
            v-if="cell.c.repairing"
            :x="cell.x" y="2" :width="cell.w" :height="h"
            rx="0.6" style="fill: var(--accent); opacity: 0.55"
            data-state="repairing"
            :data-cell-start="cell.c.startMs"
          />
          <template v-else>
            <rect
              v-for="(s, j) in cell.slices"
              :key="j"
              :x="cell.x"
              :y="2 + cell.slices.slice(0, j).reduce((a, p) => a + p.h, 0)"
              :width="cell.w"
              :height="s.h"
              rx="0.6"
              :style="s.kind === 'notStored'
                ? { fill: 'url(#rsHatch)', stroke: 'var(--border-strong)', strokeWidth: 0.5 }
                : { fill: fill[s.kind], opacity: s.provisional ? 0.38 : 1 }"
              :data-state="s.kind"
              :data-provisional="s.provisional ? 'true' : undefined"
              :data-cell-start="cell.c.startMs"
              :data-stored-minutes="cell.c.storedMinutes"
            />
          </template>
        </template>
      </template>

      <!-- One transparent hit target per cell: hover and keyboard focus reach the same readout,
           and a cell narrower than a finger still has a target of its own. -->
      <rect
        v-for="(cell, i) in laid"
        :key="'hit' + i"
        :x="cell.hitX" y="0" :width="cell.hitW" :height="h + 5"
        fill="transparent"
        tabindex="0"
        role="button"
        data-affordance="non-geometric"
        :aria-label="cell.c.repairing ? 'interval being recomputed' : 'reliability interval'"
        data-testid="strip-cell-hit"
        :data-cell-start="cell.c.startMs"
        @pointerenter="enter(cell.c)"
        @pointerleave="leave()"
        @focus="enter(cell.c)"
        @blur="leave()"
      />

      <!-- sealed_through: the window's own right edge. Everything past it is in no number. -->
      <rect
        v-if="sealedThroughMs !== undefined && sealedThroughMs > axisFromMs && sealedThroughMs < axisToMs"
        :x="x(sealedThroughMs) - 0.6" y="0" width="1.2" :height="h + 5"
        style="fill: var(--ink-2)" data-marker="sealed-through"
      />

      <!-- boundary marks, one per CLUSTER, anchored at the cluster's earliest real boundary -->
      <rect
        v-for="(cl, i) in clusters"
        :key="'c' + i"
        :x="x(cl.ms)"
        y="0"
        :width="cl.members.every((m) => m.epochOnly) ? MARK_W * 0.5 : MARK_W"
        :height="h + 5"
        :style="cl.members.every((m) => m.epochOnly)
          ? { fill: 'var(--ink-3)', opacity: 0.7 }
          : { fill: 'var(--accent)' }"
        :data-marker="cl.members.every((m) => m.epochOnly) ? 'epoch' : 'revision'"
        :data-cluster-count="cl.count"
        :data-cluster-first="cl.ms"
        :data-cluster-last="cl.lastMs"
      />
    </svg>

    <!-- The readout, positioned over the cell it belongs to. Content comes from the parent. -->
    <div
      v-if="hovered"
      class="pointer-events-none absolute z-10"
      :style="{ left: hovered.pct + '%', top: '100%', transform: 'translateX(-50%)' }"
      data-testid="strip-readout"
    >
      <slot name="readout" :cell="hovered.cell" />
    </div>

    <!-- A sub-pixel segment: a NON-GEOMETRIC marker at its start, saying it is not to scale.
         Hollow and dashed so it cannot be read as a measured extent. -->
    <span
      v-if="subPixel && laneExtent"
      class="absolute flex -translate-x-1/2 flex-col items-center"
      :style="{ left: (x(laneExtent.from) / W) * 100 + '%', top: '0' }"
      data-testid="strip-subpixel"
      :data-from="laneExtent.from"
      :data-to="laneExtent.to"
      role="img"
      aria-label="sub-pixel segment, not to scale"
    >
      <i
        class="block h-[9px] w-[9px] rotate-45 border-[1.4px] border-dashed border-degraded"
        aria-hidden="true"
      ></i>
      <b class="block h-[6px] w-px bg-degraded" aria-hidden="true"></b>
    </span>

    <!-- FR-025 change marks: shape = kind, fill = accent; the stem points at the instant. -->
    <span
      v-for="(p, i) in placedMarks"
      :key="p.m.key ?? i"
      class="absolute top-0 flex -translate-x-1/2 flex-col items-center gap-[2px] rounded-[2px]"
      :style="{ left: p.pct + '%', ...(p.m.selected ? { outline: '2px solid var(--focus)', outlineOffset: '1px' } : {}) }"
      role="img"
      :aria-label="p.m.label"
      :title="p.m.label"
      data-testid="strip-mark"
      :data-kind="p.m.kind"
      :data-at="p.m.at"
      :data-selected="p.m.selected ? 'true' : undefined"
    >
      <i class="block h-[11px] w-[11px] bg-accent" :style="{ clipPath: kindClip(p.m.kind) }" aria-hidden="true"></i>
      <b class="block h-[8px] w-px bg-accent" aria-hidden="true"></b>
    </span>
  </div>
</template>
