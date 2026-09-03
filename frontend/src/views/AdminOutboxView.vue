<script setup lang="ts">
import { onMounted, ref } from "vue";
import { api } from "@/api/client";
import type { components } from "@/api/schema";
import AppShell from "@/components/AppShell.vue";
import { instantLabelShort } from "@/lib/wallclock";

type OutboxEvent = components["schemas"]["OutboxEvent"];

const events = ref<OutboxEvent[]>([]);
const loading = ref(true);
const error = ref("");
const busy = ref<Record<string, boolean>>({});
const replayingAll = ref(false);
const expanded = ref<Record<string, boolean>>({});

async function load() {
  loading.value = true;
  error.value = "";
  const res = await api.GET("/api/v1/admin/outbox/dead", { params: { query: { limit: 200 } } });
  if (res.error) {
    error.value = "You need global-admin rights to view the dead-letter queue.";
    events.value = [];
  } else {
    events.value = res.data ?? [];
  }
  loading.value = false;
}

async function replay(id: string) {
  busy.value[id] = true;
  try {
    const res = await api.POST("/api/v1/admin/outbox/dead/{eventID}/replay", { params: { path: { eventID: id } } });
    if (!res.error) events.value = events.value.filter((e) => e.id !== id);
  } finally {
    busy.value[id] = false;
  }
}

async function replayAll() {
  if (!events.value.length || replayingAll.value) return;
  replayingAll.value = true;
  try {
    const res = await api.POST("/api/v1/admin/outbox/dead/replay-all");
    if (!res.error) await load();
  } finally {
    replayingAll.value = false;
  }
}

// NFR-025b: an outbox row's time names its zone. Correlating a delivery with a provider's log is
// the whole use of this screen, and it was rendering a local time silently.
const fmtDate = (s?: string) => instantLabelShort(s);
function prettyPayload(p: unknown): string {
  try {
    return JSON.stringify(p, null, 2);
  } catch {
    return String(p);
  }
}

onMounted(load);
</script>

<template>
  <AppShell active="admin-outbox" :crumbs="['cerbix', 'Admin', 'Dead-letter queue']">
    <div class="mx-auto max-w-[1180px] px-[22px] pb-16 pt-[26px]">
      <div class="mb-5 flex items-end justify-between gap-4">
        <div>
          <h1 class="text-[21px] font-semibold tracking-tight">Dead-letter queue</h1>
          <p class="mt-[3px] text-[13px] text-ink-3">Outbound webhook and notification deliveries that exhausted their retries. Replay requeues them for delivery.</p>
        </div>
        <button
          v-if="events.length"
          type="button"
          :disabled="replayingAll"
          class="h-[34px] shrink-0 rounded-sm bg-accent px-4 text-[13px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-50"
          @click="replayAll"
        >{{ replayingAll ? "Replaying…" : `Replay all (${events.length})` }}</button>
      </div>

      <div v-if="error" class="rounded border border-border bg-surface p-4 text-[13px] text-ink-3 shadow-card">{{ error }}</div>

      <section v-else class="overflow-hidden rounded border border-border bg-surface shadow-card">
        <table class="w-full text-[13px]">
          <thead>
            <tr class="text-[10.5px] uppercase tracking-[0.06em] text-ink-3">
              <th class="border-b border-border px-4 py-[10px] text-left">Topic</th>
              <th class="border-b border-border px-4 py-[10px] text-right">Attempts</th>
              <th class="border-b border-border px-4 py-[10px] text-left">Last error</th>
              <th class="border-b border-border px-4 py-[10px] text-left">Failed at</th>
              <th class="border-b border-border px-4 py-[10px]"></th>
            </tr>
          </thead>
          <tbody>
            <template v-for="e in events" :key="e.id">
              <tr class="hover:bg-surface-2">
                <td class="border-b border-border px-4 py-[11px]">
                  <button type="button" class="font-mono text-[12.5px] font-medium text-ink hover:text-accent" :title="'Show payload'" @click="expanded[e.id!] = !expanded[e.id!]">{{ e.topic }}</button>
                </td>
                <td class="border-b border-border px-4 py-[11px] text-right font-mono tnum text-ink-2">{{ e.attempts }}</td>
                <td class="max-w-[420px] truncate border-b border-border px-4 py-[11px] font-mono text-[12px] text-down" :title="e.last_error">{{ e.last_error || "—" }}</td>
                <td class="border-b border-border px-4 py-[11px] text-ink-3">{{ fmtDate(e.updated_at) }}</td>
                <td class="border-b border-border px-4 py-[11px] text-right">
                  <button type="button" :disabled="busy[e.id!]" class="text-[12.5px] text-accent hover:underline disabled:opacity-50" @click="replay(e.id!)">{{ busy[e.id!] ? "…" : "Replay" }}</button>
                </td>
              </tr>
              <tr v-if="expanded[e.id!]">
                <td colspan="5" class="border-b border-border bg-surface-2 px-4 py-3">
                  <pre class="overflow-x-auto rounded-sm border border-border bg-surface p-3 font-mono text-[11.5px] text-ink-2">{{ prettyPayload(e.payload) }}</pre>
                </td>
              </tr>
            </template>
            <tr v-if="!events.length && !loading">
              <td colspan="5" class="px-4 py-12 text-center text-[13px] text-ink-3">Nothing dead-lettered — all outbound deliveries are healthy.</td>
            </tr>
          </tbody>
        </table>
      </section>
    </div>
  </AppShell>
</template>
