<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import { api } from "@/api/client";
import type { components } from "@/api/schema";
import AppShell from "@/components/AppShell.vue";
import StatusPill from "@/components/StatusPill.vue";
import { useLive } from "@/stores/live";
import { useSession } from "@/stores/session";
import { useWorkspace } from "@/stores/workspace";
import {
  buildPoints, emptySpans, gapLabel, panelStats, strokeSegments, widestSpan,
  type EmptySpan, type PanelPoint,
  type ExpectedRunAnswer,
} from "@/lib/latencypanel";
import { instantLabel, utcDayLabel, utcInstantLabel } from "@/lib/wallclock";
import { isoInstant, utcDayBefore, utcDayKey } from "@/lib/datekeys";

type Monitor = components["schemas"]["Monitor"];
type WindowSLA = components["schemas"]["WindowSLA"];
type Heartbeat = components["schemas"]["Heartbeat"];
type Incident = components["schemas"]["Incident"];
type DailyAvailability = components["schemas"]["DailyAvailability"];

const route = useRoute();
const router = useRouter();
const ws = useWorkspace();
const live = useLive();
const session = useSession();
const canWrite = computed(() => !!monitor.value && session.canProjectWrite(ws.orgId, monitor.value.project_id ?? ""));
// FR-017: a file-managed monitor's declarative fields are read-only through the UI/API; edit/
// delete/pause are disabled with an explanation (the file provider owns the desired state).
const fileManaged = computed(() => monitor.value?.management?.source === "file");

// Every service actively covering ANY of this monitor's signals, by name and deduplicated: the chip
// says who pages instead, and one service usually covers both signals.
const delegatedOwners = computed(() => {
  const d = monitor.value?.delegation;
  if (!d) return [];
  const names = new Set<string>();
  for (const sig of [d.live, d.burn]) {
    if (!sig?.delegated) continue;
    for (const o of sig.owners ?? []) names.add(o.name);
  }
  return [...names];
});
// The id is REACTIVE. It was read once, so the view kept loading and mutating the monitor it was
// first mounted with while the URL said another — a search hit for a second monitor changed the
// address bar and nothing else. The RouterView is keyed by path too; this is the layer that does not
// depend on the shell remembering to remount.
const id = computed(() => route.params.id as string);
// Every load takes a ticket. A slower response for the PREVIOUS monitor must not overwrite the
// current one's screen, which is the race a remount alone does not close.
let loadTicket = 0;

const loading = ref(true);
const monitor = ref<Monitor | null>(null);
const escalationPolicyName = ref("");
const pushCopied = ref(false);
const pushUrl = computed(() =>
  monitor.value?.push_token ? `${window.location.origin}/api/v1/public/push/${monitor.value.push_token}` : "",
);
async function copyPushUrl() {
  try {
    await navigator.clipboard.writeText(pushUrl.value);
    pushCopied.value = true;
    setTimeout(() => (pushCopied.value = false), 1500);
  } catch {
    /* clipboard blocked; the URL is shown for manual copy */
  }
}
const windows = ref<WindowSLA[]>([]);
const heartbeats = ref<Heartbeat[]>([]);
const availability = ref<DailyAvailability[]>([]);
const openIncident = ref<Incident | null>(null);

const confirmingDelete = ref(false);
const deleting = ref(false);
const pausing = ref(false);

const isPush = computed(() => monitor.value?.type === "push");

// ── The Response time panel (func-truthful-rendering §6, FR-031, D-0235) ──────────────
// POINTS ONLY, on real timestamps, drawing every heartbeat the page fetched. What this replaces:
// a `.filter((v) => v > 0)` that dropped every failure carrying no latency, a bare-index x axis
// that carried no time, and a dashed reference taken from the 24h window while the drawn series
// was the last <=60 checks. There is no connecting stroke and no fill, because neither can span
// time whose continuity is not proven — and it cannot be proven from stored facts at all, which
// is what FR-032 exists for. Absence is rendered positively by the observation ruler instead.
const panelPoints = computed(() => buildPoints(heartbeats.value));
const timeoutMs = computed(() => (monitor.value?.timeout_seconds ?? 0) * 1000);
const stats = computed(() => panelStats(panelPoints.value, timeoutMs.value));

// The ruler's hit target is a CSS-PIXEL rule, so it is measured rather than guessed from the
// viewBox. Without a ResizeObserver the panel falls back to its designed width, which changes
// which empty spans MERGE for interaction and nothing else.
const PLOT_FALLBACK_PX = 1072;
const HIT_PX = 12;
const plotBox = ref<SVGSVGElement | null>(null);
const plotPx = ref(PLOT_FALLBACK_PX);
let plotRO: ResizeObserver | null = null;
onMounted(() => {
  if (typeof ResizeObserver === "undefined" || !plotBox.value) return;
  plotRO = new ResizeObserver((entries) => {
    const w = entries[0]?.contentRect.width;
    if (w && w > 0) plotPx.value = w;
  });
  plotRO.observe(plotBox.value);
});
onUnmounted(() => plotRO?.disconnect());

const chart = computed(() => {
  const pts = panelPoints.value;
  if (pts.length < 2) return null;
  const W = 1000, H = 170, pl = 8, pr = 8, pt = 14, pb = 16;
  const t0 = pts[0].ms;
  const t1 = pts[pts.length - 1].ms;
  const span = Math.max(1, t1 - t0);
  const max = stats.value.extent || 1;
  const X = (ms: number) => pl + ((ms - t0) / span) * (W - pl - pr);
  const Y = (v: number) => pt + (1 - v / max) * (H - pt - pb);
  return {
    W, H, baseY: H - pb, t0, t1, yOf: Y,
    dots: pts.filter((p) => p.latency != null).map((p) => ({ p, x: X(p.ms), y: Y(p.latency as number) })),
    // a failure with no latency is a baseline mark: visible, and never a fabricated value
    marks: pts.filter((p) => p.latency == null).map((p) => ({ p, x: X(p.ms) })),
    ticks: pts.map((p) => ({ p, x: X(p.ms) })),
    p95y: stats.value.p95 != null ? Y(stats.value.p95) : null,
    timeoutY: stats.value.timeoutInScale ? Y(timeoutMs.value) : null,
    x: X,
  };
});

// ── FR-032 phase E: the stroke, and the second ruler band (§14, D-0240) ───────────────
// The line is back, and it is back for ONE reason: the ledger now records that a run was
// EXPECTED, so "a check was due and missing" is a stored fact rather than a chart heuristic.
// The rule itself lives in `strokeSegments` — this view only draws what it returns, and the
// expectation ruler below is drawn from the SAME windows, so the band cannot disagree with the
// line it explains.
//
// A null answer keeps FR-031's behaviour exactly: points, no stroke. That covers an older
// server, a failed fetch, a push monitor and a disabled one — none of which may be read as
// "probably continuous".
const expectedRuns = ref<ExpectedRunAnswer | null>(null);

const strokeRuns = computed(() => strokeSegments(panelPoints.value, expectedRuns.value));

/** The polylines, one per segment. Nothing is drawn BETWEEN them, which is the whole rule. */
const strokePaths = computed(() => {
  const c = chart.value;
  if (!c) return [];
  const pts = panelPoints.value;
  return strokeRuns.value
    .map((seg) => {
      const run: string[] = [];
      for (let i = seg.fromIndex; i <= seg.toIndex; i++) {
        const p = pts[i];
        // A failure carrying no latency sits on the baseline: the stroke passes through it at
        // the baseline rather than skipping it, because the check DID happen and the window it
        // answered is `covered`. Skipping it would draw a line across a run that exists.
        run.push(`${c.x(p.ms)},${p.latency == null ? c.baseY : c.yOf(p.latency)}`);
      }
      return run.join(" ");
    })
    .filter((d) => d.includes(" "));
});

/**
 * The ONE owner of what each verdict looks like — `docs/design/mock-expected-run-reserved.html`,
 * approved 2026-09-05.
 *
 * It is a MAP and it has no default arm, which is the whole point. The chain of ternaries this
 * replaced sent every unmapped verdict to `empty`, the `--down` outline whose meaning is "a run was
 * due here and nothing ran" — so `unknown`, `issued_never_claimed` and `claimed_never_finished`
 * were all drawn as missed runs, and `unknown` had been assigned the HATCH by the phase-E mock this
 * panel was approved against. On the shipped default (`ledger.carrier_enabled` off) every window
 * reads `unknown`, so the panel drew a full row of missed runs on an instance where nothing was
 * missed. A default arm is how a rendering claims something nobody decided.
 *
 * Three verdicts share `empty` because they say the same thing ABOUT THE WINDOW — nothing ran in
 * it — and the verdict itself stays on the cell for the readout and the tests.
 *
 * **`satisfies` is what makes "no default arm" true rather than said.** The map was typed
 * `Record<string, string>` first, which permits deleting `reserved` or `unknown` and compiles — the
 * comment above claimed a compile-time hole the type did not give (reviewer P1 at party [38]).
 * Bound to the GENERATED verdict union instead, a missing key is a build failure, while the
 * incoming wire value stays a plain string and keeps the runtime hatch fallback for a verdict this
 * build has never heard of. The two are different questions and both are answered.
 */
type ExpectedRunVerdict = components["schemas"]["ExpectedRunVerdict"];
type ExpectationCellKind = "covered" | "late" | "empty" | "notStored" | "reserved";

const CELL_FOR_VERDICT = {
  covered: "covered",
  covered_late: "late",
  expected_never_issued: "empty",
  issued_never_claimed: "empty",
  claimed_never_finished: "empty",
  // The ledger cannot answer: no result could ever correlate to a window dispatched below the
  // ledger carrier. The hatch claims nothing, which is the only honest cell for it.
  unknown: "notStored",
  // FR-032 phase F (§7.4): identity minted and committed, no dispatch recorded. Neither absence
  // verdict may be borrowed — one asserts nothing ran, the other asserts a publish — so it gets a
  // neutral outline: the geometry of the missed-run cell in a hue that is not failure.
  reserved: "reserved",
} satisfies Record<ExpectedRunVerdict, ExpectationCellKind>;

