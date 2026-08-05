<script setup lang="ts">
// Root shell. Individual views own their own layout for now.
import { onMounted } from "vue";
import { useBranding } from "@/stores/branding";

// Instance branding (product name, accent color, announcement) is public and
// applied once at startup so it themes the login page too.
onMounted(() => useBranding().load());
</script>

<template>
  <RouterView />
</template>
