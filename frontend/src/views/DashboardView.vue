<script setup lang="ts">
import { computed, onMounted, ref, watch } from "vue";
import { api } from "@/api/client";
import type { components } from "@/api/schema";
import AppShell from "@/components/AppShell.vue";
import Kpi from "@/components/Kpi.vue";
import MonitorCard from "@/components/MonitorCard.vue";
import { useLive } from "@/stores/live";
import { useSession } from "@/stores/session";
import { useUi } from "@/stores/ui";
import { useWorkspace } from "@/stores/workspace";

type Monitor = components["schemas"]["Monitor"];
type WindowSLA = components["schemas"]["WindowSLA"];
type Seg = "up" | "degraded" | "down" | "maint" | "none";

interface Card {
  monitor: Monitor;
  uptime: string;
  latency: string;
  segments: Seg[];
  spark: number[];
  budgetLeft: number | null;
  budgetMet: boolean;
}

type DailyAvailability = components["schemas"]["DailyAvailability"];

const ws = useWorkspace();
const session = useSession();
const ui = useUi();
const live = useLive();
const loading = ref(true);
const cards = ref<Card[]>([]);
type Trend = { dir: "pos" | "neg" | "flat"; label: string };
const kpis = ref({
  availability: "—",
  availSub: "",
  availTrend: undefined as Trend | undefined,
  up: "0",
  total: "0",
  downSub: "",
  budget: "—",
  budgetSub: "",
  budgetMet: true,
  p95: "—",
  p95Sub: "",
  p95Trend: undefined as Trend | undefined,
});
const timeline = ref<{ pct: number | null; label: string }[]>([]);
const empty = ref("");
const emptyKind = ref<"" | "org" | "project" | "monitor" | "error">("");
const canManageOrg = computed(() => !!ws.orgId && session.isOrgAdmin(ws.orgId));
const emptyTitle = computed(() => {
  switch (emptyKind.value) {
    case "org":
      return "Welcome to cerbix";
    case "project":
      return `No projects in ${ws.orgName || "this organization"} yet`;
    case "monitor":
      return "No monitors yet";
    default:
      return "Something went wrong";
  }
});

// Map a day's uptime to a strip-segment color token.
function dayFill(p: number | null): string {
  if (p === null) return "var(--inset)";
  if (p >= 99.5) return "var(--up)";
  if (p >= 95) return "var(--degraded)";
  return "var(--down)";
}

function pct(n?: number) {
  return n === undefined ? "—" : `${n.toFixed(2)}%`;
}

// Aggregate 90-day uptime shown in the timeline header (mean of days with data).
const timelineUptime = computed(() => {
  const vals = timeline.value.filter((d) => d.pct !== null).map((d) => d.pct as number);
  if (!vals.length) return "—";
  return `${(vals.reduce((a, b) => a + b, 0) / vals.length).toFixed(2)}% uptime`;
});

async function loadMonitorExtras(m: Monitor): Promise<Card> {
  const [slaRes, hbRes] = await Promise.all([
    api.GET("/api/v1/monitors/{monitorID}/sla", { params: { path: { monitorID: m.id! } } }),
    api.GET("/api/v1/monitors/{monitorID}/heartbeats", { params: { path: { monitorID: m.id! }, query: { limit: 48 } } }),
  ]);
  const w30 = slaRes.data?.windows?.find((w: WindowSLA) => w.window === "30d");
  const hbs = (hbRes.data ?? []).slice().reverse();
  const segments: Seg[] = hbs.map((h) => (h.up ? "up" : "down"));
  const spark = hbs.map((h) => h.latency_ms ?? 0).filter((v) => v > 0);
  const eb = w30?.error_budget;
  return {
    monitor: m,
    uptime: m.type === "push" ? "—" : pct(w30?.uptime_percent),
    latency: w30?.avg_latency_ms ? `${Math.round(w30.avg_latency_ms)} ms` : "—",
    segments,
    spark,
    budgetLeft: eb ? Math.max(0, 100 - (eb.burned_percent ?? 0)) : null,
    budgetMet: eb?.met ?? true,
  };
}