/**
 * The cell for one wire value. The parameter is a STRING and not the union on purpose: what arrives
 * is whatever the server sent, and a build that has never heard of a verdict must render the cell
 * that claims nothing rather than refuse the answer or accuse the window.
 */
function cellFor(verdict: string): ExpectationCellKind {
  return (CELL_FOR_VERDICT as Record<string, ExpectationCellKind>)[verdict] ?? "notStored";
}

/** One cell per due window, carrying its verdict. Only windows inside the drawn span are shown. */
const expectationCells = computed(() => {
  const c = chart.value;
  const answer = expectedRuns.value;
  if (!c || !answer) return [];
  const ledgerFrom = answer.ledger_from == null ? Number.POSITIVE_INFINITY : Date.parse(answer.ledger_from);
  const pts = panelPoints.value;
  const cw = Math.max(1.2, ((c.W - 16) / Math.max(1, pts.length - 1)) * 0.62);
  const out: ExpectationCell[] = [];
  for (const w of answer.windows) {
    const ms = Date.parse(w.due_at);
    if (Number.isNaN(ms) || ms < c.t0 || ms > c.t1) continue;
    // Before `ledger_from` the ledger holds nothing, and the cell says exactly that — not
    // covered, and not missed either (§12.3). It outranks the verdict because it is a fact about
    // the LEDGER rather than about the window.
    //
    // A verdict this panel has never heard of falls here too, and lands on the hatch: an unknown
    // name is precisely the case where the panel cannot say what happened, and the cell that
    // claims nothing is the safe one. The old default claimed a missed run instead.
    // WHY the cell is `notStored` has to travel with it. Two different situations produce that
    // kind — time the ledger holds nothing for, and a window dispatched below the ledger carrier —
    // and the readout has to tell them apart: saying "before the ledger begins" about the second
    // would be as wrong as letting the second's wording escape onto the first, which is what
    // `cellLabel` did until the final-tree audit.
    const beforeLedger = ms < ledgerFrom;
    const kind = beforeLedger ? "notStored" : cellFor(w.verdict);
    out.push({
      x: c.x(ms) - cw / 2, w: cw, kind, verdict: w.verdict, ms, beforeLedger,
      reservedAt: w.reserved_at ?? null, withheldReason: w.withheld_reason ?? "",
    });
  }
  return out;
});

/**
 * Which cells are drawn as an OUTLINE rather than a fill. Both of them mean "this window holds no
 * completed run", and neither may be filled: a fill reads as a recorded failure, which is a claim
 * about what happened rather than about what is absent.
 */
function outlined(kind: string): boolean {
  return kind === "empty" || kind === "reserved";
}

type ExpectationCell = {
  x: number; w: number; kind: string; verdict: string; ms: number;
  /** The window is before `ledger_from`, which outranks its stored verdict in the words too. */
  beforeLedger: boolean;
  reservedAt: string | null; withheldReason: string;
};

/** The hovered or focused expectation cell, which drives its own readout. */
const hoverCell = ref<ExpectationCell | null>(null);

/**
 * What one cell says in words. The verdicts are NAMED rather than described, because the name is
 * what the API returns and what the spec argues about — a reader who sees `covered_late` here can
 * find it in `openapi.yaml` and in §14.1, and a paraphrase would cost them that.
 */
function cellLabel(c: ExpectationCell): string {
  const due = `window due ${instantLabel(isoInstant(new Date(c.ms)))}`;
  // UNCONDITIONAL, and before the verdict switch. `ledger_from` outranks every verdict — that is
  // §12.3 — and it has to outrank it in the WORDS as well as in the fill. This test carried an
  // exception for `unknown` at first, so a pre-`ledger_from` window whose stored verdict happened to
  // be `unknown` was described as "dispatched on a carrier that carries no job identity": a claim
  // about a dispatch, made about time the ledger says it cannot answer for. The geometry was right
  // and the sentence beside it was not, which is the defect class this arc keeps producing. It is
  // reachable whenever a truncation fence moves `ledger_from` past a row that still exists.
  if (c.beforeLedger) {
    return `${due} — before the ledger begins: not stored, and claimable as nothing`;
  }
  switch (c.verdict) {
    case "covered":
      return `${due} — covered: a run answered it inside the interval that spaced it`;
    case "covered_late":
      return `${due} — covered_late: a run answered it, later than the interval that spaced it`;
    case "expected_never_issued":
      return `${due} — expected_never_issued: nothing was dispatched for it`;
    case "issued_never_claimed":
      return `${due} — issued_never_claimed: a job was published and no executor took it`;
    case "claimed_never_finished":
      return `${due} — claimed_never_finished: an executor started and no outcome arrived`;
    case "unknown":
      return `${due} — unknown: dispatched on a carrier that carries no job identity, so nothing could correlate`;
    case "reserved": {
      const at = c.reservedAt ? instantLabel(c.reservedAt) : instantLabel(isoInstant(new Date(c.ms)));
      const why = c.withheldReason ? ` · ${c.withheldReason}` : "";
      return `${due} — reserved at ${at}: no dispatch recorded${why}`;
    }
    default:
      return `${due} — ${c.verdict}`;
  }
}

/** Where `ledger_from` falls inside the drawn span, if it does. */
const ledgerFromX = computed(() => {
  const c = chart.value;
  const from = expectedRuns.value?.ledger_from;
  if (!c || from == null) return null;
  const ms = Date.parse(from);
  if (Number.isNaN(ms) || ms <= c.t0 || ms > c.t1) return null;
  return c.x(ms);
});

/**
 * FR-032 §13a — every page of the drawn span's windows, not just the first.
 *
 * The panel used to ask for one page of 200 and ignore `next_cursor`. `strokeSegments` reads "every
 * window between these two points is `covered`", so evaluating it over a page is evaluating it over
 * a set that may be missing members — and the expectation ruler simply stopped part-way through the
 * chart with nothing saying it had. That is the one distinction this whole requirement exists to
 * make: "no window was due here" and "we did not fetch them" must not look the same.
 *
 * A span with a gap in it holds far more windows than points: sixty heartbeats either side of a
 * two-day outage on a 60-second monitor are 2,880 windows. Paging is bounded so a pathological span
 * cannot turn one panel into a hundred requests — past the bound the answer is abandoned entirely
 * rather than truncated, because a partial answer is exactly what this is fixing.
 */
const expectedRunPageLimit = 200;
const expectedRunMaxPages = 16;

/**
 * The widest range the endpoint will answer, learned from the SERVER rather than assumed.
 *
 * It opens at the documented default and is corrected by the first answer that carries
 * `retention_days`. Two separate things follow from that, and conflating them is what the review
 * of revision 31 found twice:
 *
 * - DISCOVERY: the value travels only on a SUCCESSFUL answer, and an instance configured below the
 *   assumption refuses the question with `range_too_wide` and no body — so discovery needs a retry
 *   at the enforced minimum, which every instance can answer.
 * - COMPLETENESS: whichever request finally succeeded was asked under the bound the panel held
 *   BEFORE the answer arrived, so it may describe less time than the panel drew. That is true of
 *   the assumed default and of the minimum fallback alike — 14 against a 90-day instance, and 2
 *   against a 7-day one, are the same defect — so completeness needs a refetch under the LEARNED
 *   bound, and the test for it is one comparison rather than a case per path.
 *
 * Module-scoped because it is a property of the instance, not of a panel.
 */
let expectedRunRetentionDays = 14;

/**
 * The enforced MINIMUM of `ledger.expected_run_retention_days`. A range this narrow is answerable on
 * every instance, which is what makes it the fallback below.
 */
const expectedRunMinRetentionDays = 2;

/** Whether `expectedRunRetentionDays` above came from the server rather than from the default. */
let expectedRunRetentionLearned = false;

/**
 * Where a request for this drawn span STARTS under a given retention bound.
 *
 * One expression, used by the request itself and by the completeness test above it. Two copies of
 * this arithmetic is how "the request was clamped" and "the panel drew earlier than that" came to
 * be compared through a proxy — `learned > assumed` — which was true in cases that needed no
 * refetch and false in one that did.
 */
function expectedRunFromMs(earliestDrawnMs: number, to: Date, retentionDays: number): number {
  return Math.max(earliestDrawnMs, to.getTime() - retentionDays * 86_400_000);
}

async function fetchExpectedRuns(
  projectID: string,
  monitorID: string,
  drawn: PanelPoint[],
): Promise<ExpectedRunAnswer | null> {
  if (drawn.length < 2) return null;
  // Half-open at the top, so the last point's own window is included.
  const to = new Date(drawn[drawn.length - 1].ms + 1000);
  const earliest = drawn[0].ms;

  // DISCOVERY. The first question is asked under whatever bound we hold; if it comes back empty and
  // we have never been told the real one, the assumption itself is a candidate cause, and the
  // enforced minimum is a range every instance answers.
  let asked = expectedRunRetentionDays;
  let answer = await pageExpectedRuns(projectID, monitorID, earliest, to, asked);
  if (!answer && !expectedRunRetentionLearned) {
    asked = expectedRunMinRetentionDays;
    answer = await pageExpectedRuns(projectID, monitorID, earliest, to, asked);
  }
  if (!answer) return null;

  // COMPLETENESS, for BOTH paths under one comparison: the answer in hand was asked under `asked`,
  // and the bound we now hold may reach further back. If it does, the windows between the two
  // starts exist and were simply never requested — and `strokeSegments` would then evaluate a set
  // missing members over that older time, where `coversInterval` sees no window and draws none. No
  // stroke and no cell reads as "no run was due here", while `ledger_from` in the same answer says
  // the ledger speaks for it. That is the one distinction this requirement exists to make, and it
  // is why the page bound below abandons an answer rather than truncating one.
  //
  // Asking again costs one extra request, once per instance: the next load asks under the learned
  // bound first, so the two starts agree and nothing is refetched.
  if (expectedRunFromMs(earliest, to, expectedRunRetentionDays) < expectedRunFromMs(earliest, to, asked)) {
    return pageExpectedRuns(projectID, monitorID, earliest, to, expectedRunRetentionDays);
  }
  return answer;
}

