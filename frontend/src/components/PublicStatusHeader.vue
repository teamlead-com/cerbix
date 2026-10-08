<script setup lang="ts">
// The public status page's sticky header: brand, page title, theme toggle and the RSS link. Shared
// by the status page and its incident history (iter-0203); moved verbatim from PublicStatusView.vue.
import { computed } from "vue";
import BrandMark from "@/components/BrandMark.vue";
import { useTheme } from "@/composables/useTheme";

defineProps<{ title: string; feedHref: string }>();

const { theme, toggle } = useTheme();
const themeLabel = computed(() => (theme.value === "dark" ? "Switch to light theme" : "Switch to dark theme"));
</script>

<template>
  <header class="sticky top-0 z-10 border-b border-border bg-surface/80 backdrop-blur">
    <div class="mx-auto flex h-[60px] max-w-[820px] items-center gap-3 px-5">
      <BrandMark :tile="28" :glyph="16" />
      <b class="text-[15px] font-semibold tracking-tight">{{ title }}</b>
      <div class="ml-auto flex items-center gap-2">
        <button
          class="grid h-[34px] w-[34px] place-items-center rounded-sm border border-border bg-surface text-ink-2 hover:border-border-strong hover:text-ink"
          type="button"
          :aria-label="themeLabel"
          :aria-pressed="theme === 'dark'"
          @click="toggle"
        >
          <svg viewBox="0 0 24 24" class="h-4 w-4" fill="none" stroke="currentColor" stroke-width="2">
            <circle cx="12" cy="12" r="4" />
            <path d="M12 2v2M12 20v2M2 12h2M20 12h2M5 5l1.5 1.5M17.5 17.5L19 19M19 5l-1.5 1.5M6.5 17.5L5 19" />
          </svg>
        </button>
        <a
          :href="feedHref"
          class="inline-flex h-[34px] items-center gap-[7px] rounded-sm border border-border bg-surface px-[13px] text-[13px] font-medium text-ink hover:border-border-strong"
        >
          <svg viewBox="0 0 24 24" class="h-[15px] w-[15px]" fill="none" stroke="currentColor" stroke-width="2">
            <path d="M4 11a9 9 0 0 1 9 9M4 4a16 16 0 0 1 16 16" />
            <circle cx="5" cy="19" r="1.4" fill="currentColor" />
          </svg>
          RSS
        </a>
      </div>
    </div>
  </header>
</template>
