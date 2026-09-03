<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref, watch } from "vue";
import { api } from "@/api/client";
import { canonicalObjective } from "@/lib/objective";
import type { components } from "@/api/schema";
import AppShell from "@/components/AppShell.vue";
import { useSession } from "@/stores/session";
import { useWorkspace } from "@/stores/workspace";
import { instantLabelShort } from "@/lib/wallclock";

type Monitor = components["schemas"]["Monitor"];
type WindowSLA = components["schemas"]["WindowSLA"];
type ErrorBudget = components["schemas"]["ErrorBudget"];
type MaintenanceWindow = components["schemas"]["MaintenanceWindow"];
type BurnRule = components["schemas"]["BurnRule"];

type SloStatus = "met" | "risk" | "breach" | "none";

interface Row {
  monitor: Monitor;
  windows: WindowSLA[];
}

const ws = useWorkspace();
const session = useSession();
const canWrite = computed(() => session.canProjectWrite(ws.orgId, ws.projectId));
const loading = ref(true);
const error = ref("");
const projectWindows = ref<WindowSLA[]>([]);
const rows = ref<Row[]>([]);
const maintenance = ref<MaintenanceWindow[]>([]);
const selectedWindow = ref<"24h" | "7d" | "30d" | "90d">("30d");

// Inline SLO-objective editing — one row at a time, driven by plain refs so
// reactivity is unambiguous (a previous per-id reactive-map approach failed to
// re-render on save).
const editingId = ref<string | null>(null);
const draft = ref("");
const savingSlo = ref(false);
const rowError = ref("");
const savedId = ref<string | null>(null); // row that just saved, for a brief ✓ flash

// ---- project objective (iter-0155, mock-project-objective.html) -----------
// A project objective is the promise about the WHOLE, and the existing budget card is a MEAN across
// monitors that happen to have one. Two numbers that could be mistaken for each other, so each says
// which question it answers.
//
// The current objective is read from the project SLA REPORT rather than through a second GET: the
// report already states the objective and the budget for a window that has one (and says nothing for a
// window that does not), so a separate read could only disagree with it.
// FR-031 §7 / D-0235: the card is READ-ONLY with an explicit Edit, and a successful save closes
// it. A closed editor cannot hold a stale draft, which retires by construction the state where an
// unsent draft and a stored fact rendered identically. But an open form is not a substitute for a
// draft-state model, so the five rules below hold regardless of the shape — visibility stops a
// stale draft being SHOWN, and the generation guard stops a stale response being WRITTEN.
const projDraft = ref("");
const projErr = ref("");
const projSaving = ref(false);
const projEditing = ref(false);
const projSaved = ref(false);
let projSavedTimer: ReturnType<typeof setTimeout> | undefined;
const projWindow = "30d"; // the approved mock's window; other windows show dashes until they have one
const projTarget = computed(() => projectWindows.value.find((w) => w.window === projWindow));
const projObjective = computed(() => projTarget.value?.objective ?? null);
const projBudgetLeft = computed(() => {
  const eb = projTarget.value?.error_budget;
  return eb ? Math.max(0, 100 - (eb.burned_percent ?? 0)) : null;
});

/** The canonical draft, or null when it is empty or outside the one rule. */
const projCanonical = computed(() => {
  const raw = String(projDraft.value ?? "").trim();
  return raw ? canonicalObjective(Number(raw)) : null;
});
/**
 * Save is offered only when it would DO something: a control that cannot change anything is not
 * offered. Equality is by the canonical value (D-0165), so `99.90` is not a change to `99.9`.
 */
const projDirty = computed(() => projCanonical.value !== null && projCanonical.value !== projObjective.value);
/**
 * The refusal is met AT THE FIELD, live, exactly as the monitor-description form meets its own:
 * with Save disabled for a value the server would refuse, requiring a CLICK to learn why would be
 * worse than the always-open form this replaces. `projErr` stays for transport failures only.
 */
const projInvalid = computed(() => String(projDraft.value ?? "").trim() !== "" && projCanonical.value === null);

function openProjectEditor() {
  projErr.value = "";
  projSaved.value = false;
  projDraft.value = projObjective.value != null ? String(projObjective.value) : "";
  projEditing.value = true;
}
function cancelProjectEditor() {
  projEditing.value = false;
  projDraft.value = "";
  projErr.value = "";
}
function flashProjectSaved() {
  projSaved.value = true;
  clearTimeout(projSavedTimer);
  projSavedTimer = setTimeout(() => (projSaved.value = false), 2500);
}

async function saveProjectObjective() {
  // The same ONE rule as every other scope, mirrored client-side (D-0165) so the operator reads why
  // instead of a 400: the open interval (0,100), four decimals, half-up.
  const objective = projCanonical.value;
  if (objective === null) {
    projErr.value = "Enter a target above 0 and below 100 (max 99.9999, e.g. 99.9).";
    return;
  }
  // The generation guard every neighbouring writer in this file already takes: a save fired under
  // project A that returns after the switch must write NOTHING into project B's screen.
  const gen = loadGen;
  projErr.value = "";
  projSaving.value = true;
  try {
    const res = await api.PUT("/api/v1/projects/{projectID}/sla-target", {
      params: { path: { projectID: ws.projectId } },
      body: { window: projWindow, objective },
    });
    if (gen !== loadGen) return;
    if (res.error) {
      projErr.value = "Could not save the project objective.";
      return;
    }
    // The draft is cleared and the editor closed on success, so an unsent draft can never render
    // as a stored fact — the defect the owner reported as "the buttons just stay there".
    projDraft.value = "";
    projEditing.value = false;
    flashProjectSaved();
    await load();
  } finally {
    if (gen === loadGen) projSaving.value = false;
  }
}

async function clearProjectObjective() {
  const gen = loadGen;
  projErr.value = "";
  projSaving.value = true;
  try {
    const res = await api.DELETE("/api/v1/projects/{projectID}/sla-target", {
      params: { path: { projectID: ws.projectId }, query: { window: projWindow } },
    });
    if (gen !== loadGen) return;
    if (res.error) {
      projErr.value = "Could not clear the project objective.";
      return;
    }
    projDraft.value = "";
    projEditing.value = false;
    await load();
  } finally {
    if (gen === loadGen) projSaving.value = false;
  }
}

