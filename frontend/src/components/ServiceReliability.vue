<script setup lang="ts">
// FR-021 phase 2, changeset 3 (iter-0144) + the honesty-gap closure (iter-0145): the
// reporting surface, against the approved mock (spec §12.2, screens 2–3). The API payloads
// already carry every honesty verdict; this component renders them and INVENTS NOTHING —
// an absent number is a dash with the payload's own reason, a window spanning definition
// revisions is the ∅ banner with segments only, and the live signal is its own explicitly
// unstable card, never a second percentage.
//
// Tenant/context discipline ([218] P0): every async assignment is gated on a load
// GENERATION captured before the first await — a delayed response from project/service A
// must never land in B's screen — and the whole state (report, health, series, editor,
// draft, errors) resets on every context or window change, so a Save can only ever act on
// the context it was typed in.
//
// Transport discipline ([221] P1-1): every request is caught at ITS OWN boundary — a
// rejected fetch (offline, connection reset; openapi-fetch rethrows those) is handled
// exactly like an HTTP error payload, and only the report request may set the primary
// error. While health has neither answered nor failed, the card renders PENDING — a
// product pill is never synthesized out of an unanswered request.
import { computed, onBeforeUnmount, onMounted, ref, watch } from "vue";

import { api } from "@/api/client";
import type { components } from "@/api/schema";
import ReliabilityStrip, { type ReliabilityTick, type StripMark } from "@/components/ReliabilityStrip.vue";
import { canonicalObjective } from "@/lib/objective";

type Report = components["schemas"]["ServiceWindowReport"];
type Health = components["schemas"]["ServiceHealthNow"];
type SeriesPoint = components["schemas"]["ReliabilitySeriesPoint"];
type Segment = Report["segments"][number];
// Lax response shape shared by every boundary: a rejected fetch is normalized into the
// same { error } form an HTTP error arrives in.
type Res<T> = { data?: T; error?: unknown };

const props = defineProps<{
  projectId: string;
  serviceId: string;
  canWrite: boolean;
  hasSli: boolean;
  /**
   * FR-025 (D14, D-0210 item 1): change marks for the MAIN timeline strip — one per terminal
   * phase, produced by ServiceChanges.vue and handed down by the view. This card fetches nothing
   * for them; the strip places them over its own time geometry. Segment strips carry none.
   */
  marks?: StripMark[];
}>();

const windows = ["24h", "7d", "30d", "90d"] as const;
const win = ref<(typeof windows)[number]>("30d");

const report = ref<Report | null>(null);
const health = ref<Health | null>(null);
const points = ref<SeriesPoint[]>([]);
// Per-segment series, keyed by segKey ([221] P1-2): each segment's strip comes from its
// OWN exact [from,to) request, so the server filters buckets BEFORE grouping and a rollup
// step straddling a reconstruction boundary can never leak into both parts. null marks a
// transport failure for that one segment.
const segmentPoints = ref<Record<string, SeriesPoint[] | null>>({});
const loading = ref(true);
const error = ref("");
// Subordinate transports fail SEPARATELY ([218] P1-1): a failed health request is not the
// genuine categorical unknown, and a failed series request is not an empty timeline.
const healthError = ref(false);
const seriesError = ref(false);

let loadGen = 0;

function segKey(seg: Segment): string {
  return `${seg.epoch_id}|${seg.from}|${seg.declared_reconstruction}`;
}

function resetContext() {
  report.value = null;
  health.value = null;
  points.value = [];
  segmentPoints.value = {};
  error.value = "";
  healthError.value = false;
  seriesError.value = false;
  editingObjective.value = false;
  objectiveDraft.value = "";
  objectiveError.value = "";
  savingObjective.value = false;
}

// Normalize a rejected fetch into the { error } shape so every consumer below handles the
// two failure forms identically at its own boundary.
function guarded<T>(p: Promise<Res<T>>): Promise<Res<T>> {
  return p.catch(() => ({ data: undefined, error: {} }));
}

