<script setup lang="ts">
const props = withDefaults(defineProps<{ size?: number }>(), {
  size: 16,
});
</script>

<template>
  <svg
    :width="props.size"
    :height="props.size"
    viewBox="0 0 32 32"
    fill="none"
    stroke="currentColor"
    stroke-width="2.5"
    stroke-linecap="square"
    stroke-linejoin="round"
    aria-hidden="true"
    data-brand-glyph="sealed-c"
  >
    <path d="M21.4 9.5a8.5 8.5 0 1 0 .1 12.9" />
    <path d="M19.9 16h4.9" />
  </svg>
</template>