async function load() {
  loading.value = true;
  empty.value = "";
  emptyKind.value = "";
  cards.value = [];
  try {
    await ws.init();
    if (!ws.orgs.length) {
      empty.value = "Organizations are the top-level tenant — a product or team, isolated from the others. Create your first one to start adding projects and monitors.";
      emptyKind.value = "org";
      return;
    }
    const projectID = ws.projectId;
    if (!projectID) {
      empty.value = "Projects are the teams and apps inside an organization — each holds its own monitors, channels and members. Add the first one.";
      emptyKind.value = "project";
      return;
    }

    const [monitors, projectSla, avail] = await Promise.all([
      api.GET("/api/v1/projects/{projectID}/monitors", { params: { path: { projectID } } }),
      api.GET("/api/v1/projects/{projectID}/sla", { params: { path: { projectID } } }),
      api.GET("/api/v1/projects/{projectID}/availability", { params: { path: { projectID }, query: { days: 90 } } }),
    ]);
    timeline.value = buildTimeline(avail.data ?? []);
    const list = monitors.data ?? [];
    const w30 = projectSla.data?.windows?.find((w: WindowSLA) => w.window === "30d");

    if (!list.length) {
      empty.value = "No monitors in this project yet. Add one to begin checking.";
      emptyKind.value = "monitor";
      return;
    }
    cards.value = await Promise.all(list.map(loadMonitorExtras));

    // Project error budget = mean of the monitors that have an SLO target.
    const budgets = cards.value.filter((c) => c.budgetLeft !== null);
    const meanBudget = budgets.length
      ? budgets.reduce((a, c) => a + (c.budgetLeft as number), 0) / budgets.length
      : null;
    const downCount = list.filter((m) => m.status === "down").length;

    // Availability trend: mean uptime of the last 30 days vs the 30 before, taken
    // from the 90-day daily strip (oldest→newest). Only shown when both periods
    // have data.
    const meanPct = (slots: { pct: number | null }[]): number | null => {
      const v = slots.map((s) => s.pct).filter((p): p is number => p !== null);
      return v.length ? v.reduce((a, b) => a + b, 0) / v.length : null;
    };
    const cur30 = meanPct(timeline.value.slice(60));
    const prev30 = meanPct(timeline.value.slice(30, 60));
    let availTrend: Trend | undefined;
    let availSub = "rolling 30-day window";
    if (cur30 !== null && prev30 !== null) {
      const d = cur30 - prev30;
      availTrend = { dir: d >= 0.005 ? "pos" : d <= -0.005 ? "neg" : "flat", label: `${d >= 0 ? "+" : ""}${d.toFixed(2)}%` };
      availSub = "vs prev 30d";
    }

    // p95 latency trend: "stable" when p95 is close to the average (tight spread),
    // else "variable"; sub reports how many checks the window covers.
    const checks = w30?.total ?? 0;
    const checksSub = checks ? `across ${checks} check${checks === 1 ? "" : "s"}` : "";
    let p95Trend: Trend | undefined;
    if (w30?.p95_latency_ms && w30?.avg_latency_ms) {
      const stable = w30.p95_latency_ms <= w30.avg_latency_ms * 1.6;
      p95Trend = { dir: stable ? "pos" : "flat", label: stable ? "stable" : "variable" };
    }

    kpis.value = {
      availability: pct(w30?.uptime_percent),
      availSub,
      availTrend,
      up: String(list.filter((m) => m.status === "up").length),
      total: String(list.length),
      downSub: downCount ? `${downCount} down` : "all operational",
      budget: meanBudget !== null ? `${Math.round(meanBudget)}%` : "—",
      budgetSub: budgets.length ? `across ${budgets.length} SLO${budgets.length > 1 ? "s" : ""}` : "no SLO set",
      budgetMet: budgets.every((c) => c.budgetMet),
      p95: w30?.p95_latency_ms ? `${Math.round(w30.p95_latency_ms)} ms` : "—",
      p95Sub: checksSub,
      p95Trend,
    };
  } catch {
    empty.value = "Could not load the dashboard.";
    emptyKind.value = "error";
  } finally {
    loading.value = false;
  }
}

