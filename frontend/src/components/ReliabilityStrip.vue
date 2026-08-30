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
//
// FR-025 (D14, D-0210 item 1): CHANGE MARKS ride on top of the strip — one per TERMINAL phase,
// kind-shaped (▲ deploy ▼ rollback ◆ flag) in the accent, never a state hue: a deploy is not
// good or bad, what FOLLOWED it is, and the ticks beneath say that in the hues the facts own.
// Geometry is time for the marks too: a mark's x is its containing tick's x plus its fraction of
// that tick's own [startMs, endMs) — so a boundary fragment or a storage hole places the mark
// where the facts around it actually are, not where a linear clock would guess. A mark inside
// a hole sits at the seam before the next tick; a mark outside the strip's span is not drawn
// (the strip has no place for it). Ticks without timestamps fall back to the optional `range`
// (linear); without either, no mark is drawn. The existing usage (no marks) renders exactly as
// before, inside a wrapper that gives the marks a place to stand.
import { computed } from "vue";

import { kindClip, type StripMark } from "@/lib/changes";

export type { StripMark };

export type ReliabilityTick = {
  state: "good" | "bad" | "unknown" | "excluded" | "repairing" | "none";
  // weight is the tick's bucket count; widths are proportional to it.
  weight: number;
  provisional?: boolean;
  // boundary markers drawn BEFORE this tick
  revisionBoundary?: boolean;
  epochBoundary?: boolean;
  // the tick's real time extent (ms since epoch), when the producer knows it — marks are placed by it
  startMs?: number;
  endMs?: number;
};

const props = withDefaults(
  defineProps<{
    ticks: ReliabilityTick[];
    height?: number;
    /** FR-025: change marks to draw over the strip, placed by `at`. */
    marks?: StripMark[];
    /** The strip's span, for placing marks when the ticks carry no timestamps. */
    range?: { from: string; to: string };
  }>(),
  { height: 26, marks: () => [], range: undefined },
);

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

type Cell = { t: ReliabilityTick; x: number; w: number };
const timed = (c: Cell): c is Cell & { t: { startMs: number; endMs: number } } =>
  typeof c.t.startMs === "number" && typeof c.t.endMs === "number" && c.t.endMs > c.t.startMs;

/** Each mark's x in strip units, or nothing when the strip has no place for it. */
function placeMark(atMs: number): number | null {
  const cells = laid.value.filter(timed);
  if (cells.length) {
    const hit = cells.find((c) => c.t.startMs <= atMs && atMs < c.t.endMs);
    if (hit) return hit.x + hit.w * ((atMs - hit.t.startMs) / (hit.t.endMs - hit.t.startMs));
    const first = cells[0];
    const last = cells[cells.length - 1];
    if (atMs < first.t.startMs || atMs >= last.t.endMs) return null;
    // Inside the span but in a hole between ticks: the seam before the next tick.
    const next = cells.find((c) => c.t.startMs > atMs);
    return next ? next.x : null;
  }
  if (props.range) {
    const from = Date.parse(props.range.from);
    const to = Date.parse(props.range.to);
    if (Number.isNaN(from) || Number.isNaN(to) || to <= from || atMs < from || atMs >= to) return null;
    return ((atMs - from) / (to - from)) * total.value;
  }
  return null;
}

const placed = computed(() => {
  const out: { m: StripMark; pct: number }[] = [];
  for (const m of props.marks) {
    const at = Date.parse(m.at);
    if (Number.isNaN(at)) continue;
    const x = placeMark(at);
    if (x == null) continue;
    out.push({ m, pct: (x / total.value) * 100 });
  }
  return out;
});
</script>

<template>
  <div class="relative block" :class="placed.length ? 'pt-[22px]' : ''" :data-marks="placed.length || undefined">
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
    <!-- Change marks: shape = kind, fill = accent; the stem points at the instant. The selected
         row's mark carries the focus ring on the whole mark (a clip-path would clip an outline on
         the glyph itself). -->
    <span
      v-for="(p, i) in placed"
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
