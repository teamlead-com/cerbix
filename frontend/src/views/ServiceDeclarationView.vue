<script setup lang="ts">
import { computed, onMounted, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";

import { api } from "@/api/client";
import type { components } from "@/api/schema";
import AppShell from "@/components/AppShell.vue";
import { useWorkspace } from "@/stores/workspace";
import { sealedLabel } from "@/lib/services";

type Detail = components["schemas"]["ServiceDetail"];
type Monitor = components["schemas"]["Monitor"];
type Policies = components["schemas"]["ServicePolicies"];

const ws = useWorkspace();
const route = useRoute();
const router = useRouter();

const loading = ref(true);
const error = ref("");
const detail = ref<Detail | null>(null);
const monitors = ref<Monitor[]>([]);

const serviceId = computed(() => String(route.params.id || ""));

// The two lists are held separately in the editor for the same reason they are stored
// separately: selecting a monitor for context must never be able to select it as an input.
const context = ref<Set<string>>(new Set());
const sli = ref<Set<string>>(new Set());
const policies = ref<Policies>({});
const expectedRevision = ref(0);
const backfillFrom = ref("");

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
    monitors.value = (mons.data ?? []).slice().sort((a, b) => (a.slug ?? "").localeCompare(b.slug ?? ""));

    const d = svc.data.declaration;
    context.value = new Set(d?.monitors ?? []);
    sli.value = new Set(d?.sli ?? []);
    policies.value = d?.policies ?? {};
    expectedRevision.value = d?.revision ?? 0;
  } catch {
    error.value = "Could not load the service.";
  } finally {
    loading.value = false;
  }
}

function toggleContext(id: string) {
  const next = new Set(context.value);
  if (next.has(id)) {
    next.delete(id);
    // Dropping a monitor from the context necessarily drops it as an input — an SLI member
    // outside the context is a number with no visible source, which the API refuses anyway.
    const s = new Set(sli.value);
    s.delete(id);
    sli.value = s;
  } else {
    next.add(id);
  }
  context.value = next;
}

function toggleSli(id: string) {
  if (!context.value.has(id)) return;
  const next = new Set(sli.value);
  if (next.has(id)) next.delete(id);
  else next.add(id);
  sli.value = next;
}

const isFirstRevision = computed(() => expectedRevision.value === 0);
const nextRevision = computed(() => expectedRevision.value + 1);

// Thresholds are declared against the DECLARED cardinality, so an unsatisfiable policy is
// caught here rather than becoming a permanent Bad nobody can explain.
const policyError = computed(() => {
  const n = sli.value.size;
  const agg = policies.value.aggregation ?? {};
  if (agg.mode !== "quorum") return "";
  const hi = agg.healthy_min ?? 0;
  const lo = agg.degraded_min ?? 0;
  if (lo && hi && lo > hi) return `degraded_min ${lo} exceeds healthy_min ${hi} — availability would have no good branch.`;
  if (hi > n) return `healthy_min ${hi} exceeds the ${n} declared reliability input${n === 1 ? "" : "s"}. With this policy the service could never be Healthy.`;
  if (lo > n) return `degraded_min ${lo} exceeds the ${n} declared reliability input${n === 1 ? "" : "s"}.`;
  return "";
});

const saving = ref(false);
const saveError = ref("");

async function save() {
  if (saving.value || policyError.value) return;
  saving.value = true;
  saveError.value = "";
  const body: Record<string, unknown> = {
    expected_revision: expectedRevision.value,
    monitors: [...context.value],
    sli: [...sli.value],
  };
  if (Object.keys(policies.value).length) body.policies = policies.value;
  if (isFirstRevision.value && backfillFrom.value) {
    body.backfill_from = new Date(backfillFrom.value).toISOString();
  }
  const res = await api.PUT("/api/v1/projects/{projectID}/services/{serviceID}/declaration", {
    params: { path: { projectID: ws.projectId, serviceID: serviceId.value } },
    body: body as never,
  });
  saving.value = false;
  if (res.error) {
    const code = (res.error as { error?: string })?.error || "";
    saveError.value =
      code === "revision_conflict"
        ? "Someone else changed this declaration while you were editing. Reload to see their revision — the two statements about what availability means are not merged."
        : code === "sli_not_in_monitors"
          ? "A reliability input is outside the operational context."
          : code === "managed_by_file"
            ? "This service is owned by a file provider; edit its declaration in the source file."
            : code || "Could not save the declaration.";
    return;
  }
  await router.push({ name: "service", params: { id: serviceId.value } });
}

