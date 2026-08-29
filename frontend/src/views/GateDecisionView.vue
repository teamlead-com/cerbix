<script setup lang="ts">
// FR-024 D-0207 item 1, mock screen 4 ("One decision, by id"): the full immutable record.
//
// Presence says what happened (D7, §5): `action` and `policy_revision` exist only when a policy
// existed; `override` only when one applied; a deleted service's `service_id` is present and NULL
// ("the referent is gone"), which is not the same as absent ("never applied"). The key/value block
// renders a row ONLY when its field is present — nothing here ever prints "null" for an absence —
// and the raw record below keeps the two apart exactly as the server sent them.
//
// Read by id, authorised by the row's persisted project (D10): there is no service-existence check,
// because the evidence is wanted exactly when the service is gone. 404 is one sentence; a 400 for a
// malformed id shows the server's message verbatim.
import { computed, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { RouterLink, useRoute } from "vue-router";

import { api } from "@/api/client";
import type { components } from "@/api/schema";
import AppShell from "@/components/AppShell.vue";
import {
  CHIP_ACC,
  CHIP_BASE,
  CHIP_DORM,
  CHIP_PLAIN,
  PILL_BASE,
  PILL_DOT,
  describeFailure,
  failureOf,
  formatValue,
  isAbort,
  reasonChip,
  secondsLabel,
  shortId,
  statePill,
  transportFailure,
} from "@/lib/gateLedger";
import { sealedLabel } from "@/lib/services";
import { useWorkspace } from "@/stores/workspace";

type Decision = components["schemas"]["GateDecision"];

const ws = useWorkspace();
const route = useRoute();

const decisionId = computed(() => String(route.params.id || ""));
const decision = ref<Decision | null>(null);
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

async function load() {
  const { controller, mine } = begin();
  loading.value = true;
  error.value = "";
  errorStatus.value = null;
  decision.value = null;
  try {
    await ws.init();
    if (stale(mine)) return;
    if (!ws.projectId || !decisionId.value) return;
    const res = await api.GET("/api/v1/projects/{projectID}/gate/decisions/{decisionID}", {
      params: { path: { projectID: ws.projectId, decisionID: decisionId.value } },
      signal: controller.signal,
    });
    if (stale(mine)) return;
    if (res.error || !res.data) {
      const f = failureOf(res);
      error.value = describeFailure(f, {
        notFound: "This decision does not exist in this project, or you cannot see it.",
        denied: "You cannot see this project's gate decisions.",
      });
      errorStatus.value = f.status;
      return;
    }
    decision.value = res.data;
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

const pill = computed(() => statePill(decision.value?.state ?? ""));
const json = computed(() => (decision.value ? JSON.stringify(decision.value, null, 2) : ""));

/** The objective is a percentage (99.9); the reliability screens print it with up to three decimals. */
function pct(v: number): string {
  return `${v.toFixed(3).replace(/\.?0+$/, "")}%`;
}
function yesNo(v: boolean | null | undefined): string {
  return v == null ? "—" : v ? "yes" : "no";
}
function stamp(iso: string | null | undefined): string {
  return iso ? sealedLabel(iso) : "—";
}
</script>

<template>
  <AppShell active="gate-decisions" :crumbs="[ws.orgName || 'cerbix', ws.projectName || '…', 'Gate decisions', decision ? shortId(decision.decision_id) : '…']">
    <div class="mx-auto max-w-[1180px] px-[22px] pb-16 pt-[26px]" data-testid="gate-decision">
      <RouterLink :to="{ name: 'gate-decisions' }" class="text-[12.5px] text-ink-3 hover:text-accent" data-testid="gate-decision-back">← all decisions</RouterLink>

      <div class="mb-[22px] mt-[10px]">
        <div class="flex flex-wrap items-center gap-[10px]">
          <h1 class="text-[21px] font-semibold tracking-tight">Gate decision</h1>
          <span v-if="decision" :class="[PILL_BASE, 'h-[30px] pl-[9px] pr-3 text-[13.5px]', pill.cls]" data-testid="gate-decision-state">
            <span :class="[PILL_DOT, 'h-[9px] w-[9px]', pill.dot]"></span>{{ pill.label }}
          </span>
        </div>
        <p class="mt-[3px] text-[13px] text-ink-3">
          One answer, as it was, evidence included · <span class="font-mono">GET …/gate/decisions/{id}</span>
        </p>
      </div>

      <div v-if="error" class="rounded border border-down/40 bg-down-weak p-4 text-[13px] text-down" data-testid="gate-decision-error" :data-status="errorStatus ?? undefined">{{ error }}</div>
      <p v-else-if="loading" class="text-[13px] text-ink-3">Loading…</p>

      <template v-else-if="decision">
        <!-- The record, field by field — a row appears only when its field is present (D7). -->
        <section class="mb-4 overflow-hidden rounded border border-border bg-surface shadow-card">
          <header class="flex flex-wrap items-center gap-[10px] border-b border-border px-4 py-[13px]">
            <h2 class="text-[13.5px] font-semibold">Record</h2>
            <span :class="[CHIP_BASE, CHIP_PLAIN, 'font-mono text-[10.5px]']">schema_version {{ decision.schema_version }}</span>
            <span v-if="decision.override" :class="[CHIP_BASE, CHIP_ACC]" data-testid="gate-decision-override-applied">override applied → action {{ decision.action }}</span>
          </header>
          <dl class="grid grid-cols-[max-content_1fr] gap-x-[18px] gap-y-[6px] px-4 py-[14px] text-[13px] max-[640px]:grid-cols-1" data-testid="gate-decision-kv">
            <dt class="text-ink-3">Decision id</dt>
            <dd class="font-mono text-[12.5px]">{{ decision.decision_id }}</dd>

            <dt class="text-ink-3">Evaluated</dt>
            <dd class="font-mono text-[12.5px] tnum">{{ sealedLabel(decision.evaluated_at) }}</dd>

            <dt class="text-ink-3">Service</dt>
            <dd class="flex flex-wrap items-center gap-[8px]">
              <template v-if="decision.service_id">
                <RouterLink :to="{ name: 'service', params: { id: decision.service_id } }" class="font-mono text-[12.5px] text-ink hover:text-accent" data-testid="gate-decision-service-link">{{ decision.service_slug }}</RouterLink>
                <span class="text-ink-2">{{ decision.service_name }}</span>
              </template>
              <template v-else>
                <span class="font-mono text-[12.5px] text-ink-3">{{ decision.service_slug }}</span>
                <span class="text-ink-3">{{ decision.service_name }}</span>
                <span :class="[CHIP_BASE, CHIP_DORM]" data-testid="gate-decision-service-deleted">service deleted</span>
              </template>
            </dd>

            <template v-if="decision.action !== undefined">
              <dt class="text-ink-3">Action</dt>
              <dd class="font-mono text-[12.5px]" data-testid="gate-decision-action">{{ decision.action }}</dd>
            </template>
            <template v-if="decision.unoverridden_action !== undefined">
              <dt class="text-ink-3">Without the override</dt>
              <dd class="font-mono text-[12.5px]">{{ decision.unoverridden_action }}</dd>
            </template>
            <template v-if="decision.policy_revision !== undefined">
              <dt class="text-ink-3">Policy</dt>
              <dd class="font-mono text-[12.5px]">rev {{ decision.policy_revision }}</dd>
            </template>
            <template v-if="decision.window !== undefined">
              <dt class="text-ink-3">Window</dt>
              <dd class="font-mono text-[12.5px]">{{ decision.window }}</dd>
            </template>
            <template v-if="decision.unknown_behavior !== undefined">
              <dt class="text-ink-3">Unknown behavior</dt>
              <dd class="font-mono text-[12.5px]">{{ decision.unknown_behavior }}</dd>
            </template>
            <template v-if="decision.max_seal_lag_seconds !== undefined">
              <dt class="text-ink-3">Max seal lag</dt>
              <dd class="font-mono text-[12.5px]">{{ secondsLabel(decision.max_seal_lag_seconds) }}</dd>
            </template>

            <template v-if="decision.override">
              <dt class="text-ink-3">Override</dt>
              <dd class="flex flex-wrap items-center gap-[8px]" data-testid="gate-decision-override">
                <span :class="[CHIP_BASE, CHIP_ACC, 'font-mono text-[10.5px]']" :title="decision.override.id">{{ shortId(decision.override.id) }}</span>
                <span class="font-mono text-[12.5px]">{{ decision.override.actor_label }}</span>
                <span class="text-ink-2">{{ decision.override.reason }}</span>
                <span class="text-ink-3">until <span class="font-mono text-[12px]">{{ sealedLabel(decision.override.expires_at) }}</span></span>
              </dd>
            </template>

            <template v-if="decision.sealed_through !== undefined">
              <dt class="text-ink-3">Sealed through</dt>
              <dd class="font-mono text-[12.5px] tnum">{{ sealedLabel(decision.sealed_through) }}</dd>
            </template>
            <template v-if="decision.seal_lag !== undefined">
              <dt class="text-ink-3">Seal lag</dt>
              <dd class="font-mono text-[12.5px]">{{ secondsLabel(decision.seal_lag) }}</dd>
            </template>
            <template v-if="decision.facts_fresh_until !== undefined">
              <dt class="text-ink-3">Facts fresh until</dt>
              <dd class="font-mono text-[12.5px] tnum">{{ sealedLabel(decision.facts_fresh_until) }}</dd>
            </template>

            <template v-if="decision.objective !== undefined">
              <dt class="text-ink-3">Objective</dt>
              <dd class="font-mono text-[12.5px]">
                {{ pct(decision.objective) }}<span v-if="decision.objective_updated_at" class="text-ink-3"> · set {{ sealedLabel(decision.objective_updated_at) }}</span>
              </dd>
            </template>
            <template v-if="decision.target_id !== undefined">
              <dt class="text-ink-3">Target</dt>
              <dd class="font-mono text-[12.5px]" :title="decision.target_id">{{ shortId(decision.target_id) }}</dd>
            </template>
            <template v-if="decision.governing_revision">
              <dt class="text-ink-3">Governing revision</dt>
              <dd class="font-mono text-[12.5px]">rev {{ decision.governing_revision.revision }} <span class="text-ink-3" :title="decision.governing_revision.id">· {{ shortId(decision.governing_revision.id) }}</span></dd>
            </template>
            <template v-if="decision.fact_revisions">
              <dt class="text-ink-3">Fact revisions</dt>
              <dd class="font-mono text-[12.5px]">
                {{ decision.fact_revisions.count }} · digest <span :title="decision.fact_revisions.digest">{{ decision.fact_revisions.digest.slice(0, 16) }}…</span>
                <span v-if="decision.fact_revisions.first_id" class="text-ink-3" :title="decision.fact_revisions.first_id"> · first {{ shortId(decision.fact_revisions.first_id) }}</span>
                <span v-if="decision.fact_revisions.last_id" class="text-ink-3" :title="decision.fact_revisions.last_id"> · last {{ shortId(decision.fact_revisions.last_id) }}</span>
              </dd>
            </template>

            <template v-if="decision.coverage_state">
              <dt class="text-ink-3">Coverage</dt>
              <dd class="flex flex-wrap items-center gap-[6px]">
                <span :class="[CHIP_BASE, decision.coverage_state.live.armed ? CHIP_PLAIN : CHIP_DORM]">
                  live · {{ decision.coverage_state.live.armed ? "armed" : "not armed" }}<span v-if="decision.coverage_state.live.reason" class="font-mono text-[10.5px]"> · {{ decision.coverage_state.live.reason }}</span>
                </span>
                <span :class="[CHIP_BASE, decision.coverage_state.burn.armed ? CHIP_PLAIN : CHIP_DORM]">
                  burn · {{ decision.coverage_state.burn.armed ? "armed" : "not armed" }}<span v-if="decision.coverage_state.burn.reason" class="font-mono text-[10.5px]"> · {{ decision.coverage_state.burn.reason }}</span>
                </span>
              </dd>
            </template>
            <template v-if="decision.coverage_lease_until !== undefined">
              <dt class="text-ink-3">Coverage lease until</dt>
              <dd class="font-mono text-[12.5px] tnum">{{ sealedLabel(decision.coverage_lease_until) }}</dd>
            </template>
          </dl>

          <div v-if="decision.burn_leases && decision.burn_leases.length" class="border-t border-border">
            <div class="px-4 pt-[12px] text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Burn leases</div>
            <div class="overflow-x-auto">
              <table class="w-full text-[13px]" data-testid="gate-decision-burn-leases">
                <thead>
                  <tr class="text-[10.5px] uppercase tracking-[0.06em] text-ink-3">
                    <th class="border-b border-border px-4 py-[8px] text-left">Rule</th>
                    <th class="border-b border-border px-3 py-[8px] text-left">Severity</th>
                    <th class="border-b border-border px-3 py-[8px] text-left">Firing</th>
                    <th class="border-b border-border px-3 py-[8px] text-left">Last verdict</th>
                    <th class="border-b border-border px-3 py-[8px] text-left">Evaluated</th>
                    <th class="border-b border-border px-3 py-[8px] text-left">Lease until</th>
                    <th class="border-b border-border px-3 py-[8px] text-left">Fresh</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="l in decision.burn_leases" :key="l.rule_key" class="hover:bg-surface-2">
                    <td class="border-b border-border px-4 py-[8px] font-mono text-[12.5px]">{{ l.rule_key }}</td>
                    <td class="border-b border-border px-3 py-[8px] font-mono text-[12.5px]">{{ l.severity }}</td>
                    <td class="border-b border-border px-3 py-[8px]">{{ yesNo(l.firing) }}</td>
                    <td class="border-b border-border px-3 py-[8px] font-mono text-[12.5px]">{{ l.last_verdict ?? "—" }}</td>
                    <td class="border-b border-border px-3 py-[8px] font-mono text-[12.5px] tnum">{{ stamp(l.evaluated_at) }}</td>
                    <td class="border-b border-border px-3 py-[8px] font-mono text-[12.5px] tnum">{{ stamp(l.lease_until) }}</td>
                    <td class="border-b border-border px-3 py-[8px]">{{ yesNo(l.fresh) }}</td>
                  </tr>
                </tbody>
              </table>
            </div>
          </div>
        </section>

        <!-- reasons[] is never empty: NOT_CONFIGURED carries its own reason and the documentation link. -->
        <section class="mb-4 overflow-hidden rounded border border-border bg-surface shadow-card">
          <header class="flex items-center gap-[10px] border-b border-border px-4 py-[13px]">
            <h2 class="text-[13.5px] font-semibold">Reasons</h2>
            <span class="text-[12px] text-ink-3">every matching or unavailable clause, total</span>
          </header>
          <ul class="divide-y divide-border" data-testid="gate-decision-reasons">
            <li
              v-for="(rs, i) in decision.reasons"
              :key="i"
              class="flex flex-wrap items-center gap-[9px] px-4 py-[10px] text-[13px]"
              data-testid="gate-reason-chip"
              :data-code="rs.code"
              :data-clause="rs.clause"
              :data-assignment="rs.assignment"
            >
              <span :class="[CHIP_BASE, reasonChip(rs).cls, 'font-mono text-[10.5px]']">{{ rs.code }}</span>
              <span v-if="rs.clause && rs.clause !== rs.code" class="font-mono text-[12px] text-ink-2">clause {{ rs.clause }}</span>
              <span v-if="rs.assignment" class="text-[12px] text-ink-3">assigned <span class="font-mono">{{ rs.assignment }}</span></span>
              <span v-if="rs.value !== undefined && rs.value !== null" class="text-[12px] text-ink-3">value <span class="font-mono text-ink-2">{{ formatValue(rs.value) }}</span></span>
              <span v-if="rs.source" class="text-[12px] text-ink-3">from <span class="font-mono">{{ rs.source }}</span></span>
              <a v-if="rs.docs" :href="rs.docs" target="_blank" rel="noopener" class="ml-auto text-[12px] text-accent hover:underline">docs ↗</a>
            </li>
            <li v-if="!decision.reasons.length" class="px-4 py-[10px] text-[13px] text-ink-3">No reasons recorded.</li>
          </ul>
        </section>

        <!-- The raw record: present-and-null and absent stay apart here, exactly as the server sent them. -->
        <section class="overflow-hidden rounded border border-border bg-surface shadow-card">
          <header class="flex items-center gap-[10px] border-b border-border px-4 py-[13px]">
            <h2 class="text-[13.5px] font-semibold">Raw record</h2>
            <span class="text-[12px] text-ink-3">absent means "never applied"; a null <span class="font-mono">service_id</span> means the service is gone</span>
          </header>
          <pre class="m-0 overflow-x-auto bg-inset px-4 py-3 font-mono text-[12.5px] leading-[1.55] text-ink tnum" data-testid="gate-decision-json">{{ json }}</pre>
        </section>
      </template>
    </div>
  </AppShell>
</template>
