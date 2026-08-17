<script setup lang="ts">
import { computed, onMounted, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";

import { api } from "@/api/client";
import type { components } from "@/api/schema";
import AppShell from "@/components/AppShell.vue";
import { useSession } from "@/stores/session";
import { useWorkspace } from "@/stores/workspace";
import ServiceReliability from "@/components/ServiceReliability.vue";
import { humanDuration, lagExact, sealedLabel } from "@/lib/services";

type Detail = components["schemas"]["ServiceDetail"];
type Monitor = components["schemas"]["Monitor"];

const ws = useWorkspace();
const session = useSession();
const route = useRoute();
const router = useRouter();

const loading = ref(true);
const error = ref("");
const detail = ref<Detail | null>(null);
const monitors = ref<Monitor[]>([]);

const serviceId = computed(() => String(route.params.id || ""));
const managed = computed(() => detail.value?.service.managed_by ?? "");
const canWrite = computed(() => session.canProjectWrite(ws.orgId, ws.projectId) && !managed.value);

async function load() {
  loading.value = true;
  error.value = "";
  try {
    await ws.init();
    if (!ws.projectId || !serviceId.value) return;
    const [svc, mons] = await Promise.all([
      api.GET("/api/v1/projects/{projectID}/services/{serviceID}", {
        params: { path: { projectID: ws.projectId, serviceID: serviceId.value } },
      }),
      api.GET("/api/v1/projects/{projectID}/monitors", {
        params: { path: { projectID: ws.projectId } },
      }),
    ]);
    if (svc.error || !svc.data) {
      error.value = "This service does not exist, or you cannot see it.";
      return;
    }
    detail.value = svc.data;
    monitors.value = mons.data ?? [];
  } catch {
    error.value = "Could not load the service.";
  } finally {
    loading.value = false;
  }
}

// The declaration stores monitor ids; a reader needs the slug, which is what a bundle would
// name. A member whose monitor is gone still renders — the revision snapshotted the name so
// history stays legible.
const byId = computed(() => new Map(monitors.value.map((m) => [m.id!, m])));
function label(id: string): string {
  return byId.value.get(id)?.slug || byId.value.get(id)?.name || id;
}
function detailOf(id: string): string {
  const m = byId.value.get(id);
  if (!m) return "no longer exists";
  const bits = [m.type ?? "", m.region || "core"];
  if (m.type !== "push") bits.push(`${m.interval_seconds}s`);
  return bits.filter(Boolean).join(" · ");
}

const sli = computed(() => new Set(detail.value?.declaration?.sli ?? []));
const contextOnly = computed(() =>
  (detail.value?.declaration?.monitors ?? []).filter((id) => !sli.value.has(id)),
);

const policies = computed(() => detail.value?.declaration?.policies);

const deleting = ref(false);
const deleteError = ref("");
async function remove() {
  if (!confirm("Delete this service and everything derived from it? Facts, ranges and the declaration history go with it.")) return;
  deleting.value = true;
  deleteError.value = "";
  const res = await api.DELETE("/api/v1/projects/{projectID}/services/{serviceID}", {
    params: { path: { projectID: ws.projectId, serviceID: serviceId.value } },
  });
  deleting.value = false;
  if (res.error) {
    deleteError.value = (res.error as { error?: string })?.error || "Could not delete the service.";
    return;
  }
  await router.push({ name: "services" });
}

onMounted(load);
watch(() => [route.params.id, ws.projectId], load);
</script>

<template>
  <AppShell active="services" :crumbs="[ws.orgName || 'cerbix', ws.projectName || '…', 'Services', detail?.service.name || '…']">
    <template #actions>
      <RouterLink
        v-if="canWrite"
        :to="{ name: 'service-declaration', params: { id: serviceId } }"
        class="flex h-[34px] items-center rounded-sm border border-border px-[13px] text-[13px] text-ink-2 hover:border-border-strong hover:text-ink"
        data-testid="service-edit-declaration"
      >
        Edit declaration
      </RouterLink>
      <button
        v-if="canWrite"
        type="button"
        :disabled="deleting"
        class="flex h-[34px] items-center rounded-sm border border-border px-[13px] text-[13px] text-down hover:border-down/50 disabled:opacity-60"
        @click="remove"
      >
        Delete
      </button>
    </template>

    <div class="mx-auto max-w-[1180px] px-[22px] pb-16 pt-[26px]">
      <div v-if="error" class="rounded border border-down/40 bg-down-weak p-4 text-[13px] text-down">{{ error }}</div>
      <p v-else-if="loading" class="text-[13px] text-ink-3">Loading…</p>

      <template v-else-if="detail">
        <div class="mb-[22px]">
          <div class="flex flex-wrap items-center gap-[9px]">
            <h1 class="text-[21px] font-semibold tracking-tight">{{ detail.service.name }}</h1>
            <span v-if="detail.declaration" class="rounded-xs border border-border px-[7px] py-px font-mono text-[11px] text-ink-3">rev {{ detail.declaration.revision }}</span>
            <span v-if="detail.epoch" class="rounded-xs border border-border px-[7px] py-px font-mono text-[11px] text-ink-3">epoch {{ detail.epoch.seq }}</span>
            <span v-if="managed" class="inline-flex items-center gap-[4px] rounded-full bg-inset px-[8px] py-[2px] font-mono text-[10.5px] text-ink-2">
              <svg viewBox="0 0 24 24" class="h-[11px] w-[11px]" fill="none" stroke="currentColor" stroke-width="2"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><path d="M14 2v6h6"/></svg>
              {{ managed }}
            </span>
          </div>
          <p class="mt-[3px] font-mono text-[12.5px] text-ink-3">{{ detail.service.slug }}</p>
          <p v-if="detail.service.description" class="mt-[6px] text-[13px] text-ink-2">{{ detail.service.description }}</p>
        </div>

        <p v-if="deleteError" class="mb-3 rounded border border-down/40 bg-down-weak p-3 text-[13px] text-down">{{ deleteError }}</p>

        <p v-if="managed" class="mb-4 rounded border border-border bg-surface-2 p-3 text-[12.5px] text-ink-2">
          Owned by the file provider <span class="font-mono">{{ managed }}</span>. Its declaration is edited in the
          source file — a change made here would be restated by the very next reconcile.
        </p>

        <!-- Phase 2 (iter-0144): the reporting surface — live health, the window report
             with every honesty state, the timeline, segments, the objective editor. -->
        <ServiceReliability
          :project-id="ws.projectId"
          :service-id="serviceId"
          :can-write="canWrite"
          :has-sli="sli.size > 0"
        />

        <!-- Declaration: two lists, side by side, separated by the rule that is the whole
             point of the model. Adding a monitor on the left never adds it on the right. -->
        <section class="mb-4 overflow-hidden rounded border border-border bg-surface shadow-card">
          <header class="flex items-center gap-2 border-b border-border px-4 py-[10px]">
            <h2 class="text-[13.5px] font-semibold">Declaration</h2>
            <div class="flex-1"></div>
            <span v-if="detail.declaration" class="font-mono text-[11.5px] text-ink-3">
              effective {{ sealedLabel(detail.declaration.effective_at) }}
              <template v-if="detail.declaration.created_by"> · by {{ detail.declaration.created_by }}</template>
            </span>
          </header>

          <div v-if="!detail.declaration" class="px-4 py-8 text-center text-[13px] text-ink-3" data-testid="service-no-declaration">
            Nothing declared yet. This service reports <b>no availability</b> — not 100%.
            <RouterLink v-if="canWrite" :to="{ name: 'service-declaration', params: { id: serviceId } }" class="text-accent hover:underline">
              Declare its reliability inputs.
            </RouterLink>
          </div>

          <div v-else class="grid grid-cols-2 gap-0 max-[760px]:grid-cols-1">
            <div class="border-r border-border p-4 max-[760px]:border-b max-[760px]:border-r-0">
              <div class="text-[11px] font-semibold uppercase tracking-[0.06em] text-ink-3">
                Operational context
                <span class="font-normal normal-case tracking-normal text-ink-3"> — what is shown on the service</span>
              </div>
              <ul class="mt-3 flex flex-col gap-[6px]">
                <li v-for="id in detail.declaration.monitors" :key="id" class="flex items-center gap-2 text-[13px]">
                  <span class="font-mono">{{ label(id) }}</span>
                  <span class="ml-auto text-[11.5px] text-ink-3">{{ detailOf(id) }}</span>
                </li>
                <li v-if="!detail.declaration.monitors.length" class="text-[12.5px] text-ink-3">No monitors.</li>
              </ul>
            </div>

            <div class="p-4">
              <div class="text-[11px] font-semibold uppercase tracking-[0.06em] text-ink-3">
                Reliability inputs
                <span class="font-normal normal-case tracking-normal text-ink-3"> — what counts toward availability</span>
              </div>
              <ul class="mt-3 flex flex-col gap-[6px]">
                <li v-for="id in detail.declaration.sli" :key="id" class="flex items-center gap-2 text-[13px]" data-testid="service-sli-member">
                  <span class="font-mono">{{ label(id) }}</span>
                  <span class="ml-auto rounded-full bg-accent-weak px-[8px] py-px text-[11px] font-medium text-accent">counts</span>
                </li>
                <li v-for="id in contextOnly" :key="'d-' + id" class="flex items-center gap-2 text-[13px] opacity-55" data-testid="service-diagnostic-member">
                  <span class="font-mono">{{ label(id) }}</span>
                  <span class="ml-auto text-[11.5px] text-ink-3">diagnostic only</span>
                </li>
                <li v-if="!detail.declaration.sli.length" class="text-[12.5px] text-ink-3">
                  No reliability inputs. Operational context, no SLO — availability is unavailable, never 100%.
                </li>
              </ul>
            </div>
          </div>
        </section>

        <!-- Policies -->
        <section v-if="policies" class="mb-4 overflow-hidden rounded border border-border bg-surface shadow-card">
          <header class="border-b border-border px-4 py-[10px]"><h2 class="text-[13.5px] font-semibold">Evaluation policy</h2></header>
          <dl class="grid grid-cols-2 gap-x-6 gap-y-3 p-4 text-[13px] max-[760px]:grid-cols-1">
            <div>
              <dt class="text-[11px] font-semibold uppercase tracking-[0.06em] text-ink-3">Aggregation</dt>
              <dd class="mt-[3px] font-mono text-[12.5px]">
                {{ policies.aggregation?.mode }}
                <template v-if="policies.aggregation?.mode === 'quorum'">
                  · degraded_min {{ policies.aggregation?.degraded_min }} · healthy_min {{ policies.aggregation?.healthy_min }}
                </template>
              </dd>
            </div>
            <div>
              <dt class="text-[11px] font-semibold uppercase tracking-[0.06em] text-ink-3">Region</dt>
              <dd class="mt-[3px] font-mono text-[12.5px]">
                {{ policies.region?.mode }} · degraded_min_regions {{ policies.region?.degraded_min_regions }} ·
                healthy_min_regions {{ policies.region?.healthy_min_regions }}
              </dd>
            </div>
            <div>
              <dt class="text-[11px] font-semibold uppercase tracking-[0.06em] text-ink-3">Missing data</dt>
              <dd class="mt-[3px] font-mono text-[12.5px]">{{ policies.missing_data }}</dd>
              <p class="mt-[3px] text-[11.5px] text-ink-3">
                What an unknown member contributes. Under <span class="font-mono">unknown</span> the bucket may become
                undecidable and leaves the window entirely — never counted as good.
              </p>
            </div>
            <div>
              <dt class="text-[11px] font-semibold uppercase tracking-[0.06em] text-ink-3">Maintenance · freshness</dt>
              <dd class="mt-[3px] font-mono text-[12.5px]">
                {{ policies.maintenance }} · ×{{ policies.freshness?.active_multiplier }} floor
                {{ policies.freshness?.active_floor }}
              </dd>
            </div>
          </dl>
        </section>

        <!-- Evaluation epoch: what was actually being measured. -->
        <section v-if="detail.epoch" class="mb-4 overflow-hidden rounded border border-border bg-surface shadow-card">
          <header class="flex items-center gap-2 border-b border-border px-4 py-[10px]">
            <h2 class="text-[13.5px] font-semibold">Evaluation epoch</h2>
            <div class="flex-1"></div>
            <span class="font-mono text-[11.5px] text-ink-3">{{ detail.epoch.members }} members snapshotted</span>
          </header>
          <div class="p-4 text-[13px]">
            <p class="text-ink-2">
              A fact references the epoch, not the revision — so a monitor edit that changes how a member is evaluated
              opens a new epoch, while one that cannot (a rename) leaves the hash untouched and opens none.
            </p>
            <dl class="mt-3 flex flex-wrap gap-x-8 gap-y-2 font-mono text-[12.5px]">
              <div><dt class="inline text-ink-3">seq </dt><dd class="inline">{{ detail.epoch.seq }}</dd></div>
              <div><dt class="inline text-ink-3">effective_at </dt><dd class="inline">{{ sealedLabel(detail.epoch.effective_at) }}</dd></div>
              <div><dt class="inline text-ink-3">snapshot_hash </dt><dd class="inline">{{ detail.epoch.snapshot_hash.slice(0, 12) }}…</dd></div>
            </dl>
          </div>
        </section>

        <!-- Materialization: the honesty surface. -->
        <section class="overflow-hidden rounded border border-border bg-surface shadow-card">
          <header class="border-b border-border px-4 py-[10px]"><h2 class="text-[13.5px] font-semibold">Materialization</h2></header>
          <div class="p-4 text-[13px]">
            <template v-if="detail.materialization.sealed_through">
              <div class="flex flex-wrap items-baseline gap-x-3 gap-y-1">
                <span class="text-[11px] font-semibold uppercase tracking-[0.06em] text-ink-3">Sealed through</span>
                <span class="font-mono text-[14px]">{{ sealedLabel(detail.materialization.sealed_through) }}</span>
                <span class="font-mono text-[12px] text-ink-3">{{ lagExact(detail.materialization.sealed_through) }} behind now</span>
              </div>
              <p class="mt-2 text-[12.5px] text-ink-3">
                Contiguity-defined: a hole holds this back rather than being jumped over, so a service that stopped
                materializing reads as a lagging timestamp instead of a plausible number. Everything after it is
                provisional and belongs in no window.
              </p>
            </template>
            <template v-else>
              <p class="text-[12.5px] text-ink-3">Nothing sealed yet — no window can be computed, and none is shown.</p>
            </template>

            <div v-if="detail.materialization.materialization_start" class="mt-3 font-mono text-[12.5px] text-ink-3">
              start {{ sealedLabel(detail.materialization.materialization_start) }}
            </div>
            <div v-if="detail.materialization.retracted_at" class="mt-1 font-mono text-[12.5px] text-degraded">
              retracted {{ sealedLabel(detail.materialization.retracted_at) }}
              <template v-if="detail.materialization.retracted_to">→ {{ sealedLabel(detail.materialization.retracted_to) }}</template>
            </div>

            <div v-if="detail.materialization.repairing.length" class="mt-4">
              <div class="text-[11px] font-semibold uppercase tracking-[0.06em] text-degraded">Repairing</div>
              <p class="mt-1 text-[12.5px] text-ink-3">
                These ranges are being recomputed. They are work in progress, not missing data, and not data.
              </p>
              <table class="mt-2 w-full text-[12.5px]">
                <thead>
                  <tr class="text-[10.5px] uppercase tracking-[0.06em] text-ink-3">
                    <th class="border-b border-border py-[7px] text-left">Range</th>
                    <th class="border-b border-border py-[7px] text-left">Reason</th>
                    <th class="border-b border-border py-[7px] text-left">State</th>
                    <th class="border-b border-border py-[7px] text-left">Progress</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="(rr, i) in detail.materialization.repairing" :key="i">
                    <td class="border-b border-border py-[7px] font-mono">{{ sealedLabel(rr.from) }} → {{ sealedLabel(rr.to) }}</td>
                    <td class="border-b border-border py-[7px] font-mono text-ink-2">{{ rr.reason }}</td>
                    <td class="border-b border-border py-[7px] font-mono" :class="rr.state === 'error' ? 'text-down' : 'text-ink-2'">
                      {{ rr.state }}<template v-if="rr.attempts"> · {{ rr.attempts }} attempts</template>
                    </td>
                    <td class="border-b border-border py-[7px] font-mono text-ink-3">
                      <template v-if="rr.cursor">{{ sealedLabel(rr.cursor) }}</template>
                      <template v-else>not started</template>
                      <div v-if="rr.last_error" class="text-[11.5px] text-down">{{ rr.last_error }}</div>
                    </td>
                  </tr>
                </tbody>
              </table>
            </div>
          </div>
        </section>

        <p class="mt-3 text-[12px] text-ink-3">
          Availability over a window, the error budget and the burn rate arrive with the next iteration — they read
          from sealed facts only, which is why the watermark above is shown before any of them.
          <span v-if="detail.materialization.sealed_through">
            The seal cadence itself is normal at around {{ humanDuration(3 * 60 * 1000) }}.
          </span>
        </p>
      </template>
    </div>
  </AppShell>
</template>
