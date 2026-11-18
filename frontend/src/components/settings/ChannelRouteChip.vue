<script setup lang="ts">
// FR-041a §4.3, D31, mock 3e / 3e′ / 3e‴: the chip under a phone channel's name in the channel list.
// The words and the kind come from `routeChip` (lib/phone.ts); this only draws them, as the mock's
// note says: a disabled reason is a SOLID chip (the row's switch is locked), "Not a route" a DASHED
// one (a channel can be enabled and not a route). `deliverable.reason: disabled` has no chip.
// On the reader's own channel with no verified number, "add one" asks for Account → Phone (3e‴).
import { computed } from "vue";
import type { components } from "@/api/schema";
import { CHIP_BASE } from "@/lib/gate";
import { routeChip } from "@/lib/phone";

const props = defineProps<{
  channel: components["schemas"]["NotificationChannel"];
  selfUserId: string | null | undefined;
}>();
const emit = defineEmits<{ "add-number": [] }>();

const ADD_ONE = "add one";

const chip = computed(() => routeChip(props.channel, { selfUserId: props.selfUserId }));
/** The bold head ("Disabled", "Not a route") and the reason after " · ". */
const parts = computed(() => {
  const c = chip.value;
  if (c.kind === "none") return null;
  const at = c.text.indexOf(" · ");
  const head = at < 0 ? c.text : c.text.slice(0, at);
  let rest = at < 0 ? "" : c.text.slice(at + 3);
  // Only the reader's own channel ends in "— add one" (routeChip); the words become a link.
  const addOne = c.kind === "notRoute" && rest.endsWith(` — ${ADD_ONE}`);
  if (addOne) rest = rest.slice(0, -ADD_ONE.length);
  return { head, rest, addOne };
});
</script>

<template>
  <span
    v-if="parts"
    :class="[
      CHIP_BASE,
      'mt-[5px] max-w-full flex-wrap font-normal',
      chip.kind === 'disabled' ? 'border-border bg-inset text-ink-2' : 'border-dashed border-border-strong bg-surface text-ink-2',
    ]"
    :data-kind="chip.kind"
    data-testid="channel-route-chip"
  >
    <b class="font-semibold" :class="chip.kind === 'disabled' ? 'text-ink' : 'text-degraded'">{{ parts.head }}</b>
    <template v-if="parts.rest"> · {{ parts.rest }}</template>
    <a
      v-if="parts.addOne"
      href="#"
      class="text-accent hover:underline"
      data-testid="channel-route-add-number"
      @click.prevent="emit('add-number')"
    >{{ ADD_ONE }}</a>
  </span>
</template>
