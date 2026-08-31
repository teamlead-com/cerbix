<script setup lang="ts">
// FR-025 D-0210 item 5, mock screen 5 ("Full timeline"): every change recorded on ONE service.
//
// The timeline is the service's (D6, D10): it lives under the service and goes with it — a deleted
// service has no timeline, and this page says so in one line. A row is a change GROUP — one
// external identity `(source, external_id)` with its phases nested in the domain's order — newest
// first by the group's LATEST phase; a `started` older than the range still travels with its group.
//
// Every request — the first page AND each "Show 50 more" — carries an EXPLICIT half-open range of
// at most 92 days computed here (lib/changesTimeline.ts), never a server default; paging reuses the
// exact `from`/`to`, the kind set and the source with the cursor. A range the server would refuse is
// refused BEFORE the request with the mock's sentence; the server's 400 `range_too_wide` renders the
// same way if it ever comes back. The traversal is LIVE, as the gate ledger's: a group never appears
// twice, a phase recorded mid-traversal can move its group ahead of the cursor, and the note under
// the table says so.
//
// The before/after column is the reliability page's own arithmetic (D8) at the horizon chosen in the
// header — ONE comparison per terminal group on the page, re-issued when the horizon moves; a group
// without a terminal phase has none and says so. Concurrency discipline as GateDecisionsView.vue
// (D-0210 item 7): one generation counter covers every read — the page, its pagination and every
// per-row comparison; a filter change, a project switch, a route change or an unmount aborts what is
// in flight, and a response that lands after its generation has passed is dropped.
//
// Read-only for viewer+ (`project:read`): no control here records, edits or deletes a change — the
// record is the pipeline's (`cerbix change record`, D14).
import { computed, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { RouterLink, useRoute } from "vue-router";

import { api } from "@/api/client";
import type { components } from "@/api/schema";
import AppShell from "@/components/AppShell.vue";
import {
  CHANGE_ERROR_TEXT,
  CHANGE_KINDS,
  CHANGE_RANGE_MAX_DAYS,
  CHIP_ACC,
  COMPARE_POOL,
  CHIP_BASE,
  CHIP_DORM,
  CHIP_PLAIN,
  type ChangeCompare,
  type ChangeGroup,
  type ChangeHorizon,
  type ChangeKind,
  type CompareSideView,
  type DecisionView,
  DEFAULT_HORIZON,
  HORIZONS,
  inPool,
  requestScope,
  NO_TERMINAL_TEXT,
  PAGE_SIZE,
  PILL_BASE,
  PILL_DOT,
  SOURCE_SLUG,
  changeRangeRefusal,
  decisionView,
  defaultChangeRange,
  deltaChip,
  deltaValueClass,
  describeChangeFailure,
  failureOf,
  formatCompareSide,
  groupKey,
  groupLatest,
  horizonLabel,
  isAbort,
  kindClip,
  kindLabel,
  kindsFromQuery,
  lagText,
  phaseInstantLabel,
  phaseLabel,
  rangeBounds,
  shortId,
  statePill,
  terminalOf,
  transportFailure,
} from "@/lib/changesTimeline";
import { useWorkspace } from "@/stores/workspace";

type Detail = components["schemas"]["ServiceDetail"];

const ws = useWorkspace();
const route = useRoute();

const serviceId = computed(() => String(route.params.id || ""));

interface Filters {
  from: string;
  to: string;
  /** The kind SET (repeatable `kind=`, OR); empty means every kind. */
  kinds: ChangeKind[];
  /** One source slug; "" means every source. */
  source: string;
}

// The DRAFT is what the inputs hold; the APPLIED filters are what the table shows. Apply copies one
// onto the other, so a half-edited range never changes the rows under the operator's eyes.
const draft = ref<Filters>({ ...defaultChangeRange(), kinds: [], source: "" });
const applied = ref<Filters>({ ...draft.value, kinds: [] });
// The request range of the applied filters, fixed at Apply: every page of this traversal sends it.
let appliedBounds: { from: string; to: string } | null = null;

// The horizon of the before/after column. NOT part of Apply: it is a header control that applies to
// every row on the page (mock: "the header's horizon applies to the before/after column"), and moving
// it re-issues the comparisons for the rows already shown — the rows themselves do not change.
const horizon = ref<ChangeHorizon>(DEFAULT_HORIZON);

const service = ref<Detail | null>(null);
const rows = ref<ChangeGroup[]>([]);
const nextCursor = ref<string | null>(null);
const loading = ref(false);
const loadingMore = ref(false);
const error = ref("");
// Which failure the error line describes: an HTTP status, 0 for the transport, "client" for a
// refusal made here before any request. Exposed as data-status so a test can tell them apart.
const errorStatus = ref<number | "client" | null>(null);

type CompareState = { status: "loading" } | { status: "ok"; data: ChangeCompare } | { status: "error"; text: string; code: number };
// One comparison per terminal group on the page, keyed by identity (`groupKey`, lib/changes.ts).
const compares = ref<Record<string, CompareState>>({});

// ── One guard for every read ────────────────────────────────────────────────────────────────
let generation = 0;
let inflight: AbortController | undefined;
let serviceInflight: AbortController | undefined;
/** The comparisons' own scope: generation, aborts and the 10 s deadline (review [37]). */
const cmpScope = requestScope();

function begin(): { controller: AbortController; mine: number } {
  inflight?.abort();
  const controller = new AbortController();
  inflight = controller;
  return { controller, mine: ++generation };
}
function stale(mine: number): boolean {
  return mine !== generation;
}
function cancelCompares() {
  cmpScope.begin();
}
function cancelAll() {
  generation++;
  inflight?.abort();
  serviceInflight?.abort();
  cancelCompares();
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

function clearRows() {
  rows.value = [];
  nextCursor.value = null;
  compares.value = {};
}

function toggleKind(k: ChangeKind) {
  const set = new Set(draft.value.kinds);
  if (set.has(k)) set.delete(k);
  else set.add(k);
  draft.value.kinds = CHANGE_KINDS.filter((x) => set.has(x));
}

/** Validate the draft; on success make it the applied filters and load the first page. */
function apply() {
  const source = draft.value.source.trim();
  const refusal = changeRangeRefusal(draft.value.from, draft.value.to) || (source && !SOURCE_SLUG.test(source) ? CHANGE_ERROR_TEXT.source_invalid : "");
  if (refusal) {
    // Refused here, before the request — and whatever was in flight is for filters the operator
    // has just moved away from, so it is dropped too. The old rows go with it: they answered a
    // different question.
    cancelAll();
    clearRows();
    setError(refusal, "client");
    return;
  }
  applied.value = { ...draft.value, kinds: [...draft.value.kinds], source };
  const b = rangeBounds(applied.value.from, applied.value.to)!;
  appliedBounds = { from: b.from, to: b.to };
  void loadFirst();
}

// The API's `kind` is repeatable (a set, OR-ed; openapi-fetch serialises an array as
// `kind=a&kind=b`); `source` is one slug; the cursor continues the SAME filtered range.
function queryFor(cursor?: string) {
  const q: { from: string; to: string; limit: number; kind?: ChangeKind[]; source?: string; cursor?: string } = {
    from: appliedBounds!.from,
    to: appliedBounds!.to,
    limit: PAGE_SIZE,
  };
  if (applied.value.kinds.length) q.kind = [...applied.value.kinds];
  if (applied.value.source) q.source = applied.value.source;
  if (cursor) q.cursor = cursor;
  return q;
}

const SERVICE_GONE = "This service does not exist, or you cannot see it.";

function failed(res: { error?: unknown; response?: Response }) {
  const f = failureOf(res);
  setError(describeChangeFailure(f, { notFound: SERVICE_GONE, denied: "You cannot see this service's changes." }), f.status);
  if (f.status === 401 || f.status === 403 || f.status === 404) {
    // One line, no table: rows a refused principal is still looking at would be a claim this
    // screen can no longer make; and a service that is gone has no timeline (D10).
    clearRows();
  }
}

function pathFor() {
  return { projectID: ws.projectId, serviceID: serviceId.value };
}

async function loadFirst() {
  const { controller, mine } = begin();
  cancelCompares();
  loading.value = true;
  loadingMore.value = false;
  setError("", null);
  clearRows();
  try {
    const res = await api.GET("/api/v1/projects/{projectID}/services/{serviceID}/changes", {
      params: { path: pathFor(), query: queryFor() },
      signal: controller.signal,
    });
    if (stale(mine)) return;
    if (res.error || !res.data) {
      failed(res);
      return;
    }
    rows.value = res.data.items ?? [];
    nextCursor.value = res.data.next_cursor ?? null;
    compareRows(rows.value, mine);
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
  // between click and answer bumps the generation, and this page is dropped when it lands. The
  // comparisons of the rows already shown are NOT aborted — they belong to this traversal.
  inflight?.abort();
  const controller = new AbortController();
  inflight = controller;
  const mine = generation;
  loadingMore.value = true;
  setError("", null);
  try {
    const res = await api.GET("/api/v1/projects/{projectID}/services/{serviceID}/changes", {
      params: { path: pathFor(), query: queryFor(cursor) },
      signal: controller.signal,
    });
    if (stale(mine)) return;
    if (res.error || !res.data) {
      failed(res);
      return;
    }
    const items = res.data.items ?? [];
    rows.value = rows.value.concat(items);
    nextCursor.value = res.data.next_cursor ?? null;
    compareRows(items, mine);
  } catch (e) {
    if (stale(mine) || isAbort(e)) return;
    setError(transportFailure(e), 0);
  } finally {
    if (!stale(mine)) loadingMore.value = false;
  }
}

// ── The before/after column (D8) ────────────────────────────────────────────────────────────
/**
 * Ask the comparison for every TERMINAL group, through the SAME bounded pool the card uses — one
 * request per row with nothing holding them back could open fifty at once on a full page, saturate
 * `change.read_inflight_process` (configurable down to 1), manufacture this screen's own 429s and
 * crowd out every other read (review [32]). A queued row needs no marking: `compareState` already
 * reads a cell it has not been given yet as `loading`, which is exactly what a queued row is.
 *
 * TWO generations fence this pool, and both are captured HERE, at scheduling time. `mine` is the
 * page's: a filter change or a project switch retires the rows themselves. `cmpGen` is the
 * comparisons' own, and it is the one a horizon switch moves — `cancelCompares()` opens a new cmp
 * generation and a new pool is scheduled for every row. Without the second check the OLD pool kept
 * dequeuing (the page's generation had not moved), each job read the CURRENT scope generation and
 * so was not stale, and two pools of four ran at once writing the same cells — review [41], a
 * regression the pooling introduced.
 */
function compareRows(groups: readonly ChangeGroup[], mine: number) {
  const queue = groups.filter((g) => terminalOf(g));
  if (!queue.length) return;
  const cmpGen = cmpScope.gen;
  void inPool(
    queue,
    COMPARE_POOL,
    (g: ChangeGroup) => compareOne(g, mine, cmpGen),
    () => stale(mine) || cmpScope.stale(cmpGen),
  );
}

/**
 * ONE comparison, on the shared bounded request scope: the generation guard, the abort set and a
 * 10 s DEADLINE (`lib/changes.ts`, `requestScope`).
 *
 * The deadline is what makes the pool safe. `inPool` holds a slot until this resolves, so without
 * one, four comparisons that never settle would occupy the whole pool and leave every queued row
 * loading forever — a failure the BOUND introduced, since the unbounded fan-out at least asked
 * every row (review [37]). A request that outlives its deadline aborts, frees its slot and leaves
 * an error cell, which is something the reader can see.
 */
async function compareOne(g: ChangeGroup, mine: number, cmpGen: number) {
  const key = groupKey(g);
  const h = horizon.value;
  // `cmpGen` is the generation this job was SCHEDULED under, never the current one: reading
  // `cmpScope.gen` here would make every job belong to whichever generation is live when it
  // happens to run, which is exactly how retired work wrote into a fresh page (review [41]).
  const out = await cmpScope.request<ChangeCompare>(cmpGen, (signal: AbortSignal) =>
    api.GET("/api/v1/projects/{projectID}/services/{serviceID}/changes/compare", {
      params: { path: pathFor(), query: { source: g.source, external_id: g.external_id, horizon: h } },
      signal,
    }),
  );
  // Dropped when the page moved on OR the horizon moved: the answer is for a column that no longer
  // exists.
  if (out.kind === "stale" || stale(mine) || h !== horizon.value) return;
  if (out.kind === "failed") {
    compares.value = { ...compares.value, [key]: { status: "error", text: describeChangeFailure(out.failure), code: out.failure.status } };
    return;
  }
  if (!out.data) {
    compares.value = { ...compares.value, [key]: { status: "error", text: "The comparison returned nothing.", code: 0 } };
    return;
  }
  compares.value = { ...compares.value, [key]: { status: "ok", data: out.data } };
}

function setHorizon(h: ChangeHorizon) {
  if (h === horizon.value) return;
  horizon.value = h;
  // Every comparison on the page is for the old horizon: abort them, forget them, ask again.
  cancelCompares();
  compares.value = {};
  compareRows(rows.value, generation);
}

// ── The service (for the header; its absence is the page's one line) ────────────────────────
async function loadService() {
  serviceInflight?.abort();
  const controller = new AbortController();
  serviceInflight = controller;
  const projectID = ws.projectId;
  const sid = serviceId.value;
  service.value = null;
  try {
    const res = await api.GET("/api/v1/projects/{projectID}/services/{serviceID}", {
      params: { path: { projectID, serviceID: sid } },
      signal: controller.signal,
    });
    if (controller.signal.aborted || projectID !== ws.projectId || sid !== serviceId.value) return;
    if (res.error || !res.data) {
      // The service itself is the subject; without it the timeline has no header to sit under, and
      // the list request for the same service answers the same way — one line, no table.
      failed(res);
      return;
    }
    service.value = res.data;
  } catch (e) {
    if (controller.signal.aborted || isAbort(e)) return;
    setError(transportFailure(e), 0);
  }
}

// ── Startup, route and project ──────────────────────────────────────────────────────────────
function routeSource(): string {
  const v = route.query?.source;
  return typeof v === "string" ? v.trim() : "";
}

/** Defaults for THIS service: the range, and the kinds/source the route names (if any). */
function start(fromRoute: boolean) {
  booted = true;
  draft.value = {
    ...defaultChangeRange(),
    kinds: fromRoute ? kindsFromQuery(route.query?.kind) : [],
    source: fromRoute ? routeSource() : "",
  };
  void loadService();
  apply();
}

// As GateDecisionsView (P1 [88]): the FIRST project selection is STARTUP, not a switch — the route's
// `?kind=`/`?source=` must survive it. A LATER change of project is a real switch, and the filters
// reset; the service of the route then most likely belongs to the previous project and the page
// says so in one line.
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
// Another service's timeline, or a moved pre-filter, while this view stays mounted.
watch(
  () => `${String(route.params.id ?? "")}|${JSON.stringify(route.query?.kind ?? "")}|${String(route.query?.source ?? "")}`,
  () => {
    if (ws.projectId) start(true);
  },
);

const name = computed(() => service.value?.service.name || "");
const showTable = computed(() => rows.value.length > 0 && !(errorStatus.value === 401 || errorStatus.value === 403 || errorStatus.value === 404));
const appliedSummary = computed(() => {
  const bits: string[] = [];
  if (applied.value.kinds.length) bits.push(applied.value.kinds.join(", "));
  if (applied.value.source) bits.push(`source ${applied.value.source}`);
  return bits.join(" · ");
});

function byOf(g: ChangeGroup): { label: string; title: string } {
  const p = groupLatest(g);
  if (!p) return { label: "—", title: "" };
  return { label: p.actor_label, title: p.via_token ? "an API token" : "a user" };
}
function phaseAt(g: ChangeGroup, i: number): string {
  const p = g.phases[i];
  return phaseInstantLabel(p.occurred_at, i > 0 ? g.phases[i - 1].occurred_at : undefined);
}
function compareOf(g: ChangeGroup): CompareState | undefined {
  return compares.value[groupKey(g)];
}
/** The cell's state word, for `data-state`: no-terminal | loading | ok | error. */
function compareState(g: ChangeGroup): string {
  if (!terminalOf(g)) return "no-terminal";
  return compareOf(g)?.status ?? "loading";
}
/** The live decision (state, action, overridden) a change rested on, or null when aged out or absent (lib/changes.ts `decisionView`). */
function liveDecision(g: ChangeGroup): Extract<DecisionView, { kind: "live" }> | null {
  const d = decisionView(g.decision);
  return d && d.kind === "live" ? d : null;
}
/** `data-state` of the decision cell: the live STATE, `aged-out`, or nothing. */
function decisionState(g: ChangeGroup): string | undefined {
  const d = decisionView(g.decision);
  return d ? (d.kind === "live" ? d.state : "aged-out") : undefined;
}
/** The AFTER figure's tone: by the delta's SIGN and nothing else (lib/changes.ts `deltaValueClass`); plain when equal or absent. */
function afterClass(c: ChangeCompare): string {
  const d = deltaChip(c.delta);
  return d && d.sign !== 0 ? deltaValueClass(d.sign) : "";
}
/** The title behind a side's text: the sealed buckets, `sealed_through`, or the page's reason. */
function sideTitle(v: CompareSideView): string {
  if (v.kind === "figure") return `${v.buckets} sealed buckets`;
  if (v.kind === "pending") return v.sealedThrough ? `sealed through ${v.sealedThrough}` : "not yet sealed";
  return v.detail || v.reason;
}
function okCompare(g: ChangeGroup): ChangeCompare | null {
  const c = compareOf(g);
  return c && c.status === "ok" ? c.data : null;
}
function errCompare(g: ChangeGroup): string | null {
  const c = compareOf(g);
  return c && c.status === "error" ? c.text : null;
}
</script>

<template>
  <AppShell active="services" :crumbs="[ws.orgName || 'cerbix', ws.projectName || '…', 'Services', name || '…', 'Timeline']">
    <div class="mx-auto max-w-[1180px] px-[22px] pb-16 pt-[26px]" data-testid="service-changes-view">
      <RouterLink :to="{ name: 'service', params: { id: serviceId } }" class="text-[12.5px] text-ink-3 hover:text-accent" data-testid="changes-back">← back to the service</RouterLink>

      <div class="mb-[22px] mt-[10px]">
        <h1 class="text-[21px] font-semibold tracking-tight" data-testid="changes-view-title">{{ name || "…" }} · timeline</h1>
        <p class="mt-[3px] text-[13px] text-ink-3">
          Every change recorded on this service ·
          <span class="font-mono">GET …/services/{s}/changes?from&amp;to&amp;kind&amp;source</span>
          · at most {{ CHANGE_RANGE_MAX_DAYS }} days a page · the header's horizon applies to the before/after column
        </p>
      </div>

      <section class="overflow-hidden rounded border border-border bg-surface shadow-card">
        <header class="flex flex-wrap items-end gap-[10px] border-b border-border px-4 py-[13px]">
          <h2 class="mr-[6px] self-center text-[13.5px] font-semibold">Changes</h2>

          <label class="flex flex-col gap-[3px]">
            <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">From</span>
            <input
              v-model="draft.from"
              type="date"
              class="h-[30px] rounded-sm border border-border bg-surface px-[9px] font-mono text-[12.5px] text-ink outline-none focus:border-accent"
              data-testid="changes-from"
            />
          </label>
          <label class="flex flex-col gap-[3px]">
            <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">To</span>
            <input
              v-model="draft.to"
              type="date"
              class="h-[30px] rounded-sm border border-border bg-surface px-[9px] font-mono text-[12.5px] text-ink outline-none focus:border-accent"
              data-testid="changes-to"
            />
          </label>
          <span
            class="self-center pb-[6px] font-mono text-[11.5px] text-ink-3"
            :class="draftDays > CHANGE_RANGE_MAX_DAYS || draftDays <= 0 ? 'text-down' : ''"
            data-testid="changes-days"
          >
            · {{ draftDays > 0 ? draftDays : "—" }} days
          </span>

          <div class="flex flex-col gap-[3px]">
            <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Kind</span>
            <div class="flex h-[30px] items-center gap-[5px]" role="group" aria-label="Kinds" data-testid="changes-kind">
              <button
                v-for="k in CHANGE_KINDS"
                :key="k"
                type="button"
                :class="[CHIP_BASE, draft.kinds.includes(k) ? CHIP_ACC : CHIP_PLAIN, 'h-[26px] cursor-pointer hover:border-border-strong']"
                :aria-pressed="draft.kinds.includes(k)"
                :data-testid="`changes-kind-${k}`"
                :data-on="draft.kinds.includes(k) ? 'true' : undefined"
                @click="toggleKind(k)"
              >
                <i class="inline-block h-[10px] w-[10px] flex-none bg-accent" :class="draft.kinds.includes(k) ? '' : 'opacity-40'" :style="{ clipPath: kindClip(k) }" aria-hidden="true"></i>{{ kindLabel(k) }}
              </button>
            </div>
          </div>

          <label class="flex flex-col gap-[3px]">
            <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Source</span>
            <input
              v-model="draft.source"
              type="text"
              placeholder="any source"
              spellcheck="false"
              class="h-[30px] w-[150px] rounded-sm border border-border bg-surface px-[9px] font-mono text-[12.5px] text-ink outline-none placeholder:font-sans placeholder:text-ink-3 focus:border-accent"
              data-testid="changes-source-filter"
              @keydown.enter.prevent="apply"
            />
          </label>

          <button
            type="button"
            class="inline-flex h-[30px] items-center rounded-sm border border-border bg-surface px-3 text-[13px] text-ink hover:border-border-strong disabled:opacity-50"
            :disabled="loading"
            data-testid="changes-apply"
            @click="apply"
          >
            Apply
          </button>

          <div class="ml-auto flex flex-col gap-[3px]">
            <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">before/after at</span>
            <div class="inline-flex h-[30px] overflow-hidden rounded-sm border border-border" role="group" aria-label="Horizon" title="applies to every row on this page" data-testid="changes-view-horizon">
              <button
                v-for="h in HORIZONS"
                :key="h"
                type="button"
                class="border-r border-border px-[10px] text-[11.5px] last:border-r-0"
                :class="horizon === h ? 'bg-accent-weak font-semibold text-accent' : 'text-ink-3 hover:text-ink'"
                :aria-pressed="horizon === h"
                :data-testid="`changes-horizon-${h}`"
                @click="setHorizon(h)"
              >
                {{ horizonLabel(h) }}
              </button>
            </div>
          </div>
          <span class="w-full text-right text-[12px] text-ink-3">newest first · {{ PAGE_SIZE }} per page</span>
        </header>

        <p
          v-if="error"
          class="border-b border-border px-4 py-[10px] text-[13px] text-down"
          data-testid="changes-view-error"
          :data-status="errorStatus ?? undefined"
        >
          {{ error }}
        </p>

        <p v-if="loading" class="px-4 py-10 text-center text-[13px] text-ink-3">Loading…</p>

        <div v-else-if="showTable" class="overflow-x-auto">
          <table class="w-full text-[13px]" data-testid="changes-table">
            <thead>
              <tr class="text-[10.5px] uppercase tracking-[0.06em] text-ink-3">
                <th class="border-b border-border px-3 py-[10px] text-left">Kind</th>
                <th class="border-b border-border px-3 py-[10px] text-left">Ref</th>
                <th class="border-b border-border px-3 py-[10px] text-left">Phases</th>
                <th class="whitespace-nowrap border-b border-border px-3 py-[10px] text-left">Source · id</th>
                <th class="border-b border-border px-3 py-[10px] text-left">By</th>
                <th class="whitespace-nowrap border-b border-border px-3 py-[10px] text-left">Rested on</th>
                <th class="border-b border-border px-3 py-[10px] text-left">Preceded</th>
                <th class="whitespace-nowrap border-b border-border px-3 py-[10px] text-left" data-testid="changes-compare-header">Before → after · {{ horizonLabel(horizon) }}</th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="g in rows"
                :key="groupKey(g)"
                class="hover:bg-surface-2"
                data-testid="changes-row"
                :data-source="g.source"
                :data-external-id="g.external_id"
                :data-kind="g.kind"
                :data-terminal="terminalOf(g)?.phase"
              >
                <td class="border-b border-border px-3 py-[9px] align-top">
                  <span class="inline-flex items-center gap-[6px] text-[12.5px] font-medium text-ink" data-testid="changes-kind-mark">
                    <i class="inline-block h-[10px] w-[10px] flex-none bg-accent" :style="{ clipPath: kindClip(g.kind) }" aria-hidden="true"></i>{{ kindLabel(g.kind) }}
                  </span>
                </td>
                <td class="border-b border-border px-3 py-[9px] align-top font-mono text-[12.5px]">
                  <span :class="g.ref ? '' : 'text-ink-3'">{{ g.ref || "—" }}</span>
                  <a
                    v-if="g.url"
                    :href="g.url"
                    target="_blank"
                    rel="noopener noreferrer"
                    :class="[CHIP_BASE, CHIP_PLAIN, 'ml-[6px] font-mono text-[10.5px] hover:border-border-strong']"
                    title="open the run"
                    data-testid="changes-url"
                  >↗ run</a>
                </td>
                <td class="border-b border-border px-3 py-[9px] align-top">
                  <span class="inline-flex items-center text-[11.5px]" data-testid="changes-phases">
                    <template v-for="(p, i) in g.phases" :key="p.id">
                      <span v-if="i" class="px-[4px] text-ink-3" aria-hidden="true">→</span>
                      <span
                        class="border px-[8px] py-[2px] first:rounded-l-[5px] last:rounded-r-[5px]"
                        :class="[
                          i === g.phases.length - 1 ? 'bg-inset font-semibold text-ink' : 'bg-surface text-ink-2',
                          p.phase === 'failed' ? 'border-down text-down' : 'border-border',
                        ]"
                        data-testid="changes-phase"
                        :data-phase="p.phase"
                        :title="p.occurred_at"
                      >{{ phaseLabel(p.phase) }} <span class="font-mono font-normal text-ink-3 tnum">{{ phaseAt(g, i) }}</span></span>
                    </template>
                  </span>
                </td>
                <td class="whitespace-nowrap border-b border-border px-3 py-[9px] align-top">
                  <span :class="[CHIP_BASE, CHIP_PLAIN, 'font-mono text-[10.5px]']" data-testid="changes-source">{{ g.source }} · {{ g.external_id }}</span>
                </td>
                <td class="border-b border-border px-3 py-[9px] align-top font-mono text-[12.5px]" :title="byOf(g).title">{{ byOf(g).label }}</td>
                <td class="whitespace-nowrap border-b border-border px-3 py-[9px] align-top" data-testid="changes-decision" :data-state="decisionState(g)">
                  <template v-if="liveDecision(g)">
                    <span :class="[PILL_BASE, statePill(liveDecision(g)!.state).cls]" :title="liveDecision(g)!.id">
                      <span :class="[PILL_DOT, statePill(liveDecision(g)!.state).dot]"></span>{{ statePill(liveDecision(g)!.state).label }}
                    </span>
                    <span
                      v-if="liveDecision(g)!.overridden && liveDecision(g)!.action"
                      :class="[CHIP_BASE, CHIP_ACC, 'ml-[5px] font-mono text-[10.5px]']"
                      title="overridden"
                      data-testid="changes-decision-override"
                    >→ {{ liveDecision(g)!.action }}</span>
                  </template>
                  <span v-else-if="decisionView(g.decision)" :class="[CHIP_BASE, CHIP_DORM, 'font-mono text-[10.5px]']" :title="decisionView(g.decision)!.id">{{ decisionView(g.decision)!.short }} · aged out</span>
                  <span v-else class="text-ink-3">—</span>
                </td>
                <td class="border-b border-border px-3 py-[9px] align-top">
                  <template v-if="g.incidents.length">
                    <div v-for="l in g.incidents" :key="l.incident_id" class="flex flex-wrap items-center gap-[5px] whitespace-nowrap" data-testid="changes-preceded" :data-role="l.role">
                      <RouterLink
                        :to="{ name: 'incident', params: { id: l.incident_id } }"
                        class="font-mono text-[12.5px] text-accent hover:underline"
                        :title="`opened ${l.opened_at} · ${l.role === 'upstream' ? 'as a probable-root upstream' : 'on this service'}`"
                        data-testid="changes-preceded-link"
                      >{{ shortId(l.incident_id) }}</RouterLink>
                      <span :class="[CHIP_BASE, CHIP_ACC, 'font-mono text-[10.5px]']" data-testid="changes-preceded-lag">{{ lagText(l.lag_seconds) }}</span>
                    </div>
                  </template>
                  <span v-else class="text-ink-3">—</span>
                </td>
                <td class="whitespace-nowrap border-b border-border px-3 py-[9px] align-top font-mono text-[12.5px] tnum" data-testid="changes-compare" :data-state="compareState(g)">
                  <span v-if="!terminalOf(g)" :class="[CHIP_BASE, CHIP_DORM, 'font-sans']">{{ NO_TERMINAL_TEXT }}</span>
                  <template v-else-if="okCompare(g)">
                    <span :title="sideTitle(formatCompareSide(okCompare(g)!.before))" data-testid="changes-compare-before">{{ formatCompareSide(okCompare(g)!.before).text }}</span>
                    <span class="px-[4px] text-ink-3">→</span>
                    <span :class="afterClass(okCompare(g)!)" :title="sideTitle(formatCompareSide(okCompare(g)!.after))" data-testid="changes-compare-after">{{ formatCompareSide(okCompare(g)!.after).text }}</span>
                    <span
                      v-if="deltaChip(okCompare(g)!.delta)"
                      :class="[CHIP_BASE, deltaChip(okCompare(g)!.delta)!.cls, 'ml-[5px] font-mono text-[10.5px]']"
                      data-testid="changes-compare-delta"
                    >{{ deltaChip(okCompare(g)!.delta)!.text }}</span>
                  </template>
                  <span v-else-if="errCompare(g)" class="font-sans text-ink-3" :title="errCompare(g) || ''">{{ errCompare(g) }}</span>
                  <span v-else class="text-ink-3">…</span>
                </td>
              </tr>
            </tbody>
          </table>
        </div>

        <p v-else-if="!error" class="px-4 py-10 text-center text-[13px] text-ink-3" data-testid="changes-view-empty">
          No change was recorded on this service between <span class="font-mono">{{ applied.from }}</span> and <span class="font-mono">{{ applied.to }}</span><template v-if="appliedSummary"> ({{ appliedSummary }})</template>.
          Changes are recorded by a pipeline with <span class="font-mono">cerbix change record</span> — deploys, rollbacks and flag flips, each with the instant it happened — and appear here beside the facts.
        </p>

        <div class="flex flex-wrap items-center justify-center gap-x-[14px] gap-y-[6px] border-t border-border px-4 py-[11px]">
          <button
            v-if="nextCursor"
            type="button"
            class="inline-flex h-[26px] items-center rounded-sm border border-border bg-surface px-[10px] text-[12.5px] text-ink hover:border-border-strong disabled:opacity-50"
            :disabled="loading || loadingMore"
            data-testid="changes-more"
            @click="loadMore"
          >
            {{ loadingMore ? "Loading…" : `Show ${PAGE_SIZE} more` }}
          </button>
          <span class="text-[12px] text-ink-3" data-testid="changes-live-note">
            the traversal is live, as the gate ledger's: a group never appears twice; a phase recorded mid-traversal can move its group ahead of the cursor, and a client that needs a fixed set re-reads
          </span>
          <span class="w-full text-center text-[12px] text-ink-3" data-testid="changes-aged-out-note">
            a decision the ledger has already aged out is shown by id and said so — the change keeps the link, the ledger kept its retention · before/after is the reliability page's own arithmetic over sealed minutes at the horizon in the header; a group without a terminal phase has none
          </span>
        </div>
      </section>
    </div>
  </AppShell>
</template>
