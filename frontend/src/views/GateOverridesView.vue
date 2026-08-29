<script setup lang="ts">
// FR-024 D-0207 item 1, mock screen 5 ("Override history"): every override this service ever had.
//
// `status` is a FUNCTION of facts, not of which housekeeping has run (D13a): a manual revoke reads
// `revoked`; a policy edit or delete — or a revision that is no longer current — reads `inert`; an
// expiry by time or by record reads `expired`; otherwise `active`. The server computes it at read
// time and this view prints what it was given.
//
// Read-only. Revocation lives on the service card, by the override's own immutable id — a "revoke
// the current one" button here would hit a newer override from a screen that never saw it (the
// race 409 `override_not_active` exists to refuse), so this history offers no such button. The
// API returns the newest 50 by `created_at DESC, id DESC`; the audit log is the full record.
import { computed, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { RouterLink, useRoute } from "vue-router";

import { api } from "@/api/client";
import type { components } from "@/api/schema";
import AppShell from "@/components/AppShell.vue";
import {
  CHIP_BASE,
  closureLabel,
  describeFailure,
  failureOf,
  isAbort,
  overrideStatusChip,
  transportFailure,
} from "@/lib/gateLedger";
import { sealedLabel } from "@/lib/services";
import { useWorkspace } from "@/stores/workspace";

type Detail = components["schemas"]["ServiceDetail"];
type OverrideRecord = components["schemas"]["GateOverrideRecord"];

const ws = useWorkspace();
const route = useRoute();

const serviceId = computed(() => String(route.params.id || ""));
const service = ref<Detail | null>(null);
const items = ref<OverrideRecord[]>([]);
const loading = ref(true);
const error = ref("");
const errorStatus = ref<number | null>(null);

let generation = 0;
let inflight: AbortController | undefined;
function begin(): { controller: AbortController; mine: number } {
  inflight?.abort();
  const controller = new AbortController();
  inflight = controller;
  return { controller, mine: ++generation };
}
const stale = (mine: number) => mine !== generation;
onBeforeUnmount(() => {
  generation++;
  inflight?.abort();
});

const SERVICE_GONE = "This service does not exist, or you cannot see it.";

async function load() {
  const { controller, mine } = begin();
  loading.value = true;
  error.value = "";
  errorStatus.value = null;
  service.value = null;
  items.value = [];
  try {
    await ws.init();
    if (stale(mine)) return;
    if (!ws.projectId || !serviceId.value) return;
    const path = { projectID: ws.projectId, serviceID: serviceId.value };
    const [svc, ovr] = await Promise.all([
      api.GET("/api/v1/projects/{projectID}/services/{serviceID}", { params: { path }, signal: controller.signal }),
      api.GET("/api/v1/projects/{projectID}/services/{serviceID}/gate/overrides", { params: { path }, signal: controller.signal }),
    ]);
    if (stale(mine)) return;
    if (svc.error || !svc.data) {
      // The service itself is the subject; without it the history has no header to sit under.
      const f = failureOf(svc);
      error.value = describeFailure(f, { notFound: SERVICE_GONE, denied: "You cannot see this service." });
      errorStatus.value = f.status;
      return;
    }
    service.value = svc.data;
    if (ovr.error || !ovr.data) {
      const f = failureOf(ovr);
      error.value = describeFailure(f, { notFound: SERVICE_GONE, denied: "You cannot see this service's overrides." });
      errorStatus.value = f.status;
      return;
    }
    items.value = ovr.data.items ?? [];
  } catch (e) {
    if (stale(mine) || isAbort(e)) return;
    error.value = transportFailure(e);
    errorStatus.value = 0;
  } finally {
    if (!stale(mine)) loading.value = false;
  }
}

onMounted(load);
watch(() => [route.params.id, ws.projectId], load);

const name = computed(() => service.value?.service.name || "");
</script>

<template>
  <AppShell active="services" :crumbs="[ws.orgName || 'cerbix', ws.projectName || '…', 'Services', name || '…', 'Override history']">
    <div class="mx-auto max-w-[1180px] px-[22px] pb-16 pt-[26px]" data-testid="gate-overrides">
      <RouterLink :to="{ name: 'service', params: { id: serviceId } }" class="text-[12.5px] text-ink-3 hover:text-accent" data-testid="gate-overrides-back">← back to the service</RouterLink>

      <div class="mb-[22px] mt-[10px]">
        <h1 class="text-[21px] font-semibold tracking-tight">{{ name || "…" }} · override history</h1>
        <p class="mt-[3px] text-[13px] text-ink-3">
          Every override this service ever had, newest first ·
          <span class="font-mono">GET …/services/{s}/gate/overrides</span>
        </p>
      </div>

      <div v-if="error" class="rounded border border-down/40 bg-down-weak p-4 text-[13px] text-down" data-testid="gate-overrides-error" :data-status="errorStatus ?? undefined">{{ error }}</div>
      <p v-else-if="loading" class="text-[13px] text-ink-3">Loading…</p>

      <section v-else class="overflow-hidden rounded border border-border bg-surface shadow-card">
        <header class="flex flex-wrap items-center gap-[10px] border-b border-border px-4 py-[13px]">
          <h2 class="text-[13.5px] font-semibold">Overrides</h2>
          <span class="ml-auto text-[12px] text-ink-3">newest 50 · the audit log is the full record</span>
        </header>

        <div v-if="items.length" class="overflow-x-auto">
          <table class="w-full text-[13px]" data-testid="gate-overrides-table">
            <thead>
              <tr class="text-[10.5px] uppercase tracking-[0.06em] text-ink-3">
                <th class="border-b border-border px-4 py-[10px] text-left">Status</th>
                <th class="whitespace-nowrap border-b border-border px-3 py-[10px] text-left">Created</th>
                <th class="border-b border-border px-3 py-[10px] text-left">By</th>
                <th class="border-b border-border px-3 py-[10px] text-left">Reason</th>
                <th class="whitespace-nowrap border-b border-border px-3 py-[10px] text-left">Until</th>
                <th class="border-b border-border px-3 py-[10px] text-left">Closed</th>
                <th class="border-b border-border px-3 py-[10px] text-left">Policy</th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="o in items"
                :key="o.id"
                class="hover:bg-surface-2"
                data-testid="gate-override-row"
                :data-id="o.id"
                :data-status="o.status"
              >
                <td class="border-b border-border px-4 py-[9px] align-top">
                  <span :class="[CHIP_BASE, overrideStatusChip(o.status)]" :title="o.id">{{ o.status }}</span>
                </td>
                <td class="whitespace-nowrap border-b border-border px-3 py-[9px] align-top font-mono text-[12.5px] tnum">{{ sealedLabel(o.created_at) }}</td>
                <td class="border-b border-border px-3 py-[9px] align-top font-mono text-[12.5px]" :title="o.via_token ? 'an API token' : 'a user'">{{ o.actor_label }}</td>
                <td class="border-b border-border px-3 py-[9px] align-top text-ink-2">{{ o.reason }}</td>
                <td class="whitespace-nowrap border-b border-border px-3 py-[9px] align-top font-mono text-[12.5px] tnum">{{ sealedLabel(o.expires_at) }}</td>
                <td class="border-b border-border px-3 py-[9px] align-top font-mono text-[12.5px] tnum" :class="o.revoked_at ? '' : 'text-ink-3'">{{ closureLabel(o) || "—" }}</td>
                <td class="border-b border-border px-3 py-[9px] align-top font-mono text-[12.5px]">rev {{ o.policy_revision }}</td>
              </tr>
            </tbody>
          </table>
        </div>

        <p v-else class="px-4 py-10 text-center text-[13px] text-ink-3" data-testid="gate-overrides-empty">
          No override has ever been added for this service.
        </p>
      </section>
    </div>
  </AppShell>
</template>
