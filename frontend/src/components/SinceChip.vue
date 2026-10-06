<script setup lang="ts">
import { utcDayLabel } from "@/lib/wallclock";

// iter-0199 / D-0268: an SLI window whose data starts after the window does names that day, so the
// number is never read as covering the whole window. The approved mock
// (docs/design/mock-sla-long-windows.html) uses ONE marker on the monitor detail cards, the SLA page
// and the public status page: neutral ink on --inset, never a status colour, because it states how
// much history a number covers, not health.
defineProps<{ day: string; window?: string }>();
</script>

<template>
  <span
    class="inline-flex items-center gap-1 whitespace-nowrap rounded-full bg-inset px-[7px] py-px font-mono text-[10.5px] text-ink-2"
    :title="window ? `The data starts ${utcDayLabel(day)}; the ${window} window reaches further back.` : `The data starts ${utcDayLabel(day)}.`"
    ><span class="h-[5px] w-[5px] rounded-full border border-ink-3" aria-hidden="true"></span>since {{ utcDayLabel(day) }}</span
  >
</template>