function slugOf(m: Monitor): string {
  return m.slug || m.name || m.id || "";
}

onMounted(load);
watch(() => [route.params.id, ws.projectId], load);
</script>

<template>
  <AppShell active="services" :crumbs="[ws.orgName || 'cerbix', ws.projectName || '…', 'Services', detail?.service.name || '…', 'Declaration']">
    <div class="mx-auto max-w-[1180px] px-[22px] pb-16 pt-[26px]">
      <div v-if="error" class="rounded border border-down/40 bg-down-weak p-4 text-[13px] text-down">{{ error }}</div>
      <p v-else-if="loading" class="text-[13px] text-ink-3">Loading…</p>

      <template v-else-if="detail">
        <div class="mb-[22px] flex flex-wrap items-start gap-4">
          <div class="min-w-[260px] flex-1">
            <h1 class="text-[21px] font-semibold tracking-tight">Edit {{ detail.service.name }}</h1>
            <p class="mt-[3px] text-[13px] text-ink-3">
              Saving creates <b>definition revision {{ nextRevision }}</b>. Revisions are never edited in place, and
              this one takes effect at the next bucket boundary — history already sealed keeps the meaning it was
              measured with.
            </p>
          </div>
          <div class="flex gap-2">
            <RouterLink
              :to="{ name: 'service', params: { id: serviceId } }"
              class="flex h-[34px] items-center rounded-sm border border-border px-[13px] text-[13px] text-ink-2 hover:border-border-strong hover:text-ink"
            >
              Cancel
            </RouterLink>
            <button
              type="button"
              :disabled="saving || !!policyError"
              class="flex h-[34px] items-center rounded-sm bg-accent px-[13px] text-[13px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-50"
              data-testid="service-declaration-save"
              @click="save"
            >
              {{ saving ? "Saving…" : `Save as revision ${nextRevision}` }}
            </button>
          </div>
        </div>

        <p v-if="saveError" data-testid="service-save-error" class="mb-4 rounded border border-down/40 bg-down-weak p-3 text-[13px] text-down">{{ saveError }}</p>

        <!-- The vertical rule between the columns is the load-bearing element: without it,
             "let me add a Redis check for diagnostics" silently redefines availability. -->
        <section class="mb-4 overflow-hidden rounded border border-border bg-surface shadow-card">
          <header class="flex items-center gap-2 border-b border-border px-4 py-[10px]">
            <h2 class="text-[13.5px] font-semibold">Declaration</h2>
            <div class="flex-1"></div>
            <span class="font-mono text-[11.5px] text-ink-3">
              {{ isFirstRevision ? "no previous revision" : `based on rev ${expectedRevision}` }}
            </span>
          </header>

          <div class="grid grid-cols-2 max-[860px]:grid-cols-1">
            <div class="border-r border-border p-4 max-[860px]:border-b max-[860px]:border-r-0">
              <div class="text-[11px] font-semibold uppercase tracking-[0.06em] text-ink-3">
                Operational context
                <span class="font-normal normal-case tracking-normal"> — what is shown on the service</span>
              </div>
              <div class="mt-3 flex max-h-[420px] flex-col gap-px overflow-y-auto">
                <label
                  v-for="m in monitors"
                  :key="m.id"
                  class="flex cursor-pointer items-center gap-[9px] rounded-sm px-2 py-[6px] text-[13px] hover:bg-surface-2"
                  :class="{ 'opacity-55': m.enabled === false }"
                >
                  <input
                    type="checkbox"
                    class="accent-accent"
                    :data-testid="'service-context-' + slugOf(m)"
                    :checked="context.has(m.id!)"
                    @change="toggleContext(m.id!)"
                  />
                  <span class="font-mono">{{ slugOf(m) }}</span>
                  <span class="ml-auto text-[11.5px] text-ink-3">{{ m.type }} · {{ m.region || "core" }}</span>
                  <span v-if="m.enabled === false" class="rounded-xs border border-border px-[5px] text-[10.5px] text-ink-3">disabled</span>
                </label>
                <p v-if="!monitors.length" class="px-2 py-4 text-[12.5px] text-ink-3">This project has no monitors yet.</p>
              </div>
            </div>

            <div class="p-4">
              <div class="text-[11px] font-semibold uppercase tracking-[0.06em] text-ink-3">
                Reliability inputs
                <span class="font-normal normal-case tracking-normal"> — what counts toward availability</span>
              </div>
              <div class="mt-3 flex max-h-[420px] flex-col gap-px overflow-y-auto">
                <label
                  v-for="m in monitors"
                  :key="m.id"
                  class="flex items-center gap-[9px] rounded-sm px-2 py-[6px] text-[13px]"
                  :class="context.has(m.id!) ? 'cursor-pointer hover:bg-surface-2' : 'cursor-not-allowed opacity-35'"
                >
                  <input
                    type="checkbox"
                    class="accent-accent"
                    :data-testid="'service-sli-' + slugOf(m)"
                    :checked="sli.has(m.id!)"
                    :disabled="!context.has(m.id!)"
                    @change="toggleSli(m.id!)"
                  />
                  <span class="font-mono">{{ slugOf(m) }}</span>
                  <span v-if="sli.has(m.id!)" class="ml-auto rounded-full bg-accent-weak px-[8px] py-px text-[11px] font-medium text-accent">counts</span>
                  <span v-else-if="context.has(m.id!)" class="ml-auto text-[11.5px] text-ink-3">diagnostic only</span>
                  <span v-else class="ml-auto text-[11.5px] text-ink-3">not in context</span>
                </label>
              </div>
              <p class="mt-3 text-[12px] text-ink-3">
                Adding a monitor on the left never selects it here. Changing <i>this</i> list is what changes the
                meaning of the number.
              </p>
            </div>
          </div>

          <div class="border-t border-border px-4 py-[10px] text-[12.5px] text-ink-3">
            <span class="font-mono">{{ sli.size }}</span> reliability input{{ sli.size === 1 ? "" : "s" }} of
            <span class="font-mono">{{ context.size }}</span> monitor{{ context.size === 1 ? "" : "s" }} in context.
            <span v-if="context.size && !sli.size"> With no inputs this service reports no availability — not 100%.</span>
          </div>
        </section>

        <!-- Policy -->
        <section class="mb-4 overflow-hidden rounded border border-border bg-surface shadow-card">
          <header class="border-b border-border px-4 py-[10px]"><h2 class="text-[13.5px] font-semibold">Evaluation policy</h2></header>
          <div class="grid grid-cols-2 gap-x-6 gap-y-4 p-4 max-[860px]:grid-cols-1">
            <div class="flex flex-col gap-[6px]">
              <span class="text-[11px] font-semibold uppercase tracking-[0.06em] text-ink-3">Aggregation</span>
              <div class="flex gap-[6px]">
                <button
                  v-for="mode in ['all', 'any', 'quorum']"
                  :key="mode"
                  type="button"
                  class="rounded-full border px-[10px] py-[3px] font-mono text-[11.5px]"
                  :class="(policies.aggregation?.mode ?? 'all') === mode ? 'border-accent bg-accent-weak text-accent' : 'border-border text-ink-2 hover:border-border-strong'"
                  @click="policies = { ...policies, aggregation: { ...(policies.aggregation ?? {}), mode: mode as 'all' | 'any' | 'quorum' } }"
                >{{ mode }}</button>
              </div>
              <div v-if="policies.aggregation?.mode === 'quorum'" class="mt-1 flex gap-3">
                <label class="flex flex-col gap-[4px]">
                  <span class="text-[11px] text-ink-3">degraded_min</span>
                  <input
                    type="number"
                    min="1"
                    class="h-[30px] w-[90px] rounded-sm border border-border bg-surface-2 px-2 font-mono text-[12.5px] outline-none focus:border-accent"
                    :value="policies.aggregation?.degraded_min ?? 1"
                    @input="policies = { ...policies, aggregation: { ...(policies.aggregation ?? {}), degraded_min: Number(($event.target as HTMLInputElement).value) } }"
                  />
                </label>
                <label class="flex flex-col gap-[4px]">
                  <span class="text-[11px] text-ink-3">healthy_min</span>
                  <input
                    type="number"
                    min="1"
                    class="h-[30px] w-[90px] rounded-sm border border-border bg-surface-2 px-2 font-mono text-[12.5px] outline-none focus:border-accent"
                    :value="policies.aggregation?.healthy_min ?? 1"
                    @input="policies = { ...policies, aggregation: { ...(policies.aggregation ?? {}), healthy_min: Number(($event.target as HTMLInputElement).value) } }"
                  />
                </label>
              </div>
            </div>

            <div class="flex flex-col gap-[6px]">
              <span class="text-[11px] font-semibold uppercase tracking-[0.06em] text-ink-3">Missing data</span>
              <div class="flex gap-[6px]">
                <button
                  v-for="mode in ['unknown', 'bad', 'ignore']"
                  :key="mode"
                  type="button"
                  class="rounded-full border px-[10px] py-[3px] font-mono text-[11.5px]"
                  :class="(policies.missing_data ?? 'unknown') === mode ? 'border-accent bg-accent-weak text-accent' : 'border-border text-ink-2 hover:border-border-strong'"
                  @click="policies = { ...policies, missing_data: mode as 'unknown' | 'bad' | 'ignore' }"
                >{{ mode }}</button>
              </div>
              <p class="text-[11.5px] text-ink-3">
                Under <span class="font-mono">unknown</span> an undecidable bucket leaves the window entirely — it is
                never counted as good. <span class="font-mono">ignore</span> drops an unknown member only while other
                known members keep the interval decidable.
              </p>
            </div>
          </div>

          <div v-if="policyError" data-testid="service-policy-error" class="mx-4 mb-4 flex items-start gap-2 rounded border border-down/40 bg-down-weak p-3 text-[12.5px] text-down">
            <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" class="mt-px shrink-0"><circle cx="12" cy="12" r="9"/><path d="M12 7v6"/><path d="M12 16.5h.01"/></svg>
            <span>{{ policyError }}</span>
          </div>
        </section>

        <!-- Retroactive adoption, first revision only. -->
        <section v-if="isFirstRevision" class="overflow-hidden rounded border border-border bg-surface shadow-card">
          <header class="border-b border-border px-4 py-[10px]"><h2 class="text-[13.5px] font-semibold">Adopt existing history</h2></header>
          <div class="p-4">
            <label class="flex flex-col gap-[5px]">
              <span class="text-[11px] font-semibold uppercase tracking-[0.06em] text-ink-3">Backfill from</span>
              <input
                v-model="backfillFrom"
                type="datetime-local"
                class="h-[32px] w-[240px] rounded-sm border border-border bg-surface-2 px-2 font-mono text-[12.5px] outline-none focus:border-accent"
              />
            </label>
            <p class="mt-2 max-w-[70ch] text-[12.5px] text-ink-3">
              Only the first revision may be retroactive. What this produces is a <b>declared reconstruction</b>
              evaluated with today's members — not evidence about how they were configured then. Later revisions always
              take effect forward, so a definition change can never restate history it was not measured with.
            </p>
          </div>
        </section>

        <p v-else class="text-[12px] text-ink-3">
          Effective from the next bucket boundary after saving. The revision in force since
          <span class="font-mono">{{ sealedLabel(detail.declaration!.effective_at) }}</span> keeps its facts.
        </p>
      </template>
    </div>
  </AppShell>
</template>