async function pageExpectedRuns(
  projectID: string,
  monitorID: string,
  earliestDrawnMs: number,
  to: Date,
  retentionDays: number,
): Promise<ExpectedRunAnswer | null> {
  // Clamped to the retention window the endpoint will answer. A panel spanning more than that
  // asked a range the API refuses with `range_too_wide`, and the whole answer — bounds included —
  // was then dropped on the floor, so the ruler silently never appeared. Clamping asks for the
  // part that CAN be answered; the rest holds no windows the SERVER's bound admits, and
  // `ledger_from` says so — which is only true when `retentionDays` is the server's own value,
  // hence the completeness test in the caller.
  const from = new Date(expectedRunFromMs(earliestDrawnMs, to, retentionDays));
  if (!(from.getTime() < to.getTime())) return null;

  const windows: ExpectedRunAnswer["windows"] = [];
  let ledgerFrom: string | null = null;
  let cursor: string | undefined;
  for (let page = 0; page < expectedRunMaxPages; page++) {
    const res = await api.GET("/api/v1/projects/{projectID}/monitors/{monitorID}/expected-runs", {
      params: {
        path: { projectID, monitorID },
        query: { from: isoInstant(from), to: isoInstant(to), limit: expectedRunPageLimit, cursor },
      },
    });
    if (!res.data) return null;
    if (typeof res.data.retention_days === "number" && res.data.retention_days > 0) {
      expectedRunRetentionDays = res.data.retention_days;
      expectedRunRetentionLearned = true;
    }
    windows.push(...(res.data.windows ?? []));
    ledgerFrom = res.data.ledger_from ?? null;
    const next = res.data.next_cursor;
    if (!next) return { windows, ledger_from: ledgerFrom };
    cursor = next;
  }
  // The bound was hit, so this span holds more windows than the panel will page through. Returning
  // what we have would let `strokeSegments` decide "every window here is covered" over a set
  // missing members — which is the one thing this requirement exists to prevent.
  return null;
}

const rulerSpans = computed(() => {
  const pts = panelPoints.value;
  if (pts.length < 2) return [];
  const span = Math.max(1, pts[pts.length - 1].ms - pts[0].ms);
  return emptySpans(pts, plotPx.value / span, HIT_PX);
});
const widestGap = computed(() => widestSpan(rulerSpans.value));

/** The hovered/focused point or empty span drives the readouts and the Recent-checks highlight. */
const hoverPoint = ref<PanelPoint | null>(null);
const hoverSpan = ref<EmptySpan | null>(null);
function spanLabel(s: EmptySpan): string {
  return `no check recorded between ${instantLabel(isoInstant(new Date(s.fromMs)))} and ${instantLabel(isoInstant(new Date(s.toMs)))}`;
}
const fmtMs = (v: number) => (v >= 1000 ? (v / 1000).toFixed(v % 1000 ? 1 : 0).replace(/\.0$/, "") + "s" : Math.round(v) + "ms");

// 90-day availability strip.
const timeline = computed(() => {
  const byDay = new Map<string, DailyAvailability>();
  for (const d of availability.value) if (d.day) byDay.set(d.day.slice(0, 10), d);
  const out: { pct: number | null; label: string }[] = [];
  const today = new Date();
  for (let i = 89; i >= 0; i--) {
    const dt = utcDayBefore(today, i);
    const key = utcDayKey(dt);
    const d = byDay.get(key);
    // The key is a lookup; the label is read by a human in a tooltip, so it names its zone.
    out.push({ pct: d && d.total ? (d.uptime_percent ?? 0) : null, label: utcDayLabel(isoInstant(dt)) });
  }
  return out;
});
const timelineUptime = computed(() => {
  const v = timeline.value.filter((d) => d.pct !== null).map((d) => d.pct as number);
  return v.length ? `${(v.reduce((a, b) => a + b, 0) / v.length).toFixed(2)}% uptime` : "—";
});

function daySegClass(p: number | null) {
  if (p === null) return "bg-inset";
  if (p >= 99.5) return "bg-up";
  if (p >= 95) return "bg-degraded";
  return "bg-down";
}
function budgetMeter(w: WindowSLA) {
  const b = w.error_budget;
  if (!b) return { width: 0, cls: "bg-up" };
  const burned = Math.max(0, Math.min(100, b.burned_percent ?? 0));
  const cls = !b.met ? "bg-down" : burned >= 80 ? "bg-degraded" : "bg-up";
  return { width: burned, cls };
}
function windowSub(w: WindowSLA) {
  if (w.error_budget) return `${Math.max(0, 100 - (w.error_budget.burned_percent ?? 0)).toFixed(0)}% budget left`;
  return `${w.up ?? 0} / ${w.total ?? 0} up`;
}

function statusPill(): "up" | "down" | "pending" {
  return (monitor.value?.status as "up" | "down" | "pending") ?? "pending";
}
function relTime(ts?: string) {
  if (!ts) return "—";
  const s = Math.max(0, Math.round((Date.now() - new Date(ts).getTime()) / 1000));
  if (s < 60) return `${s}s`;
  const m = Math.round(s / 60);
  if (m < 60) return `${m}m`;
  return `${Math.round(m / 60)}h`;
}
const lastChecked = computed(() => (heartbeats.value[0]?.ts ? relTime(heartbeats.value[0].ts) + " ago" : "—"));
function fmtDate(ts?: string) {
  return utcDayLabel(ts);
}
const projectName = computed(() => ws.projects.find((p) => p.id === monitor.value?.project_id)?.name || "—");
function heartbeatCode(h: Heartbeat): string {
  if (h.code) return String(h.code);
  return h.up ? "—" : "ERR";
}

// Dependency graph: parents (depends on) and dependents (required by), from the
// project monitor list. A down parent is what suppresses this monitor's alerts.
const projectMonitors = ref<Monitor[]>([]);
const dependsOn = computed(() =>
  (monitor.value?.depends_on ?? [])
    .map((pid) => projectMonitors.value.find((m) => m.id === pid))
    .filter((m): m is Monitor => !!m),
);
const requiredBy = computed(() => projectMonitors.value.filter((m) => (m.depends_on ?? []).includes(id.value)));

async function load() {
  const ticket = ++loadTicket;
  const monitorID = id.value;
  loading.value = true;
  const [mon, sla, hb] = await Promise.all([
    api.GET("/api/v1/monitors/{monitorID}", { params: { path: { monitorID } } }),
    api.GET("/api/v1/monitors/{monitorID}/sla", { params: { path: { monitorID } } }),
    api.GET("/api/v1/monitors/{monitorID}/heartbeats", { params: { path: { monitorID }, query: { limit: 60 } } }),
  ]);
  if (ticket !== loadTicket) return;
  monitor.value = mon.data ?? null;
  windows.value = sla.data?.windows ?? [];
  heartbeats.value = hb.data ?? [];

  const pid = monitor.value?.project_id;
  if (pid) {
    await ws.init();
    // FR-032 §13a. The window the panel actually drew, so the ledger answer and the points
    // describe the same span: asking for a fixed range would fetch windows for time the panel is
    // not showing, and — worse — could miss the ones it is.
    const drawn = panelPoints.value;
    const [av, inc, mons, runs] = await Promise.all([
      api.GET("/api/v1/monitors/{monitorID}/availability", { params: { path: { monitorID }, query: { days: 90 } } }),
      api.GET("/api/v1/projects/{projectID}/incidents", { params: { path: { projectID: pid } } }),
      api.GET("/api/v1/projects/{projectID}/monitors", { params: { path: { projectID: pid } } }),
      fetchExpectedRuns(pid, monitorID, drawn),
    ]);
    if (ticket !== loadTicket) return;
    // A failed or absent answer leaves the panel exactly as FR-031 left it: points, no stroke.
    // That is the honest default — the alternative is a line drawn because nothing said not to.
    expectedRuns.value = runs;
    availability.value = av.data ?? [];
    projectMonitors.value = mons.data ?? [];
    openIncident.value = (inc.data ?? []).find((i) => i.monitor_id === monitorID && i.status !== "resolved") ?? null;
    // Resolve the attached escalation policy's name for the Configuration card.
    if (monitor.value?.escalation_policy_id) {
      const pol = await api.GET("/api/v1/projects/{projectID}/escalation-policies", { params: { path: { projectID: pid } } });
      escalationPolicyName.value = (pol.data ?? []).find((p) => p.id === monitor.value?.escalation_policy_id)?.name ?? "";
    }
  }
  loading.value = false;
  // Services back the §15.5 successor picker. Loaded only when this monitor can have one, so a
  // plain HTTP monitor's page does not pay for a list it will never show.
  if (monitor.value && (monitor.value.type === "composite" || monitor.value.superseded_by_service_id)) {
    await loadServices();
  }
}