async function load() {
  const gen = ++loadGen;
  resetContext();
  loading.value = true;
  const path = { projectID: props.projectId, serviceID: props.serviceId };

  // Health is an INDEPENDENT generation-gated task ([228] P1-1): it neither gates the
  // timeline nor waits for the report, and it settles honestly — into pills or into its
  // error state — even when the report itself fails. guarded() means this can never be an
  // unhandled rejection.
  void (async () => {
    const h = await guarded<Health>(
      api.GET("/api/v1/projects/{projectID}/services/{serviceID}/health", { params: { path } }),
    );
    if (gen !== loadGen) return;
    if (h.error || !h.data) healthError.value = true;
    else health.value = h.data;
  })();

  // The report — the ONLY owner of the primary error. A subordinate failure must never
  // hide an intact report behind this state.
  const rep = await guarded<Report>(
    api.GET("/api/v1/projects/{projectID}/services/{serviceID}/reliability", {
      params: { path, query: { window: win.value } },
    }),
  );
  if (gen !== loadGen) return;
  loading.value = false;
  if (rep.error || !rep.data) {
    error.value = "Could not load the reliability report.";
    return;
  }
  report.value = rep.data;

  const step = win.value === "24h" ? "hour" : "day";
  const seriesGET = (from: string, to: string) =>
    guarded<{ points?: SeriesPoint[] }>(
      api.GET("/api/v1/projects/{projectID}/services/{serviceID}/reliability/series", {
        params: { path, query: { from, to, step } },
      }),
    );

  // The main timeline: the sealed window PLUS the provisional tail ([218] P1-2). The
  // report window ends at sealed_through by contract, but the approved motif draws the
  // buckets AFTER sealed_through at reduced opacity — so the tail is its own bounded
  // request, [sealed_through, min(ceil(as_of to the step), sealed_through + 90d)); a
  // single combined request could exceed the API's 90d span cap. Either request failing
  // (payload or transport) owns seriesError.
  if (rep.data.sealed_through && rep.data.from && rep.data.to) {
    const stepMs = step === "hour" ? 3600_000 : 86400_000;
    const sealedMs = Date.parse(rep.data.sealed_through);
    const tailEndMs = Math.min(
      Math.ceil(Date.parse(rep.data.as_of) / stepMs) * stepMs,
      sealedMs + 90 * 86400_000,
    );
    const sealedReq = seriesGET(rep.data.from, rep.data.to);
    const tailReq: Promise<Res<{ points?: SeriesPoint[] }>> =
      tailEndMs > sealedMs
        ? seriesGET(rep.data.sealed_through, new Date(tailEndMs).toISOString())
        : Promise.resolve({ data: { points: [] as SeriesPoint[] }, error: undefined });
    const [sealedRes, tailRes] = await Promise.all([sealedReq, tailReq]);
    if (gen !== loadGen) return;
    if (sealedRes.error || !sealedRes.data) {
      seriesError.value = true;
    } else {
      // Deterministic merge: the sealed window first, then the tail — both arrive ordered
      // from the API and the requested ranges are disjoint by construction.
      if (tailRes.error) seriesError.value = true;
      const tail = tailRes.error || !tailRes.data ? [] : (tailRes.data.points ?? []);
      points.value = [...(sealedRes.data.points ?? []), ...tail];
    }
  }

  // Per-segment strips ([221] P1-2, bounded per [228] P1-2). A UNIQUE-epoch segment needs
  // no request at all: the API already splits series points at epoch boundaries, so the
  // epoch key alone slices the fetched global series exactly (provisional tail excluded —
  // segments are sealed ranges by construction). Only the AMBIGUOUS same-epoch
  // reconstruction parts — where the series carries no discriminator — get exact
  // [from,to) requests (SQL filters buckets before grouping), through a small worker pool
  // (bound 4), each result assigned AS IT COMPLETES so one hung request cannot suppress
  // the strips that already answered.
  const segs = rep.data.segments ?? [];
  const renderSegments =
    segs.length > 0 && (rep.data.aggregate_withheld === "spans_definition_revisions" || segs.length > 1);
  if (renderSegments) {
    const epochCount: Record<string, number> = {};
    for (const seg of segs) epochCount[seg.epoch_id] = (epochCount[seg.epoch_id] || 0) + 1;
    const map: Record<string, SeriesPoint[] | null> = {};
    const exact: Segment[] = [];
    for (const seg of segs) {
      if (epochCount[seg.epoch_id] === 1)
        // The global slice inherits the global verdict: if the main series failed, this
        // strip is a transport failure too, never a legitimately empty strip.
        map[segKey(seg)] = seriesError.value
          ? null
          : points.value.filter((p) => p.epoch_id === seg.epoch_id && !p.provisional);
      else exact.push(seg);
    }
    segmentPoints.value = map;
    let next = 0;
    const worker = async () => {
      for (;;) {
        const i = next++;
        if (i >= exact.length) return;
        const seg = exact[i];
        const r = await seriesGET(seg.from, seg.to);
        if (gen !== loadGen) return;
        segmentPoints.value = {
          ...segmentPoints.value,
          [segKey(seg)]: r.error || !r.data ? null : (r.data.points ?? []),
        };
      }
    };
    for (let w = 0; w < Math.min(4, exact.length); w++) void worker();
  }
}
onMounted(load);
watch(() => [props.projectId, props.serviceId, win.value], load);
// Unmount invalidates the generation too: any still-pending load/save continuation from
// this instance returns early instead of touching torn-down state.
onBeforeUnmount(() => {
  loadGen++;
});

