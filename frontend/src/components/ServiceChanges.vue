<script setup lang="ts">
// FR-025 / AC-0165-7 part 1, built to the APPROVED mock (docs/design/mock-change-intelligence.html,
// screen 1) as D-0210 item 1 reads it into the product: the `Changes` card on the service detail,
// between `Release gate` and `Dependencies`.
//
// Four rules this card keeps:
//
//   * it is a RECORD, not a control (D14): nothing here creates, edits or deletes a change. The
//     record is the pipeline's (`cerbix change record`); the card reads GET …/changes and, per
//     terminal row, GET …/changes/compare. Every read is `project:read` (viewer+), so no role is
//     consulted here and no flag is taken from the view.
//   * the marks on the facts strip are one per TERMINAL phase, placed by `occurred_at`, kind-shaped
//     and never a state hue (invariant 19). The card does not draw a second strip: it EMITS the
//     marks (`marks`) and ServiceDetailView hands them to ServiceReliability, whose strip already
//     owns the time geometry — the same facts, one strip, no second series fetch.
//   * before/after is the reliability page's own arithmetic over sealed minutes at the horizon the
//     header chooses — ONE horizon for every row — a figure, or `pending`/`withheld` in the page's
//     own words (D8, D-0211); a started-only group has none, says so, and is never asked.
//   * `preceded`, never "caused" (D7); the decision a change rested on is the ledger's live
//     state/action or `aged out` (D11).
//
// Concurrency discipline as ServiceGate.vue (D-0210 item 7), in TWO scopes: the LIST (one
// generation and one set of AbortControllers for the first page and each "Show 10 more") and the
// COMPARISONS (their own generation, so a horizon change aborts and re-issues every row's compare
// without re-reading the list). A range change reopens both; a prop change or unmount aborts both;
// a response that lands after its generation has passed is dropped, never applied. Compares run
// through a pool of four, so a page of ten never opens ten reads at once — §5a's read permits are
// shared by the whole process.
import { computed, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { RouterLink } from "vue-router";

import { api } from "@/api/client";
import {
  CARD_PAGE_SIZE,
  DEFAULT_HORIZON,
  DEFAULT_RANGE_DAYS,
  HORIZONS,
  NO_TERMINAL_TEXT,
  RANGES,
  cliRecordLine,
  decisionView,
  defaultRange,
  deltaChip,
  describeChangeFailure,
  formatCompareSide,
  groupActors,
  COMPARE_POOL,
  groupKey,
  inPool,
  requestScope,
  groupLatest,
  horizonLabel,
  instantLabel,
  kindClip,
  kindLabel,
  lagText,
  phaseTone,
  rangeLabel,
  sortPhases,
  stripMarksOf,
  terminalOf,
  type ChangeCompare,
  type ChangeGroup,
  type ChangeGroupList,
  type ChangeHorizon,
  type ChangePhase,
  type CompareSideView,
  type DecisionView,
  type DeltaChip,
  type StripMark,
} from "@/lib/changes";
import { CHIP_ACC, CHIP_BASE, CHIP_DORM, CHIP_PLAIN, PILL_BASE, PILL_DOT, statePill } from "@/lib/gate";
import { sealedLabel } from "@/lib/services";

const props = defineProps<{
  projectId: string;
  serviceId: string;
  /** The service's slug, for the empty state's sentence. */
  serviceSlug?: string;
}>();

const emit = defineEmits<{
  /** One mark per terminal phase of the groups on the card (D14); the view forwards them to the strip. */
  marks: [marks: StripMark[]];
}>();

// Transport, the generation guard and the 10 s deadline live in `lib/changes.ts` (`requestScope`)
// so this card and the full timeline cannot drift apart on them (review [37]).
const list = requestScope();
const cmp = requestScope();

// ── The header's two controls ─────────────────────────────────────────────────────────────────
const rangeDays = ref(DEFAULT_RANGE_DAYS);
const horizon = ref<ChangeHorizon>(DEFAULT_HORIZON);
/** The clock every relative label on the card is read against; refreshed on every (re)load. */
const now = ref(new Date());

// ── The list ──────────────────────────────────────────────────────────────────────────────────
type ListStatus = "loading" | "ok" | "unavailable" | "error";
const status = ref<ListStatus>("loading");
const listError = ref("");
const listErrorStatus = ref<number | null>(null);
const groups = ref<ChangeGroup[]>([]);
const nextCursor = ref<string | null>(null);
const loadingMore = ref(false);
const moreError = ref("");
/** The request range of THIS traversal, fixed at load: every "Show 10 more" sends the same one (D6). */
let range: { from: string; to: string } | null = null;

const LIST_TEXT = {
  notFound: "This service does not exist, or you cannot see it.",
  denied: "You cannot read this service's changes.",
  fallback: "Could not read the changes.",
};

type Path = { projectID: string; serviceID: string };

async function load() {
  const gen = list.begin();
  cmp.begin();
  now.value = new Date();
  status.value = "loading";
  listError.value = "";
  listErrorStatus.value = null;
  groups.value = [];
  nextCursor.value = null;
  compares.value = {};
  moreError.value = "";
  loadingMore.value = false;
  hovered.value = "";
  // Captured once: the pair and the range this load is FOR, whatever the props say by the time it answers.
  const path: Path = { projectID: props.projectId, serviceID: props.serviceId };
  range = defaultRange(rangeDays.value, now.value);
  const { from, to } = range;
  const out = await list.request<ChangeGroupList>(gen, (signal) =>
    api.GET("/api/v1/projects/{projectID}/services/{serviceID}/changes", {
      params: { path, query: { from, to, limit: CARD_PAGE_SIZE } },
      signal,
    }),
  );
  if (out.kind === "stale") return;
  if (out.kind === "failed") {
    status.value = out.failure.status === 401 || out.failure.status === 403 ? "unavailable" : "error";
    listError.value = describeChangeFailure(out.failure, LIST_TEXT);
    listErrorStatus.value = out.failure.status;
    return;
  }
  groups.value = out.data?.items ?? [];
  nextCursor.value = out.data?.next_cursor ?? null;
  status.value = "ok";
  scheduleCompares(groups.value, cmp.gen, path);
}

/** The next page of THE SAME traversal: the same range, the cursor of the last returned group. */
async function loadMore() {
  if (!nextCursor.value || loadingMore.value || !range) return;
  const gen = list.gen;
  const path: Path = { projectID: props.projectId, serviceID: props.serviceId };
  const { from, to } = range;
  const cursor = nextCursor.value;
  loadingMore.value = true;
  moreError.value = "";
  const out = await list.request<ChangeGroupList>(gen, (signal) =>
    api.GET("/api/v1/projects/{projectID}/services/{serviceID}/changes", {
      params: { path, query: { from, to, limit: CARD_PAGE_SIZE, cursor } },
      signal,
    }),
  );
  if (out.kind === "stale") return;
  loadingMore.value = false;
  if (out.kind === "failed") {
    moreError.value = describeChangeFailure(out.failure, LIST_TEXT);
    return;
  }
  // A live traversal never returns a group twice (D6); the guard costs nothing and keeps the keys unique.
  const known = new Set(groups.value.map(groupKey));
  const fresh = (out.data?.items ?? []).filter((g) => !known.has(groupKey(g)));
  groups.value = [...groups.value, ...fresh];
  nextCursor.value = out.data?.next_cursor ?? null;
  scheduleCompares(fresh, cmp.gen, path);
}

function setRange(days: number) {
  if (days === rangeDays.value) return;
  rangeDays.value = days;
  void load();
}

// ── The comparisons: one per terminal group, at the header's horizon ──────────────────────────
type CompareCell = { state: "loading" } | { state: "ok"; data: ChangeCompare } | { state: "failed"; text: string; status: number };
const compares = ref<Record<string, CompareCell>>({});

const COMPARE_TEXT = {
  notFound: "This change is no longer on the timeline.",
  denied: "You cannot read this comparison.",
  fallback: "Could not read the comparison.",
};

/**
 * Ask the comparison for every TERMINAL group in `items` under compare generation `gen`, through
 * a pool of four. A started-only group is never asked (it would be 404 `no_terminal_phase`); its
 * row says so instead. Each answer is applied AS IT ARRIVES so one slow read never holds the rest.
 */
function scheduleCompares(items: readonly ChangeGroup[], gen: number, path: Path) {
  const queue = items.filter((g) => terminalOf(g));
  if (!queue.length) return;
  const h = horizon.value;
  const next: Record<string, CompareCell> = { ...compares.value };
  for (const g of queue) next[groupKey(g)] = { state: "loading" };
  compares.value = next;
  void inPool(
    queue,
    COMPARE_POOL,
    async (g: ChangeGroup) => {
      const out = await cmp.request<ChangeCompare>(gen, (signal) =>
        api.GET("/api/v1/projects/{projectID}/services/{serviceID}/changes/compare", {
          params: { path, query: { source: g.source, external_id: g.external_id, horizon: h } },
          signal,
        }),
      );
      if (out.kind === "stale") return;
      let cell: CompareCell;
      if (out.kind === "failed") cell = { state: "failed", text: describeChangeFailure(out.failure, COMPARE_TEXT), status: out.failure.status };
      else if (out.data) cell = { state: "ok", data: out.data };
      else cell = { state: "failed", text: "The comparison returned nothing.", status: 0 };
      compares.value = { ...compares.value, [groupKey(g)]: cell };
    },
    () => cmp.stale(gen),
  );
}

/** The horizon applies to EVERY row: abort what is in flight, forget every figure, ask again. */
function setHorizon(h: ChangeHorizon) {
  if (h === horizon.value) return;
  horizon.value = h;
  const gen = cmp.begin();
  compares.value = {};
  if (status.value === "ok") scheduleCompares(groups.value, gen, { projectID: props.projectId, serviceID: props.serviceId });
}

// ── Marks: emitted, never drawn here ──────────────────────────────────────────────────────────
/** The row under the pointer or focus; its mark on the strip above carries the focus ring. */
const hovered = ref("");
const marks = computed(() => stripMarksOf(groups.value, hovered.value, now.value));
watch(marks, (m) => emit("marks", m), { immediate: true });

// ── Lifecycle ─────────────────────────────────────────────────────────────────────────────────
onMounted(load);
watch(() => [props.projectId, props.serviceId], load);
onBeforeUnmount(() => {
  list.close();
  cmp.close();
  emit("marks", []);
});

// ── The rows, as the template reads them ──────────────────────────────────────────────────────
type CompareView =
  | { state: "no-terminal" }
  | { state: "loading" }
  | { state: "failed"; text: string; status: number }
  | { state: "ok"; before: CompareSideView; after: CompareSideView; delta: DeltaChip | null };

interface Row {
  g: ChangeGroup;
  key: string;
  terminal: ChangePhase | undefined;
  phases: ChangePhase[];
  latestId: string;
  actor: string;
  actorsTitle: string;
  decision: DecisionView | null;
  compare: CompareView;
}

const rows = computed<Row[]>(() =>
  groups.value.map((g) => {
    const key = groupKey(g);
    const terminal = terminalOf(g);
    const latest = groupLatest(g);
    const actors = groupActors(g);
    const cell = compares.value[key];
    let compare: CompareView;
    if (!terminal) compare = { state: "no-terminal" };
    else if (!cell || cell.state === "loading") compare = { state: "loading" };
    else if (cell.state === "failed") compare = { state: "failed", text: cell.text, status: cell.status };
    else compare = { state: "ok", before: formatCompareSide(cell.data.before), after: formatCompareSide(cell.data.after), delta: deltaChip(cell.data.delta) };
    return {
      g,
      key,
      terminal,
      phases: sortPhases(g.phases),
      latestId: latest?.id ?? "",
      actor: latest?.actor_label ?? "",
      actorsTitle: actors.length > 1 ? `phases by ${actors.join(", ")}` : latest?.via_token ? "an API token" : "a user",
      decision: decisionView(g.decision),
      compare,
    };
  }),
);

const countText = computed(() => {
  if (status.value === "loading") return "…";
  if (status.value !== "ok") return "—";
  return `${groups.value.length}${nextCursor.value ? "+" : ""}`;
});
const origin = typeof window !== "undefined" && window.location ? window.location.origin : "";
const cli = computed(() => cliRecordLine(props.projectId, props.serviceId, origin));
const timelinePath = computed(() => `/services/${props.serviceId}/changes`);

function phaseClass(p: ChangePhase, r: Row): string {
  const tone = phaseTone(p.phase);
  const on = r.latestId === p.id ? "bg-inset font-semibold text-ink" : "bg-surface text-ink-2";
  if (tone === "down") return `${on} border-down text-down`;
  if (tone === "muted") return `${on} border-border text-ink-3`;
  return `${on} border-border`;
}
function compareTo(g: ChangeGroup) {
  return { name: "service-change-compare", params: { id: props.serviceId }, query: { source: g.source, external_id: g.external_id, horizon: horizon.value } };
}

// ── Shared classes (token-mapped; see tailwind.config.js) — the compositions ServiceGate.vue uses ──
const BTN_SM = "inline-flex h-[26px] items-center rounded-sm border border-border bg-surface px-[10px] text-[12.5px] text-ink-2 hover:border-border-strong hover:text-ink disabled:cursor-not-allowed disabled:opacity-50";
const chipPlain = `${CHIP_BASE} ${CHIP_PLAIN}`;
const chipMono = `${chipPlain} font-mono text-[10.5px]`;
const chipDorm = `${CHIP_BASE} ${CHIP_DORM}`;
const chipDormMono = `${chipDorm} font-mono text-[10.5px]`;
const chipAcc = `${CHIP_BASE} ${CHIP_ACC} font-mono text-[10.5px]`;
const LBL = "text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3";
const ERR_BOX = "flex items-start gap-[9px] rounded-sm border border-down bg-down-weak px-3 py-[9px] text-[13px] text-down";
const ROW = "flex flex-wrap items-center gap-[9px] border-b border-border px-4 py-[11px] last:border-b-0";
const SEG = "inline-flex overflow-hidden rounded-sm border border-border";
const SEG_ITEM = "border-r border-border px-[10px] py-[3px] text-[11.5px] last:border-r-0";
const SEG_ON = "bg-accent-weak font-semibold text-accent";
const SEG_OFF = "text-ink-3 hover:text-ink";
const PHASE = "inline-flex items-center gap-[4px] border px-2 py-[2px] text-[11.5px]";
const MARK = "inline-flex items-center gap-[6px] text-[12.5px] font-medium text-ink";
</script>

<template>
  <section class="mb-4 overflow-hidden rounded border border-border bg-surface shadow-card" data-testid="service-changes">
    <header class="flex flex-wrap items-center gap-[10px] border-b border-border px-4 py-[10px]">
      <h2 class="text-[13.5px] font-semibold">Changes</h2>
      <span :class="chipMono" data-testid="changes-count" :data-count="status === 'ok' ? groups.length : undefined">{{ rangeLabel(rangeDays) }} · {{ countText }}</span>
      <div class="flex-1"></div>
      <span :class="LBL">range</span>
      <span :class="SEG" role="group" aria-label="range" data-testid="changes-range" :data-days="rangeDays">
        <button
          v-for="r in RANGES"
          :key="r.days"
          type="button"
          :class="[SEG_ITEM, r.days === rangeDays ? SEG_ON : SEG_OFF]"
          :aria-pressed="r.days === rangeDays"
          :data-testid="`changes-range-${r.days}d`"
          :data-days="r.days"
          @click="setRange(r.days)"
        >{{ r.label }}</button>
      </span>
      <span :class="LBL" class="ml-2">before/after at</span>
      <span :class="SEG" role="group" aria-label="before/after horizon" title="applies to every row on this card" data-testid="changes-horizon" :data-horizon="horizon">
        <button
          v-for="h in HORIZONS"
          :key="h"
          type="button"
          :class="[SEG_ITEM, h === horizon ? SEG_ON : SEG_OFF]"
          :aria-pressed="h === horizon"
          :data-testid="`changes-horizon-${h}`"
          :data-horizon="h"
          @click="setHorizon(h)"
        >{{ horizonLabel(h) }}</button>
      </span>
      <RouterLink :to="timelinePath" class="font-mono text-[11.5px] text-accent hover:underline" data-testid="changes-timeline-link">full timeline →</RouterLink>
    </header>

    <p v-if="status === 'loading'" class="px-4 py-6 text-[13px] text-ink-3" data-testid="changes-loading">Loading…</p>

    <!-- 401/403: one line, no controls, nothing else asked. -->
    <p v-else-if="status === 'unavailable'" class="px-4 py-6 text-[13px] text-ink-3" data-testid="changes-unavailable" :data-status="listErrorStatus ?? undefined">
      {{ listError }}
    </p>

    <!-- A refusal or the transport's own words, verbatim, with the way back. -->
    <div v-else-if="status === 'error'" class="p-4">
      <div :class="ERR_BOX" role="alert">
        <span aria-hidden="true">⚠</span>
        <span class="flex-1" data-testid="changes-error" :data-status="listErrorStatus ?? undefined">{{ listError }}</span>
        <button type="button" class="flex-none rounded-[5px] border border-down bg-surface px-2 py-[2px] text-[11.5px]" data-testid="changes-retry" @click="load">Retry</button>
      </div>
    </div>

    <!-- The empty state: nothing recorded in the range, and the exact command that records one. -->
    <div v-else-if="!rows.length" class="flex flex-wrap items-start gap-[14px] p-4" data-testid="changes-empty">
      <p class="min-w-[260px] flex-1 text-[13px] text-ink-2">
        No pipeline has recorded a change on
        <template v-if="serviceSlug"><span class="font-mono">{{ serviceSlug }}</span></template><template v-else>this service</template>
        in the {{ rangeLabel(rangeDays) }}. Changes are recorded by a pipeline with
        <span class="font-mono">cerbix change record</span> — deploys, rollbacks and flag flips, each with the instant it
        happened — and appear here beside the facts.
      </p>
      <pre class="tnum min-w-[300px] flex-1 overflow-x-auto rounded-sm border border-border bg-inset px-[14px] py-3 font-mono text-[12.5px] leading-[1.55] text-ink" data-testid="changes-cli">{{ cli }}</pre>
    </div>

    <div v-else class="flex flex-col gap-[14px] p-4">
      <!-- The strip is the reliability card's; this line says where the marks went. -->
      <p class="text-[12px] text-ink-3" data-testid="changes-marks-note" :data-marks="marks.length">
        <template v-if="marks.length">{{ marks.length }} {{ marks.length === 1 ? "mark" : "marks" }} on the reliability timeline above</template>
        <template v-else>no mark on the reliability timeline above yet</template>
        — one per terminal phase, placed by <span class="font-mono">occurred_at</span>; a change that has only
        <span class="font-mono">started</span> is listed but not yet marked.
      </p>

      <div class="rounded-[7px] border border-border">
        <div
          v-for="r in rows"
          :key="r.key"
          :class="[ROW, hovered === r.key ? 'bg-accent-weak' : '']"
          data-testid="changes-group"
          :data-source="r.g.source"
          :data-external-id="r.g.external_id"
          :data-kind="r.g.kind"
          :data-terminal="r.terminal?.phase"
          @mouseenter="hovered = r.key"
          @mouseleave="hovered = ''"
          @focusin="hovered = r.key"
          @focusout="hovered = ''"
        >
          <!-- Kind: shape + text, in the accent — never a state hue. -->
          <span :class="MARK" data-testid="changes-kind" :data-kind="r.g.kind">
            <i class="inline-block h-[10px] w-[10px] flex-none bg-accent" :style="{ clipPath: kindClip(r.g.kind) }" aria-hidden="true"></i>{{ kindLabel(r.g.kind) }}
          </span>
          <span class="font-mono text-[13.5px] font-medium" :class="r.g.ref ? '' : 'text-ink-3'" data-testid="changes-ref" :title="r.g.ref ? undefined : 'no ref was given'">{{ r.g.ref || "—" }}</span>

          <!-- Phases in the domain's order; the latest is "on"; failed reads down, cancelled muted. -->
          <span class="inline-flex items-center text-[11.5px]" data-testid="changes-phases">
            <template v-for="(p, i) in r.phases" :key="p.id">
              <span v-if="i" class="px-1 text-ink-3" aria-hidden="true">→</span>
              <span
                :class="[PHASE, phaseClass(p, r), i === 0 ? 'rounded-l-[5px]' : '', i === r.phases.length - 1 ? 'rounded-r-[5px]' : '']"
                data-testid="changes-phase"
                :data-phase="p.phase"
                :title="`${sealedLabel(p.occurred_at)} · ${p.actor_label}`"
              >{{ p.phase }} <span class="font-mono font-normal text-ink-3">{{ instantLabel(p.occurred_at, now) }}</span></span>
            </template>
          </span>

          <span :class="chipMono" data-testid="changes-source">{{ r.g.source }} · {{ r.g.external_id }}</span>
          <a v-if="r.g.url" :href="r.g.url" target="_blank" rel="noopener noreferrer" :class="chipMono" title="open the run" data-testid="changes-run-link">↗ run</a>
          <span class="flex-1"></span>
          <span :class="chipMono" data-testid="changes-actor" :title="r.actorsTitle">{{ r.actor }}</span>

          <!-- The row's second line: what it rested on, what it preceded, what followed it. -->
          <span class="flex w-full flex-wrap items-center gap-x-[6px] gap-y-[4px] text-[12.5px] text-ink-3">
            <template v-if="r.decision">
              <span
                class="inline-flex flex-wrap items-center gap-[6px]"
                data-testid="changes-decision"
                :data-decision-id="r.decision.id"
                :data-state="r.decision.kind === 'live' ? r.decision.state : undefined"
                :data-aged-out="r.decision.kind === 'aged_out' ? 'true' : undefined"
              >
                <template v-if="r.decision.kind === 'live'">
                  rested on decision
                  <RouterLink :to="{ name: 'gate-decision', params: { id: r.decision.id } }" class="font-mono text-ink hover:text-accent" :title="r.decision.id">{{ r.decision.short }}</RouterLink>
                  <span :class="[PILL_BASE, statePill(r.decision.state).cls]">
                    <span :class="[PILL_DOT, statePill(r.decision.state).dot]"></span>{{ statePill(r.decision.state).label }}
                  </span>
                  <span v-if="r.decision.overridden && r.decision.action" :class="chipAcc" data-testid="changes-decision-override">override → {{ r.decision.action }}</span>
                </template>
                <template v-else>
                  rested on decision <span :class="chipDormMono" :title="r.decision.id">{{ r.decision.short }} · aged out</span>
                </template>
              </span>
              <span aria-hidden="true">·</span>
            </template>

            <template v-for="l in r.g.incidents" :key="l.incident_id">
              <span class="inline-flex flex-wrap items-center gap-[5px] text-ink-2" data-testid="changes-preceded" :data-incident-id="l.incident_id" :data-role="l.role" :data-lag="l.lag_seconds">
                <b class="font-semibold">preceded</b>
                <RouterLink :to="{ name: 'incident', params: { id: l.incident_id } }" class="text-accent hover:underline" :title="`incident ${l.incident_id} · opened ${sealedLabel(l.opened_at)}`">incident {{ l.incident_id.slice(0, 8) }}</RouterLink>
                by <span class="font-mono">{{ lagText(l.lag_seconds) }}</span>
                <span v-if="l.role === 'upstream'" :class="chipDormMono">upstream · probable root</span>
              </span>
              <span aria-hidden="true">·</span>
            </template>

            <!-- before/after at the header's horizon: a figure, pending, withheld — or unavailable. -->
            <span class="inline-flex flex-wrap items-center gap-[5px]" data-testid="changes-compare" :data-state="r.compare.state" :data-horizon="horizon">
              <template v-if="r.compare.state === 'no-terminal'">
                <span :class="chipDorm">{{ NO_TERMINAL_TEXT }}</span>
              </template>
              <template v-else-if="r.compare.state === 'loading'">
                before/after at {{ horizonLabel(horizon) }}: <span class="font-mono">…</span>
              </template>
              <template v-else-if="r.compare.state === 'failed'">
                before/after at {{ horizonLabel(horizon) }}:
                <span class="text-down" data-testid="changes-compare-error" :data-status="r.compare.status">{{ r.compare.text }}</span>
              </template>
              <template v-else>
                before/after at {{ horizonLabel(horizon) }}:
                <span class="font-mono text-ink" data-testid="changes-compare-before" :data-kind="r.compare.before.kind">{{ r.compare.before.text }}</span>
                <span aria-hidden="true">→</span>
                <span class="font-mono text-ink" data-testid="changes-compare-after" :data-kind="r.compare.after.kind">{{ r.compare.after.text }}</span>
                <span v-if="r.compare.delta" :class="[CHIP_BASE, r.compare.delta.cls, 'font-mono text-[10.5px]']" data-testid="changes-compare-delta" :data-sign="r.compare.delta.sign">{{ r.compare.delta.text }}</span>
              </template>
              <RouterLink v-if="r.terminal" :to="compareTo(r.g)" class="font-mono text-[11.5px] text-accent hover:underline" data-testid="changes-compare-link">before/after →</RouterLink>
            </span>
          </span>
        </div>
      </div>

      <div class="flex flex-wrap items-center gap-[10px]">
        <button v-if="nextCursor" type="button" :class="BTN_SM" :disabled="loadingMore" data-testid="changes-more" @click="loadMore">
          {{ loadingMore ? "Loading…" : "Show 10 more" }}
        </button>
        <span v-if="moreError" class="text-[12px] text-down" data-testid="changes-more-error">{{ moreError }}</span>
        <span class="text-[12px] text-ink-3">
          before/after is the reliability page's own arithmetic over sealed minutes at the horizon chosen in the header
          (every row); a group without a terminal phase has none (<span class="font-mono">no_terminal_phase</span>).
        </span>
      </div>
    </div>
  </section>
</template>
