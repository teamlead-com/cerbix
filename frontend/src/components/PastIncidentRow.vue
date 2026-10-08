<script setup lang="ts">
// One past (resolved) incident as an accordion row. The status page's *Past incidents* and the
// incident history render the SAME row (func-status-pages-incidents.md §13.5, approved mock), so
// the markup has one owner. Moved verbatim from PublicStatusView.vue in iter-0203.
import type { components } from "@/api/schema";
import { impactBadge, incidentDuration, relTime, statusBadge } from "@/lib/incident";
import { renderSections } from "@/lib/postmortem";

type IncidentDetail = components["schemas"]["IncidentDetail"];

defineProps<{
  inc: IncidentDetail;
  headerId: string;
  panelId: string;
  expanded: boolean;
}>();
const emit = defineEmits<{ toggle: [] }>();
</script>

<template>
  <div class="border-b border-border last:border-b-0" data-testid="past-incident-row">
    <!-- collapsed row (click to expand) -->
    <button
      :id="headerId"
      type="button"
      class="flex w-full min-w-0 flex-wrap items-center gap-3 px-[18px] py-[13px] text-left hover:bg-surface-2"
      :aria-expanded="expanded"
      :aria-controls="panelId"
      @click="emit('toggle')"
      @keydown.enter.prevent="emit('toggle')"
      @keydown.space.prevent="emit('toggle')"
    >
      <span class="h-[8px] w-[8px] flex-none rounded-full bg-up"></span>
      <div class="min-w-0 flex-1">
        <div class="text-[13.5px] font-medium">{{ inc.title }}</div>
        <div class="font-mono text-[11.5px] text-ink-3">
          resolved {{ relTime(inc.resolved_at ?? undefined)
          }}<template v-if="incidentDuration(inc.started_at, inc.resolved_at)">
            · lasted
            {{ incidentDuration(inc.started_at, inc.resolved_at) }}</template
          >
          ·
          {{ impactBadge(inc.impact).label.toLowerCase() }}
          impact<template v-if="inc.postmortem">
            · <span class="text-accent">postmortem</span></template
          >
        </div>
      </div>
      <span
        class="rounded-full px-[9px] py-[2px] text-[11.5px] font-semibold"
        :class="statusBadge(inc.status).cls"
        >{{ statusBadge(inc.status).label }}</span
      >
      <svg
        viewBox="0 0 24 24"
        class="h-[15px] w-[15px] flex-none text-ink-3 transition-transform"
        :class="expanded ? 'rotate-180' : ''"
        fill="none"
        stroke="currentColor"
        stroke-width="2"
      >
        <path d="M6 9l6 6 6-6" />
      </svg>
    </button>
    <!-- expanded: postmortem if published, else the update timeline -->
    <div
      v-if="expanded"
      :id="panelId"
      role="region"
      :aria-labelledby="headerId"
      class="border-t border-border bg-surface-2 px-[18px] py-[15px]"
    >
      <template v-if="inc.postmortem && renderSections(inc.postmortem.body).length">
        <div class="mb-[10px] text-[11px] font-semibold uppercase tracking-[0.06em] text-ink-3">
          Postmortem
        </div>
        <div v-for="sec in renderSections(inc.postmortem.body)" :key="sec.heading" class="mb-3 last:mb-0">
          <h4 class="mb-1 text-[12px] font-semibold uppercase tracking-[0.05em] text-ink-3">
            {{ sec.heading }}
          </h4>
          <p class="whitespace-pre-wrap text-[13px] leading-relaxed text-ink-2">
            {{ sec.content }}
          </p>
        </div>
      </template>
      <template v-else-if="inc.updates && inc.updates.length">
        <div class="mb-[10px] text-[11px] font-semibold uppercase tracking-[0.06em] text-ink-3">
          Timeline
        </div>
        <div
          v-for="(u, updateIndex) in inc.updates"
          :key="updateIndex"
          class="border-l-2 border-border pb-[11px] pl-3 last:pb-0"
        >
          <div class="flex items-center gap-2">
            <span
              class="rounded-full px-[8px] py-[1px] text-[11px] font-semibold"
              :class="statusBadge(u.status).cls"
              >{{ statusBadge(u.status).label }}</span
            >
            <span class="font-mono text-[11px] text-ink-3">{{ relTime(u.created_at) }}</span>
          </div>
          <p v-if="u.body" class="mt-[3px] whitespace-pre-wrap text-[13px] text-ink-2">
            {{ u.body }}
          </p>
        </div>
      </template>
      <p v-else class="text-[13px] text-ink-3">No further details were published.</p>
    </div>
  </div>
</template>