// ── Honesty states ────────────────────────────────────────────────────────────
// The payload's own vocabulary, spelled for an operator. Every absent number keeps its
// reason next to the dash; nothing is ever rendered as 100%/0× out of an empty denominator.
const reasonText: Record<string, string> = {
  no_sli: "No reliability inputs declared — no SLO, and never 100%.",
  nothing_sealed: "Nothing is sealed yet; numbers appear when the first buckets seal.",
  window_precedes_materialization_era: "The window reaches before this service's history begins.",
  storage_gap: "Stored buckets are missing inside the window; the surviving rows cannot vouch for it.",
  zero_decidable_time: "Nothing was measured in this window — never rendered as 100%.",
  decidable_coverage_below_min: "Measured coverage is below the supported minimum (0.95); the number is partial.",
  spans_definition_revisions: "What availability means changed inside this window.",
  no_objective: "No objective is set for this window.",
  sealed_through_behind_window: "the sealed watermark has not reached this window yet",
  nothing_measured: "nothing was measured in this window",
};
function reasonOf(code?: string): string {
  return (code && reasonText[code]) || code || "";
}

const spansRevisions = computed(() => report.value?.aggregate_withheld === "spans_definition_revisions");
const showNumbers = computed(
  () => !!report.value && (report.value.status === "ok" || report.value.status === "partial") && !spansRevisions.value,
);

function pct(v?: number | null): string {
  return v == null ? "—" : `${v.toFixed(3).replace(/\.?0+$/, "")}%`;
}
function fracPct(v?: number | null): string {
  return v == null ? "—" : `${(v * 100).toFixed(1).replace(/\.0$/, "")}%`;
}
function burnLabel(b: NonNullable<Report["burn"]>[number]): string {
  return b.rate == null ? "—" : `${b.rate.toFixed(1).replace(/\.0$/, "")}×`;
}
function dayLabel(iso?: string): string {
  if (!iso) return "";
  const d = new Date(iso);
  return `${d.getUTCDate().toString().padStart(2, "0")}.${(d.getUTCMonth() + 1).toString().padStart(2, "0")}.${d.getUTCFullYear()}`;
}