// Fill a 90-slot strip (oldest→newest) from the sparse per-day availability list.
function buildTimeline(days: DailyAvailability[]): { pct: number | null; label: string }[] {
  const byDay = new Map<string, DailyAvailability>();
  for (const d of days) {
    if (d.day) byDay.set(d.day.slice(0, 10), d);
  }
  const out: { pct: number | null; label: string }[] = [];
  const today = new Date();
  for (let i = 89; i >= 0; i--) {
    const dt = new Date(today);
    dt.setUTCDate(today.getUTCDate() - i);
    const key = dt.toISOString().slice(0, 10);
    const d = byDay.get(key);
    out.push({ pct: d && d.total ? (d.uptime_percent ?? 0) : null, label: key });
  }
  return out;
}

onMounted(() => {
  load();
  live.connect();
});
watch(() => ws.projectId, load);

// Patch cards live as SSE status events arrive.
watch(
  () => live.statuses,
  (map) => {
    let up = 0;
    for (const c of cards.value) {
      const s = map[c.monitor.id ?? ""];
      if (s) {
        c.monitor = { ...c.monitor, status: s.status as Monitor["status"] };
        if (s.latency_ms && c.monitor.type !== "push") c.latency = `${Math.round(s.latency_ms)} ms`;
      }
      if (c.monitor.status === "up") up++;
    }
    if (cards.value.length) kpis.value.up = String(up);
  },
  { deep: true },
);
</script>

