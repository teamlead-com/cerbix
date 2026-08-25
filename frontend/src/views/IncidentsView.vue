<script setup lang="ts">
import { computed, onMounted, ref, watch } from "vue";
import { api } from "@/api/client";
import type { components } from "@/api/schema";
import AppShell from "@/components/AppShell.vue";
import IncidentSubjectChip from "@/components/IncidentSubjectChip.vue";
import { useSession } from "@/stores/session";
import { useWorkspace } from "@/stores/workspace";
import { forwardStatuses, relTime } from "@/lib/incident";
import { renderSections } from "@/lib/postmortem";

type Incident = components["schemas"]["Incident"];
type IncidentUpdate = components["schemas"]["IncidentUpdate"];
type Postmortem = components["schemas"]["Postmortem"];
type Monitor = components["schemas"]["Monitor"];

const ws = useWorkspace();
const session = useSession();
const loading = ref(true);
const incidents = ref<Incident[]>([]);
const error = ref("");

// Segmented filter: Active | Resolved | All.
type Filter = "active" | "resolved" | "all";
const filter = ref<Filter>("active");

const activeCount = computed(() => incidents.value.filter((i) => i.status !== "resolved").length);
const resolvedCount = computed(() => incidents.value.filter((i) => i.status === "resolved").length);
const shown = computed(() => {
  if (filter.value === "active") return incidents.value.filter((i) => i.status !== "resolved");
  if (filter.value === "resolved") return incidents.value.filter((i) => i.status === "resolved");
  return incidents.value;
});

// Master/detail selection — the right panel loads the incident's timeline,
// postmortem, and affected monitor on demand. Editing lives in IncidentDetailView.
const selectedID = ref<string | null>(null);
const selected = computed(() => incidents.value.find((i) => i.id === selectedID.value) ?? null);
const updates = ref<IncidentUpdate[]>([]);
const postmortem = ref<Postmortem | null>(null);
const affected = ref<Monitor | null>(null);

// The SUBJECT names, resolved ONCE per screen rather than per row (FR-022). Two requests at most,
// and only when some incident actually carries that anchor: a list of N incidents must not become
// N lookups, and a project with no service incidents pays nothing.
const serviceSlugs = ref<Record<string, string>>({});
const monitorNames = ref<Record<string, string>>({});
async function loadSubjectNames(list: Incident[]) {
  const projectID = ws.projectId;
  if (!projectID) return;
  const wantServices = list.some((i) => i.service_id);
  const wantMonitors = list.some((i) => i.monitor_id);
  const [svcs, mons] = await Promise.all([
    wantServices
      ? api.GET("/api/v1/projects/{projectID}/services", { params: { path: { projectID } } })
      : Promise.resolve(null),
    wantMonitors
      ? api.GET("/api/v1/projects/{projectID}/monitors", { params: { path: { projectID } } })
      : Promise.resolve(null),
  ]);
  const s: Record<string, string> = {};
  // The list endpoint returns each service WRAPPED with its rollup counts, so the identity is nested.
  for (const v of svcs?.data ?? []) if (v.service?.id) s[v.service.id] = v.service.slug || v.service.name || "";
  serviceSlugs.value = s;
  const m: Record<string, string> = {};
  for (const v of mons?.data ?? []) if (v.id) m[v.id] = v.name || "";
  monitorNames.value = m;
}
const detailLoading = ref(false);

// --- token-driven badge classes, matching the design system ---
type Cls = { chip: string; dot: string; label: string };
const STATUS: Record<string, Cls> = {
  investigating: { chip: "text-degraded bg-degraded-weak", dot: "bg-degraded", label: "Investigating" },
  identified: { chip: "text-maint bg-maint-weak", dot: "bg-maint", label: "Identified" },
  monitoring: { chip: "text-accent bg-accent-weak", dot: "bg-accent", label: "Monitoring" },
  resolved: { chip: "text-up bg-up-weak", dot: "bg-up", label: "Resolved" },
};
function st(s?: string): Cls {
  return STATUS[s ?? ""] ?? { chip: "text-ink-3 bg-surface-2", dot: "bg-ink-3", label: s ?? "—" };
}
const IMPACT: Record<string, string> = {
  minor: "text-degraded border-degraded/30",
  major: "text-down border-down/40",
  critical: "text-down border-down/60",
  none: "text-ink-3 border-border",
};
function impactCls(i?: string): string {
  return IMPACT[i ?? ""] ?? "text-ink-3 border-border";
}

