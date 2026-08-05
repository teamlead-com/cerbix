<script setup lang="ts">
defineProps<{
  label: string;
  value: string;
  unit?: string;
  sub?: string;
  trend?: { dir: "pos" | "neg" | "flat"; label: string };
}>();

const arrow = (dir: string) => (dir === "pos" ? "▲" : dir === "neg" ? "▼" : "▪");
const trendCls = (dir: string) => (dir === "pos" ? "text-up" : dir === "neg" ? "text-down" : "text-ink-3");
</script>

<template>
  <div class="flex flex-col gap-2 rounded border border-border bg-surface p-4 shadow-card">
    <div class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">{{ label }}</div>
    <div class="font-mono text-[26px] font-medium leading-none tracking-tight tnum">
      {{ value }}<span v-if="unit" class="text-[15px] text-ink-3">{{ unit }}</span>
    </div>
    <div v-if="sub || trend" class="flex items-center gap-[5px] text-xs text-ink-3">
      <span v-if="trend" class="inline-flex items-center gap-[3px] font-mono text-[11.5px]" :class="trendCls(trend.dir)">
        {{ arrow(trend.dir) }} {{ trend.label }}
      </span>
      <span v-if="sub">{{ sub }}</span>
    </div>
  </div>
</template>
