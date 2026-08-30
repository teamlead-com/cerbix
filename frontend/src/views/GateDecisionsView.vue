<script setup lang="ts">
// FR-024 D-0207 item 1, mock screen 4 ("Decision history"): the project's gate-decision LEDGER.
//
// The ledger is project-scoped and never service-nested (D10): a decision outlives its service, so
// a deleted service's history stays readable here — the row keeps the slug and name it had. The
// list is LIVE keyset paging, not a snapshot (§5): a key returned once is never returned again, a
// decision committed while the operator pages may appear later or not at all; the note under the
// table says so, and a pipeline that needs a fixed record reads its decision by id.
//
// Every request — the first page AND each "Show 50 more" — carries an EXPLICIT half-open range
// of at most 31 days computed here (lib/gateLedger.ts), never a server default; paging reuses the
// exact same `from`/`to` with the cursor. A range the server would refuse is refused BEFORE the
// request, with the server's own sentence; the server's 400 `range_too_wide` renders the same way
// if it ever comes back. The state filter is the SERVER's (iter-0164): a picked state travels as
// `state=<STATE>` on the first page and on every "Show 50 more", so a page of 50 is 50 matching
// rows and the cursor continues the filtered set — nothing is filtered here.
//
// Concurrency discipline as ServiceAlerting.vue (D-0207 item 5): one generation counter and one
// AbortController cover EVERY read, pagination included. A filter change, a project switch or an
// unmount aborts what is in flight, and a response that lands after its generation has passed is
// dropped — including a "Show 50 more" page that arrives after the filters moved under it.
//
// This view is read-only for viewer+ (`gate:evaluate`), so no role check appears here; and the
// SPA never POSTs a decision (D-0207 item 4) — opening this page creates nothing.
import { computed, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { RouterLink, useRoute } from "vue-router";

import { api } from "@/api/client";
import type { components } from "@/api/schema";
import AppShell from "@/components/AppShell.vue";
import {
  CHIP_ACC,
  CHIP_BASE,
  CHIP_DORM,
  GATE_STATES,
  type GateState,
  MAX_RANGE_DAYS,
  PAGE_SIZE,
  PILL_BASE,
  PILL_DOT,
  defaultRange,
  describeFailure,
  failureOf,
  isAbort,
  rangeBounds,
  rangeRefusal,
  reasonChip,
  revisionLabel,
  shortId,
  statePill,
  transportFailure,
} from "@/lib/gateLedger";
import { sealedLabel } from "@/lib/services";
import { useWorkspace } from "@/stores/workspace";

type Summary = components["schemas"]["GateDecisionSummary"];
type ServiceSummary = components["schemas"]["ServiceSummary"];

const ws = useWorkspace();
const route = useRoute();

interface Filters {
  from: string;
  to: string;
  service: string;
  state: "" | GateState;
}

// The DRAFT is what the inputs hold; the APPLIED filters are what the table shows. Apply copies
// one onto the other, so a half-edited range never changes the rows under the operator's eyes.
const draft = ref<Filters>({ ...defaultRange(), service: "", state: "" });
const applied = ref<Filters>({ ...draft.value });
// The request range of the applied filters, fixed at Apply: every page of this traversal sends it.
let appliedBounds: { from: string; to: string } | null = null;

const services = ref<ServiceSummary[]>([]);
const rows = ref<Summary[]>([]);
const nextCursor = ref<string | null>(null);
const loading = ref(false);
const loadingMore = ref(false);
const error = ref("");
// Which failure the error line describes: an HTTP status, 0 for the transport, "client" for a
// refusal made here before any request. Exposed as data-status so a test can tell them apart.
const errorStatus = ref<number | "client" | null>(null);

// ── One guard for every read ────────────────────────────────────────────────────────────────
let generation = 0;
let inflight: AbortController | undefined;
let servicesInflight: AbortController | undefined;

function begin(): { controller: AbortController; mine: number } {
  inflight?.abort();
  const controller = new AbortController();
  inflight = controller;
  return { controller, mine: ++generation };
}
function stale(mine: number): boolean {
  return mine !== generation;
}
function cancelAll() {
  generation++;
  inflight?.abort();
  servicesInflight?.abort();
  loading.value = false;
  loadingMore.value = false;
}
onBeforeUnmount(cancelAll);

// ── The range and the filters ───────────────────────────────────────────────────────────────
const draftDays = computed(() => rangeBounds(draft.value.from, draft.value.to)?.days ?? 0);

function setError(msg: string, status: number | "client" | null) {
  error.value = msg;
  errorStatus.value = status;
}

/** Validate the draft; on success make it the applied filters and load the first page. */
function apply() {
  const refusal = rangeRefusal(draft.value.from, draft.value.to);
  if (refusal) {
    // Refused here, before the request — and whatever was in flight is for filters the operator
    // has just moved away from, so it is dropped too. The old rows go with it: they answered a
    // different question.
    cancelAll();
    rows.value = [];
    nextCursor.value = null;
    setError(refusal, "client");
    return;
  }
  applied.value = { ...draft.value };
  const b = rangeBounds(applied.value.from, applied.value.to)!;
  appliedBounds = { from: b.from, to: b.to };
  void loadFirst();
}

// The API's `state` is repeatable (a set, OR-ed; openapi-fetch serialises an array as
// `state=A&state=B`); this picker selects one, so the set has one member — or is absent.
function queryFor(cursor?: string) {
  const q: { from: string; to: string; limit: number; service_id?: string; state?: GateState[]; cursor?: string } = {
    from: appliedBounds!.from,
    to: appliedBounds!.to,
    limit: PAGE_SIZE,
  };
  if (applied.value.service) q.service_id = applied.value.service;
  if (applied.value.state) q.state = [applied.value.state];
  if (cursor) q.cursor = cursor;
  return q;
}

function failed(res: { error?: unknown; response?: Response }) {
  const f = failureOf(res);
  setError(
    describeFailure(f, {
      notFound: "This project does not exist, or you cannot see it.",
      denied: "You cannot see this project's gate decisions.",
    }),
    f.status,
  );
  if (f.status === 401 || f.status === 403) {
    // One line, no table: rows that a refused principal is still looking at would be a claim
    // this screen can no longer make.
    rows.value = [];
    nextCursor.value = null;
  }
}

async function loadFirst() {
  const { controller, mine } = begin();
  loading.value = true;
  loadingMore.value = false;
  setError("", null);
  rows.value = [];
  nextCursor.value = null;
  try {
    const res = await api.GET("/api/v1/projects/{projectID}/gate/decisions", {
      params: { path: { projectID: ws.projectId }, query: queryFor() },
      signal: controller.signal,
    });
    if (stale(mine)) return;
    if (res.error || !res.data) {
      failed(res);
      return;
    }
    rows.value = res.data.items ?? [];
    nextCursor.value = res.data.next_cursor ?? null;
  } catch (e) {
    if (stale(mine) || isAbort(e)) return;
    setError(transportFailure(e), 0);
  } finally {
    if (!stale(mine)) loading.value = false;
  }
}

async function loadMore() {
  const cursor = nextCursor.value;
  if (!cursor || loading.value || loadingMore.value) return;
  // The same generation guard as the first page: a filter change, a project switch or an unmount
  // between click and answer bumps the generation, and this page is dropped when it lands.
  const { controller, mine } = begin();
  loadingMore.value = true;
  setError("", null);
  try {
    const res = await api.GET("/api/v1/projects/{projectID}/gate/decisions", {
      params: { path: { projectID: ws.projectId }, query: queryFor(cursor) },
      signal: controller.signal,
    });
    if (stale(mine)) return;
    if (res.error || !res.data) {
      failed(res);
      return;
    }
    rows.value = rows.value.concat(res.data.items ?? []);
    nextCursor.value = res.data.next_cursor ?? null;
  } catch (e) {
    if (stale(mine) || isAbort(e)) return;
    setError(transportFailure(e), 0);
  } finally {
    if (!stale(mine)) loadingMore.value = false;
  }
}

// The service picker's options. Best-effort: a failed list leaves "All services" plus whatever
// the route pre-selected, so the filter the operator arrived with still shows as selected.
async function loadServices() {
  servicesInflight?.abort();
  const controller = new AbortController();
  servicesInflight = controller;
  const projectID = ws.projectId;
  try {
    const res = await api.GET("/api/v1/projects/{projectID}/services", {
      params: { path: { projectID } },
      signal: controller.signal,
    });
    // Keyed on the project, not on the page generation: an Apply while the list is still coming
    // must not throw the picker's options away — the options belong to the project, not the page.
    if (controller.signal.aborted || projectID !== ws.projectId) return;
    services.value = (res.data ?? []).slice().sort((a, b) => a.service.slug.localeCompare(b.service.slug));
  } catch {
    /* the picker degrades to "All services" + the pre-selected id */
  }
}

const serviceOptions = computed(() => {
  const opts = services.value.map((s) => ({ id: s.service.id, label: s.service.slug, title: s.service.name }));
  const picked = draft.value.service;
  if (picked && !opts.some((o) => o.id === picked)) opts.unshift({ id: picked, label: shortId(picked), title: picked });
  return opts;
});

function routeService(): string {
  const v = route.query?.service;
  return typeof v === "string" ? v : "";
}

/** Defaults for THIS project: the range, no state, and the service the route names (if any). */
function start(fromRoute: boolean) {
  booted = true;
  draft.value = { ...defaultRange(), service: fromRoute ? routeService() : "", state: "" };
  apply();
  void loadServices();
}

// P1 [88]: the FIRST project selection is STARTUP, not a switch. On a cold load of
// `/gate/decisions?service=<id>` the workspace is not initialised yet; `ws.init()` picks the
// project and the watcher below fires — and the route's pre-filter must survive that first
// request. A LATER change of project is a real switch: the route's service belonged to the
// previous project and would only ever yield an empty page, so the filters reset. The marker
// is explicit; the two paths (watcher, onMounted) both consult it, so whichever runs first
// starts once and the other stands down — never by ordering luck.
let booted = false;

onMounted(async () => {
  await ws.init();
  if (ws.projectId && !booted) start(true);
});
watch(
  () => ws.projectId,
  (id) => {
    if (!id) {
      cancelAll();
      return;
    }
    start(!booted);
  },
);
// The card's "all decisions →" link moves the pre-filter while this view may be mounted.
watch(
  () => route.query?.service,
  () => {
    if (ws.projectId) start(true);
  },
);

const stateLabel = (s: GateState) => statePill(s).label;
</script>

<template>
  <AppShell active="gate-decisions" :crumbs="[ws.orgName || 'cerbix', ws.projectName || '…', 'Gate decisions']">
    <div class="mx-auto max-w-[1180px] px-[22px] pb-16 pt-[26px]" data-testid="gate-decisions">
      <div class="mb-[22px]">
        <h1 class="text-[21px] font-semibold tracking-tight">{{ ws.projectName || "…" }} · gate decisions</h1>
        <p class="mt-[3px] text-[13px] text-ink-3">
          Every answer the gate gave, immutable, project-wide ·
          <span class="font-mono">GET /api/v1/projects/{p}/gate/decisions</span>
        </p>
      </div>

      <section class="overflow-hidden rounded border border-border bg-surface shadow-card">
        <header class="flex flex-wrap items-end gap-[10px] border-b border-border px-4 py-[13px]">
          <h2 class="mr-[6px] self-center text-[13.5px] font-semibold">Decisions</h2>

          <label class="flex flex-col gap-[3px]">
            <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">From</span>
            <input
              v-model="draft.from"
              type="date"
              class="h-[30px] rounded-sm border border-border bg-surface px-[9px] font-mono text-[12.5px] text-ink outline-none focus:border-accent"
              data-testid="gate-decisions-from"
            />
          </label>
          <label class="flex flex-col gap-[3px]">
            <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">To</span>
            <input
              v-model="draft.to"
              type="date"
              class="h-[30px] rounded-sm border border-border bg-surface px-[9px] font-mono text-[12.5px] text-ink outline-none focus:border-accent"
              data-testid="gate-decisions-to"
            />
          </label>
          <span class="self-center pb-[6px] font-mono text-[11.5px] text-ink-3" :class="draftDays > MAX_RANGE_DAYS || draftDays <= 0 ? 'text-down' : ''">
            · {{ draftDays > 0 ? draftDays : "—" }} days
          </span>

          <label class="flex flex-col gap-[3px]">
            <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Service</span>
            <select
              v-model="draft.service"
              class="h-[30px] max-w-[220px] rounded-sm border border-border bg-surface px-[9px] text-[12.5px] text-ink outline-none focus:border-accent"
              data-testid="gate-decisions-service"
            >
              <option value="">All services</option>
              <option v-for="o in serviceOptions" :key="o.id" :value="o.id" :title="o.title">{{ o.label }}</option>
            </select>
          </label>
          <label class="flex flex-col gap-[3px]">
            <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">State</span>
            <select
              v-model="draft.state"
              class="h-[30px] rounded-sm border border-border bg-surface px-[9px] text-[12.5px] text-ink outline-none focus:border-accent"
              data-testid="gate-decisions-state"
            >
              <option value="">Any state</option>
              <option v-for="s in GATE_STATES" :key="s" :value="s">{{ stateLabel(s) }}</option>
            </select>
          </label>
          <button
            type="button"
            class="inline-flex h-[30px] items-center rounded-sm border border-border bg-surface px-3 text-[13px] text-ink hover:border-border-strong disabled:opacity-50"
            :disabled="loading"
            data-testid="gate-decisions-apply"
            @click="apply"
          >
            Apply
          </button>

          <span class="ml-auto self-center text-[12px] text-ink-3">newest first · {{ PAGE_SIZE }} per page</span>
        </header>

        <p
          v-if="error"
          class="border-b border-border px-4 py-[10px] text-[13px] text-down"
          data-testid="gate-decisions-error"
          :data-status="errorStatus ?? undefined"
        >
          {{ error }}
        </p>

        <p v-if="loading" class="px-4 py-10 text-center text-[13px] text-ink-3">Loading…</p>

        <div v-else-if="rows.length" class="overflow-x-auto">
          <table class="w-full text-[13px]" data-testid="gate-decisions-table">
            <thead>
              <tr class="text-[10.5px] uppercase tracking-[0.06em] text-ink-3">
                <th class="whitespace-nowrap border-b border-border px-3 py-[10px] text-left">Evaluated</th>
                <th class="border-b border-border px-3 py-[10px] text-left">Service</th>
                <th class="border-b border-border px-3 py-[10px] text-left">State</th>
                <th class="border-b border-border px-3 py-[10px] text-left">Action</th>
                <th class="border-b border-border px-3 py-[10px] text-left">Reasons</th>
                <th class="border-b border-border px-3 py-[10px] text-left">Policy</th>
                <th class="border-b border-border px-3 py-[10px] text-left">Override</th>
                <th class="border-b border-border px-3 py-[10px] text-left">Decision</th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="r in rows"
                :key="r.decision_id"
                class="hover:bg-surface-2"
                data-testid="gate-decision-row"
                :data-id="r.decision_id"
                :data-state="r.state"
              >
                <td class="whitespace-nowrap border-b border-border px-3 py-[9px] align-top font-mono text-[12.5px] tnum">{{ sealedLabel(r.evaluated_at) }}</td>
                <td class="border-b border-border px-3 py-[9px] align-top">
                  <RouterLink
                    v-if="r.service_id"
                    :to="{ name: 'service', params: { id: r.service_id } }"
                    class="font-mono text-[12.5px] text-ink hover:text-accent"
                    :title="r.service_name"
                  >{{ r.service_slug }}</RouterLink>
                  <template v-else>
                    <span class="font-mono text-[12.5px] text-ink-3" :title="r.service_name">{{ r.service_slug }}</span>
                    <span :class="[CHIP_BASE, CHIP_DORM, 'ml-[6px]']" data-testid="gate-decision-service-deleted">service deleted</span>
                  </template>
                </td>
                <td class="border-b border-border px-3 py-[9px] align-top">
                  <span :class="[PILL_BASE, statePill(r.state).cls]">
                    <span :class="[PILL_DOT, statePill(r.state).dot]"></span>{{ statePill(r.state).label }}
                  </span>
                </td>
                <td class="border-b border-border px-3 py-[9px] align-top font-mono text-[12.5px]">
                  <span v-if="r.action">{{ r.action }}</span>
                  <span v-else class="text-ink-3">—</span>
                </td>
                <td class="border-b border-border px-3 py-[9px] align-top">
                  <div class="flex flex-wrap gap-1">
                    <span
                      v-for="(rs, i) in r.reasons"
                      :key="i"
                      :class="[CHIP_BASE, reasonChip(rs).cls, 'font-mono text-[10.5px]']"
                      :title="reasonChip(rs).title"
                      data-testid="gate-reason-chip"
                      :data-code="rs.code"
                      :data-clause="rs.clause"
                      :data-assignment="rs.assignment"
                    >{{ reasonChip(rs).text }}</span>
                  </div>
                </td>
                <td class="border-b border-border px-3 py-[9px] align-top font-mono text-[12.5px]" :class="r.policy_revision == null ? 'text-ink-3' : ''">{{ revisionLabel(r.policy_revision) }}</td>
                <td class="border-b border-border px-3 py-[9px] align-top">
                  <span v-if="r.override_id" :class="[CHIP_BASE, CHIP_ACC, 'font-mono text-[10.5px]']" :title="r.override_id">{{ shortId(r.override_id) }}</span>
                  <span v-else class="text-ink-3">—</span>
                </td>
                <td class="border-b border-border px-3 py-[9px] align-top">
                  <RouterLink
                    :to="{ name: 'gate-decision', params: { id: r.decision_id } }"
                    class="font-mono text-[12.5px] text-accent hover:underline"
                    :title="r.decision_id"
                    data-testid="gate-decision-link"
                  >{{ shortId(r.decision_id) }}</RouterLink>
                </td>
              </tr>
            </tbody>
          </table>
        </div>

        <p v-else-if="!error" class="px-4 py-10 text-center text-[13px] text-ink-3" data-testid="gate-decisions-empty">
          <template v-if="applied.state">No <span class="font-mono">{{ stateLabel(applied.state) }}</span> decisions</template>
          <template v-else>No decisions</template>
          between <span class="font-mono">{{ applied.from }}</span> and <span class="font-mono">{{ applied.to }}</span><template v-if="applied.service"> for this service</template>.
          The gate writes one row per <span class="font-mono">cerbix gate check</span>; opening this page writes nothing.
        </p>

        <div class="flex flex-wrap items-center justify-center gap-[14px] border-t border-border px-4 py-[11px]">
          <button
            v-if="nextCursor"
            type="button"
            class="inline-flex h-[26px] items-center rounded-sm border border-border bg-surface px-[10px] text-[12.5px] text-ink hover:border-border-strong disabled:opacity-50"
            :disabled="loading || loadingMore"
            data-testid="gate-decisions-more"
            @click="loadMore"
          >
            {{ loadingMore ? "Loading…" : `Show ${PAGE_SIZE} more` }}
          </button>
          <span class="text-[12px] text-ink-3" data-testid="gate-decisions-live-note">
            decisions made while you page may appear later or not at all — the list is live, not a snapshot; a pipeline that needs a fixed record reads its decision by id
          </span>
        </div>
      </section>
    </div>
  </AppShell>
</template>
