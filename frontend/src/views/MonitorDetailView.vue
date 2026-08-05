<script setup lang="ts">
import { computed, onMounted, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import { api } from "@/api/client";
import type { components } from "@/api/schema";
import AppShell from "@/components/AppShell.vue";
import StatusPill from "@/components/StatusPill.vue";
import { useLive } from "@/stores/live";
import { useSession } from "@/stores/session";
import { useWorkspace } from "@/stores/workspace";

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
const id = route.params.id as string;

const loading = ref(true);
const monitor = ref<Monitor | null>(null);
const windows = ref<WindowSLA[]>([]);
const heartbeats = ref<Heartbeat[]>([]);
const availability = ref<DailyAvailability[]>([]);
const openIncident = ref<Incident | null>(null);

const confirmingDelete = ref(false);
const deleting = ref(false);
const pausing = ref(false);

const isPush = computed(() => monitor.value?.type === "push");
const win = (name: string) => windows.value.find((w) => w.window === name);
const win30 = computed(() => win("30d"));

// Latency chart (area + line + p95 reference), oldest→newest.
const latencySeries = computed(() =>
  heartbeats.value.slice().reverse().map((h) => h.latency_ms ?? 0).filter((v) => v > 0),
);
const p95Chart = computed(() => win("24h")?.p95_latency_ms || win30.value?.p95_latency_ms || 0);
const chart = computed(() => {
  const vals = latencySeries.value;
  if (vals.length < 2) return null;
  const W = 1000, H = 170, pl = 8, pr = 8, pt = 14, pb = 12;
  const max = Math.max(...vals, p95Chart.value) * 1.15 || 1;
  const X = (i: number) => pl + (i / (vals.length - 1)) * (W - pl - pr);
  const Y = (v: number) => pt + (1 - v / max) * (H - pt - pb);
  const pts = vals.map((v, i) => [X(i), Y(v)] as const);
  const line = pts.map((p, i) => (i ? "L" : "M") + p[0].toFixed(1) + " " + p[1].toFixed(1)).join(" ");
  const area = `${line} L${pts[pts.length - 1][0].toFixed(1)} ${H - pb} L${pts[0][0].toFixed(1)} ${H - pb} Z`;
  return { W, H, line, area, p95y: p95Chart.value ? Y(p95Chart.value).toFixed(1) : null, end: pts[pts.length - 1] };
});

// 90-day availability strip.
const timeline = computed(() => {
  const byDay = new Map<string, DailyAvailability>();
  for (const d of availability.value) if (d.day) byDay.set(d.day.slice(0, 10), d);
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
  return ts ? new Date(ts).toISOString().slice(0, 10) : "—";
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
const requiredBy = computed(() => projectMonitors.value.filter((m) => (m.depends_on ?? []).includes(id)));

async function load() {
  loading.value = true;
  const [mon, sla, hb] = await Promise.all([
    api.GET("/api/v1/monitors/{monitorID}", { params: { path: { monitorID: id } } }),
    api.GET("/api/v1/monitors/{monitorID}/sla", { params: { path: { monitorID: id } } }),
    api.GET("/api/v1/monitors/{monitorID}/heartbeats", { params: { path: { monitorID: id }, query: { limit: 60 } } }),
  ]);
  monitor.value = mon.data ?? null;
  windows.value = sla.data?.windows ?? [];
  heartbeats.value = hb.data ?? [];

  const pid = monitor.value?.project_id;
  if (pid) {
    await ws.init();
    const [av, inc, mons] = await Promise.all([
      api.GET("/api/v1/monitors/{monitorID}/availability", { params: { path: { monitorID: id }, query: { days: 90 } } }),
      api.GET("/api/v1/projects/{projectID}/incidents", { params: { path: { projectID: pid } } }),
      api.GET("/api/v1/projects/{projectID}/monitors", { params: { path: { projectID: pid } } }),
    ]);
    availability.value = av.data ?? [];
    projectMonitors.value = mons.data ?? [];
    openIncident.value = (inc.data ?? []).find((i) => i.monitor_id === id && i.status !== "resolved") ?? null;
  }
  loading.value = false;
}

async function togglePause() {
  if (!monitor.value) return;
  pausing.value = true;
  try {
    const res = await api.PATCH("/api/v1/monitors/{monitorID}", {
      params: { path: { monitorID: id } },
      body: { enabled: !monitor.value.enabled },
    });
    if (res.data) monitor.value = res.data;
  } finally {
    pausing.value = false;
  }
}
async function remove() {
  deleting.value = true;
  try {
    const res = await api.DELETE("/api/v1/monitors/{monitorID}", { params: { path: { monitorID: id } } });
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

// Reflect live status changes for this monitor immediately.
watch(
  () => live.statuses[id],
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
            <span v-if="!monitor.enabled" class="rounded-full bg-pending-weak px-[9px] py-px text-[11.5px] font-medium text-ink-3">Paused</span>
          </div>
          <div class="mt-[7px] flex flex-wrap items-center gap-x-[10px] gap-y-1 text-[13px] text-ink-3">
            <span class="font-mono text-ink-2">{{ monitor.type === "http" ? "GET " : "" }}{{ monitor.target || "push heartbeat" }}</span>
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
        <div v-if="canWrite" class="ml-auto flex flex-wrap gap-2">
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
            <button type="button" class="inline-flex h-[34px] items-center rounded-sm border border-border bg-surface px-[13px] text-[13px] text-ink-2 hover:border-down/60 hover:text-down" @click="confirmingDelete = true">Delete</button>
          </template>
          <template v-else>
            <span class="self-center text-[12.5px] text-ink-3">Delete this monitor?</span>
            <button type="button" class="h-[34px] rounded-sm bg-down px-[13px] text-[13px] font-medium text-white hover:opacity-90 disabled:opacity-50" :disabled="deleting" @click="remove">{{ deleting ? "Deleting…" : "Confirm" }}</button>
            <button type="button" class="h-[34px] rounded-sm border border-border px-[13px] text-[13px] text-ink-2 hover:border-border-strong" @click="confirmingDelete = false">Cancel</button>
          </template>
        </div>
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
          <span class="text-[11.5px] font-mono text-ink-3">
            <template v-if="win('24h')">avg {{ Math.round(win('24h')!.avg_latency_ms ?? 0) }}ms · p95 {{ Math.round(win('24h')!.p95_latency_ms ?? 0) }}ms</template>
          </span>
        </div>
        <div class="px-4 pt-4">
          <svg v-if="chart" :viewBox="`0 0 ${chart.W} ${chart.H}`" class="block w-full" :style="{ height: '180px' }" preserveAspectRatio="none">
            <defs><linearGradient id="latGrad" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="var(--accent)" stop-opacity="0.2" /><stop offset="1" stop-color="var(--accent)" stop-opacity="0" /></linearGradient></defs>
            <line v-if="chart.p95y" x1="0" :y1="chart.p95y" :x2="chart.W" :y2="chart.p95y" stroke="var(--degraded)" stroke-width="1.4" stroke-dasharray="5 4" stroke-opacity="0.9" />
            <path :d="chart.area" fill="url(#latGrad)" />
            <path :d="chart.line" fill="none" stroke="var(--accent)" stroke-width="2" stroke-linejoin="round" stroke-linecap="round" />
            <circle :cx="chart.end[0]" :cy="chart.end[1]" r="3" fill="var(--accent)" />
          </svg>
          <p v-else class="py-6 text-[13px] text-ink-3">No latency samples yet.</p>
        </div>
        <div class="flex gap-4 px-4 pb-3 pt-2 text-[12px] text-ink-3">
          <span class="inline-flex items-center gap-[6px]"><i class="inline-block h-0 w-[14px] border-t-2 border-accent"></i> Response time</span>
          <span class="inline-flex items-center gap-[6px]"><i class="inline-block h-0 w-[14px] border-t-2 border-dashed border-degraded"></i> p95</span>
        </div>
      </section>

      <!-- availability 90d -->
      <section class="mb-4 rounded border border-border bg-surface shadow-card">
        <div class="flex items-center gap-2 border-b border-border px-4 py-[13px]">
          <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Availability · 90 days</span>
          <span class="ml-auto font-mono text-[12px] text-ink-2">{{ timelineUptime }}</span>
        </div>
        <div class="px-4 pb-4 pt-[14px]">
          <div class="flex h-[34px] items-stretch gap-[2px]">
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
                <tr v-for="(h, i) in heartbeats.slice(0, 12)" :key="i" class="hover:bg-surface-2">
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

          <section v-if="monitor" class="rounded border border-border bg-surface shadow-card">
            <div class="border-b border-border px-4 py-[13px]"><h3 class="text-[13px] font-semibold">Configuration</h3></div>
            <dl class="grid grid-cols-[auto_1fr] gap-x-[14px] gap-y-[9px] px-4 py-[14px] text-[13px]">
              <dt class="text-ink-3">Target</dt><dd class="m-0 break-all text-right font-mono text-[12.5px] text-ink-2">{{ monitor.target || "—" }}</dd>
              <dt class="text-ink-3">Type</dt><dd class="m-0 text-right font-mono text-[12.5px] text-ink-2">{{ monitor.type }}</dd>
              <template v-if="!isPush">
                <dt class="text-ink-3">Interval</dt><dd class="m-0 text-right font-mono text-[12.5px] text-ink-2">{{ monitor.interval_seconds }}s</dd>
                <dt class="text-ink-3">Timeout</dt><dd class="m-0 text-right font-mono text-[12.5px] text-ink-2">{{ monitor.timeout_seconds }}s</dd>
                <dt class="text-ink-3">Retries</dt><dd class="m-0 text-right font-mono text-[12.5px] text-ink-2">{{ monitor.retries }}</dd>
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
            </dl>
          </section>
        </div>
      </div>
    </div>
  </AppShell>
</template>