function detectedLabel(src?: string): string {
  if (src === "auto") return "auto-detected";
  if (src === "api") return "opened via API";
  return "declared manually";
}
function duration(inc: Incident): string {
  if (inc.status !== "resolved" || !inc.started_at) return "ongoing";
  if (!inc.resolved_at) return "—";
  const ms = new Date(inc.resolved_at).getTime() - new Date(inc.started_at).getTime();
  const m = Math.max(1, Math.round(ms / 60000));
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  return `${h}h ${m % 60}m`;
}

async function load() {
  loading.value = true;
  error.value = "";
  try {
    await ws.init();
    if (!ws.projectId) {
      incidents.value = [];
      return;
    }
    const res = await api.GET("/api/v1/projects/{projectID}/incidents", {
      params: { path: { projectID: ws.projectId } },
    });
    incidents.value = res.data ?? [];
    // Best-effort: a failed name lookup leaves the chips stating the KIND alone, which is still
    // the answer they exist to give. It must never take the incident list down with it.
    loadSubjectNames(incidents.value).catch(() => {});
    // Auto-select the first incident in the current filter.
    const first = shown.value[0];
    if (first?.id) select(first.id);
    else selectedID.value = null;
  } catch {
    error.value = "Could not load incidents.";
  } finally {
    loading.value = false;
  }
}

async function select(id: string) {
  selectedID.value = id;
  detailLoading.value = true;
  updates.value = [];
  postmortem.value = null;
  affected.value = null;
  try {
    const inc = incidents.value.find((i) => i.id === id);
    const [u, pm] = await Promise.all([
      api.GET("/api/v1/incidents/{incidentID}/updates", { params: { path: { incidentID: id } } }),
      api.GET("/api/v1/incidents/{incidentID}/postmortem", { params: { path: { incidentID: id } } }),
    ]);
    updates.value = u.data ?? [];
    postmortem.value = pm.data ?? null;
    if (inc?.monitor_id) {
      const m = await api.GET("/api/v1/monitors/{monitorID}", { params: { path: { monitorID: inc.monitor_id } } });
      affected.value = m.data ?? null;
    }
  } finally {
    detailLoading.value = false;
  }
}

// Quick inline status changes for the selected incident (full editing still lives
// in IncidentDetailView via "Manage"). Intermediate statuses transition in one
// click; Resolve is a deliberate, separate action (it's terminal).
const canWrite = computed(() => !!selected.value && session.canProjectWrite(ws.orgId, selected.value.project_id ?? ""));
// The working statuses shown as a segmented control (resolve has its own button).
const workingStatuses = computed(() => forwardStatuses(selected.value?.status));
const resolving = ref(false);
async function setStatus(status: string, body = "") {
  const inc = selected.value;
  if (!inc?.id || resolving.value || !ws.projectId || inc.status === status) return;
  const id = inc.id;
  resolving.value = true;
  try {
    const res = await api.POST("/api/v1/incidents/{incidentID}/updates", {
      params: { path: { incidentID: id } },
      body: { status: status as IncidentUpdate["status"], body },
    });
    if (!res.error) {
      const list = await api.GET("/api/v1/projects/{projectID}/incidents", { params: { path: { projectID: ws.projectId } } });
      incidents.value = list.data ?? [];
      await select(id); // keep this incident selected to show the new state
    }
  } finally {
    resolving.value = false;
  }
}
const resolveSelected = () => setStatus("resolved", "Resolved.");

// Keep the selection valid when the filter changes.
watch(filter, () => {
  if (!shown.value.some((i) => i.id === selectedID.value)) {
    const first = shown.value[0];
    if (first?.id) select(first.id);
    else selectedID.value = null;
  }
});

onMounted(load);
watch(() => ws.projectId, load);
</script>