async function togglePause() {
  if (!monitor.value) return;
  pausing.value = true;
  try {
    const res = await api.PATCH("/api/v1/monitors/{monitorID}", {
      params: { path: { monitorID: id.value } },
      body: { enabled: !monitor.value.enabled },
    });
    if (res.data) monitor.value = res.data;
  } finally {
    pausing.value = false;
  }
}
// ── FR-021 §15.5: the composite lifecycle ─────────────────────────────────────────────────
//
// Three separate acts, on purpose. Recording a successor changes nothing; retiring changes both
// the lifecycle statement and execution; converting builds a service and leaves the composite
// running. Collapsing any two of them into one button is how an operator ends up stopping a
// monitor they only meant to annotate.
const services = ref<components["schemas"]["Service"][]>([]);
const lifecycleBusy = ref(false);
const lifecycleError = ref("");
const confirmingRetire = ref(false);
const successorChoice = ref("");
const isComposite = computed(() => monitor.value?.type === "composite");
const retired = computed(() => !!monitor.value?.retired_at);
const successorName = computed(
  () => services.value.find((sv) => sv.id === monitor.value?.superseded_by_service_id)?.name ?? "",
);

async function loadServices() {
  if (!ws.projectId) return;
  const res = await api.GET("/api/v1/projects/{projectID}/services", {
    params: { path: { projectID: ws.projectId } },
  });
  // The endpoint answers with SUMMARIES — the service is nested under `service`, alongside the
  // revision and coverage fields this picker has no use for. There is no cast here on purpose: the
  // generated `paths` type already describes the response, and the cast this line used to carry is
  // precisely what let it read `sv.id` and `sv.name` off the wrong object, rendering a list of blank
  // options and posting `undefined`. Mapping instead of asserting puts `vue-tsc` back in charge of a
  // shape the SPA does not own.
  services.value = (res.data ?? []).map((row) => row.service);
}

async function lifecycle(run: () => Promise<{ data?: unknown; error?: unknown }>) {
  lifecycleBusy.value = true;
  lifecycleError.value = "";
  try {
    const res = await run();
    if (res.error || !res.data) {
      lifecycleError.value = (res.error as { error?: string })?.error || "The action did not complete.";
      return false;
    }
    return true;
  } finally {
    lifecycleBusy.value = false;
  }
}

async function saveSuccessor() {
  const ok = await lifecycle(async () => {
    const res = await api.PUT("/api/v1/monitors/{monitorID}/successor", {
      params: { path: { monitorID: id.value } },
      body: { service_id: successorChoice.value } as never,
    });
    if (res.data) monitor.value = res.data;
    return res;
  });
  if (ok) successorChoice.value = "";
}

async function retire() {
  await lifecycle(async () => {
    const res = await api.POST("/api/v1/monitors/{monitorID}/retire", { params: { path: { monitorID: id.value } } });
    if (res.data) monitor.value = res.data;
    return res;
  });
  confirmingRetire.value = false;
}

async function reactivate() {
  await lifecycle(async () => {
    const res = await api.POST("/api/v1/monitors/{monitorID}/reactivate", { params: { path: { monitorID: id.value } } });
    if (res.data) monitor.value = res.data;
    return res;
  });
}

// The SLI is the operator's STATEMENT, never inferred (§15.5): the children are always the
// operational context, but which of them MEASURE availability is a declaration. So the dialog
// starts with every child pre-selected — the common intent — and requires the operator to confirm
// it, rather than sending a selection nobody looked at.
const convertOpen = ref(false);
const childChoices = ref<{ id: string; name: string; chosen: boolean }[]>([]);
const chosenSLI = computed(() => childChoices.value.filter((c) => c.chosen).map((c) => c.id));

async function openConvert() {
  lifecycleError.value = "";
  const children = (monitor.value?.config?.children ?? "").split(",").map((c) => c.trim()).filter(Boolean);
  if (!ws.projectId) return;
  const res = await api.GET("/api/v1/projects/{projectID}/monitors", {
    params: { path: { projectID: ws.projectId } },
  });
  const byID = new Map((res.data ?? []).map((m) => [m.id, m.name]));
  childChoices.value = children.map((cid) => ({ id: cid, name: byID.get(cid) ?? cid, chosen: true }));
  convertOpen.value = true;
}

async function convertToService() {
  if (!chosenSLI.value.length) {
    lifecycleError.value = "Choose at least one child as a reliability input.";
    return;
  }
  const ok = await lifecycle(async () => {
    const res = await api.POST("/api/v1/monitors/{monitorID}/convert-to-service", {
      params: { path: { monitorID: id.value } },
      body: { sli: chosenSLI.value },
    });
    if (res.data) {
      // `monitor` is optional in the schema, so it is checked rather than asserted. The cast this
      // line used to carry claimed it was always there; the reload below keeps the screen honest if
      // a future response ever omits it.
      if (res.data.monitor) {
        monitor.value = res.data.monitor;
      }
      await loadServices();
    }
    return res;
  });
  if (ok) convertOpen.value = false;
}

async function remove() {
  deleting.value = true;
  try {
    const res = await api.DELETE("/api/v1/monitors/{monitorID}", { params: { path: { monitorID: id.value } } });
    if (!res.error) router.push({ name: "monitors" });
  } finally {
    deleting.value = false;
    confirmingDelete.value = false;
  }
}

onMounted(() => {
  load();
  live.connect();
});

// Reload when the ROUTE identity changes, or when the workspace does — the pattern ServiceDetail
// already used. Either one alone leaves an entrance open: a search hit changes both, a workspace
// switcher changes only the second.
watch(() => [id.value, ws.projectId], load);

// Reflect live status changes for this monitor immediately.
watch(
  () => live.statuses[id.value],
  (s) => {
    if (s && monitor.value) monitor.value = { ...monitor.value, status: s.status as Monitor["status"] };
  },
);
</script>

