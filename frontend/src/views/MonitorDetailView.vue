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
const id = route.params.id as string;

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
      params: { path: { monitorID: id } },
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
      params: { path: { monitorID: id } },
      body: { service_id: successorChoice.value } as never,
    });
    if (res.data) monitor.value = res.data;
    return res;
  });
  if (ok) successorChoice.value = "";
}

async function retire() {
  await lifecycle(async () => {
    const res = await api.POST("/api/v1/monitors/{monitorID}/retire", { params: { path: { monitorID: id } } });
    if (res.data) monitor.value = res.data;
    return res;
  });
  confirmingRetire.value = false;
}

async function reactivate() {
  await lifecycle(async () => {
    const res = await api.POST("/api/v1/monitors/{monitorID}/reactivate", { params: { path: { monitorID: id } } });
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
      params: { path: { monitorID: id } },
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