// ── Timeline ticks from the merged series ───────────────────────────────────
// Worst-signal colouring per tick, TIME-WEIGHTED by the point's bucket count ([218] P1-3):
// the API deliberately splits a rollup step at epoch/revision/provisional boundaries, so a
// 6-bucket boundary fragment must not occupy the width of a 24-bucket day. A point
// intersecting an active repair range is MASKED with the work encoding rather than
// presented as data ([218] P1-4, §12.1).
function stepMsOf(): number {
  return win.value === "24h" ? 3600_000 : 86400_000;
}
function intersectsRepair(startMs: number, endMs: number): boolean {
  const ranges = report.value?.repairing ?? [];
  if (!ranges.length) return false;
  return ranges.some((r) => Date.parse(r.from) < endMs && startMs < Date.parse(r.to));
}
// clip: the point's REAL extent. `start` is the truncated rollup step start, so a point
// from an exact segment request may describe only part of that step — the repair
// intersection must use [max(stepStart, clip.from), min(stepStart+step, clip.to)) or a
// repair confined to one reconstruction half would mask the other half too ([228] P1-3).
function tickOf(
  p: SeriesPoint,
  prevEpoch: string,
  prevRevision: string,
  clip?: { fromMs: number; toMs: number },
): ReliabilityTick {
  const d = p.durations;
  const stepStart = Date.parse(p.start);
  const startMs = clip ? Math.max(stepStart, clip.fromMs) : stepStart;
  const endMs = clip ? Math.min(stepStart + stepMsOf(), clip.toMs) : stepStart + stepMsOf();
  let state: ReliabilityTick["state"] = "none";
  // The ORDER is §9.1's display rule, not a convenience: bad, then UNKNOWN, then good, then
  // excluded. Unknown outranking good is the half that matters and the half this once had
  // backwards — a step that was partly unmeasured rendered green because some of it happened
  // to be good, which is the picture claiming to have seen an hour it never saw. The rule is
  // deliberately pessimistic for the same reason a one-second outage colours the whole tick:
  // the timeline may overstate a problem, never hide one, and the exact number lives in the
  // figure underneath.
  if (intersectsRepair(startMs, endMs)) state = "repairing";
  else if ((d.BadUs ?? 0) > 0) state = "bad";
  else if ((d.UnknownUs ?? 0) > 0) state = "unknown";
  else if ((d.GoodUs ?? 0) > 0) state = "good";
  else if ((d.ExcludedUs ?? 0) > 0) state = "excluded";
  return {
    state,
    weight: p.buckets || 1,
    provisional: p.provisional,
    revisionBoundary: prevRevision !== "" && p.revision_id !== prevRevision,
    epochBoundary: prevEpoch !== "" && p.epoch_id !== prevEpoch && p.revision_id === prevRevision,
    // The tick's real extent, so the strip can place a change mark by its instant (FR-025).
    startMs,
    endMs,
  };
}
const ticks = computed<ReliabilityTick[]>(() => {
  const out: ReliabilityTick[] = [];
  let prevEpoch = "";
  let prevRevision = "";
  for (const p of points.value) {
    out.push(tickOf(p, prevEpoch, prevRevision));
    prevEpoch = p.epoch_id;
    prevRevision = p.revision_id;
  }
  return out;
});
const provisionalBuckets = computed(() =>
  points.value.filter((p) => p.provisional).reduce((sum, p) => sum + (p.buckets || 0), 0),
);
const repairingMasked = computed(() =>
  points.value.some((p) => {
    const s = Date.parse(p.start);
    return intersectsRepair(s, s + stepMsOf());
  }),
);

// The segment's own strip: a unique-epoch segment is the exact epoch slice of the global
// series; a same-epoch reconstruction part comes from its own exact request ([228] P1-2).
// Ticks are CLIPPED to the segment's [from,to) so a repair confined to one half never
// masks the other ([228] P1-3). An entry that has not answered yet is PENDING, a null
// entry is that segment's own transport failure.
function segmentStrip(seg: Segment): { state: "pending" | "failed" | "ready"; ticks: ReliabilityTick[] } {
  const pts = segmentPoints.value[segKey(seg)];
  if (pts === undefined) return { state: "pending", ticks: [] };
  if (pts === null) return { state: "failed", ticks: [] };
  const clip = { fromMs: Date.parse(seg.from), toMs: Date.parse(seg.to) };
  return { state: "ready", ticks: pts.map((p) => tickOf(p, "", "", clip)) };
}