// ---- burn-rate rules editor (multi-window, Google SRE) --------------------
// A rule fires when the burn is ≥ threshold in BOTH windows. Burn alerting is
// on iff at least one rule exists; the SRE default pair seeds new configs.
const rulesDraft = ref<BurnRule[]>([]);
const WINDOW_OPTS = [
  { label: "5 min", s: 300 },
  { label: "30 min", s: 1800 },
  { label: "1 hour", s: 3600 },
  { label: "6 hours", s: 21600 },
  { label: "24 hours", s: 86400 },
] as const;
function sreDefaults(): BurnRule[] {
  return [
    { long_window_seconds: 3600, short_window_seconds: 300, threshold: 14.4, severity: "page" },
    { long_window_seconds: 21600, short_window_seconds: 1800, threshold: 6, severity: "ticket" },
  ];
}
function addRule() {
  if (rulesDraft.value.length >= 4) return;
  rulesDraft.value.push({ long_window_seconds: 3600, short_window_seconds: 300, threshold: 14.4, severity: rulesDraft.value.length ? "ticket" : "page" });
}
function removeRule(i: number) {
  rulesDraft.value.splice(i, 1);
}
function validateRules(): string {
  for (const [i, r] of rulesDraft.value.entries()) {
    const thr = Number(r.threshold);
    if (!thr || thr <= 0) return `Rule ${i + 1}: burn-rate threshold must be positive.`;
    if ((r.long_window_seconds ?? 0) <= (r.short_window_seconds ?? 0)) return `Rule ${i + 1}: the long window must be longer than the short one.`;
  }
  return "";
}
// Collapsed-row badge: worst firing severity wins (page > ticket > enabled).
function burnBadge(w?: WindowSLA): { icon: string; cls: string; title: string } | null {
  if (!w?.burn_alert) return null;
  const rules = w.burn_rules ?? [];
  if (rules.some((r) => r.firing && r.severity === "page"))
    return { icon: "🔥", cls: "", title: "Page burn-rate rule firing right now" };
  if (rules.some((r) => r.firing))
    return { icon: "⚠️", cls: "", title: "Ticket burn-rate rule firing right now" };
  return { icon: "🔥", cls: "text-ink-3", title: "Burn-rate alerting enabled" };
}

// ---- window helpers ------------------------------------------------------
const w30 = (r: Row) => r.windows.find((w) => w.window === "30d");
const activeWin = (r: Row) => r.windows.find((w) => w.window === selectedWindow.value);

function budgetRemaining(eb?: ErrorBudget): number {
  if (!eb) return 0;
  if (eb.remaining_ratio != null) return Math.max(0, Math.min(100, eb.remaining_ratio * 100));
  return Math.max(0, 100 - (eb.burned_percent ?? 0));
}
function burnRate(eb?: ErrorBudget): number | null {
  if (!eb) return null;
  const allowed = eb.allowed_downtime_ratio ?? 0;
  const actual = eb.actual_downtime_ratio ?? 0;
  if (allowed <= 0) return actual > 0 ? 99 : 0;
  return actual / allowed;
}
function sloStatus(eb?: ErrorBudget): SloStatus {
  if (!eb) return "none";
  if (!eb.met) return "breach";
  return budgetRemaining(eb) <= 25 ? "risk" : "met";
}

function budgetMeter(eb?: ErrorBudget): { width: number; cls: string; label: string } | null {
  const st = sloStatus(eb);
  if (st === "none") return null;
  if (st === "breach") return { width: 100, cls: "bg-down", label: "0%" };
  const remaining = budgetRemaining(eb);
  return { width: remaining, cls: st === "risk" ? "bg-degraded" : "bg-up", label: `${Math.round(remaining)}%` };
}

const stLabel: Record<SloStatus, string> = { met: "Meeting", risk: "At risk", breach: "Breaching", none: "No SLO" };
const stPill: Record<SloStatus, string> = {
  met: "text-up bg-up-weak",
  risk: "text-degraded bg-degraded-weak",
  breach: "text-down bg-down-weak",
  none: "text-ink-3 bg-inset",
};
const stDot: Record<SloStatus, string> = { met: "bg-up", risk: "bg-degraded", breach: "bg-down", none: "bg-ink-3" };

function burnFmt(b: number | null): string {
  if (b == null) return "—";
  if (b >= 99) return "∞";
  return `${b.toFixed(1)}×`;
}
function burnCls(b: number | null): string {
  if (b == null) return "text-ink-3";
  if (b >= 2) return "text-down";
  if (b > 1) return "text-degraded";
  return "text-ink-2";
}
function objFmt(v?: number | null): string {
  return v == null ? "—" : `${v}%`;
}

// ---- 30-day roll-up KPIs (fixed window, per the page header) -------------
const sloRows = computed(() => rows.value.filter((r) => w30(r)?.error_budget));
const composite30 = computed(() => projectWindows.value.find((w) => w.window === "30d")?.uptime_percent);
const metCount = computed(() => sloRows.value.filter((r) => w30(r)!.error_budget!.met).length);
const breachCount = computed(() => sloRows.value.filter((r) => !w30(r)!.error_budget!.met).length);
const budgetRemainingAvg = computed(() => {
  if (!sloRows.value.length) return null;
  const sum = sloRows.value.reduce((a, r) => a + budgetRemaining(w30(r)!.error_budget), 0);
  return sum / sloRows.value.length;
});
const burnAvg = computed(() => {
  const vals = sloRows.value.map((r) => burnRate(w30(r)!.error_budget)).filter((v): v is number => v != null);
  if (!vals.length) return null;
  return vals.reduce((a, b) => a + b, 0) / vals.length;
});
// Budget-remaining bar for the summary panel (mirrors the burn-down artifact).
const budgetPanel = computed(() => {
  const rem = budgetRemainingAvg.value;
  if (rem == null) return null;
  const cls = rem <= 10 ? "bg-down" : rem <= 30 ? "bg-degraded" : "bg-up";
  return { rem, cls };
});

function pct2(n?: number) {
  return n === undefined ? "—" : `${n.toFixed(2)}%`;
}
// NFR-025b: a rendered timestamp names its zone. This used to be a bare `toLocaleString`, so a
// report time read `03.09.2026, 17:55` with nothing saying whose 17:55 — beside a reliability card
// that renders UTC dates. Minute precision is kept; the offset is not optional.
function fmt(ts?: string) {
  return instantLabelShort(ts);
}
function duration(a?: string, b?: string): string {
  if (!a || !b) return "";
  const m = Math.round((new Date(b).getTime() - new Date(a).getTime()) / 60000);
  if (m < 60) return `${m}m`;
  return `${Math.floor(m / 60)}h ${m % 60}m`;
}
const isUpcoming = (a?: string) => (a ? new Date(a).getTime() > Date.now() : false);
const monitorName = (id?: string | null) =>
  id ? (rows.value.find((r) => r.monitor.id === id)?.monitor.name ?? "monitor") : "Project-wide";

// ---- data loading --------------------------------------------------------
async function loadRow(m: Monitor): Promise<Row> {
  const res = await api.GET("/api/v1/monitors/{monitorID}/sla", { params: { path: { monitorID: m.id! } } });
  return { monitor: m, windows: res.data?.windows ?? [] };
}

async function load() {
  // The generation is captured BEFORE the first await and checked before EVERY write —
  // success, error and finally alike. Without this, a slow response for project A landed its
  // windows, maintenance and rows into an already-selected project B: the store refuses a
  // cross-tenant confirm, but a screen showing A's numbers under B's name misleads either way.
  const gen = loadGen;
  loading.value = true;
  error.value = "";
  try {
    await ws.init();
    const projectID = ws.projectId;
    if (gen !== loadGen) return;
    if (!projectID) {
      rows.value = [];
      projectWindows.value = [];
      maintenance.value = [];
      return;
    }
    const [projSla, monitors, maint] = await Promise.all([
      api.GET("/api/v1/projects/{projectID}/sla", { params: { path: { projectID } } }),
      api.GET("/api/v1/projects/{projectID}/monitors", { params: { path: { projectID } } }),
      api.GET("/api/v1/projects/{projectID}/maintenance", { params: { path: { projectID } } }),
    ]);
    if (gen !== loadGen) return;
    projectWindows.value = projSla.data?.windows ?? [];
    reportEnabled.value = projSla.data?.sla_report_weekly ?? false;
    // Archived windows stay in the API payload as history, not in the scheduler list: before
    // this filter an archived window came back on every reload, undoing the archive visually.
    maintenance.value = (maint.data ?? []).filter((w) => !w.archived_at);
    const loaded = await Promise.all((monitors.data ?? []).map(loadRow));
    if (gen !== loadGen) return;
    rows.value = loaded;
  } catch {
    if (gen !== loadGen) return;
    error.value = "Could not load SLA data.";
  } finally {
    if (gen === loadGen) loading.value = false;
  }
}

