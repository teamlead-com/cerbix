<script setup lang="ts">
// FR-041a §4.3 / D31, mock 3e″: a phone channel that is the ONLY route of some paging services.
// Read from the channel's own `only_route_for` / `only_route_for_total` (§14 SPA point 5) — computed
// by the server's route predicate, never recomputed here. Shown at the top of the channel's edit
// form (Task 4) and under its row in the list (Task 5).
import { computed } from "vue";
import type { components } from "@/api/schema";

const props = defineProps<{
  services: components["schemas"]["ServiceWithoutRoute"][];
  total?: number;
}>();

/** Services past the server's bound of 50, which it counts but does not list. */
const more = computed(() => Math.max(0, (props.total ?? props.services.length) - props.services.length));
</script>

<template>
  <div class="rounded border border-degraded/40 bg-degraded-weak px-3 py-[9px] text-[12.5px] leading-[1.55] text-ink-2" data-testid="phone-only-route">
    <div class="flex flex-wrap items-center gap-[6px]">
      <b class="font-semibold text-degraded">Only route for</b>
      <span
        v-for="(s, i) in services"
        :key="`${i}/${s.slug}`"
        class="inline-flex items-center gap-[5px] whitespace-nowrap rounded-full border border-border bg-surface px-2 py-px text-[11.5px] text-ink"
        :title="s.service"
        data-testid="phone-only-route-service"
      ><span class="text-[10px] font-semibold uppercase tracking-[0.05em] text-ink-3">service</span> <span>{{ s.slug }}</span></span>
      <span v-if="more > 0" class="font-medium text-ink-2" data-testid="phone-only-route-more">… and {{ more }} more.</span>
    </div>
    <p class="mt-[6px]">If a message to it is not sent — storm limit, refused by the provider, outcome unknown — nobody hears about those services' incidents, and their members' own alerts stay held. Add a second channel, or set re-notify on the members.</p>
  </div>
</template>
