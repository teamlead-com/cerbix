<script setup lang="ts">
import { computed } from "vue";

const props = withDefaults(
  defineProps<{ values: number[]; width?: number; height?: number; color?: string }>(),
  { width: 108, height: 30, color: "var(--accent)" },
);

const geom = computed(() => {
  const vals = props.values.length ? props.values : [0, 0];
  const w = props.width;
  const h = props.height;
  const pad = 3;
  const min = Math.min(...vals);
  const max = Math.max(...vals);
  const rng = max - min || 1;
  const pts = vals.map((v, i) => {
    const x = pad + (i / (vals.length - 1 || 1)) * (w - pad * 2);
    const y = pad + (1 - (v - min) / rng) * (h - pad * 2);
    return [x, y] as const;
  });
  const line = pts.map((p, i) => `${i ? "L" : "M"}${p[0].toFixed(1)} ${p[1].toFixed(1)}`).join(" ");
  const area = `${line} L${pts[pts.length - 1][0].toFixed(1)} ${h - pad} L${pts[0][0].toFixed(1)} ${h - pad} Z`;
  return { line, area, end: pts[pts.length - 1], baseY: h - pad };
});
const gid = computed(() => "sl" + Math.round(props.values.reduce((a, b) => a + b, 0)) + props.values.length);
</script>

<template>
  <svg :width="width" :height="height" :viewBox="`0 0 ${width} ${height}`" fill="none" aria-hidden="true" class="text-ink-3">
    <defs>
      <linearGradient :id="gid" x1="0" y1="0" x2="0" y2="1">
        <stop offset="0" :stop-color="color" stop-opacity="0.22" />
        <stop offset="1" :stop-color="color" stop-opacity="0" />
      </linearGradient>
    </defs>
    <line x1="3" :y1="geom.baseY" :x2="width - 3" :y2="geom.baseY" stroke="currentColor" stroke-opacity="0.1" />
    <path :d="geom.area" :fill="`url(#${gid})`" />
    <path :d="geom.line" :stroke="color" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" />
    <circle :cx="geom.end[0].toFixed(1)" :cy="geom.end[1].toFixed(1)" r="2.6" :fill="color" />
  </svg>
</template>
