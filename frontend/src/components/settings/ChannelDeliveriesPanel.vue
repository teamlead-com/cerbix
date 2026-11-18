<script setup lang="ts">
// FR-041a §4.5, mock 3g: the last 50 phone deliveries of one channel, opened from its row in
// Settings → Notification channels and drawn under the table (mock decision 15). Readable by every
// project reader (`project:read`), so a viewer opens it too.
//
// Rules that shape it:
// - The rows are the API's, in the API's order — NEWEST first — and never re-sorted.
// - The row itself is PhoneDeliveryRow, shared with the incident's Phone panel; this list adds the
//   alert, the incident's ref and the mask after the reason chip.
// - Every word comes from lib/phone.ts, so a delivery reads the same here and on the incident:
//   the reason chip and its tone, `alertSummary` (subject, state and step; ONE label for a monitor's
//   recovery), `deliveryDetail` (HTTP status, provider code, attempts — never "timeout" or
//   "retrying"; a storm limit named from `notes`, never its value), `deliveryNotes` (an unknown note
//   is shown as its raw code alone, never dropped).
// - Masked numbers only: the row prints the server's `number_masked`, nothing else.
// - A failed read is not "nothing was sent": it says so in the dashed grammar of mock 4d.
// - The time is the viewer's local clock with the offset in force at the instant (NFR-025, D-0235).
import { computed, onMounted, ref } from "vue";
import { RouterLink } from "vue-router";
import { api } from "@/api/client";
import PhoneDeliveryRow from "@/components/PhoneDeliveryRow.vue";
import { CHIP_BASE } from "@/lib/gate";
import { type PhoneDelivery, alertSummary } from "@/lib/phone";

const props = defineProps<{ channelId: string; channelName: string }>();
const emit = defineEmits<{ close: [] }>();

const deliveries = ref<PhoneDelivery[]>([]);
const state = ref<"loading" | "ready" | "failed">("loading");

async function load() {
  state.value = "loading";
  try {
    const res = await api.GET("/api/v1/notification-channels/{channelID}/phone-deliveries", {
      params: { path: { channelID: props.channelId } },
    });
    if (res.error || !res.data) {
      state.value = "failed";
      return;
    }
    deliveries.value = res.data.deliveries ?? [];
    state.value = "ready";
  } catch {
    state.value = "failed";
  }
}
onMounted(load);

/** What each row prints beside the shared row's chip, worked out once per read. */
const rows = computed(() => deliveries.value.map((d) => ({ d, summary: alertSummary(d) })));
</script>

<template>
  <section class="rounded border border-border bg-surface shadow-card" data-testid="channel-deliveries">
    <header class="flex flex-wrap items-center gap-[10px] border-b border-border px-4 py-[13px]">
      <h3 class="text-[13px] font-semibold" data-testid="deliveries-title">Deliveries · {{ channelName }}</h3>
      <span :class="[CHIP_BASE, 'border-border bg-surface text-ink-2']" data-testid="deliveries-limit">last 50</span>
      <button type="button" class="ml-auto text-[12.5px] text-ink-3 hover:text-ink hover:underline" data-testid="deliveries-close" @click="emit('close')">✕ Close</button>
    </header>

    <p v-if="state === 'loading'" class="px-4 py-[22px] text-[13px] text-ink-3" data-testid="deliveries-loading">Loading…</p>
    <span
      v-else-if="state === 'failed'"
      class="m-4 inline-flex items-center rounded-[6px] border border-dashed border-border-strong px-[9px] py-1 text-[12.5px] text-ink-3"
      data-testid="deliveries-unavailable"
    >Deliveries could not be read — this is not “nothing was sent”.</span>
    <p v-else-if="deliveries.length === 0" class="px-4 py-[22px] text-[13px] text-ink-3" data-testid="deliveries-empty">No alert has been sent through this channel yet.</p>
    <template v-else>
      <PhoneDeliveryRow v-for="r in rows" :key="r.d.id" :delivery="r.d">
        <span data-testid="delivery-alert">{{ r.summary }}</span>
        <RouterLink
          v-if="r.d.incident_id && r.d.incident_ref"
          :to="{ name: 'incident', params: { id: r.d.incident_id } }"
          class="font-mono text-[12px] text-accent hover:underline"
          data-testid="delivery-ref"
        >{{ r.d.incident_ref }}</RouterLink>
        <span v-else class="text-ink-3" data-testid="delivery-no-incident">no incident</span>
        <span v-if="r.d.number_masked" class="font-mono text-[12px] text-ink-2" data-testid="delivery-mask">{{ r.d.number_masked }}</span>
      </PhoneDeliveryRow>
      <p class="px-4 py-3 text-[13px] text-ink-3" data-testid="deliveries-footnote">Older deliveries are kept 90 days and are not listed here.</p>
    </template>
  </section>
</template>