function editSlo(r: Row) {
  const obj = w30(r)?.objective;
  draft.value = obj != null ? String(obj) : "99.9";
  const w = w30(r);
  // Deep-copy the current rule set; a fresh burn config starts empty (the user
  // opts in via "Reset to SRE defaults" or "+ Add rule").
  rulesDraft.value = (w?.burn_alert ? (w?.burn_rules ?? []) : []).map((x) => ({ ...x }));
  rowError.value = "";
  savedId.value = null;
  editingId.value = r.monitor.id!;
}
function cancelEdit() {
  editingId.value = null;
  rowError.value = "";
}
async function saveSlo(m: Monitor) {
  // `draft` is bound to <input type="number">, so v-model may hand back a number,
  // not a string — normalize before any string ops (a bare .trim() throws on a number).
  const raw = String(draft.value ?? "").trim();
  // The ONE objective rule, mirrored from the server (D-0165): raw in the open (0,100),
  // canonical half-up at four decimals, canonical also in (0,100) — 99.99995 rounds to 100
  // and is rejected HERE, not as a server 400; the canonical value is what gets sent.
  const objective = raw ? canonicalObjective(Number(raw)) : null;
  if (objective === null) {
    rowError.value = "Enter a target above 0 and below 100 (max 99.9999, e.g. 99.9).";
    return;
  }
  const ruleErr = validateRules();
  if (ruleErr) {
    rowError.value = ruleErr;
    return;
  }
  rowError.value = "";
  savingSlo.value = true;
  const gen = loadGen;
  try {
    const rules = rulesDraft.value.map((r) => ({ ...r, threshold: Number(r.threshold) }));
    const res = await api.PUT("/api/v1/monitors/{monitorID}/sla-target", {
      params: { path: { monitorID: m.id! } },
      body: { objective, window: "30d", burn_alert: rules.length > 0, burn_rules: rules },
    });
    if (gen !== loadGen) return;
    if (res.error) {
      rowError.value = (res.error as { error?: string })?.error || "Could not save the objective.";
      return;
    }
    const updated = await loadRow(m);
    if (gen !== loadGen) return;
    const idx = rows.value.findIndex((r) => r.monitor.id === m.id);
    if (idx >= 0) rows.value[idx] = updated;
    editingId.value = null; // close the editor
    savedId.value = m.id!; // brief ✓ flash on the row
    setTimeout(() => {
      if (savedId.value === m.id) savedId.value = null;
    }, 1600);
  } catch {
    if (gen === loadGen) rowError.value = "Could not save the objective.";
  } finally {
    if (gen === loadGen) savingSlo.value = false;
  }
}

// ---- weekly SLA report toggle -------------------------------------------
const reportEnabled = ref(false);
const reportSaving = ref(false);
async function toggleReport() {
  if (!ws.projectId || reportSaving.value) return;
  const gen = loadGen;
  const next = !reportEnabled.value;
  reportSaving.value = true;
  try {
    const res = await api.PUT("/api/v1/projects/{projectID}/sla-report", {
      params: { path: { projectID: ws.projectId } },
      body: { enabled: next },
    });
    if (gen !== loadGen) return; // project A's toggle must not land on project B's switch
    if (!res.error) reportEnabled.value = res.data?.sla_report_weekly ?? next;
  } finally {
    if (gen === loadGen) reportSaving.value = false;
  }
}

// ---- maintenance scheduler ----------------------------------------------
const showMaint = ref(false);
const maintForm = reactive({ monitor_id: "", starts_at: "", ends_at: "", reason: "" });
const maintSaving = ref(false);
const maintError = ref("");

// A window that reaches back over SEALED reliability facts rewrites numbers somebody may
// already have quoted, so the API refuses it without a token issued for exactly that change.
// This is the operator's half of that: ask what would change, show it, and only then confirm.
// Without it the ordinary "we had maintenance last night" gets a 409 with nowhere to go.
type PreviewSplit = components["schemas"]["ReliabilitySplit"];
type PreviewService = { service_id: string; before: PreviewSplit; after: PreviewSplit; projected: boolean; reason?: string };
// annulTarget is the window an annul preview is for; null means the preview is a create.
const preview = ref<{ preview_id: string; coverage: string; services: PreviewService[] } | null>(null);
const annulTarget = ref<MaintenanceWindow | null>(null);

function pct(s: PreviewSplit): string {
  const decided = (s.good_us ?? 0) + (s.bad_us ?? 0);
  if (decided === 0) return "—";
  return (((s.good_us ?? 0) / decided) * 100).toFixed(3) + "%";
}

// Health beside availability: a change can move one without the other, and a table showing
// only the first says "no change" for a change.
// Three different remediations, three different words. A single "not projected" made the
// operator guess between narrowing the range, retrying, and a range that can never run.
function reasonLabel(reason?: string): string {
  switch (reason) {
    case "range_too_long":
      return "range too long — narrow it";
    case "wall_budget":
      return "timed out — try again";
    case "evidence_gone":
      return "evidence deleted — unrecomputable";
    default:
      return "not projected";
  }
}

// The aggregate line derives from the rows' actual reasons: a hardcoded "range too long"
// under a wall_budget or evidence_gone row told the operator the wrong remediation.
function approximateSummary(services: PreviewService[]): string {
  const reasons = new Set(services.filter((s) => !s.projected).map((s) => s.reason ?? ""));
  const parts: string[] = [];
  if (reasons.has("range_too_long")) parts.push("the range is too long to project exactly — narrow it");
  if (reasons.has("wall_budget")) parts.push("projection timed out — try again");
  if (reasons.has("evidence_gone")) parts.push("some evidence was deleted — those ranges can never be recomputed");
  if (!parts.length) parts.push("the projection is incomplete");
  return "This preview cannot be confirmed: " + parts.join("; ") + ".";
}

function healthPct(s: PreviewSplit): string {
  const decided = (s.healthy_us ?? 0) + (s.degraded_us ?? 0) + (s.down_us ?? 0);
  if (decided === 0) return "—";
  return (((s.healthy_us ?? 0) / decided) * 100).toFixed(3) + "%";
}

// loadGen invalidates in-flight answers: a response that started under another project (or an
// older click) must not render into the current one. The store refuses a cross-tenant confirm
// anyway, but a preview of project A drawn inside project B misattributes state either way.
let loadGen = 0;