// ── The objective editor: the ONE client rule (lib/objective.ts, D-0165) ────────
// A STORED objective stays editable ([218] P1-5, §11.3 — a mutable current-view
// parameter): the editor opens prefilled with the canonical current value.
const editingObjective = ref(false);
const objectiveDraft = ref("");
const objectiveError = ref("");
const savingObjective = ref(false);
function openObjective() {
  objectiveDraft.value = report.value?.objective != null ? String(report.value.objective) : "";
  objectiveError.value = "";
  editingObjective.value = true;
}
async function saveObjective() {
  // v-model on <input type="number"> may hand back a number, not a string — normalize
  // before any string ops (the same trap SlaView documents).
  const raw = String(objectiveDraft.value ?? "").trim();
  const objective = raw ? canonicalObjective(Number(raw)) : null;
  if (objective === null) {
    objectiveError.value = "Enter a target above 0 and below 100 (max 99.9999, e.g. 99.9).";
    return;
  }
  objectiveError.value = "";
  savingObjective.value = true;
  // The path/window are read AT CLICK TIME and the response is gated on the generation: a
  // late response from a previous context must not touch this screen's editor state. The
  // PUT is caught at its own boundary too — a rejected fetch is the same failure as an
  // error payload, never an unhandled rejection with a stuck Save button.
  const gen = loadGen;
  const res = await guarded(
    api.PUT("/api/v1/projects/{projectID}/services/{serviceID}/sla-target", {
      params: { path: { projectID: props.projectId, serviceID: props.serviceId } },
      body: { objective, window: win.value },
    }),
  );
  if (gen !== loadGen) return;
  savingObjective.value = false;
  if (res.error) {
    objectiveError.value = (res.error as { error?: string })?.error || "Could not save the objective.";
    return;
  }
  editingObjective.value = false;
  await load();
}

const pillClass: Record<string, string> = {
  healthy: "border-up/50 text-up",
  ok: "border-up/50 text-up",
  degraded: "border-degraded/50 text-degraded",
  down: "border-down/50 text-down",
  failing: "border-down/50 text-down",
  unknown: "border-border text-ink-3",
};
</script>

