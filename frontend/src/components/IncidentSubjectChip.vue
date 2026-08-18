<script setup lang="ts">
// The incident's SUBJECT, as a chip (FR-022; mock `docs/design/mock-service-incidents.html`, panels 1–2).
//
// A chip rather than a second column or a title prefix: a column reshapes every existing row for the
// sake of a field most rows do not have, and a prefix buries the discriminator inside the text people
// search. An incident anchored to NEITHER — a manual project-level record — renders NOTHING, which is
// what makes this a discriminator rather than decoration.
//
// The name is passed in, never fetched here: a chip that loads its own label turns a list of N rows
// into N requests. Callers resolve the names ONCE per screen; without one the chip still states the
// KIND, because "which sort of thing is this" is the question it exists to answer and it must not
// wait on a second round trip to answer it.
const props = defineProps<{
  serviceId?: string;
  serviceSlug?: string;
  monitorId?: string;
  monitorName?: string;
}>();

const kind = () => (props.serviceId ? "service" : props.monitorId ? "monitor" : "");
const name = () => (props.serviceId ? props.serviceSlug : props.monitorName) || "";
</script>

<template>
  <RouterLink
    v-if="serviceId"
    :to="{ name: 'service', params: { id: serviceId } }"
    class="inline-flex items-center gap-[5px] rounded-full border border-border bg-surface-2 px-[8px] py-[2px] text-[11.5px] hover:border-border-strong"
    data-testid="incident-subject"
    :title="'This incident is an incident OF the service ' + (serviceSlug || serviceId)"
  >
    <span class="text-[10.5px] font-semibold uppercase tracking-[0.05em] text-ink-3">{{ kind() }}</span>
    <span v-if="name()" class="font-medium">· {{ name() }}</span>
  </RouterLink>
  <RouterLink
    v-else-if="monitorId"
    :to="{ name: 'monitor', params: { id: monitorId } }"
    class="inline-flex items-center gap-[5px] rounded-full border border-border bg-surface-2 px-[8px] py-[2px] text-[11.5px] hover:border-border-strong"
    data-testid="incident-subject"
    :title="'This incident is an incident OF the monitor ' + (monitorName || monitorId)"
  >
    <span class="text-[10.5px] font-semibold uppercase tracking-[0.05em] text-ink-3">{{ kind() }}</span>
    <span v-if="name()" class="font-medium">· {{ name() }}</span>
  </RouterLink>
</template>
