<script setup lang="ts">
// ONE readout for every reliability cell, on the overview strip and in a segment lane alike
// (func-truthful-rendering §5.4, invariant 6b).
//
// It exists as a component rather than as markup in two places because the two DRIFTED: the lane's
// readout rendered only the time labels, so a cell whose problem was marked as too small to draw
// could carry a marker with nothing naming the state — reviewer P1 at party [186]. Two copies of a
// readout is exactly how that happens, so there is one.
//
// It takes the SLICES THE STRIP ACTUALLY DREW, not the cell alone. `belowFloor` is not a property
// of a cell: the floor is fixed in pixels and the cap is a share of the strip's height, so the same
// cell can be fully funded at 30px and marked at 14px. A previous version recomputed the stack at a
// nominal height and its comment claimed the marker state was height-independent — which was
// simply false, and is why a lane's marker had no matching readout.
import { computed } from "vue";

import { CANONICAL_BUCKET_MS, type Cell, type Slice } from "@/lib/reliabilitygeometry";
import { utcCellExtentLabel, utcExtentLabel } from "@/lib/wallclock";

const props = defineProps<{ cell: Cell; slices: Slice[]; compact?: boolean }>();

const usToText = (us: number): string => {
  const total = Math.round(us / 1_000_000);
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const sec = total % 60;
  return [h ? `${h}h` : "", m ? `${m}m` : "", !h && sec ? `${sec}s` : ""].filter(Boolean).join(" ") || "0s";
};

const view = computed(() => {
  const cell = props.cell;
  const fromIso = new Date(cell.startMs).toISOString();
  const toIso = new Date(cell.endMs).toISOString();
  const extentMinutes = (cell.endMs - cell.startMs) / CANONICAL_BUCKET_MS;
  const rows: { label: string; value: string }[] = [];
  const push = (label: string, us: number) => {
    if (us > 0) rows.push({ label, value: usToText(us) });
  };
  push("good", cell.sealed.good);
  push("down", cell.sealed.bad);
  push("unknown", cell.sealed.unknown);
  push("excluded", cell.sealed.excluded);
  push("provisional good", cell.provisional.good);
  push("provisional down", cell.provisional.bad);
  push("provisional unknown", cell.provisional.unknown);
  push("provisional excluded", cell.provisional.excluded);
  const missing = Math.max(0, extentMinutes - cell.storedMinutes);
  if (missing > 0) push("no stored bucket", missing * CANONICAL_BUCKET_MS * 1000);

  // The states the drawn stack could not bring up to their floor, each with the duration it stands
  // for — the marker says one exists, and this is where the promise is actually kept.
  const marked = props.slices
    .filter((s) => s.belowFloor)
    .map((s) => {
      const src = s.provisional ? cell.provisional : cell.sealed;
      const us = s.kind === "bad" ? src.bad : s.kind === "unknown" ? src.unknown : src.excluded;
      const name = s.kind === "bad" ? "down" : s.kind;
      return `${s.provisional ? "provisional " : ""}${name} ${usToText(us)}`;
    });

  return {
    local: utcCellExtentLabel(fromIso, toIso),
    utc: utcExtentLabel(fromIso, toIso),
    rows,
    stored: `${cell.storedMinutes} of ${Math.round(extentMinutes)}`,
    repairing: cell.repairing,
    marked,
  };
});
</script>

<template>
  <div
    class="rounded-sm border border-border-strong bg-surface shadow-card"
    :class="compact ? 'p-[8px_10px] text-[11.5px]' : 'p-[9px_11px] text-[12px]'"
    data-testid="svc-cell-readout"
  >
    <div class="font-mono" :class="compact ? 'text-ink' : 'text-[12.5px] text-ink'">{{ view.local }}</div>
    <div class="font-mono text-ink-3" :class="compact ? 'text-[11px]' : 'text-[11.5px]'">{{ view.utc }}</div>
    <p v-if="view.repairing" class="mt-[6px] text-[11.5px] text-accent">
      being recomputed — rendered as work in progress, never as data
    </p>
    <table v-else class="mt-[6px] border-collapse text-[11.5px]">
      <tbody>
        <tr v-for="(r, i) in view.rows" :key="i">
          <td class="pr-[14px] text-ink-3">{{ r.label }}</td>
          <td class="text-right font-mono text-ink-2">{{ r.value }}</td>
        </tr>
        <tr>
          <td class="pr-[14px] text-ink-3">stored buckets</td>
          <td class="text-right font-mono text-ink-2">{{ view.stored }}</td>
        </tr>
        <tr v-if="view.marked.length">
          <td colspan="2" class="pt-[6px] text-left text-degraded" data-testid="svc-cell-belowfloor">
            marked, too small to draw at this size: {{ view.marked.join(", ") }}
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