<template>
  <div>
    <!-- The live signal: its own card, two layers, never merged, never a percentage. -->
    <section class="mb-4 overflow-hidden rounded border border-border bg-surface shadow-card" data-testid="svc-health">
      <header class="flex items-center border-b border-border px-4 py-[10px]">
        <h2 class="text-[13.5px] font-semibold">Health</h2>
        <div class="flex-1" />
        <span class="font-mono text-[11px] text-ink-3">current · unstable by definition</span>
      </header>
      <p v-if="healthError" class="p-4 text-[12.5px] text-down" data-testid="svc-health-error">
        The health request failed — a transport problem, not an UNKNOWN state.
      </p>
      <!-- No answer yet is PENDING, never a product pill: unknown is a real categorical
           state the server computes, not a placeholder ([221] P1-1). -->
      <p v-else-if="!health" class="p-4 text-[12.5px] text-ink-3" data-testid="svc-health-pending">
        Checking the live signal…
      </p>
      <div v-else class="grid gap-4 p-4 sm:grid-cols-2">
        <div class="flex flex-col gap-[7px] sm:border-r sm:border-border sm:pr-4">
          <span class="text-[11px] uppercase tracking-wide text-ink-3">SLI status — customer-facing</span>
          <span
            class="inline-flex w-fit items-center rounded-full border px-[10px] py-[2px] text-[12px] font-medium capitalize"
            :class="pillClass[health.sli]"
            data-testid="svc-health-sli"
          >{{ health.sli }}</span>
          <span class="text-[12px] text-ink-3">Derived from the declared reliability inputs only.</span>
        </div>
        <div class="flex flex-col gap-[7px]">
          <span class="text-[11px] uppercase tracking-wide text-ink-3">Diagnostics — operational context</span>
          <span
            class="inline-flex w-fit items-center rounded-full border px-[10px] py-[2px] text-[12px] font-medium capitalize"
            :class="pillClass[health.diagnostics]"
            data-testid="svc-health-diag"
          >{{ health.diagnostics }}</span>
          <span v-if="health.failing_monitors?.length" class="font-mono text-[12px] text-ink-2">
            {{ health.failing_monitors.join(", ") }} failing — not a reliability input unless declared in the SLI.
          </span>
          <span v-else class="text-[12px] text-ink-3">Every monitor on this service, whether or not it is in the SLI.</span>
        </div>
      </div>
    </section>

    <!-- The report: one window, every honesty state. -->
    <section class="mb-4 overflow-hidden rounded border border-border bg-surface shadow-card" data-testid="svc-reliability">
      <header class="flex flex-wrap items-center gap-2 border-b border-border px-4 py-[10px]">
        <h2 class="text-[13.5px] font-semibold">Reliability</h2>
        <span v-if="report?.sealed_through" class="rounded-xs border border-border px-[7px] py-px font-mono text-[11px] text-ink-3">
          window [sealed_through − {{ win }}, sealed_through)
        </span>
        <div class="flex-1" />
        <div class="flex overflow-hidden rounded-sm border border-border">
          <button
            v-for="w in windows"
            :key="w"
            type="button"
            class="px-[10px] py-[4px] font-mono text-[11.5px]"
            :class="w === win ? 'bg-inset text-ink' : 'text-ink-3 hover:text-ink'"
            :data-testid="`svc-window-${w}`"
            @click="win = w"
          >{{ w }}</button>
        </div>
      </header>

      <div class="p-4">
        <p v-if="error" class="rounded border border-down/40 bg-down-weak p-3 text-[13px] text-down">{{ error }}</p>
        <p v-else-if="loading" class="text-[13px] text-ink-3">Loading…</p>

        <template v-else-if="report">
          <!-- ∅: a window spanning definition revisions has NO aggregate, not even labelled. -->
          <div
            v-if="spansRevisions"
            class="mb-4 flex items-start gap-3 rounded border border-border bg-surface-2 p-4"
            data-testid="svc-no-total"
          >
            <span class="font-mono text-[20px] leading-none text-ink-3">∅</span>
            <div>
              <p class="text-[13px] font-semibold">No single availability number for this window</p>
              <p class="mt-[3px] text-[12.5px] text-ink-2">
                What availability means for this service changed inside these {{ win }}. A number spanning that
                boundary would average two different definitions, so it is not offered — not even labelled as
                approximate. Each segment below is complete on its own terms.
              </p>
            </div>
          </div>

          <!-- A non-ok, non-partial window states its reason instead of a number. -->
          <div
            v-else-if="!showNumbers"
            class="mb-4 flex items-start gap-3 rounded border border-border bg-surface-2 p-4"
            data-testid="svc-report-reason"
          >
            <span class="inline-flex items-center rounded-full border border-border px-[10px] py-[2px] font-mono text-[11.5px] text-ink-2">{{ report.status }}</span>
            <p class="text-[12.5px] text-ink-2">{{ reasonOf(report.reason) }}</p>
          </div>

          <div class="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
            <div class="rounded border border-border bg-surface-2 p-3" data-testid="svc-kpi-availability">
              <span class="block text-[11px] uppercase tracking-wide text-ink-3">Availability · {{ win }}</span>
              <span class="mt-1 block font-mono text-[22px]">{{ showNumbers ? pct(report.availability) : "—" }}</span>
              <span v-if="report.status === 'partial'" class="mt-[2px] block text-[11.5px] text-degraded">partial — {{ reasonOf(report.reason) }}</span>
              <span v-else-if="report.objective != null" class="mt-[2px] block text-[11.5px] text-ink-3">
                objective {{ pct(report.objective) }}
                <button
                  v-if="canWrite && hasSli"
                  type="button"
                  class="ml-1 underline decoration-dotted hover:text-ink"
                  data-testid="svc-objective-open"
                  @click="openObjective"
                >edit</button>
              </span>
            </div>
            <div class="rounded border border-border bg-surface-2 p-3" data-testid="svc-kpi-budget">
              <span class="block text-[11px] uppercase tracking-wide text-ink-3">Error budget</span>
              <span class="mt-1 block font-mono text-[22px]">{{ report.budget ? fracPct(1 - report.budget.burned_percent / 100) : "—" }}</span>
              <span v-if="report.budget" class="mt-[2px] block text-[11.5px] text-ink-3">objective {{ pct(report.budget.objective) }} · {{ report.budget.met ? "met" : "breached" }}</span>
              <span v-else-if="report.objective == null" class="mt-[2px] block text-[11.5px] text-ink-3">
                no objective set
                <button
                  v-if="canWrite && hasSli"
                  type="button"
                  class="ml-1 underline decoration-dotted hover:text-ink"
                  data-testid="svc-objective-open"
                  @click="openObjective"
                >set one</button>
              </span>
            </div>
            <div class="rounded border border-border bg-surface-2 p-3" data-testid="svc-kpi-burn">
              <span class="block text-[11px] uppercase tracking-wide text-ink-3">Burn rate</span>
              <template v-if="report.burn?.length">
                <span class="mt-1 block font-mono text-[22px]">{{ burnLabel(report.burn[0]) }}</span>
                <span class="mt-[2px] block text-[11.5px] text-ink-3">
                  <span v-for="b in report.burn" :key="b.window" class="mr-2">
                    <span class="font-mono">{{ b.window }}</span>
                    <template v-if="b.rate != null"> <span class="font-mono">{{ burnLabel(b) }}</span></template>
                    <template v-else> — {{ reasonOf(b.reason) || b.status }}</template>
                  </span>
                </span>
              </template>
              <template v-else>
                <span class="mt-1 block font-mono text-[22px]">—</span>
                <span class="mt-[2px] block text-[11.5px] text-ink-3">{{ report.objective == null ? "no objective set" : "" }}</span>
              </template>
            </div>
            <div class="rounded border border-border bg-surface-2 p-3" data-testid="svc-kpi-coverage">
              <span class="block text-[11px] uppercase tracking-wide text-ink-3">Decidable coverage</span>
              <span class="mt-1 block font-mono text-[22px]">{{ report.sealed_through ? fracPct(report.coverage) : "—" }}</span>
              <span class="mt-[2px] block text-[11.5px] text-ink-3">storage {{ report.storage_continuity ? "contiguous" : `${report.sealed_buckets} of ${report.expected_buckets} buckets` }}</span>
            </div>
          </div>

          <!-- Objective editor: the ONE client rule; prefilled when a target exists. -->
          <div v-if="editingObjective" class="mt-3 flex flex-wrap items-center gap-2 rounded border border-border bg-surface-2 p-3" data-testid="svc-objective-editor">
            <span class="text-[12px] text-ink-2">Objective for {{ win }}:</span>
            <input
              v-model="objectiveDraft"
              type="number"
              min="0"
              max="99.9999"
              step="0.01"
              placeholder="99.9"
              class="w-[92px] rounded-sm border border-border bg-surface px-2 py-[4px] text-right font-mono text-[12.5px] outline-none focus:border-accent"
              data-testid="svc-objective-input"
            />
            <button
              type="button"
              :disabled="savingObjective"
              class="rounded-sm border border-border px-[10px] py-[4px] text-[12px] hover:border-border-strong disabled:opacity-60"
              data-testid="svc-objective-save"
              @click="saveObjective"
            >Save</button>
            <button type="button" class="text-[12px] text-ink-3 hover:text-ink" @click="editingObjective = false; objectiveError = ''">Cancel</button>
            <span v-if="objectiveError" class="text-[12px] text-down" data-testid="svc-objective-error">{{ objectiveError }}</span>
          </div>

          <!-- Timeline: sealed rollups keyed by epoch, PLUS the provisional tail after
               sealed_through at reduced opacity; repair ranges masked as work. -->
          <p v-if="seriesError" class="mt-4 rounded border border-down/40 bg-down-weak p-3 text-[12.5px] text-down" data-testid="svc-series-error">
            The timeline request failed — a transport problem, not an empty timeline.
          </p>
          <div v-else-if="ticks.length" class="mt-4" data-testid="svc-timeline">
            <ReliabilityStrip :ticks="ticks" :height="30" :marks="marks ?? []" />
            <div class="mt-2 flex flex-wrap items-center gap-x-4 gap-y-1 text-[11.5px] text-ink-3">
              <span><span class="mr-[6px] inline-block h-[12px] w-[2px] bg-accent align-[-2px]"></span>definition revision boundary</span>
              <span><span class="mr-[6px] inline-block h-[12px] w-px bg-ink-3 align-[-2px] opacity-70"></span>evaluation epoch marker</span>
              <span v-if="provisionalBuckets" data-testid="svc-provisional-note">
                {{ provisionalBuckets }} {{ provisionalBuckets === 1 ? "bucket" : "buckets" }} after sealed_through
                {{ provisionalBuckets === 1 ? "is" : "are" }} drawn at reduced opacity and excluded from every number above.
              </span>
              <span v-if="repairingMasked" data-testid="svc-repairing-mask-note">
                ticks under an active repair are masked as work in progress, not shown as data.
              </span>
            </div>
          </div>

          <!-- Repair in progress is work, not data. -->
          <p v-if="report.repairing?.length" class="mt-3 rounded border border-maint/40 bg-maint-weak p-3 text-[12.5px] text-ink-2" data-testid="svc-repairing">
            {{ report.repairing.length }} range{{ report.repairing.length === 1 ? " is" : "s are" }} being recomputed —
            rendered as work in progress, never as data.
          </p>

          <!-- Segments: each complete on its own terms — range, numbers, and its OWN strip
               fetched for exactly its [from,to). -->
          <div v-if="report.segments?.length && (spansRevisions || report.segments.length > 1)" class="mt-4 flex flex-col gap-2" data-testid="svc-segments">
            <div
              v-for="seg in report.segments"
              :key="segKey(seg)"
              class="rounded border border-border bg-surface-2 p-3"
              data-testid="svc-segment"
            >
              <div class="flex flex-wrap items-center gap-2">
                <span class="rounded-xs border border-accent/50 px-[7px] py-px font-mono text-[11px] text-accent">rev {{ seg.revision }}</span>
                <span class="rounded-xs border border-border px-[7px] py-px font-mono text-[11px] text-ink-3">epoch {{ seg.epoch_seq }}</span>
                <span class="font-mono text-[11.5px] text-ink-3" data-testid="svc-segment-range">{{ dayLabel(seg.from) }} – {{ dayLabel(seg.to) }}</span>
                <span
                  v-if="seg.declared_reconstruction"
                  class="rounded-xs border border-degraded/50 px-[7px] py-px font-mono text-[11px] text-degraded"
                  data-testid="svc-reconstruction"
                >declared reconstruction</span>
                <div class="flex-1" />
                <span class="font-mono text-[12px]">availability {{ pct(seg.availability) }}</span>
                <span class="font-mono text-[12px] text-ink-3">coverage {{ fracPct(seg.coverage) }}</span>
              </div>
              <p v-if="segmentStrip(seg).state === 'failed'" class="mt-2 text-[11.5px] text-down" data-testid="svc-segment-series-error">
                This segment's timeline request failed — a transport problem, not an empty strip.
              </p>
              <p v-else-if="segmentStrip(seg).state === 'pending'" class="mt-2 text-[11.5px] text-ink-3" data-testid="svc-segment-strip-pending">
                Loading this segment's timeline…
              </p>
              <div v-else-if="segmentStrip(seg).ticks.length" class="mt-2" data-testid="svc-segment-strip">
                <ReliabilityStrip :ticks="segmentStrip(seg).ticks" :height="16" />
              </div>
            </div>
          </div>
        </template>
      </div>
    </section>
  </div>
</template>