function resetMaintenanceState() {
  loadGen++;
  preview.value = null;
  annulTarget.value = null;
  maintError.value = "";
  maintForm.monitor_id = "";
  maintForm.starts_at = "";
  maintForm.ends_at = "";
  maintForm.reason = "";
  showMaint.value = false;
  // The collections are per-tenant too: keeping project A's windows on screen while B loads
  // shows A's data under B's name for however long the requests take.
  rows.value = [];
  projectWindows.value = [];
  maintenance.value = [];
  // …and so is every piece of editor and busy state. A toggle rendered from project A while
  // B loads, or a busy flag a late response never clears, misleads exactly as long as the
  // stale collections would have.
  // …the project-objective card included. It was the ONE editor this function did not cover, and
  // a value typed for project A was still in the box under project B, where Save wrote it as a
  // perfectly legitimate write the store cannot refuse (D-0235 decision 11, reviewer P0).
  projDraft.value = "";
  projErr.value = "";
  projSaving.value = false;
  projEditing.value = false;
  projSaved.value = false;
  reportEnabled.value = false;
  reportSaving.value = false;
  editingId.value = null;
  rowError.value = "";
  savingSlo.value = false;
  savedId.value = null;
  maintSaving.value = false;
}

onUnmounted(() => {
  // A response landing after the view is gone must write nothing.
  loadGen++;
});

async function addMaintenance(previewID?: string) {
  if (!ws.projectId || !maintForm.starts_at || !maintForm.ends_at) return;
  maintSaving.value = true;
  maintError.value = "";
  const body: components["schemas"]["CreateMaintenance"] = {
    starts_at: new Date(maintForm.starts_at).toISOString(),
    ends_at: new Date(maintForm.ends_at).toISOString(),
    reason: maintForm.reason,
  };
  if (maintForm.monitor_id) body.monitor_id = maintForm.monitor_id;
  if (previewID) body.preview_id = previewID;
  const gen = loadGen;
  try {
    const res = await api.POST("/api/v1/projects/{projectID}/maintenance", {
      params: { path: { projectID: ws.projectId } },
      body,
    });
    if (gen !== loadGen) return; // the project moved under this response
    if (res.error || !res.data) {
      const code = (res.error as { error?: string })?.error || "";
      if (code === "preview_required") {
        // Not an error the operator caused — the change simply has to be shown first.
        await loadPreview();
        return;
      }
      maintError.value =
        code === "preview_stale"
          ? "Something changed while you were looking. Preview again to see the current effect."
          : code === "preview_approximate"
            ? "This range is too long to project exactly, so it cannot be confirmed. Narrow it."
            : code || "Could not schedule maintenance.";
      return;
    }
    maintenance.value.push(res.data);
    maintForm.monitor_id = "";
    maintForm.starts_at = "";
    maintForm.ends_at = "";
    maintForm.reason = "";
    preview.value = null;
    showMaint.value = false;
  } catch {
    if (gen === loadGen) maintError.value = "Could not schedule maintenance.";
  } finally {
    if (gen === loadGen) maintSaving.value = false;
  }
}

async function loadPreview() {
  const gen = loadGen;
  const res = await api.POST("/api/v1/projects/{projectID}/maintenance/preview", {
    params: { path: { projectID: ws.projectId } },
    body: {
      monitor_id: maintForm.monitor_id || undefined,
      mutation: "create",
      starts_at: new Date(maintForm.starts_at).toISOString(),
      ends_at: new Date(maintForm.ends_at).toISOString(),
    },
  });
  if (gen !== loadGen) return;
  if (res.error || !res.data) {
    maintError.value = (res.error as { error?: string })?.error || "Could not preview the change.";
    return;
  }
  annulTarget.value = null;
  preview.value = res.data as typeof preview.value;
}

// ---- annul: say a window never applied -----------------------------------
//
// Distinct from archiving on purpose. Archiving retires a window and keeps its past effect;
// annulling REWRITES history, so it goes preview → shown change → tokened confirm, exactly
// like a retroactive create.
async function startAnnul(w: MaintenanceWindow) {
  if (!w.id || !w.starts_at || !w.ends_at) return;
  const gen = loadGen;
  maintError.value = "";
  const res = await api.POST("/api/v1/projects/{projectID}/maintenance/preview", {
    params: { path: { projectID: ws.projectId } },
    body: {
      monitor_id: w.monitor_id || undefined,
      mutation: "annul",
      maintenance_id: w.id,
      starts_at: w.starts_at,
      ends_at: w.ends_at,
    },
  });
  if (gen !== loadGen) return;
  if (res.error || !res.data) {
    maintError.value = (res.error as { error?: string })?.error || "Could not preview the annul.";
    return;
  }
  showMaint.value = true;
  annulTarget.value = w;
  preview.value = res.data as typeof preview.value;
}

async function confirmAnnul() {
  const target = annulTarget.value;
  const token = preview.value?.preview_id;
  if (!target || !token) return;
  const gen = loadGen;
  maintSaving.value = true;
  try {
    const res = await api.POST("/api/v1/maintenance/{maintenanceID}/annul", {
      params: { path: { maintenanceID: target.id! } },
      body: { preview_id: token },
    });
    if (gen !== loadGen) return;
    if (res.error) {
      maintError.value = (res.error as { error?: string })?.error || "Could not annul the window.";
      return;
    }
    maintenance.value = maintenance.value.filter((w) => w.id !== target.id);
    preview.value = null;
    annulTarget.value = null;
    showMaint.value = false;
  } finally {
    if (gen === loadGen) maintSaving.value = false;
  }
}

async function deleteMaintenance(id: string) {
  const gen = loadGen;
  const res = await api.DELETE("/api/v1/maintenance/{maintenanceID}", { params: { path: { maintenanceID: id } } });
  if (gen !== loadGen) return; // a late archive from project A must not edit B's list
  if (!res.error) maintenance.value = maintenance.value.filter((w) => w.id !== id);
}

onMounted(load);
watch(() => ws.projectId, () => {
  // Sensitive per-tenant state does not survive a project switch: a preview computed for
  // project A must never be confirmable while the screen shows project B.
  resetMaintenanceState();
  load();
});
</script>