<template>
  <AppShell active="dashboard" :crumbs="[ws.orgName || 'cerbix', ws.projectName || '…', 'Dashboard']">
    <template #actions>
      <RouterLink
        v-if="session.canProjectWrite(ws.orgId, ws.projectId)"
        :to="{ name: 'monitor-new' }"
        class="flex h-[34px] items-center gap-[7px] rounded-sm bg-accent px-[13px] text-[13px] font-medium text-accent-ink hover:bg-accent-2"
      >
        <svg viewBox="0 0 24 24" class="h-[15px] w-[15px]" fill="none" stroke="currentColor" stroke-width="2.2"><path d="M12 5v14M5 12h14" /></svg>
        New monitor
      </RouterLink>
    </template>

    <div class="mx-auto max-w-[1180px] px-[22px] pb-16 pt-[26px]">
      <div class="mb-[22px]">
        <h1 class="text-[21px] font-semibold tracking-tight">{{ ws.projectName || "Dashboard" }}</h1>
        <p class="mt-[3px] text-[13px] text-ink-3">
          <span v-if="loading">Loading…</span>
          <span v-else>{{ cards.length }} monitors · {{ ws.orgName }}</span>
        </p>
      </div>

      <div v-if="empty && !loading" class="grid place-items-center py-16">
        <div class="max-w-[480px] text-center">
          <div class="mx-auto mb-[18px] grid h-14 w-14 place-items-center rounded-[14px] bg-accent-weak text-accent">
            <svg v-if="emptyKind === 'org'" viewBox="0 0 24 24" class="h-[26px] w-[26px]" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M3 21h18" /><path d="M5 21V7l7-4 7 4v14" /><path d="M9 21v-6h6v6" /><path d="M9 9h.01M15 9h.01M9 12h.01M15 12h.01" /></svg>
            <svg v-else-if="emptyKind === 'project'" viewBox="0 0 24 24" class="h-[26px] w-[26px]" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z" /></svg>
            <svg v-else-if="emptyKind === 'monitor'" viewBox="0 0 24 24" class="h-[26px] w-[26px]" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M3 12h4l2 6 4-14 2 8h6" /></svg>
            <svg v-else viewBox="0 0 24 24" class="h-[26px] w-[26px]" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="9" /><path d="M12 8v4M12 16h.01" /></svg>
          </div>
          <h2 class="mb-[6px] text-[19px] font-semibold tracking-tight">{{ emptyTitle }}</h2>
          <p class="mb-5 text-[13.5px] text-ink-3">{{ empty }}</p>

          <button
            v-if="emptyKind === 'org' && session.isGlobalAdmin"
            type="button"
            class="inline-flex h-[34px] items-center gap-[7px] rounded-sm bg-accent px-[13px] text-[13px] font-medium text-accent-ink hover:bg-accent-2"
            @click="ui.openCreate('org')"
          >
            <svg viewBox="0 0 24 24" class="h-[15px] w-[15px]" fill="none" stroke="currentColor" stroke-width="2.2"><path d="M12 5v14M5 12h14" /></svg>
            New organization
          </button>
          <button
            v-else-if="emptyKind === 'project' && canManageOrg"
            type="button"
            class="inline-flex h-[34px] items-center gap-[7px] rounded-sm bg-accent px-[13px] text-[13px] font-medium text-accent-ink hover:bg-accent-2"
            @click="ui.openCreate('project')"
          >
            <svg viewBox="0 0 24 24" class="h-[15px] w-[15px]" fill="none" stroke="currentColor" stroke-width="2.2"><path d="M12 5v14M5 12h14" /></svg>
            New project
          </button>
          <RouterLink
            v-else-if="emptyKind === 'monitor'"
            :to="{ name: 'monitor-new' }"
            class="inline-flex h-[34px] items-center gap-[7px] rounded-sm bg-accent px-[13px] text-[13px] font-medium text-accent-ink hover:bg-accent-2"
          >
            <svg viewBox="0 0 24 24" class="h-[15px] w-[15px]" fill="none" stroke="currentColor" stroke-width="2.2"><path d="M12 5v14M5 12h14" /></svg>
            New monitor
          </RouterLink>
          <p v-else-if="emptyKind === 'org'" class="text-[12.5px] text-ink-3">Ask a global admin to create an organization.</p>
          <p v-else-if="emptyKind === 'project'" class="text-[12.5px] text-ink-3">Ask an org admin to create a project in {{ ws.orgName }}.</p>
        </div>
      </div>

      <template v-else>
        <!-- hero: KPIs as individual rounded cards + a 90-day availability card -->
        <div class="mb-6 flex flex-col gap-3">
          <div class="grid grid-cols-4 gap-3 max-[900px]:grid-cols-2">
            <Kpi label="Availability · 30d" :value="kpis.availability" :sub="kpis.availSub" :trend="kpis.availTrend" />
            <Kpi label="Monitors up" :value="kpis.up" :unit="' / ' + kpis.total" :sub="kpis.downSub" />
            <Kpi label="Error budget · 30d" :value="kpis.budget" :sub="kpis.budgetSub" />
            <Kpi label="P95 latency · 30d" :value="kpis.p95" :sub="kpis.p95Sub" :trend="kpis.p95Trend" />
          </div>
          <div class="rounded border border-border bg-surface px-[18px] pb-[17px] pt-[15px] shadow-card">
            <div class="mb-[10px] flex items-baseline gap-[10px]">
              <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Project availability · 90 days</span>
              <span class="ml-auto font-mono text-[12px] text-ink-2">{{ timelineUptime }}</span>
            </div>
            <svg
              :viewBox="`0 0 ${timeline.length} 10`"
              preserveAspectRatio="none"
              :style="{ height: '38px', width: '100%', display: 'block' }"
              role="img"
              aria-label="90-day project availability"
            >
              <rect
                v-for="(d, i) in timeline"
                :key="i"
                :x="i + 0.13"
                y="0"
                :width="0.74"
                height="10"
                rx="0.2"
                ry="0.7"
                :style="{ fill: dayFill(d.pct) }"
              >
                <title>{{ d.pct === null ? d.label + " · no data" : d.label + " · " + d.pct.toFixed(2) + "%" }}</title>
              </rect>
            </svg>
            <div class="mt-[10px] flex gap-4 text-[12px] text-ink-3">
              <span class="inline-flex items-center gap-[6px]"><i class="inline-block h-2 w-2 rounded-[2px] bg-up"></i> Operational</span>
              <span class="inline-flex items-center gap-[6px]"><i class="inline-block h-2 w-2 rounded-[2px] bg-degraded"></i> Degraded</span>
              <span class="inline-flex items-center gap-[6px]"><i class="inline-block h-2 w-2 rounded-[2px] bg-down"></i> Down</span>
              <span class="ml-auto font-mono text-[11px]">90 days ago → today</span>
            </div>
          </div>
        </div>

        <div class="grid grid-cols-[repeat(auto-fill,minmax(320px,1fr))] gap-3">
          <MonitorCard
            v-for="c in cards"
            :key="c.monitor.id"
            :monitor="c.monitor"
            :uptime="c.uptime"
            :latency="c.latency"
            :segments="c.segments"
            :spark="c.spark"
            :budget-left="c.budgetLeft"
            :budget-met="c.budgetMet"
          />
        </div>
      </template>
    </div>
  </AppShell>
</template>
