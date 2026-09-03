<script setup lang="ts">
import StatusPill from "@/components/StatusPill.vue";
import UptimeBar from "@/components/UptimeBar.vue";
import Sparkline from "@/components/Sparkline.vue";
import type { components } from "@/api/schema";

type Monitor = components["schemas"]["Monitor"];
type Seg = "up" | "degraded" | "down" | "maint" | "none";

const props = defineProps<{
  monitor: Monitor;
  uptime?: string;
  latency?: string;
  segments?: Seg[];
  spark?: number[];
  budgetLeft?: number | null;
  budgetMet?: boolean;
}>();

const statusFor = (): "up" | "down" | "pending" =>
  (props.monitor.status as "up" | "down" | "pending") ?? "pending";

// Error-budget meter: width = budget remaining, colored by health.
const budgetMeter = () => {
  const left = props.budgetLeft ?? 0;
  const cls = props.budgetMet === false ? "bg-down" : left <= 20 ? "bg-degraded" : "bg-up";
  return { width: Math.max(0, Math.min(100, left)), cls };
};
</script>

<template>
  <RouterLink
    :to="{ name: 'monitor', params: { id: monitor.id } }"
    class="flex flex-col gap-[13px] rounded border border-border bg-surface p-4 shadow-card transition hover:-translate-y-px hover:border-border-strong"
  >
    <div class="flex items-center gap-[9px]">
      <span class="font-mono text-[13.5px] font-semibold tracking-tight">{{ monitor.name }}</span>
      <span class="rounded-xs border border-border px-[5px] py-px font-mono text-[10.5px] uppercase tracking-[0.04em] text-ink-3">
        {{ monitor.type }}
      </span>
      <StatusPill class="ml-auto" :status="statusFor()" />
    </div>
    <!-- FR-030: the BEGINNING of the description, one line, cut by the panel's own width so the grid
         keeps its shape; absent for a monitor without one, so the card keeps today's height. -->
    <div v-if="monitor.description" class="-mt-[6px] truncate text-[12px] leading-[1.35] text-ink-3" :title="monitor.description" data-testid="monitor-description">{{ monitor.description }}</div>

    <UptimeBar v-if="segments && segments.length" :segments="segments" :height="26" />

    <div class="flex items-end gap-4">
      <div class="flex flex-col gap-[2px]">
        <span class="text-[10.5px] font-semibold uppercase tracking-[0.06em] text-ink-3">Uptime 30d</span>
        <span class="font-mono text-base tnum">{{ uptime ?? "—" }}</span>
      </div>
      <div v-if="monitor.type !== 'push'" class="flex flex-col gap-[2px]">
        <span class="text-[10.5px] font-semibold uppercase tracking-[0.06em] text-ink-3">Latency</span>
        <span class="font-mono text-base tnum">{{ latency ?? "—" }}</span>
      </div>
      <Sparkline v-if="spark && spark.length" class="ml-auto" :values="spark" />
    </div>

    <div v-if="budgetLeft !== null && budgetLeft !== undefined && monitor.type !== 'push'" class="flex flex-col gap-[6px]">
      <div class="flex items-baseline justify-between">
        <span class="text-[11px] text-ink-3">Error budget</span>
        <span class="font-mono text-[12px] tnum" :class="budgetMet === false ? 'text-down' : 'text-ink-2'">{{ Math.round(budgetLeft) }}% left</span>
      </div>
      <div class="h-[6px] overflow-hidden rounded-full bg-inset">
        <i class="block h-full rounded-full" :class="budgetMeter().cls" :style="{ width: budgetMeter().width + '%' }"></i>
      </div>
    </div>
  </RouterLink>
</template>
