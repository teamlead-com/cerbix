<script setup lang="ts">
// FR-041a §4.5 and §5.5, mock §4 (4a–4g): the incident page's Phone panel — every phone delivery
// made for this incident, with its reason. The view reads `GET /incidents/{id}/phone-activity` and
// hands the answer here; this component decides whether there is a panel and draws it.
//
// - WHEN (revision 6, §5.5): a panel when the incident has a delivery, or when phone alerts are on
//   (`/me` `telephony_enabled`). Off and no delivery → nothing at all (4g-1), so an instance that does
//   not use phones sees nothing new. The rule reads the list first: a trace is never hidden because
//   the switch is off now (4g-2, P7). A FAILED read is 4d whatever the switch says, because the
//   deliveries are unknown. While the list is being read, nothing is drawn.
// - ORDER: the API's — oldest first, as the Timeline above it (P3-2). Never re-sorted.
// - A ROW is PhoneDeliveryRow, shared with a channel's Deliveries list: the reason chip, then the
//   channel, the mask (never a number), SMS / Call and the alert. The alert drops the subject's name,
//   which the page's header already shows ("Service alert · down").
// - Nothing here is posted as an update, and no public view shows it (D22, P19): hence the chip
//   "not on the status page".
import { computed } from "vue";
import PhoneDeliveryRow from "@/components/PhoneDeliveryRow.vue";
import { type PhoneDelivery, alertSummary, kindLabel } from "@/lib/phone";

const props = defineProps<{
  state: "loading" | "ready" | "failed";
  deliveries: PhoneDelivery[];
  telephonyEnabled: boolean;
}>();

const shown = computed(
  () => props.state === "failed" || (props.state === "ready" && (props.deliveries.length > 0 || props.telephonyEnabled)),
);
const count = computed(() => {
  const n = props.deliveries.length;
  return n === 1 ? "1 delivery" : `${n} deliveries`;
});
const PILL = "inline-flex items-center whitespace-nowrap rounded-full border border-border px-2 py-px text-[11px] text-ink-3";
</script>

<template>
  <section v-if="shown" class="mb-5 rounded border border-border bg-surface shadow-card" data-testid="incident-phone-panel">
    <header class="flex flex-wrap items-center gap-[10px] border-b border-border px-4 py-[13px]">
      <h2 class="text-[13px] font-semibold" data-testid="incident-phone-title">Phone</h2>
      <span v-if="state === 'ready' && deliveries.length" :class="PILL" data-testid="incident-phone-count">{{ count }}</span>
      <span :class="[PILL, 'ml-auto']" data-testid="incident-phone-public">not on the status page</span>
    </header>

    <span
      v-if="state === 'failed'"
      class="mx-4 my-3 inline-flex items-center gap-[6px] rounded-[6px] border border-dashed border-border-strong px-[9px] py-1 text-[12.5px] text-ink-3"
      data-testid="incident-phone-unavailable"
    >Phone activity could not be read — this is not “nothing was sent”.</span>
    <p v-else-if="!deliveries.length" class="px-4 py-[22px] text-center text-[13px] text-ink-3" data-testid="incident-phone-empty">No phone alert was sent for this incident.</p>
    <div v-else>
      <PhoneDeliveryRow v-for="d in deliveries" :key="d.id" :delivery="d" when="ago">
        <span class="font-medium" data-testid="delivery-channel">{{ d.channel_name }}</span>
        <span v-if="d.number_masked" class="font-mono text-[12px] text-ink-2" data-testid="delivery-mask">{{ d.number_masked }}</span>
        <span class="inline-flex items-center gap-[5px] whitespace-nowrap text-[12px] text-ink-2" data-testid="delivery-kind">
          <svg v-if="d.kind === 'voice'" viewBox="0 0 24 24" class="h-[13px] w-[13px] flex-none" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><path d="M5 4h4l2 5-3 2a11 11 0 0 0 5 5l2-3 5 2v4a2 2 0 0 1-2 2A16 16 0 0 1 3 6a2 2 0 0 1 2-2" /></svg>
          <svg v-else viewBox="0 0 24 24" class="h-[13px] w-[13px] flex-none" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><path d="M4 5h16v11H8l-4 4z" /></svg>
          {{ kindLabel(d.kind) }}
        </span>
        <span class="text-ink-3" data-testid="delivery-alert">{{ alertSummary(d, { withSubject: false }) }}</span>
      </PhoneDeliveryRow>
    </div>
  </section>
</template>