<template>
  <AppShell active="sla" :crumbs="[ws.orgName || 'cerbix', ws.projectName || '…', 'SLA & SLO']">
    <div class="mx-auto max-w-[1120px] px-[22px] pb-16 pt-6">
      <!-- page head -->
      <div class="mb-[18px] flex flex-wrap items-start gap-[14px]">
        <div>
          <h1 class="text-[21px] font-semibold tracking-tight">SLA &amp; SLO</h1>
          <p class="mt-[3px] text-[13px] text-ink-3">
            Objectives, error budgets and burn rate across <b class="text-ink-2">{{ ws.projectName || "…" }}</b>.
          </p>
        </div>
        <div v-if="canWrite" class="ml-auto flex items-center gap-2">
          <button
            type="button"
            :disabled="reportSaving"
            class="inline-flex h-[34px] items-center gap-[7px] rounded-sm border px-[13px] text-[13px] disabled:opacity-50"
            :class="reportEnabled ? 'border-accent bg-accent-weak text-accent' : 'border-border bg-surface text-ink hover:border-border-strong'"
            :title="reportEnabled ? 'Weekly SLA report is on — sent to this project\'s channels' : 'Email a weekly SLA summary to this project\'s channels'"
            @click="toggleReport"
          >
            <svg viewBox="0 0 24 24" class="h-[15px] w-[15px]" fill="none" stroke="currentColor" stroke-width="2"><path d="M4 5h16M4 12h16M4 19h10" /></svg>
            Weekly report{{ reportEnabled ? " · on" : "" }}
          </button>
          <button type="button" class="inline-flex h-[34px] items-center gap-[7px] rounded-sm border border-border bg-surface px-[13px] text-[13px] text-ink hover:border-border-strong" @click="showMaint = !showMaint">
            <svg viewBox="0 0 24 24" class="h-[15px] w-[15px]" fill="none" stroke="currentColor" stroke-width="2"><path d="M12 5v14M5 12h14" /></svg>
            Maintenance
          </button>
        </div>
      </div>

      <div v-if="error" class="rounded border border-down/40 bg-down-weak p-4 text-[13px] text-down">{{ error }}</div>

      <template v-else>
        <!-- summary: KPIs + budget-remaining panel -->
        <div class="mb-4 grid grid-cols-[1.1fr_1fr] gap-4 max-[960px]:grid-cols-1">
          <div class="grid grid-cols-2 gap-3">
            <div class="flex flex-col gap-[7px] rounded border border-border bg-surface p-[14px] shadow-card">
              <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Composite availability · 30d</span>
              <span class="font-mono text-[24px] font-medium leading-none tracking-tight tnum">{{ composite30 !== undefined ? composite30.toFixed(2) : "—" }}<small class="text-[13px] text-ink-3">%</small></span>
              <span class="text-[12px] text-ink-3">across {{ sloRows.length }} SLO monitor{{ sloRows.length === 1 ? "" : "s" }}</span>
            </div>
            <div class="flex flex-col gap-[7px] rounded border border-border bg-surface p-[14px] shadow-card">
              <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Meeting SLO</span>
              <span class="font-mono text-[24px] font-medium leading-none tracking-tight tnum">{{ metCount }}<small class="text-[13px] text-ink-3"> / {{ sloRows.length }}</small></span>
              <span class="text-[12px]" :class="breachCount ? 'text-down' : 'text-ink-3'">{{ breachCount ? `${breachCount} breaching budget` : "all within budget" }}</span>
            </div>
            <div class="flex flex-col gap-[7px] rounded border border-border bg-surface p-[14px] shadow-card">
              <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Budget remaining</span>
              <span class="font-mono text-[24px] font-medium leading-none tracking-tight tnum">{{ budgetRemainingAvg != null ? Math.round(budgetRemainingAvg) : "—" }}<small class="text-[13px] text-ink-3">%</small></span>
              <span class="text-[12px] text-ink-3">of the 30-day pool</span>
            </div>
            <div class="flex flex-col gap-[7px] rounded border border-border bg-surface p-[14px] shadow-card">
              <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Burn rate</span>
              <span class="font-mono text-[24px] font-medium leading-none tracking-tight tnum" :class="burnCls(burnAvg)">{{ burnFmt(burnAvg) }}</span>
              <span class="text-[12px] text-ink-3">{{ burnAvg != null && burnAvg > 1 ? "elevated — burning faster than budget" : "steady within budget" }}</span>
            </div>
          </div>

          <!-- project objective: the promise about the whole, next to the mean across its parts -->
          <div class="mb-[14px] flex flex-col gap-[10px] rounded border border-border bg-surface p-[14px_16px] shadow-card" data-testid="project-objective">
            <div class="flex items-baseline gap-[10px]">
              <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Project objective · 30d</span>
              <span class="ml-auto font-mono text-[12px] text-ink-3" title="A project objective cannot page: the schema refuses burn alerting at this scope.">reporting only</span>
            </div>
            <div class="flex items-end gap-[14px]">
              <span v-if="projObjective != null" class="font-mono text-[32px] font-medium leading-none tracking-tight tnum" data-testid="project-objective-value">
                {{ projObjective }}<small class="text-[16px] text-ink-3">%</small>
              </span>
              <span v-else class="font-mono text-[15px] text-ink-3" data-testid="project-objective-unset">not set</span>
              <span v-if="projBudgetLeft != null" class="pb-1 text-[12px] text-ink-3" data-testid="project-objective-budget">
                {{ Math.round(projBudgetLeft) }}% of the project's own budget left
              </span>
              <span v-else class="pb-1 text-[12px] text-ink-3">the card below is a mean across monitors — not a promise about the project</span>
            </div>
            <!-- Read-only with an explicit Edit; a successful save closes it. A closed editor
                 cannot hold a stale draft (FR-031 §7). -->
            <div v-if="canWrite && !projEditing" class="flex items-center gap-[10px]">
              <span v-if="projSaved" class="font-mono text-[12.5px] text-up" data-testid="project-objective-saved">✓ saved</span>
              <span class="flex-1"></span>
              <button
                type="button"
                class="h-[32px] rounded-sm border border-border-strong px-[14px] text-[12.5px] text-ink-2 hover:border-accent hover:text-accent"
                data-testid="project-objective-edit"
                @click="openProjectEditor"
              >
                Edit
              </button>
            </div>
            <div v-else-if="canWrite" class="flex flex-col gap-[6px]">
              <div class="flex items-center gap-[10px]">
                <input
                  v-model="projDraft"
                  type="number"
                  step="0.0001"
                  :placeholder="projObjective != null ? String(projObjective) : '99.9'"
                  class="h-[32px] w-[110px] rounded-sm border bg-surface px-[10px] text-right font-mono text-[13px] text-ink"
                  :class="projDirty ? 'border-accent' : 'border-border-strong'"
                  aria-label="Project objective"
                  data-testid="project-objective-input"
                />
                <button
                  type="button"
                  class="h-[32px] rounded-sm border border-accent bg-accent px-[14px] text-[12.5px] text-accent-ink disabled:cursor-not-allowed disabled:opacity-45"
                  :disabled="projSaving || !projDirty"
                  data-testid="project-objective-save"
                  @click="saveProjectObjective"
                >
                  Save
                </button>
                <button
                  type="button"
                  class="h-[32px] rounded-sm border border-border-strong px-[12px] text-[12.5px] text-ink-2 hover:border-accent hover:text-accent disabled:opacity-60"
                  :disabled="projSaving"
                  data-testid="project-objective-cancel"
                  @click="cancelProjectEditor"
                >
                  Cancel
                </button>
                <span class="flex-1"></span>
                <button
                  v-if="projObjective != null"
                  type="button"
                  class="h-[32px] rounded-sm border border-border-strong px-[12px] text-[12.5px] text-ink-2 hover:border-accent hover:text-accent disabled:opacity-60"
                  :disabled="projSaving"
                  data-testid="project-objective-clear"
                  @click="clearProjectObjective"
                >
                  Clear
                </button>
              </div>
              <!-- The card can SAY which state it is in, which is the whole fix: an unsent draft
                   and a stored fact used to render identically. -->
              <p v-if="projInvalid" class="text-[11.5px] text-down" data-testid="project-objective-invalid">
                Enter a target above 0 and below 100 (max 99.9999, e.g. 99.9).
              </p>
              <p v-else-if="projDirty" class="text-[11.5px] text-accent" data-testid="project-objective-dirty">
                unsaved draft {{ projCanonical }}%
              </p>
              <p v-else class="text-[11.5px] text-ink-3" data-testid="project-objective-clean">
                unchanged — nothing to save
              </p>
            </div>
            <p v-if="projErr" class="text-[12.5px] text-down" data-testid="project-objective-error">{{ projErr }}</p>
          </div>

          <div class="flex flex-col rounded border border-border bg-surface p-[14px_16px] shadow-card">
            <div class="mb-[10px] flex items-baseline gap-[10px]">
              <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Error budget remaining · 30d</span>
              <span class="ml-auto font-mono text-[12px] text-ink-2">{{ budgetPanel ? Math.round(budgetPanel.rem) + "% left" : "—" }}</span>
            </div>
            <template v-if="budgetPanel">
              <div class="mt-1 flex items-end gap-[14px]">
                <span class="font-mono text-[40px] font-medium leading-none tracking-tight tnum" :class="budgetPanel.rem <= 10 ? 'text-down' : budgetPanel.rem <= 30 ? 'text-degraded' : ''">{{ Math.round(budgetPanel.rem) }}<small class="text-[16px] text-ink-3">%</small></span>
                <span class="pb-1 text-[12px] text-ink-3">mean across SLO monitors</span>
              </div>
              <div class="mt-[14px] h-[10px] overflow-hidden rounded-full bg-inset">
                <i class="block h-full rounded-full" :class="budgetPanel.cls" :style="{ width: budgetPanel.rem + '%' }"></i>
              </div>
              <div class="mt-[10px] flex items-center gap-3 text-[11.5px] text-ink-3">
                <span class="inline-flex items-center gap-[6px]"><span class="h-[7px] w-[7px] rounded-full bg-up"></span>{{ metCount }} meeting</span>
                <span v-if="breachCount" class="inline-flex items-center gap-[6px]"><span class="h-[7px] w-[7px] rounded-full bg-down"></span>{{ breachCount }} breaching</span>
                <span class="ml-auto font-mono" :class="burnCls(burnAvg)">burn {{ burnFmt(burnAvg) }}</span>
              </div>
            </template>
            <p v-else class="flex flex-1 items-center text-[13px] text-ink-3">No SLO objectives defined yet.</p>
          </div>
        </div>

        <!-- objectives table -->
        <div class="mb-3 mt-[6px] flex items-center gap-[10px]">
          <h2 class="text-[13px] font-semibold">Objectives by monitor</h2>
          <span class="font-mono text-[12px] text-ink-3">{{ rows.length }}</span>
          <div class="ml-auto inline-flex overflow-hidden rounded-sm border border-border">
            <button
              v-for="wname in (['24h', '7d', '30d', '90d'] as const)"
              :key="wname"
              type="button"
              class="border-r border-border px-[11px] py-[5px] text-[12px] last:border-r-0"
              :class="selectedWindow === wname ? 'bg-surface-2 font-medium text-ink' : 'bg-surface text-ink-2 hover:text-ink'"
              @click="selectedWindow = wname"
            >
              {{ wname }}
            </button>
          </div>
        </div>

        <section class="overflow-x-auto rounded border border-border bg-surface shadow-card">
          <table class="w-full text-[13px]">
            <thead>
              <tr class="text-[10.5px] uppercase tracking-[0.06em] text-ink-3">
                <th class="border-b border-border px-[14px] py-[10px] text-left">Monitor</th>
                <th class="border-b border-border px-[14px] py-[10px] text-right">SLO</th>
                <th class="border-b border-border px-[14px] py-[10px] text-right">SLI · {{ selectedWindow }}</th>
                <th class="border-b border-border px-[14px] py-[10px] text-right">Error budget</th>
                <th class="border-b border-border px-[14px] py-[10px] text-right">Burn</th>
                <th class="border-b border-border px-[14px] py-[10px] text-left">Status</th>
              </tr>
            </thead>
            <tbody>
              <template v-for="r in rows" :key="r.monitor.id">
              <tr class="hover:bg-surface-2">
                <td class="border-b border-border px-[14px] py-[11px]">
                  <RouterLink :to="{ name: 'monitor', params: { id: r.monitor.id } }" class="font-mono text-[13px] font-semibold text-ink hover:text-accent">{{ r.monitor.name }}</RouterLink>
                  <span class="ml-[6px] rounded-xs border border-border px-[4px] py-px font-mono text-[9.5px] uppercase tracking-[0.03em] text-ink-3">{{ r.monitor.type }}</span>
                </td>
                <!-- SLO objective (inline editable) -->
                <td class="border-b border-border px-[14px] py-[9px] text-right">
                  <div v-if="editingId === r.monitor.id" class="flex flex-col items-end gap-[4px]">
                    <div class="flex items-center justify-end gap-[6px]">
                      <div class="relative">
                        <input
                          v-model="draft"
                          type="number" min="0" max="99.9999" step="0.01" placeholder="99.9" autofocus
                          class="w-[84px] rounded-sm border border-border bg-surface-2 pl-2 pr-[18px] py-[5px] text-right font-mono text-[12.5px] outline-none focus:border-accent"
                          @keydown.enter.prevent="saveSlo(r.monitor)"
                          @keydown.esc="cancelEdit"
                        />
                        <span class="pointer-events-none absolute right-[7px] top-1/2 -translate-y-1/2 font-mono text-[12px] text-ink-3">%</span>
                      </div>
                      <button type="button" class="rounded-sm bg-accent px-[10px] py-[5px] text-[12px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-50" :disabled="savingSlo" @click="saveSlo(r.monitor)">{{ savingSlo ? "…" : "Save" }}</button>
                      <button type="button" class="rounded-sm border border-border px-[8px] py-[5px] text-[12px] text-ink-3 hover:border-border-strong" title="Cancel" @click="cancelEdit">✕</button>
                    </div>
                    <span v-if="rowError" class="text-[11px] text-down">{{ rowError }}</span>
                  </div>
                  <button
                    v-else-if="canWrite && w30(r)?.objective != null"
                    type="button"
                    class="inline-flex items-center gap-[5px] font-mono text-[13px] font-semibold text-ink hover:text-accent"
                    title="Edit SLO objective"
                    @click="editSlo(r)"
                  >
                    {{ objFmt(w30(r)?.objective) }}
                    <span v-if="burnBadge(w30(r))" :class="burnBadge(w30(r))!.cls" :title="burnBadge(w30(r))!.title">{{ burnBadge(w30(r))!.icon }}</span>
                    <svg v-if="savedId === r.monitor.id" viewBox="0 0 24 24" class="h-[13px] w-[13px] text-up" fill="none" stroke="currentColor" stroke-width="3"><path d="M5 12l5 5 9-11" /></svg>
                  </button>
                  <button
                    v-else-if="canWrite"
                    type="button"
                    class="rounded-sm border border-border px-[9px] py-[4px] text-[12px] text-ink-2 hover:border-accent hover:text-accent"
                    title="Set an SLO objective for this monitor"
                    @click="editSlo(r)"
                  >Set</button>
                  <span v-else class="font-mono text-[13px] text-ink-3">{{ objFmt(w30(r)?.objective) }}</span>
                </td>
                <!-- SLI (selected window) -->
                <td class="border-b border-border px-[14px] py-[11px] text-right font-mono font-semibold tnum">{{ pct2(activeWin(r)?.uptime_percent) }}</td>
                <!-- Error budget meter -->
                <td class="border-b border-border px-[14px] py-[11px]">
                  <div v-if="budgetMeter(activeWin(r)?.error_budget)" class="flex items-center justify-end gap-[9px]">
                    <div class="h-[6px] w-[74px] overflow-hidden rounded-full bg-inset">
                      <i class="block h-full rounded-full" :class="budgetMeter(activeWin(r)?.error_budget)!.cls" :style="{ width: budgetMeter(activeWin(r)?.error_budget)!.width + '%' }"></i>
                    </div>
                    <span class="min-w-[34px] text-right font-mono text-[12px] text-ink-2 tnum">{{ budgetMeter(activeWin(r)?.error_budget)!.label }}</span>
                  </div>
                  <div v-else class="text-right font-mono text-ink-3">—</div>
                </td>
                <!-- Burn -->
                <td class="border-b border-border px-[14px] py-[11px] text-right font-mono text-[12.5px] tnum" :class="burnCls(burnRate(activeWin(r)?.error_budget))">{{ burnFmt(burnRate(activeWin(r)?.error_budget)) }}</td>
                <!-- Status -->
                <td class="border-b border-border px-[14px] py-[11px]">
                  <span class="inline-flex h-[22px] items-center gap-[6px] rounded-full px-[9px] text-[11.5px] font-semibold" :class="stPill[sloStatus(activeWin(r)?.error_budget)]">
                    <span class="h-[7px] w-[7px] rounded-full" :class="stDot[sloStatus(activeWin(r)?.error_budget)]"></span>{{ stLabel[sloStatus(activeWin(r)?.error_budget)] }}
                  </span>
                </td>
              </tr>
              <!-- Burn-rate rules editor (expands under the row being edited) -->
              <tr v-if="editingId === r.monitor.id">
                <td colspan="6" class="border-b border-border bg-surface-2 px-[18px] py-[14px]">
                  <p class="mb-[10px] text-[12px] text-ink-3">
                    Burn-rate alert rules — a rule fires when the error-budget burn is ≥ threshold over <b class="text-ink-2">both</b> windows. Max 4 rules; none = burn alerting off.
                  </p>
                  <div class="overflow-x-auto rounded-sm border border-border bg-surface">
                    <table class="w-full text-[13px]">
                      <thead>
                        <tr class="bg-surface-2 text-[10.5px] uppercase tracking-[0.06em] text-ink-3">
                          <th class="border-b border-border px-3 py-2 text-left">Severity</th>
                          <th class="border-b border-border px-3 py-2 text-left">Burn rate</th>
                          <th class="border-b border-border px-3 py-2 text-left">Long window</th>
                          <th class="border-b border-border px-3 py-2 text-left">Short window</th>
                          <th class="border-b border-border px-3 py-2 text-left">State</th>
                          <th class="border-b border-border px-3 py-2"></th>
                        </tr>
                      </thead>
                      <tbody>
                        <tr v-for="(rule, ri) in rulesDraft" :key="ri">
                          <td class="border-b border-border px-3 py-2">
                            <select v-model="rule.severity" class="rounded-sm border border-border-strong bg-surface px-2 py-[4px] text-[12.5px] outline-none focus:border-accent">
                              <option value="page">🔥 page</option>
                              <option value="ticket">⚠️ ticket</option>
                            </select>
                          </td>
                          <td class="border-b border-border px-3 py-2 whitespace-nowrap">
                            <input v-model.number="rule.threshold" type="number" min="0.1" step="0.1" class="w-[64px] rounded-sm border border-border-strong bg-surface px-2 py-[4px] text-right font-mono text-[12.5px] tnum outline-none focus:border-accent" />
                            <span class="ml-1 text-[12px] text-ink-3">×</span>
                          </td>
                          <td class="border-b border-border px-3 py-2">
                            <select v-model.number="rule.long_window_seconds" class="rounded-sm border border-border-strong bg-surface px-2 py-[4px] text-[12.5px] outline-none focus:border-accent">
                              <option v-for="o in WINDOW_OPTS" :key="o.s" :value="o.s">{{ o.label }}</option>
                            </select>
                          </td>
                          <td class="border-b border-border px-3 py-2">
                            <select v-model.number="rule.short_window_seconds" class="rounded-sm border border-border-strong bg-surface px-2 py-[4px] text-[12.5px] outline-none focus:border-accent">
                              <option v-for="o in WINDOW_OPTS" :key="o.s" :value="o.s">{{ o.label }}</option>
                            </select>
                          </td>
                          <td class="border-b border-border px-3 py-2">
                            <span v-if="rule.firing" class="inline-flex items-center gap-[5px] rounded-full px-[8px] py-[2px] text-[11px] font-semibold" :class="rule.severity === 'page' ? 'bg-down-weak text-down' : 'bg-degraded-weak text-degraded'">
                              <span class="h-[6px] w-[6px] rounded-full bg-current"></span>firing
                            </span>
                            <span v-else class="inline-flex items-center gap-[5px] rounded-full bg-up-weak px-[8px] py-[2px] text-[11px] font-semibold text-up">
                              <span class="h-[6px] w-[6px] rounded-full bg-current"></span>quiet
                            </span>
                          </td>
                          <td class="border-b border-border px-2 py-2 text-right">
                            <button type="button" class="rounded-xs px-[6px] py-[2px] text-[14px] text-ink-3 hover:bg-down-weak hover:text-down" title="Remove rule" @click="removeRule(ri)">✕</button>
                          </td>
                        </tr>
                        <tr v-if="!rulesDraft.length">
                          <td colspan="6" class="px-3 py-4 text-center text-[12.5px] text-ink-3">No rules — burn-rate alerting is off for this objective.</td>
                        </tr>
                      </tbody>
                    </table>
                  </div>
                  <div class="mt-[10px] flex items-center gap-3">
                    <button type="button" class="text-[12.5px] font-medium text-accent hover:text-accent-2 disabled:opacity-50" :disabled="rulesDraft.length >= 4" @click="addRule">+ Add rule</button>
                    <button type="button" class="text-[12.5px] font-medium text-accent hover:text-accent-2" title="page 14.4× (1h ∧ 5m) · ticket 6× (6h ∧ 30m)" @click="rulesDraft = sreDefaults()">Reset to SRE defaults</button>
                    <span class="ml-auto text-[11.5px] text-ink-3">saved together with the objective ↑</span>
                  </div>
                </td>
              </tr>
              </template>
              <tr v-if="!rows.length && !loading">
                <td colspan="6" class="px-4 py-10 text-center text-[13px] text-ink-3">No monitors in this project yet.</td>
              </tr>
            </tbody>
          </table>
        </section>

        <!-- maintenance windows -->
        <div class="mb-3 mt-[26px] flex items-center gap-[10px]">
          <h2 class="text-[13px] font-semibold">Maintenance windows</h2>
          <span class="font-mono text-[12px] text-ink-3">excluded from SLA</span>
          <button v-if="canWrite" type="button" class="ml-auto rounded-sm border border-border px-[11px] py-[6px] text-[12.5px] hover:border-border-strong" @click="showMaint = !showMaint">
            {{ showMaint ? "Close" : "Schedule window" }}
          </button>
        </div>

        <div v-if="showMaint && canWrite" class="mb-4 flex flex-col gap-3 rounded border border-border bg-surface p-4 shadow-card">
          <div class="grid grid-cols-[1fr_1fr_1fr] gap-3 max-[900px]:grid-cols-1">
            <label class="flex flex-col gap-[6px]">
              <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Scope</span>
              <select v-model="maintForm.monitor_id" class="rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent">
                <option value="">Project-wide</option>
                <option v-for="r in rows" :key="r.monitor.id" :value="r.monitor.id">{{ r.monitor.name }}</option>
              </select>
            </label>
            <label class="flex flex-col gap-[6px]">
              <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Starts</span>
              <input v-model="maintForm.starts_at" type="datetime-local" class="rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent" />
            </label>
            <label class="flex flex-col gap-[6px]">
              <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Ends</span>
              <input v-model="maintForm.ends_at" type="datetime-local" class="rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent" />
            </label>
          </div>
          <label class="flex flex-col gap-[6px]">
            <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Reason</span>
            <input v-model="maintForm.reason" type="text" placeholder="Database upgrade" class="rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent" />
          </label>
          <div v-if="maintError" class="text-[12.5px] text-down">{{ maintError }}</div>

          <!-- A window reaching back over sealed facts restates numbers people may already
               have quoted, so it is SHOWN before it is applied. -->
          <div v-if="preview" class="rounded border border-degraded/40 bg-degraded-weak p-3" data-testid="maint-preview">
            <div class="text-[12.5px] font-semibold text-degraded">
              {{ annulTarget ? "Annulling says this window never applied — sealed facts are recomputed without it." : "This reaches back over sealed reliability facts." }}
            </div>
            <p class="mt-1 text-[12px] text-ink-2">
              Confirming restates numbers that have already been reported. Here is what changes, on both axes:
            </p>
            <table class="mt-2 w-full text-[12.5px]">
              <thead>
                <tr class="text-[10.5px] uppercase tracking-[0.06em] text-ink-3">
                  <th class="py-1 text-left">Service</th>
                  <th class="py-1 text-right">Availability now</th>
                  <th class="py-1 text-right">After</th>
                  <th class="py-1 text-right">Health now</th>
                  <th class="py-1 text-right">After</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="svc in preview.services" :key="svc.service_id">
                  <td class="py-1 font-mono">{{ svc.service_id.slice(0, 8) }}</td>
                  <td class="py-1 text-right font-mono">{{ pct(svc.before) }}</td>
                  <td class="py-1 text-right font-mono" :class="svc.projected ? '' : 'text-ink-3'">
                    {{ svc.projected ? pct(svc.after) : reasonLabel(svc.reason) }}
                  </td>
                  <td class="py-1 text-right font-mono">{{ healthPct(svc.before) }}</td>
                  <td class="py-1 text-right font-mono" :class="svc.projected ? '' : 'text-ink-3'">
                    {{ svc.projected ? healthPct(svc.after) : "—" }}
                  </td>
                </tr>
                <tr v-if="!preview.services.length">
                  <td colspan="5" class="py-2 text-center text-ink-3">No service reads reliability from this monitor.</td>
                </tr>
              </tbody>
            </table>
            <p v-if="preview.coverage !== 'complete'" class="mt-2 text-[12px] text-degraded">
              {{ approximateSummary(preview.services) }}
            </p>
          </div>

          <div class="flex gap-2">
            <button
              v-if="!preview"
              type="button"
              :disabled="maintSaving || !maintForm.starts_at || !maintForm.ends_at"
              class="h-[34px] rounded-sm bg-accent px-4 text-[13px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-50"
              @click="addMaintenance()"
            >
              {{ maintSaving ? "Scheduling…" : "Schedule" }}
            </button>
            <template v-else>
              <button
                type="button"
                :disabled="maintSaving || preview.coverage !== 'complete'"
                class="h-[34px] rounded-sm bg-accent px-4 text-[13px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-50"
                data-testid="maint-confirm"
                @click="annulTarget ? confirmAnnul() : addMaintenance(preview.preview_id)"
              >
                {{ maintSaving ? "Applying…" : annulTarget ? "Confirm annul and restate" : "Confirm and restate" }}
              </button>
              <button
                type="button"
                class="h-[34px] rounded-sm border border-border px-4 text-[13px] text-ink-2 hover:border-border-strong"
                @click="preview = null; annulTarget = null"
              >
                Cancel
              </button>
            </template>
          </div>
        </div>

        <section class="rounded border border-border bg-surface shadow-card">
          <div v-for="w in maintenance" :key="w.id" class="flex items-center gap-[14px] border-b border-border px-4 py-3 last:border-b-0">
            <span class="grid h-[32px] w-[32px] flex-none place-items-center rounded-sm bg-maint-weak text-maint">
              <svg viewBox="0 0 24 24" class="h-4 w-4" fill="none" stroke="currentColor" stroke-width="2"><path d="M14 6l4 4M3 21l3-1 11-11-3-3L3 18v3z" /></svg>
            </span>
            <div class="min-w-0">
              <b class="text-[13.5px]">{{ w.reason || "Maintenance" }}</b>
              <span class="block truncate text-[12px] text-ink-3"><span class="font-mono text-ink-2">{{ monitorName(w.monitor_id) }}</span> · {{ fmt(w.starts_at) }} → {{ fmt(w.ends_at) }}</span>
            </div>
            <div class="ml-auto flex items-center gap-[14px] text-right">
              <div>
                <span v-if="isUpcoming(w.starts_at)" class="mb-[3px] inline-block rounded-xs border border-maint/30 px-[6px] py-px text-[10px] font-bold uppercase tracking-[0.05em] text-maint">Upcoming</span>
                <span class="block font-mono text-[12.5px] text-ink-2">{{ fmt(w.starts_at) }}</span>
                <small class="block text-[11px] text-ink-3">{{ duration(w.starts_at, w.ends_at) }}</small>
              </div>
              <!-- Two different removals, on purpose. Archive retires the window and keeps
                   the time it covered excluded; annul says it NEVER applied and restates the
                   sealed facts — which is why annul goes through a shown, tokened preview. -->
              <button
                v-if="canWrite"
                type="button"
                class="rounded-xs px-[7px] py-[2px] text-[11.5px] text-ink-3 hover:bg-degraded-weak hover:text-degraded"
                data-testid="maint-annul"
                title="Annul: this window never applied — recompute the facts without it"
                @click="startAnnul(w)"
              >annul</button>
              <button v-if="canWrite" type="button" class="text-ink-3 hover:text-down" aria-label="Archive window" title="Archive: retire the window; its past effect stays" @click="deleteMaintenance(w.id!)">
                <svg viewBox="0 0 24 24" class="h-[15px] w-[15px]" fill="none" stroke="currentColor" stroke-width="2"><path d="M6 6l12 12M18 6L6 18" /></svg>
              </button>
            </div>
          </div>
          <div v-if="!maintenance.length" class="px-4 py-8 text-center text-[13px] text-ink-3">No maintenance scheduled.</div>
        </section>
      </template>
    </div>
  </AppShell>
</template>
