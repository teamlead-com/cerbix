<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from "vue";
import { api } from "@/api/client";
import type { components } from "@/api/schema";
import AppShell from "@/components/AppShell.vue";
import { useSession } from "@/stores/session";
import { useWorkspace } from "@/stores/workspace";

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
function fmt(ts?: string) {
  return ts ? new Date(ts).toLocaleString([], { dateStyle: "medium", timeStyle: "short" }) : "—";
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
  loading.value = true;
  error.value = "";
  try {
    await ws.init();
    const projectID = ws.projectId;
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
    projectWindows.value = projSla.data?.windows ?? [];
    reportEnabled.value = projSla.data?.sla_report_weekly ?? false;
    maintenance.value = maint.data ?? [];
    rows.value = await Promise.all((monitors.data ?? []).map(loadRow));
  } catch {
    error.value = "Could not load SLA data.";
  } finally {
    loading.value = false;
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
  const objective = Number(raw);
  if (!raw || Number.isNaN(objective) || objective <= 0 || objective > 100) {
    rowError.value = "Enter a target between 0 and 100 (e.g. 99.9).";
    return;
  }
  const ruleErr = validateRules();
  if (ruleErr) {
    rowError.value = ruleErr;
    return;
  }
  rowError.value = "";
  savingSlo.value = true;
  try {
    const rules = rulesDraft.value.map((r) => ({ ...r, threshold: Number(r.threshold) }));
    const res = await api.PUT("/api/v1/monitors/{monitorID}/sla-target", {
      params: { path: { monitorID: m.id! } },
      body: { objective, window: "30d", burn_alert: rules.length > 0, burn_rules: rules },
    });
    if (res.error) {
      rowError.value = (res.error as { error?: string })?.error || "Could not save the objective.";
      return;
    }
    const updated = await loadRow(m);
    const idx = rows.value.findIndex((r) => r.monitor.id === m.id);
    if (idx >= 0) rows.value[idx] = updated;
    editingId.value = null; // close the editor
    savedId.value = m.id!; // brief ✓ flash on the row
    setTimeout(() => {
      if (savedId.value === m.id) savedId.value = null;
    }, 1600);
  } catch {
    rowError.value = "Could not save the objective.";
  } finally {
    savingSlo.value = false;
  }
}

// ---- weekly SLA report toggle -------------------------------------------
const reportEnabled = ref(false);
const reportSaving = ref(false);
async function toggleReport() {
  if (!ws.projectId || reportSaving.value) return;
  const next = !reportEnabled.value;
  reportSaving.value = true;
  try {
    const res = await api.PUT("/api/v1/projects/{projectID}/sla-report", {
      params: { path: { projectID: ws.projectId } },
      body: { enabled: next },
    });
    if (!res.error) reportEnabled.value = res.data?.sla_report_weekly ?? next;
  } finally {
    reportSaving.value = false;
  }
}

// ---- maintenance scheduler ----------------------------------------------
const showMaint = ref(false);
const maintForm = reactive({ monitor_id: "", starts_at: "", ends_at: "", reason: "" });
const maintSaving = ref(false);
const maintError = ref("");

async function addMaintenance() {
  if (!ws.projectId || !maintForm.starts_at || !maintForm.ends_at) return;
  maintSaving.value = true;
  maintError.value = "";
  const body: components["schemas"]["CreateMaintenance"] = {
    starts_at: new Date(maintForm.starts_at).toISOString(),
    ends_at: new Date(maintForm.ends_at).toISOString(),
    reason: maintForm.reason,
  };
  if (maintForm.monitor_id) body.monitor_id = maintForm.monitor_id;
  try {
    const res = await api.POST("/api/v1/projects/{projectID}/maintenance", {
      params: { path: { projectID: ws.projectId } },
      body,
    });
    if (res.error || !res.data) {
      maintError.value = (res.error as { error?: string })?.error || "Could not schedule maintenance.";
      return;
    }
    maintenance.value.push(res.data);
    maintForm.monitor_id = "";
    maintForm.starts_at = "";
    maintForm.ends_at = "";
    maintForm.reason = "";
    showMaint.value = false;
  } catch {
    maintError.value = "Could not schedule maintenance.";
  } finally {
    maintSaving.value = false;
  }
}

async function deleteMaintenance(id: string) {
  const res = await api.DELETE("/api/v1/maintenance/{maintenanceID}", { params: { path: { maintenanceID: id } } });
  if (!res.error) maintenance.value = maintenance.value.filter((w) => w.id !== id);
}

onMounted(load);
watch(() => ws.projectId, load);
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
                          type="number" min="0" max="100" step="0.01" placeholder="99.9" autofocus
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
          <div>
            <button type="button" :disabled="maintSaving || !maintForm.starts_at || !maintForm.ends_at" class="h-[34px] rounded-sm bg-accent px-4 text-[13px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-50" @click="addMaintenance">
              {{ maintSaving ? "Scheduling…" : "Schedule" }}
            </button>
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
              <button v-if="canWrite" type="button" class="text-ink-3 hover:text-down" aria-label="Delete window" @click="deleteMaintenance(w.id!)">
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