<template>
  <AppShell active="incidents" :crumbs="[ws.orgName || 'cerbix', ws.projectName || '…', 'Incidents']">
    <template #actions>
      <RouterLink
        v-if="session.canProjectWrite(ws.orgId, ws.projectId)"
        :to="{ name: 'incident-new' }"
        class="flex h-[34px] items-center gap-[7px] rounded-sm bg-down px-[13px] text-[13px] font-medium text-white hover:brightness-105"
      >
        <svg viewBox="0 0 24 24" class="h-[15px] w-[15px]" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="9" /><path d="M12 8v5M12 16h.01" /></svg>
        Declare incident
      </RouterLink>
    </template>

    <div class="mx-auto max-w-[1180px] px-[22px] pb-16 pt-[26px]">
      <div class="mb-[18px]">
        <h1 class="text-[21px] font-semibold tracking-tight">Incidents</h1>
        <p class="mt-[3px] text-[13px] text-ink-3">
          <span v-if="loading">Loading…</span>
          <span v-else>{{ activeCount }} active · {{ resolvedCount }} resolved in the last 90 days</span>
        </p>
      </div>

      <div v-if="error" class="rounded border border-down/40 bg-down-weak p-4 text-[13px] text-down">{{ error }}</div>

      <template v-else>
        <!-- filter -->
        <div class="mb-4 inline-flex overflow-hidden rounded-sm border border-border">
          <button
            v-for="f in (['active', 'resolved', 'all'] as Filter[])"
            :key="f"
            type="button"
            class="border-r border-border px-[13px] py-[6px] text-[12.5px] capitalize last:border-r-0"
            :class="filter === f ? 'bg-surface-2 font-medium text-ink' : 'bg-surface text-ink-2 hover:text-ink'"
            @click="filter = f"
          >
            {{ f }}
            <span v-if="f !== 'all'" class="ml-1 font-mono text-ink-3">{{ f === "active" ? activeCount : resolvedCount }}</span>
          </button>
        </div>

        <div class="grid grid-cols-[320px_1fr] items-start gap-4 max-[920px]:grid-cols-1">
          <!-- list -->
          <div class="flex flex-col gap-2">
            <button
              v-for="inc in shown"
              :key="inc.id"
              type="button"
              class="flex flex-col gap-2 rounded border bg-surface p-[12px_13px] text-left shadow-card transition-colors"
              :class="inc.id === selectedID ? 'border-accent shadow-[0_0_0_1px_var(--accent-weak)]' : 'border-border hover:border-border-strong'"
              @click="select(inc.id!)"
            >
              <div class="flex items-center gap-2">
                <span class="inline-flex h-[22px] items-center gap-[6px] rounded-full px-[7px] text-[11.5px] font-semibold" :class="st(inc.status).chip">
                  <span class="h-[7px] w-[7px] rounded-full" :class="st(inc.status).dot"></span>{{ st(inc.status).label }}
                </span>
                <span class="rounded-xs border px-[7px] py-px text-[10.5px] font-bold uppercase tracking-[0.05em]" :class="impactCls(inc.impact)">{{ inc.impact }}</span>
                <IncidentSubjectChip
                  :service-id="inc.service_id"
                  :service-slug="serviceSlugs[inc.service_id ?? '']"
                  :monitor-id="inc.monitor_id"
                  :monitor-name="monitorNames[inc.monitor_id ?? '']"
                />
              </div>
              <div class="text-[13.5px] font-semibold leading-snug">{{ inc.title }}</div>
              <div class="flex items-center">
                <span class="font-mono text-[11.5px] text-ink-3">{{ inc.id?.slice(0, 8) }}</span>
                <span class="ml-auto font-mono text-[11.5px] text-ink-3">{{ relTime(inc.started_at) }}</span>
              </div>
            </button>
            <p v-if="!shown.length && !loading" class="rounded border border-border bg-surface p-6 text-center text-[13px] text-ink-3">
              No {{ filter === "all" ? "" : filter }} incidents.
            </p>
          </div>

          <!-- detail -->
          <div v-if="selected" class="overflow-hidden rounded border border-border bg-surface shadow-card">
            <div class="border-b border-border px-5 py-[18px]">
              <div class="mb-2 flex flex-wrap items-center gap-[10px]">
                <span class="inline-flex h-[22px] items-center gap-[6px] rounded-full px-[7px] text-[11.5px] font-semibold" :class="st(selected.status).chip">
                  <span class="h-[7px] w-[7px] rounded-full" :class="st(selected.status).dot"></span>{{ st(selected.status).label }}
                </span>
                <span class="rounded-xs border px-[7px] py-px text-[10.5px] font-bold uppercase tracking-[0.05em]" :class="impactCls(selected.impact)">{{ selected.impact }} impact</span>
                <IncidentSubjectChip
                  :service-id="selected.service_id"
                  :service-slug="serviceSlugs[selected.service_id ?? '']"
                  :monitor-id="selected.monitor_id"
                  :monitor-name="monitorNames[selected.monitor_id ?? '']"
                />
                <div class="ml-auto flex items-center gap-2">
                  <button
                    v-if="selected.status !== 'resolved' && canWrite"
                    type="button"
                    :disabled="resolving"
                    class="inline-flex h-[30px] items-center gap-[6px] rounded-sm border border-border px-[11px] text-[12.5px] text-ink-2 hover:border-up/60 hover:text-up disabled:opacity-50"
                    @click="resolveSelected"
                  >
                    <svg viewBox="0 0 24 24" class="h-[13px] w-[13px]" fill="none" stroke="currentColor" stroke-width="2.2"><path d="M5 12l5 5L20 7" /></svg>
                    {{ resolving ? "Resolving…" : "Resolve" }}
                  </button>
                  <RouterLink :to="{ name: 'incident', params: { id: selected.id } }" class="inline-flex h-[30px] items-center gap-[6px] rounded-sm border border-border px-[11px] text-[12.5px] text-ink hover:border-border-strong">
                    Manage
                    <svg viewBox="0 0 24 24" class="h-[13px] w-[13px]" fill="none" stroke="currentColor" stroke-width="2"><path d="M5 12h14M13 6l6 6-6 6" /></svg>
                  </RouterLink>
                </div>
              </div>
              <h2 class="text-[18px] font-semibold tracking-tight text-balance">{{ selected.title }}</h2>
              <div class="mt-[6px] flex flex-wrap gap-x-4 gap-y-2 text-[12.5px] text-ink-3">
                <span class="font-mono text-ink-2">{{ selected.id?.slice(0, 8) }}</span>
                <span>started <b class="font-semibold text-ink-2">{{ relTime(selected.started_at) }}</b></span>
                <span>duration <b class="font-semibold text-ink-2">{{ duration(selected) }}</b></span>
                <span>{{ detectedLabel(selected.source) }}</span>
              </div>
              <div v-if="affected" class="mt-3 flex flex-wrap gap-[6px]">
                <RouterLink
                  :to="{ name: 'monitor', params: { id: affected.id } }"
                  class="inline-flex items-center gap-[6px] rounded-xs border border-border bg-inset px-[8px] py-[2px] font-mono text-[11.5px] text-ink-2 hover:border-border-strong"
                >
                  <span class="h-[6px] w-[6px] rounded-full" :class="affected.status === 'down' ? 'bg-down' : 'bg-degraded'"></span>{{ affected.name }}
                </RouterLink>
              </div>
              <!-- quick status change (one click; Resolve is the separate button above) -->
              <div v-if="selected.status !== 'resolved' && canWrite" class="mt-[14px] flex flex-wrap items-center gap-[6px]">
                <span class="mr-1 text-[12px] text-ink-3">Set status</span>
                <button
                  v-for="s in workingStatuses"
                  :key="s"
                  type="button"
                  :disabled="resolving"
                  class="rounded-sm border px-[11px] py-[5px] text-[12px] font-medium transition-colors disabled:opacity-50"
                  :class="selected.status === s ? 'border-accent bg-accent-weak text-accent' : 'border-border text-ink-2 hover:border-border-strong hover:text-ink'"
                  @click="setStatus(s)"
                >{{ st(s).label }}</button>
              </div>
            </div>

            <!-- timeline -->
            <div class="px-5 pb-[18px] pt-2">
              <div class="py-[12px] text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Timeline</div>
              <p v-if="detailLoading" class="text-[13px] text-ink-3">Loading…</p>
              <ul v-else-if="updates.length" class="m-0 list-none p-0">
                <li v-for="(u, i) in updates" :key="u.id" class="relative pb-[14px] pl-[26px]">
                  <span class="absolute left-[5px] top-[6px] h-[9px] w-[9px] rounded-full ring-[3px] ring-surface" :class="st(u.status).dot"></span>
                  <span v-if="i < updates.length - 1" class="absolute left-[9px] top-[16px] bottom-0 w-[1.5px] bg-border"></span>
                  <div class="mb-[5px] flex items-center gap-[9px]">
                    <span class="inline-flex h-[22px] items-center gap-[6px] rounded-full px-[7px] text-[11.5px] font-semibold" :class="st(u.status).chip">
                      <span class="h-[7px] w-[7px] rounded-full" :class="st(u.status).dot"></span>{{ st(u.status).label }}
                    </span>
                    <span class="ml-auto font-mono text-[11.5px] text-ink-3">{{ relTime(u.created_at) }}</span>
                  </div>
                  <div class="whitespace-pre-wrap text-[13.5px] leading-relaxed text-ink-2">{{ u.body }}</div>
                  <div v-if="u.author" class="mt-[5px] text-[12px] text-ink-3">by <b class="font-semibold text-ink-2">{{ u.author }}</b></div>
                </li>
              </ul>
              <p v-else class="text-[13px] text-ink-3">No updates yet.</p>
            </div>

            <!-- postmortem -->
            <div class="border-t border-border px-5 py-[18px]">
              <div class="mb-3 flex items-center gap-[10px]">
                <h3 class="text-[14px] font-semibold">Postmortem</h3>
                <span v-if="postmortem?.published_at" class="rounded-full bg-up-weak px-[8px] py-[2px] text-[10.5px] font-bold uppercase tracking-[0.05em] text-up">Published</span>
                <RouterLink v-if="selected.status === 'resolved' && canWrite" :to="{ name: 'incident', params: { id: selected.id } }" class="ml-auto inline-flex h-[30px] items-center gap-[6px] rounded-sm border border-border px-[11px] text-[12.5px] text-ink hover:border-border-strong">
                  <svg viewBox="0 0 24 24" class="h-[13px] w-[13px]" fill="none" stroke="currentColor" stroke-width="2"><path d="M4 20h4L18.5 9.5a2.1 2.1 0 0 0-3-3L5 17v3z" /></svg>
                  {{ postmortem ? "Edit postmortem" : "Write postmortem" }}
                </RouterLink>
              </div>
              <div v-if="postmortem" class="flex max-w-[68ch] flex-col gap-3">
                <div v-for="sec in renderSections(postmortem.body)" :key="sec.heading">
                  <h4 class="mb-1 text-[12px] font-semibold uppercase tracking-[0.05em] text-ink-3">{{ sec.heading }}</h4>
                  <p class="whitespace-pre-wrap text-[13.5px] leading-relaxed text-ink-2">{{ sec.content }}</p>
                </div>
              </div>
              <p v-else-if="selected.status === 'resolved'" class="text-[13px] text-ink-3">No postmortem yet. Document what happened for the team and the status page.</p>
              <p v-else class="text-[13px] text-ink-3">A postmortem can be written once the incident is resolved.</p>
              <div v-if="postmortem" class="mt-[14px] text-[12px] text-ink-3">
                <template v-if="postmortem.author">Published by <b class="font-semibold text-ink-2">{{ postmortem.author }}</b> · </template>
                also available via <span class="font-mono">GET /api/v1/incidents/{{ selected.id?.slice(0, 8) }}/postmortem</span>
              </div>
            </div>
          </div>

          <div v-else class="rounded border border-border bg-surface p-10 text-center text-[13px] text-ink-3 shadow-card">
            Select an incident to see its timeline.
          </div>
        </div>
      </template>
    </div>
  </AppShell>
</template>
