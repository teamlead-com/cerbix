<script setup lang="ts">
import { computed } from "vue";

type Status = "up" | "down" | "degraded" | "maint" | "pending";
// label overrides the default status text (e.g. "Confirming 2/3…" on the
// degraded style while a failure is being confirmed).
const props = defineProps<{ status: Status; label?: string }>();

const meta: Record<Status, { label: string; text: string; bg: string; dot: string }> = {
  up: { label: "Operational", text: "text-up", bg: "bg-up-weak", dot: "bg-up" },
  down: { label: "Down", text: "text-down", bg: "bg-down-weak", dot: "bg-down" },
  degraded: { label: "Degraded", text: "text-degraded", bg: "bg-degraded-weak", dot: "bg-degraded" },
  maint: { label: "Maintenance", text: "text-maint", bg: "bg-maint-weak", dot: "bg-maint" },
  pending: { label: "Pending", text: "text-pending", bg: "bg-inset", dot: "bg-pending" },
};
const m = computed(() => meta[props.status]);
</script>

<template>
  <span class="inline-flex h-6 items-center gap-[6px] rounded-full pl-[7px] pr-[9px] text-xs font-medium" :class="[m.text, m.bg]">
    <span class="relative flex h-[7px] w-[7px]">
      <span v-if="status === 'up'" class="absolute inline-flex h-full w-full animate-ping rounded-full opacity-75 motion-reduce:hidden" :class="m.dot"></span>
      <span class="relative inline-flex h-[7px] w-[7px] rounded-full" :class="m.dot"></span>
    </span>
    {{ label ?? m.label }}
  </span>
</template>
