<script setup lang="ts">
// FR-041a §4.5, mock 3g and 4a/4b: ONE phone delivery as a row, shared by the two lists that show
// deliveries — a channel's last 50 (Settings, ChannelDeliveriesPanel) and the incident's Phone panel
// (IncidentPhonePanel) — so a delivery reads the same in both.
//
// The row draws what both lists have in common: the time column, the reason chip that leads the
// first line, the second line and the why sentence. The rest of the first line differs per list
// (a channel's list names the alert and the incident; the incident's names the channel and the
// kind), so the parent puts it in the default slot, after the chip.
//
// Every word comes from lib/phone.ts:
// - the reason chip and its hue (`deliveryReasonLabel`, `deliveryReasonTone`);
// - `deliveryDetail`: HTTP status, provider code, attempts — never "timeout" or "retrying"; a storm
//   limit named from `notes`, never its value; the recipient's name for a not_deliverable cause;
// - `deliveryNotes`: the link notes with their words; an unknown note as its raw chip alone (P7).
// The time is the viewer's local clock with the offset in force at the instant (NFR-025, D-0235);
// its second line is the calendar day (`when: "day"`, mock 3g) or the age (`when: "ago"`, mock 4a,
// as the incident's Timeline above it prints its times).
import { computed } from "vue";
import { CHIP_BASE } from "@/lib/gate";
import { relTime } from "@/lib/incident";
import {
  type PhoneDelivery,
  DELIVERY_TONE_CLASS,
  deliveryDetail,
  deliveryNotes,
  deliveryReasonLabel,
  deliveryReasonTone,
} from "@/lib/phone";
import { instantClockDayLabel } from "@/lib/wallclock";

const props = withDefaults(defineProps<{ delivery: PhoneDelivery; when?: "day" | "ago" }>(), { when: "day" });

const at = computed(() => instantClockDayLabel(props.delivery.created_at));
const detail = computed(() => deliveryDetail(props.delivery));
const notes = computed(() => deliveryNotes(props.delivery));
const chipClass = computed(() => [
  CHIP_BASE,
  "rounded-full font-medium",
  DELIVERY_TONE_CLASS[deliveryReasonTone(props.delivery.reason)],
]);
const NOTE_CHIP = "inline-flex items-center whitespace-nowrap rounded-[4px] border border-border px-[6px] font-mono text-[10.5px] text-ink-3";
</script>

<template>
  <div
    class="grid grid-cols-[96px_1fr] gap-x-[14px] gap-y-1 border-b border-border px-4 py-3 last:border-b-0 max-[560px]:grid-cols-1"
    :data-id="delivery.id"
    data-testid="delivery-row"
  >
    <div class="pt-[2px] font-mono text-[11.5px] text-ink-3">
      <b class="block font-medium text-ink-2" data-testid="delivery-clock">{{ at.clock }}</b>
      <span v-if="when === 'ago'" class="block" data-testid="delivery-ago">{{ relTime(delivery.created_at) }}</span>
      <span v-else class="block" data-testid="delivery-day">{{ at.day }}</span>
      <span class="block text-[10.5px]" data-testid="delivery-offset">{{ at.offset }}</span>
    </div>
    <div class="min-w-0">
      <div class="flex flex-wrap items-center gap-x-[10px] gap-y-[6px] text-[13px]">
        <span :class="chipClass" data-testid="delivery-reason"><i class="h-[7px] w-[7px] flex-none rounded-full bg-current" aria-hidden="true"></i>{{ deliveryReasonLabel(delivery.reason) }}</span>
        <slot />
      </div>
      <div class="mt-[5px] flex flex-wrap items-center gap-x-[10px] gap-y-[6px] text-[12px] text-ink-3" data-testid="delivery-detail">
        <template v-for="(p, i) in detail.parts" :key="`p${i}`">
          <span v-if="p.style === 'note'" :class="NOTE_CHIP" data-testid="delivery-note">{{ p.text }}</span>
          <span v-else :class="p.style === 'mono' ? 'font-mono text-[11.5px] text-ink-2' : ''">{{ p.text }}</span>
        </template>
        <template v-for="(n, i) in notes" :key="`n${i}`">
          <span :class="NOTE_CHIP" data-testid="delivery-note">{{ n.code }}</span>
          <span v-if="n.label" data-testid="delivery-note-label">{{ n.label }}</span>
        </template>
      </div>
      <p v-if="detail.why" class="mt-1 text-[12px] leading-[1.5] text-ink-3" data-testid="delivery-why">{{ detail.why }}</p>
    </div>
  </div>
</template>