<template>
  <AppShell active="monitors" :crumbs="[ws.orgName || 'cerbix', ws.projectName || '…', 'monitors', monitor?.name || '…']">
    <div class="mx-auto max-w-[1180px] px-[22px] pb-16 pt-6">
      <!-- header -->
      <div v-if="monitor" class="mb-[22px] flex flex-wrap items-start gap-[14px]">
        <div class="min-w-0">
          <div class="flex flex-wrap items-center gap-[10px]">
            <h1 class="font-mono text-[22px] font-semibold tracking-tight">{{ monitor.name }}</h1>
            <span class="rounded-xs border border-border px-[6px] py-px font-mono text-[10.5px] uppercase tracking-[0.04em] text-ink-3">{{ monitor.type }}</span>
            <StatusPill :status="statusPill()" />
            <!-- Retired is its OWN badge, not "Paused": conflating an afternoon's pause with
                 "superseded forever" is the distinction §15.5 exists to keep. -->
            <span v-if="retired" class="rounded-full border border-border px-[9px] py-px text-[11.5px] font-medium text-ink-3" data-testid="monitor-retired">Retired</span>
            <span v-else-if="!monitor.enabled" class="rounded-full bg-pending-weak px-[9px] py-px text-[11.5px] font-medium text-ink-3">Paused</span>
            <!-- FR-021 §16.1: a delegated monitor is NEVER greyed out. It keeps its real pill —
                 DOWN reads as DOWN — and gains a dashed chip naming who pages instead of it.
                 Dimming it would make the system show something other than what it knows. -->
            <span
              v-if="delegatedOwners.length"
              class="rounded-full border border-dashed border-accent px-[9px] py-px text-[11.5px] font-medium text-accent"
              data-testid="monitor-delegated"
            >paging delegated → {{ delegatedOwners.join(", ") }}</span>
            <span v-if="monitor.superseded_by_service_id" class="rounded-full border border-border px-[9px] py-px text-[11.5px] font-medium text-ink-3" data-testid="monitor-superseded">
              superseded by {{ successorName || "a service" }}
            </span>
          </div>
          <!-- FR-030: the whole description, where there is room to read it. -->
          <p v-if="monitor.description" class="mt-[6px] max-w-[65ch] text-[13.5px] leading-[1.5] text-ink-2" data-testid="monitor-description">{{ monitor.description }}</p>
          <!-- WHICH signals are delegated and which are not, because they are delegated apart:
               a monitor whose DOWN transitions are covered while its own budget alerts are not is
               the ordinary case, and "delegated" without saying which would be worse than silence. -->
          <ul v-if="monitor.delegation" class="mt-[7px] space-y-px text-[12.5px]" data-testid="monitor-delegation">
            <li
              v-for="sig in [
                { key: 'live', d: monitor.delegation.live, what: 'DOWN transitions and escalation' },
                { key: 'burn', d: monitor.delegation.burn, what: 'Burn alerts' },
              ]"
              :key="sig.key"
              :data-testid="`monitor-delegation-${sig.key}`"
            >
              <span class="text-ink-3">{{ sig.what }}:</span>
              <span v-if="sig.d.delegated" class="text-accent">
                delegated to {{ (sig.d.owners ?? []).map((o) => o.name).join(", ") }}
              </span>
              <span v-else class="text-ink-2">
                this monitor still alerts for itself<span v-if="sig.d.reason" class="text-ink-3"> ({{ sig.d.reason }})</span>
              </span>
            </li>
          </ul>
          <div class="mt-[7px] flex flex-wrap items-center gap-x-[10px] gap-y-1 text-[13px] text-ink-3">
            <span class="font-mono text-ink-2">{{ monitor.type === "http" ? (monitor.method || "GET") + " " : "" }}{{ monitor.target || "push heartbeat" }}</span>
            <template v-if="!isPush">
              <span class="inline-block h-[3px] w-[3px] rounded-full bg-border-strong"></span>
              <span>every <span class="font-mono text-ink-2">{{ monitor.interval_seconds }}s</span></span>
              <span class="inline-block h-[3px] w-[3px] rounded-full bg-border-strong"></span>
              <span>timeout <span class="font-mono text-ink-2">{{ monitor.timeout_seconds }}s</span></span>
              <span class="inline-block h-[3px] w-[3px] rounded-full bg-border-strong"></span>
              <span>retries <span class="font-mono text-ink-2">{{ monitor.retries }}</span></span>
            </template>
            <span class="inline-block h-[3px] w-[3px] rounded-full bg-border-strong"></span>
            <span>checked <span class="font-mono text-ink-2">{{ lastChecked }}</span></span>
          </div>
        </div>
        <div v-if="fileManaged" class="ml-auto max-w-[340px] rounded-sm border border-border bg-inset px-[12px] py-[8px] text-[12px] text-ink-2">
          <div class="flex items-center gap-[6px] font-medium text-ink">
            <svg viewBox="0 0 24 24" class="h-[13px] w-[13px]" fill="none" stroke="currentColor" stroke-width="2"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><path d="M14 2v6h6"/></svg>
            Managed by file
          </div>
          <p class="mt-[3px] leading-snug">Provider <span class="font-mono">{{ monitor?.management?.provider }}</span> · <span class="font-mono">{{ monitor?.management?.path }}</span>. Declarative fields are read-only; edit the bundle file to change them.</p>
        </div>
        <div v-if="canWrite && !fileManaged" class="ml-auto flex flex-wrap gap-2">
          <template v-if="!confirmingDelete">
            <button type="button" class="inline-flex h-[34px] items-center gap-[7px] rounded-sm border border-border bg-surface px-[13px] text-[13px] text-ink hover:border-border-strong disabled:opacity-50" :disabled="pausing" @click="togglePause">
              <svg viewBox="0 0 24 24" class="h-[15px] w-[15px]" fill="none" stroke="currentColor" stroke-width="2">
                <template v-if="monitor.enabled"><path d="M8 5v14M16 5v14" /></template>
                <path v-else d="M5 3l14 9-14 9V3z" />
              </svg>
              {{ monitor.enabled ? (pausing ? "Pausing…" : "Pause") : pausing ? "Resuming…" : "Resume" }}
            </button>
            <RouterLink :to="{ name: 'monitor-edit', params: { id } }" class="inline-flex h-[34px] items-center gap-[7px] rounded-sm border border-border bg-surface px-[13px] text-[13px] text-ink hover:border-border-strong">
              <svg viewBox="0 0 24 24" class="h-[15px] w-[15px]" fill="none" stroke="currentColor" stroke-width="2"><path d="M4 20h4L18.5 9.5a2.1 2.1 0 0 0-3-3L5 17v3z" /></svg>
              Edit
            </RouterLink>
            <button v-if="isComposite && !monitor.superseded_by_service_id" type="button" class="inline-flex h-[34px] items-center rounded-sm border border-border bg-surface px-[13px] text-[13px] text-ink hover:border-accent hover:text-accent disabled:opacity-50" :disabled="lifecycleBusy" data-testid="convert-to-service" @click="openConvert">Build a service from this</button>
            <button v-if="!retired" type="button" class="inline-flex h-[34px] items-center rounded-sm border border-border bg-surface px-[13px] text-[13px] text-ink-2 hover:border-border-strong disabled:opacity-50" :disabled="lifecycleBusy" data-testid="retire-monitor" @click="confirmingRetire = true">Retire</button>
            <button v-else type="button" class="inline-flex h-[34px] items-center rounded-sm border border-border bg-surface px-[13px] text-[13px] text-ink hover:border-accent hover:text-accent disabled:opacity-50" :disabled="lifecycleBusy" data-testid="reactivate-monitor" @click="reactivate">Reactivate</button>
            <button type="button" class="inline-flex h-[34px] items-center rounded-sm border border-border bg-surface px-[13px] text-[13px] text-ink-2 hover:border-down/60 hover:text-down" @click="confirmingDelete = true">Delete</button>
          </template>
          <template v-else>
            <span class="self-center text-[12.5px] text-ink-3">Delete this monitor?</span>
            <button type="button" class="h-[34px] rounded-sm bg-down px-[13px] text-[13px] font-medium text-white hover:opacity-90 disabled:opacity-50" :disabled="deleting" @click="remove">{{ deleting ? "Deleting…" : "Confirm" }}</button>
            <button type="button" class="h-[34px] rounded-sm border border-border px-[13px] text-[13px] text-ink-2 hover:border-border-strong" @click="confirmingDelete = false">Cancel</button>
          </template>
        </div>
      </div>

      <!-- The retire confirmation states BOTH consequences, because retiring is two facts and an
           operator who reads only "removed from the active list" would be surprised by the second. -->
      <div v-if="confirmingRetire" class="mb-4 rounded border border-border bg-surface-2 px-4 py-3 text-[13px]" data-testid="retire-confirm">
        <div class="font-medium">Retire “{{ monitor?.name }}”?</div>
        <p class="mt-1 leading-snug text-ink-2">
          It stops probing and stops paging on-call, and it leaves the active list. Nothing is deleted —
          its heartbeats, incidents and past numbers stay, and you can reactivate it at any time.
        </p>
        <div class="mt-3 flex gap-2">
          <button type="button" class="h-[34px] rounded-sm border border-border bg-surface px-[13px] text-[13px] hover:border-border-strong disabled:opacity-50" :disabled="lifecycleBusy" data-testid="retire-confirm-yes" @click="retire">{{ lifecycleBusy ? "Retiring…" : "Retire" }}</button>
          <button type="button" class="h-[34px] rounded-sm border border-border px-[13px] text-[13px] text-ink-2 hover:border-border-strong" @click="confirmingRetire = false">Cancel</button>
        </div>
      </div>

      <!-- Converting a composite: the operator states the SLI. Every child stays in the operational
           context; the checkboxes decide which ones MEASURE availability, and unticking one changes
           what the service's number means — so the consequence is written next to them. -->
      <div v-if="convertOpen" class="mb-4 rounded border border-border bg-surface-2 px-4 py-3 text-[13px]" data-testid="convert-dialog">
        <div class="font-medium">Build a service from “{{ monitor?.name }}”</div>
        <p class="mt-1 leading-snug text-ink-2">
          This composite keeps probing and alerting. Every child below joins the new service's
          operational context; the ones you tick become its <b>reliability inputs</b> — what its
          availability number is computed from.
        </p>
        <ul class="mt-3 flex flex-col gap-1">
          <li v-for="c in childChoices" :key="c.id" class="flex items-center gap-2">
            <input :id="'sli-' + c.id" v-model="c.chosen" type="checkbox" class="h-[14px] w-[14px]" :data-testid="'sli-choice'" />
            <label :for="'sli-' + c.id" class="text-[13px]">{{ c.name }}</label>
          </li>
          <li v-if="!childChoices.length" class="text-[12.5px] text-ink-3">This composite has no children to declare.</li>
        </ul>
        <p v-if="!chosenSLI.length && childChoices.length" class="mt-2 text-[12.5px] text-degraded" data-testid="sli-empty-warning">
          With nothing ticked the service would report no availability at all — pick at least one.
        </p>
        <div class="mt-3 flex gap-2">
          <button type="button" class="h-[34px] rounded-sm border border-accent px-[13px] text-[13px] text-accent hover:bg-accent-weak disabled:opacity-50" :disabled="lifecycleBusy || !chosenSLI.length" data-testid="convert-confirm" @click="convertToService">{{ lifecycleBusy ? "Building…" : "Build the service" }}</button>
          <button type="button" class="h-[34px] rounded-sm border border-border px-[13px] text-[13px] text-ink-2 hover:border-border-strong" @click="convertOpen = false">Cancel</button>
        </div>
      </div>

      <!-- The composite link, from the monitor's end. It is an ANNOTATION: this block never claims
           the monitor stopped working, because it did not. -->
      <div v-if="monitor && (isComposite || monitor.superseded_by_service_id)" class="mb-4 rounded border border-border bg-surface px-4 py-3 text-[13px]" data-testid="successor-block">
        <div class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Superseded by</div>
        <p v-if="monitor.superseded_by_service_id" class="mt-2 leading-snug text-ink-2">
          <RouterLink :to="{ name: 'service', params: { id: monitor.superseded_by_service_id } }" class="text-accent hover:underline">{{ successorName || "the linked service" }}</RouterLink>
          now expresses what this monitor expresses. This monitor keeps probing and alerting until you retire it.
        </p>
        <p v-else class="mt-2 leading-snug text-ink-3">Nothing yet. Naming a successor changes nothing about this monitor — it is a note that says where the same question is now answered.</p>
        <div v-if="canWrite && !fileManaged" class="mt-3 flex flex-wrap items-end gap-2">
          <select v-model="successorChoice" class="h-[34px] w-[200px] rounded-sm border border-border bg-surface-2 px-3 text-[13px] outline-none focus:border-accent" data-testid="successor-select">
            <option value="">— none —</option>
            <option v-for="sv in services" :key="sv.id" :value="sv.id">{{ sv.name }}</option>
          </select>
          <button type="button" class="h-[34px] rounded-sm border border-border px-[13px] text-[13px] hover:border-accent hover:text-accent disabled:opacity-50" :disabled="lifecycleBusy" data-testid="successor-save" @click="saveSuccessor">Save</button>
        </div>
      </div>

      <p v-if="lifecycleError" class="mb-4 text-[12.5px] text-down" data-testid="lifecycle-error">{{ lifecycleError }}</p>

      <div v-if="monitor?.last_probe_error_reason" data-testid="monitor-probe-error" class="mb-4 rounded border border-degraded/50 bg-degraded-weak px-4 py-3 text-[13px] text-ink-2">
        <div class="font-semibold text-degraded">Executor could not run the latest credentialed probe</div>
        <div class="mt-1">Reason <code class="font-mono">{{ monitor.last_probe_error_reason }}</code><span v-if="monitor.last_probe_error_at"> · {{ relTime(monitor.last_probe_error_at) }} ago</span>. Monitor liveness was not changed by this error.</div>
      </div>

      <!-- SLA windows -->
      <div class="mb-4 grid grid-cols-4 gap-3 max-[900px]:grid-cols-2">
        <div
          v-for="w in windows"
          :key="w.window"
          class="flex flex-col gap-[9px] rounded border bg-surface p-[14px] shadow-card"
          :class="w.window === '30d' ? 'border-accent shadow-[0_0_0_1px_var(--accent-weak)]' : 'border-border'"
        >
          <div class="flex items-center gap-2">
            <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">{{ w.window }}<span v-if="w.window === '30d' && w.objective"> · SLO</span></span>
            <span v-if="w.error_budget" class="ml-auto rounded-full px-[7px] py-px text-[10.5px] font-semibold" :class="w.error_budget.met ? 'bg-up-weak text-up' : 'bg-down-weak text-down'">{{ w.error_budget.met ? "met" : "breach" }}</span>
          </div>
          <div class="font-mono text-[24px] font-medium leading-none tracking-tight tnum">{{ (w.uptime_percent ?? 0).toFixed(2) }}<span class="text-[13px] text-ink-3">%</span></div>
          <div class="font-mono text-[11.5px] text-ink-3 tnum">{{ windowSub(w) }}</div>
          <div class="h-[6px] overflow-hidden rounded-full bg-inset">
            <i class="block h-full rounded-full" :class="budgetMeter(w).cls" :style="{ width: budgetMeter(w).width + '%' }"></i>
          </div>
        </div>
      </div>

      <!-- response time -->
      <section v-if="!isPush" class="mb-4 rounded border border-border bg-surface shadow-card">
        <div class="flex items-center gap-[10px] border-b border-border px-4 py-[13px]">
          <h3 class="text-[13px] font-semibold">Response time</h3>
          <span class="font-mono text-[11.5px] text-ink-3" data-testid="lat-header">
            <template v-if="stats.avg != null">
              avg {{ fmtMs(stats.avg) }} · p95 {{ fmtMs(stats.p95!) }} ·
              <template v-if="stats.measured === stats.drawn">last {{ stats.drawn }} checks</template>
              <template v-else>{{ stats.measured }} of {{ stats.drawn }} checks with a recorded latency</template>
            </template>
            <template v-else-if="stats.drawn">no check recorded a latency</template>
          </span>
          <span class="flex-1"></span>
          <span class="font-mono text-[11.5px] text-ink-3" data-testid="lat-timeout">
            timeout {{ monitor?.timeout_seconds }}s<template v-if="!stats.timeoutInScale && stats.avg != null"> — outside this scale</template>
          </span>
        </div>
        <div class="px-4 pt-3">
          <!-- The invariant, stated once (§6.2): the space between points is unobserved time, and
               this panel makes no claim about whether a check was due there. -->
          <p class="mb-2 text-[11.5px] text-ink-3" data-testid="lat-subtitle">
            <template v-if="chart">
              last {{ stats.drawn }} checks · {{ instantLabel(isoInstant(new Date(chart.t0))) }} →
              {{ instantLabel(isoInstant(new Date(chart.t1))) }} · points only, no stroke and no fill —
              every interval between adjacent points is time cerbix did not observe, and this panel makes
              no claim about whether a check was due there.
            </template>
          </p>
          <svg
            v-if="chart"
            ref="plotBox"
            :viewBox="`0 0 ${chart.W} ${chart.H}`"
            class="block w-full"
            :style="{ height: '180px' }"
            preserveAspectRatio="none"
            data-testid="lat-plot"
          >
            <line x1="8" :y1="chart.baseY" :x2="chart.W - 8" :y2="chart.baseY" stroke="var(--border)" stroke-width="1" />
            <line
              v-if="chart.timeoutY !== null"
              x1="0" :y1="chart.timeoutY" :x2="chart.W" :y2="chart.timeoutY"
              stroke="var(--down)" stroke-width="1.4" stroke-dasharray="6 4" data-testid="lat-timeout-rule"
            />
            <line
              v-if="chart.p95y !== null"
              x1="0" :y1="chart.p95y" :x2="chart.W" :y2="chart.p95y"
              stroke="var(--degraded)" stroke-width="1.4" stroke-dasharray="5 4" stroke-opacity="0.9"
            />
            <!-- a failure that recorded no latency: a baseline mark, never a dropped point -->
            <rect
              v-for="(m, i) in chart.marks"
              :key="'m' + i"
              :x="m.x - 1.6" :y="chart.baseY - 9" width="3.2" height="9" rx="1"
              fill="var(--down)"
              data-testid="lat-baseline-mark"
              :data-ts="m.p.hb.ts"
              @pointerenter="hoverPoint = m.p" @pointerleave="hoverPoint = null"
            />
            <!-- FR-032 §14: the stroke, one polyline per DEFENSIBLE segment and nothing between
                 them. Drawn before the points so a marker always sits on top of its own line.
                 Where a segment ends the line simply stops: no dash, no fainter join. A dashed
                 line through unprovable time is the same claim in a costume. -->
            <polyline
              v-for="(d, i) in strokePaths"
              :key="'stroke' + i"
              :points="d"
              fill="none"
              stroke="var(--accent)"
              stroke-width="1.8"
              stroke-linejoin="round"
              stroke-linecap="round"
              data-testid="lat-stroke"
            />
            <circle
              v-for="(d, i) in chart.dots"
              :key="'d' + i"
              :cx="d.x" :cy="d.y"
              :r="hoverPoint === d.p ? 4.2 : 2.6"
              fill="var(--accent)"
              :stroke="hoverPoint === d.p ? 'var(--surface)' : 'none'"
              :stroke-width="hoverPoint === d.p ? 1.6 : 0"
              tabindex="0"
              data-testid="lat-point"
              :data-ts="d.p.hb.ts"
              @pointerenter="hoverPoint = d.p" @pointerleave="hoverPoint = null"
              @focus="hoverPoint = d.p" @blur="hoverPoint = null"
            />
          </svg>
          <p v-else class="py-6 text-[13px] text-ink-3">No checks recorded yet.</p>

          <!-- The observation ruler (§6.2): one neutral tick per RECORDED check and nothing
               between them, so unobserved time is drawn rather than left as whitespace. Its empty
               spans carry ONE meaning uniformly and there is no threshold on that meaning. -->
          <svg
            v-if="chart"
            :viewBox="`0 0 ${chart.W} 14`"
            class="block w-full"
            :style="{ height: '14px' }"
            preserveAspectRatio="none"
            role="img"
            aria-label="observation ruler: one tick per recorded check"
            data-testid="lat-ruler"
          >
            <line x1="8" y1="7" :x2="chart.W - 8" y2="7" stroke="var(--border)" stroke-width="1" />
            <rect v-for="(t, i) in chart.ticks" :key="'t' + i" :x="t.x - 0.8" y="1" width="1.6" height="11" rx="0.5" fill="var(--ink-2)" data-testid="lat-ruler-tick" />
            <rect
              v-for="(sp, i) in rulerSpans"
              :key="'s' + i"
              :x="chart.x(sp.fromMs)" y="0" :width="Math.max(0.4, chart.x(sp.toMs) - chart.x(sp.fromMs))" height="14"
              fill="transparent" tabindex="0" role="button"
              :aria-label="spanLabel(sp)"
              data-testid="lat-ruler-span"
              :data-intervals="sp.intervals"
              :data-merged="sp.merged ? 'true' : undefined"
              @pointerenter="hoverSpan = sp" @pointerleave="hoverSpan = null"
              @focus="hoverSpan = sp" @blur="hoverSpan = null"
            />
          </svg>

          <!-- The expectation ruler (FR-032 §14): one cell per DUE window, carrying its verdict.
               It does not replace the observation ruler above and could not: that one is what
               cerbix SAW, this one is what cerbix EXPECTED, and a monitor can have a dense band
               above and a broken one below. Collapsing them would make those states identical.
               The cells and the stroke come from the same windows, so the band is a legend for
               the line rather than a second opinion about it. -->
          <svg
            v-if="chart && expectedRuns"
            :viewBox="`0 0 ${chart.W} 14`"
            class="block w-full"
            :style="{ height: '14px' }"
            preserveAspectRatio="none"
            role="img"
            aria-label="expectation ruler: one cell per due window"
            data-testid="lat-expect-ruler"
          >
            <defs>
              <pattern id="er-hatch" width="6" height="6" patternUnits="userSpaceOnUse" patternTransform="rotate(45)">
                <rect width="6" height="6" fill="var(--inset)" />
                <rect width="2" height="6" fill="var(--border-strong)" />
              </pattern>
            </defs>
            <line x1="8" y1="7" :x2="chart.W - 8" y2="7" stroke="var(--border)" stroke-width="1" />
            <!-- A window nothing ran in is OUTLINED, never filled: a fill would read as a
                 recorded failure, and an empty window is a fact about emptiness. -->
            <rect
              v-for="(c, i) in expectationCells"
              :key="'ec' + i"
              :x="outlined(c.kind) ? c.x + 0.6 : c.x"
              :y="outlined(c.kind) ? 2.6 : 2"
              :width="outlined(c.kind) ? Math.max(0.4, c.w - 1.2) : c.w"
              :height="outlined(c.kind) ? 8.8 : 10"
              rx="1.5"
              :fill="c.kind === 'notStored' ? 'url(#er-hatch)' : c.kind === 'late' ? 'var(--degraded)' : c.kind === 'covered' ? 'var(--ink-3)' : 'none'"
              :opacity="c.kind === 'covered' ? 0.5 : 1"
              :stroke="c.kind === 'empty' ? 'var(--down)' : c.kind === 'reserved' ? 'var(--ink-3)' : 'none'"
              :stroke-width="outlined(c.kind) ? 1.4 : 0"
              data-testid="lat-expect-cell"
              :data-kind="c.kind"
              :data-verdict="c.verdict"
            />
            <!-- The hit areas. A cell is about two pixels wide at sixty checks, so what a pointer
                 or a Tab key reaches is a wider transparent rect over it — the same device the
                 observation ruler's empty spans use, and for the same reason. -->
            <rect
              v-for="(c, i) in expectationCells"
              :key="'eh' + i"
              :x="c.x - 2.5" y="0" :width="c.w + 5" height="14"
              fill="transparent" tabindex="0" role="button"
              :aria-label="cellLabel(c)"
              data-testid="lat-expect-hit"
              @pointerenter="hoverCell = c" @pointerleave="hoverCell = null"
              @focus="hoverCell = c" @blur="hoverCell = null"
            />
            <!-- Where the ledger starts answering at all. -->
            <rect v-if="ledgerFromX != null" :x="ledgerFromX - 0.9" y="0" width="1.8" height="14" fill="var(--ink-2)" data-testid="lat-ledger-from" />
          </svg>
        </div>

        <div class="flex flex-wrap gap-x-4 gap-y-1 px-4 pb-3 pt-2 text-[12px] text-ink-3">
          <span class="inline-flex items-center gap-[6px]"><i class="inline-block h-[8px] w-[8px] rounded-full bg-accent"></i> a recorded check</span>
          <span class="inline-flex items-center gap-[6px]"><i class="inline-block h-[9px] w-[3px] rounded-xs bg-down"></i> down with no latency recorded — drawn on the baseline, not dropped</span>
          <span class="inline-flex items-center gap-[6px]"><i class="inline-block h-0 w-[14px] border-t-2 border-dashed border-degraded"></i> p95 · last {{ stats.drawn }} checks</span>
          <span class="inline-flex items-center gap-[6px]"><i class="inline-block h-[11px] w-[2px] bg-ink-2"></i> observation ruler — its empty spans ARE unobserved time</span>
          <template v-if="expectedRuns">
            <span class="inline-flex items-center gap-[6px]"><i class="inline-block h-0 w-[14px] border-t-2 border-accent"></i> stroke — every window across it is <span class="font-mono">covered</span></span>
            <span class="inline-flex items-center gap-[6px]"><i class="inline-block h-[9px] w-[9px] rounded-xs border border-ink-3"></i> <span class="font-mono">reserved</span> — recorded before dispatch, no dispatch recorded: neither a missed run nor a covered one</span>
            <span class="inline-flex items-center gap-[6px]"><i class="inline-block h-[9px] w-[9px] rounded-xs bg-degraded"></i> <span class="font-mono">covered_late</span> — answered too far from its window</span>
            <span class="inline-flex items-center gap-[6px]"><i class="inline-block h-[9px] w-[9px] rounded-xs border-[1.5px] border-down"></i> a window nothing ran in</span>
          </template>
        </div>

        <!-- Readouts. Times are local with the offset named, over the canonical UTC instant. -->
        <!-- One cell's own words. It sits BEFORE the point readout because a window is the
             narrower claim: a reader hovering the expectation band is asking what the ledger says
             about that window, not what the nearest check measured. -->
        <div v-if="hoverCell" class="mx-4 mb-3 rounded-sm border border-border-strong bg-surface-2 p-[9px_11px]" data-testid="lat-cell-readout">
          <p class="text-[12.5px] text-ink-2">{{ cellLabel(hoverCell) }}</p>
          <p class="mt-[3px] font-mono text-[11.5px] text-ink-3">{{ utcInstantLabel(isoInstant(new Date(hoverCell.ms))) }}</p>
        </div>
        <div v-else-if="hoverPoint" class="mx-4 mb-3 rounded-sm border border-border-strong bg-surface-2 p-[9px_11px]" data-testid="lat-point-readout">
          <div class="font-mono text-[12.5px]">
            {{ instantLabel(hoverPoint.hb.ts) }} ·
            {{ hoverPoint.latency != null ? fmtMs(hoverPoint.latency) : "no latency recorded" }}
          </div>
          <div class="font-mono text-[11.5px] text-ink-3">{{ utcInstantLabel(hoverPoint.hb.ts) }}</div>
          <div class="mt-1 text-[12px] text-ink-2">
            <span :class="hoverPoint.hb.up ? 'text-up' : 'text-down'">{{ hoverPoint.hb.up ? "Operational" : "Down" }}</span>
            · <span class="font-mono">{{ heartbeatCode(hoverPoint.hb as Heartbeat) }}</span>
            · {{ hoverPoint.hb.msg || "ok" }}
          </div>
        </div>
        <div v-else-if="hoverSpan" class="mx-4 mb-3 rounded-sm border border-border-strong bg-surface-2 p-[9px_11px]" data-testid="lat-span-readout">
          <div class="font-mono text-[12.5px]">{{ spanLabel(hoverSpan) }}</div>
          <div class="font-mono text-[11.5px] text-ink-3">
            {{ utcInstantLabel(isoInstant(new Date(hoverSpan.fromMs))) }} → {{ utcInstantLabel(isoInstant(new Date(hoverSpan.toMs))) }}
          </div>
          <p class="mt-1 text-[11.5px] text-ink-3">
            <template v-if="hoverSpan.merged">
              one focus target covering {{ hoverSpan.intervals }} adjacent intervals too narrow to focus
              separately — its bounds are the real outer bounds, and the drawing is unchanged.
            </template>
            <template v-else>
              not late, not missed, not covered, not anomalous — only that no check was recorded here.
            </template>
          </p>
        </div>
        <p v-else-if="widestGap" class="mx-4 mb-3 text-[11.5px] text-ink-3" data-testid="lat-widest-gap">
          widest interval between two recorded checks: {{ gapLabel(widestGap.toMs - widestGap.fromMs) }},
          {{ instantLabel(isoInstant(new Date(widestGap.fromMs))) }} →
          {{ instantLabel(isoInstant(new Date(widestGap.toMs))) }} — the panel says only that, never
          that a check was missed.
        </p>
      </section>

      <!-- availability 90d -->
      <section class="mb-4 rounded border border-border bg-surface shadow-card">
        <div class="flex items-center gap-2 border-b border-border px-4 py-[13px]">
          <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Availability · 90 days</span>
          <span class="ml-auto font-mono text-[12px] text-ink-2">{{ timelineUptime }}</span>
        </div>
        <div class="px-4 pb-4 pt-[14px]">
          <div class="flex h-[34px] items-stretch gap-[2px]" data-testid="monitor-timeline">
            <span v-for="(d, i) in timeline" :key="i" class="min-w-0 flex-1 rounded-[2px]" :class="daySegClass(d.pct)" :title="d.pct === null ? d.label + ' · no data' : d.label + ' · ' + d.pct.toFixed(2) + '%'"></span>
          </div>
        </div>
      </section>

      <div class="grid grid-cols-[1.55fr_1fr] gap-4 max-[960px]:grid-cols-1">
        <!-- recent checks -->
        <section class="self-start rounded border border-border bg-surface shadow-card">
          <div class="flex items-center gap-[10px] border-b border-border px-4 py-[13px]">
            <h3 class="text-[13px] font-semibold">Recent checks</h3>
            <span class="ml-auto text-[11px] uppercase tracking-[0.07em] text-ink-3">last {{ Math.min(heartbeats.length, 12) }}</span>
          </div>
          <div class="overflow-x-auto">
            <table class="w-full text-[13px]">
              <thead>
                <tr class="text-[10.5px] uppercase tracking-[0.06em] text-ink-3">
                  <th class="border-b border-border px-4 py-[9px] text-left">Time</th>
                  <th class="border-b border-border px-4 py-[9px] text-left">Status</th>
                  <th class="border-b border-border px-4 py-[9px] text-left">Code</th>
                  <th class="border-b border-border px-4 py-[9px] text-left">Latency</th>
                  <th class="border-b border-border px-4 py-[9px] text-left">Result</th>
                </tr>
              </thead>
              <tbody>
                <tr
                  v-for="(h, i) in heartbeats.slice(0, 12)"
                  :key="i"
                  class="hover:bg-surface-2"
                  :class="hoverPoint && hoverPoint.hb.ts === h.ts ? 'bg-accent-weak' : ''"
                  :data-highlighted="hoverPoint && hoverPoint.hb.ts === h.ts ? 'true' : undefined"
                  data-testid="recent-check-row"
                >
                  <td class="border-b border-border px-4 py-[9px] font-mono text-ink-3">{{ relTime(h.ts) }} ago</td>
                  <td class="border-b border-border px-4 py-[9px]">
                    <span class="inline-flex items-center gap-[7px]"><span class="h-[7px] w-[7px] rounded-full" :class="h.up ? 'bg-up' : 'bg-down'"></span><span class="text-[12.5px] text-ink-2">{{ h.up ? "Operational" : "Down" }}</span></span>
                  </td>
                  <td class="border-b border-border px-4 py-[9px]"><span class="rounded-xs px-[6px] py-px font-mono text-[12.5px]" :class="h.up ? 'bg-up-weak text-up' : 'bg-down-weak text-down'">{{ heartbeatCode(h) }}</span></td>
                  <td class="border-b border-border px-4 py-[9px] font-mono">{{ h.latency_ms ? h.latency_ms + " ms" : "—" }}</td>
                  <td class="border-b border-border px-4 py-[9px] text-[12.5px] text-ink-3">{{ h.msg || "ok" }}</td>
                </tr>
                <tr v-if="!heartbeats.length && !loading"><td colspan="5" class="px-4 py-8 text-center text-[13px] text-ink-3">No checks recorded yet.</td></tr>
              </tbody>
            </table>
          </div>
        </section>

        <!-- right rail -->
        <div class="flex flex-col gap-4">
          <section v-if="monitor?.config?.password_ref" class="rounded border border-border bg-surface shadow-card">
            <div class="border-b border-border px-4 py-[13px]"><h3 class="text-[13px] font-semibold">Credential source</h3></div>
            <div class="px-4 py-3"><span class="rounded bg-inset px-2 py-1 font-mono text-[12px]">password_ref: {{ monitor.config.password_ref }}</span></div>
          </section>
          <section v-if="monitor && monitor.conditions && monitor.conditions.length" class="rounded border border-border bg-surface shadow-card">
            <div class="border-b border-border px-4 py-[13px]"><h3 class="text-[13px] font-semibold">Conditions</h3></div>
            <div v-for="(c, i) in monitor.conditions" :key="i" class="border-b border-border px-4 py-[11px] font-mono text-[13px] last:border-b-0">{{ c }}</div>
          </section>

          <!-- Dependency graph: parents that mute this monitor's alerts + dependents -->
          <section v-if="dependsOn.length || requiredBy.length" class="rounded border border-border bg-surface shadow-card">
            <div class="border-b border-border px-4 py-[13px]"><h3 class="text-[13px] font-semibold">Dependencies</h3></div>
            <div class="px-4 py-[6px]">
              <div v-for="p in dependsOn" :key="'d' + p.id" class="flex items-center gap-[10px] border-b border-border py-[8px] text-[13px] last:border-b-0">
                <span class="w-[86px] text-[11px] uppercase tracking-[0.05em] text-ink-3">depends on</span>
                <span class="h-[7px] w-[7px] rounded-full" :class="p.status === 'down' ? 'bg-down' : 'bg-up'"></span>
                <RouterLink :to="{ name: 'monitor', params: { id: p.id } }" class="font-mono hover:text-accent">{{ p.name }}</RouterLink>
                <span v-if="p.status === 'down'" class="text-[11.5px] text-ink-3">← suppressing this monitor's alerts</span>
              </div>
              <div v-for="c in requiredBy" :key="'r' + c.id" class="flex items-center gap-[10px] border-b border-border py-[8px] text-[13px] last:border-b-0">
                <span class="w-[86px] text-[11px] uppercase tracking-[0.05em] text-ink-3">required by</span>
                <span class="h-[7px] w-[7px] rounded-full" :class="c.status === 'down' ? 'bg-down' : 'bg-up'"></span>
                <RouterLink :to="{ name: 'monitor', params: { id: c.id } }" class="font-mono hover:text-accent">{{ c.name }}</RouterLink>
              </div>
            </div>
          </section>

          <section v-if="openIncident" class="rounded border border-border bg-surface shadow-card">
            <div class="flex items-center gap-[10px] border-b border-border px-4 py-[13px]">
              <h3 class="text-[13px] font-semibold text-degraded">Open incident</h3>
              <span class="ml-auto inline-flex items-center gap-[6px] rounded-full bg-degraded-weak px-[9px] py-px text-[11.5px] font-medium text-degraded"><span class="h-[7px] w-[7px] rounded-full bg-degraded"></span>{{ openIncident.status }}</span>
            </div>
            <RouterLink :to="{ name: 'incident', params: { id: openIncident.id } }" class="block px-4 py-[14px] hover:bg-surface-2">
              <div class="text-[14px] font-semibold">{{ openIncident.title }}</div>
              <div class="mt-1 font-mono text-[12px] text-ink-3">opened {{ relTime(openIncident.started_at) }} ago · {{ openIncident.source }}</div>
            </RouterLink>
          </section>

          <!-- push endpoint: the heartbeat URL is the whole point of a push monitor -->
          <section v-if="monitor && isPush && monitor.push_token" class="rounded border border-border bg-surface shadow-card">
            <div class="flex items-center gap-2 border-b border-border px-4 py-[13px]">
              <h3 class="text-[13px] font-semibold">Push endpoint</h3>
              <span class="rounded-full bg-accent-weak px-[9px] py-px text-[10.5px] font-semibold uppercase tracking-[0.04em] text-accent">dead-man's switch</span>
            </div>
            <div class="flex flex-col gap-3 px-4 py-[14px]">
              <div>
                <div class="mb-[5px] text-[10.5px] font-semibold uppercase tracking-[0.06em] text-ink-3">Heartbeat URL — POST at least every {{ monitor.interval_seconds }}s</div>
                <div class="flex items-center gap-2">
                  <code class="min-w-0 flex-1 overflow-x-auto whitespace-nowrap rounded-sm border border-border bg-inset px-[11px] py-[9px] font-mono text-[12.5px]">{{ pushUrl }}</code>
                  <button type="button" class="h-[34px] flex-none rounded-sm border border-border-strong px-3 text-[12.5px] text-ink-2 hover:border-ink-3" @click="copyPushUrl">{{ pushCopied ? "Copied ✓" : "Copy" }}</button>
                </div>
              </div>
              <pre class="overflow-x-auto rounded-sm border border-border bg-inset px-3 py-[10px] font-mono text-[12px] text-ink-2">*/1 * * * * curl -fsS -X POST {{ pushUrl }} &gt;/dev/null</pre>
              <p class="text-[12px] leading-relaxed text-ink-3">
                The monitor stays <b class="font-medium text-up">up</b> while heartbeats keep arriving; miss
                <span class="font-mono text-ink-2">interval + grace ({{ monitor.interval_seconds }}s + {{ monitor.grace_seconds || 0 }}s)</span>
                and it goes <b class="font-medium text-down">down</b>. The token is a secret — the URL needs no other authentication.
              </p>
            </div>
          </section>

          <section v-if="monitor" class="rounded border border-border bg-surface shadow-card">
            <div class="border-b border-border px-4 py-[13px]"><h3 class="text-[13px] font-semibold">Configuration</h3></div>
            <dl class="grid grid-cols-[auto_1fr] gap-x-[14px] gap-y-[9px] px-4 py-[14px] text-[13px]">
              <dt class="text-ink-3">Target</dt><dd class="m-0 break-all text-right font-mono text-[12.5px] text-ink-2">{{ monitor.target || "—" }}</dd>
              <dt class="text-ink-3">Type</dt><dd class="m-0 text-right font-mono text-[12.5px] text-ink-2">{{ monitor.type }}</dd>
              <template v-if="monitor.type === 'http'">
                <dt class="text-ink-3">Method</dt><dd class="m-0 text-right font-mono text-[12.5px] text-ink-2">{{ monitor.method || "GET" }}</dd>
              </template>
              <template v-if="!isPush">
                <dt class="text-ink-3">Interval</dt><dd class="m-0 text-right font-mono text-[12.5px] text-ink-2">{{ monitor.interval_seconds }}s</dd>
                <dt class="text-ink-3">Timeout</dt><dd class="m-0 text-right font-mono text-[12.5px] text-ink-2">{{ monitor.timeout_seconds }}s</dd>
                <dt class="text-ink-3">Retries</dt><dd class="m-0 text-right font-mono text-[12.5px] text-ink-2">{{ monitor.retries }}</dd>
                <dt class="text-ink-3">Failure threshold</dt><dd class="m-0 text-right font-mono text-[12.5px] text-ink-2">{{ monitor.failure_threshold || 1 }} checks</dd>
                <template v-if="monitor.confirm_interval_seconds">
                  <dt class="text-ink-3">Confirm interval</dt><dd class="m-0 text-right font-mono text-[12.5px] text-ink-2">{{ monitor.confirm_interval_seconds }}s</dd>
                </template>
                <template v-if="monitor.renotify_seconds">
                  <dt class="text-ink-3">Re-notify</dt><dd class="m-0 text-right font-mono text-[12.5px] text-ink-2">every {{ monitor.renotify_seconds }}s while down</dd>
                </template>
              </template>
              <template v-if="isPush">
                <dt class="text-ink-3">Grace period</dt><dd class="m-0 text-right font-mono text-[12.5px] text-ink-2">{{ monitor.grace_seconds || 0 }}s</dd>
              </template>
              <template v-if="monitor.escalation_policy_id">
                <dt class="text-ink-3">Escalation policy</dt>
                <dd class="m-0 text-right text-[12.5px]"><RouterLink :to="{ name: 'escalation' }" class="text-accent hover:underline">{{ escalationPolicyName || "attached" }}</RouterLink></dd>
              </template>
              <dt class="text-ink-3">Auto-incident</dt><dd class="m-0 text-right font-mono text-[12.5px] text-ink-2">{{ monitor.auto_incident === false ? "off" : "on" }}</dd>
              <template v-if="!isPush">
                <dt class="text-ink-3">Region</dt><dd class="m-0 text-right font-mono text-[12.5px] text-ink-2">{{ monitor.region || "core" }}</dd>
              </template>
              <template v-if="(monitor.tags || []).length">
                <dt class="text-ink-3">Tags</dt>
                <dd class="m-0 flex flex-wrap justify-end gap-[5px]">
                  <span v-for="t in monitor.tags" :key="t" class="rounded-full bg-inset px-[8px] py-px font-mono text-[10.5px] text-ink-3">{{ t }}</span>
                </dd>
              </template>
              <dt class="text-ink-3">Project</dt><dd class="m-0 text-right font-mono text-[12.5px] text-ink-2">{{ projectName }}</dd>
              <dt class="text-ink-3">Created</dt><dd class="m-0 text-right font-mono text-[12.5px] text-ink-2">{{ fmtDate(monitor.created_at) }}</dd>
              <dt class="text-ink-3">Updated</dt><dd class="m-0 text-right font-mono text-[12.5px] text-ink-2">{{ fmtDate(monitor.updated_at) }}</dd>
            </dl>
          </section>
        </div>
      </div>
    </div>
  </AppShell>
</template>
