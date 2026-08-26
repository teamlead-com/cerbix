<script setup lang="ts">
// Root shell. Individual views own their own layout for now.
import { onMounted } from "vue";
import { useRoute } from "vue-router";
import { useBranding } from "@/stores/branding";

// Instance branding (product name, accent color, announcement) is public and
// applied once at startup so it themes the login page too.
onMounted(() => useBranding().load());

// Keyed by PATH, so navigating between two of the same kind remounts the view.
//
// Vue Router reuses a component when only the params change, and the detail views read
// `route.params.id` once and load on mount — so a search hit for another monitor changed the URL and
// left the previous one on screen, with its mutations still pointing at it. The views also watch
// their identity now; the key is the layer that does not depend on each view remembering to.
//
// PATH and not `fullPath`: several views keep state in the query string (`?tab=`), and remounting on
// a query change would throw that away for no reason.
const route = useRoute();
</script>

<template>
  <RouterView :key="route.path" />
</template>
