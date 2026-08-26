<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from "vue";
import { useRoute } from "vue-router";
import { api } from "@/api/client";
import type { components } from "@/api/schema";
import AppShell from "@/components/AppShell.vue";
import IncidentSubjectChip from "@/components/IncidentSubjectChip.vue";
import { useSession } from "@/stores/session";
import { useWorkspace } from "@/stores/workspace";
import { forwardStatuses, impactBadge, relTime, statusBadge } from "@/lib/incident";
import { PM_SECTIONS, emptySections, parsePostmortem, renderSections, serializePostmortem } from "@/lib/postmortem";

type Incident = components["schemas"]["AuthedIncident"];
type IncidentUpdate = components["schemas"]["IncidentUpdate"];
type Postmortem = components["schemas"]["Postmortem"];
type ImpactLink = components["schemas"]["ServiceImpactLink"];

const route = useRoute();
const ws = useWorkspace();
const session = useSession();
// REACTIVE, for the same reason as the monitor page: read once, the view kept showing and mutating
// the incident it was mounted with while the URL named another. `canWrite` compounded it — it
// compares the WORKSPACE's org against the loaded incident's project, so a stale tenant hid
// acknowledge, resolve and postmortem from a legitimate editor of the incident actually on screen.
const id = computed(() => route.params.id as string);
// A slower response for the previous incident must not overwrite the current one.
let loadTicket = 0;

const loading = ref(true);
const incident = ref<Incident | null>(null);
const canWrite = computed(() => !!incident.value && session.canProjectWrite(ws.orgId, incident.value.project_id ?? ""));
const updates = ref<IncidentUpdate[]>([]);
const postmortem = ref<Postmortem | null>(null);
// The SUBJECT's name for the chip (FR-022). One request, for the ONE anchor this incident has, and
// only when it has one — a project-level incident asks for nothing.
const subjectName = ref("");

const isResolved = computed(() => incident.value?.status === "resolved");
const isAcked = computed(() => !!incident.value?.acknowledged_at);

// FR-021 phase 3 (§14.4): the impact links of the service graph, as CHIPS — the structured
// relation, never parsed prose. Ranking is presentation only: nearest first by path length,
// then slug, exactly as the 🕸 note orders them. `impacts: null` with impacts_unavailable is
// a FAILED read and says so; an empty array is the honest "no links".
const impacts = computed<ImpactLink[]>(() => {
  const list = incident.value?.impacts ?? [];
  return [...list].sort((a, b) => (a.path?.length ?? 0) - (b.path?.length ?? 0) || a.slug.localeCompare(b.slug));
});
const impactsUnavailable = computed(() => !!incident.value?.impacts_unavailable);
const probableRoots = computed(() => impacts.value.filter((l) => l.role === "probable_root"));
const affected = computed(() => impacts.value.filter((l) => l.role === "affected"));

async function acknowledge() {
  if (!incident.value) return;
  posting.value = true;
  const res = await api.POST("/api/v1/incidents/{incidentID}/acknowledge", {
    params: { path: { incidentID: id.value } },
  });
  // The acknowledge response is the BASE incident — it carries no impacts and no
  // impacts_unavailable ([298] P1-1). Assigning it wholesale would silently erase the
  // enrichment the detail was loaded with, so merge the base fields into what we have
  // and keep the impact state this endpoint knows nothing about.
  if (!res.error && res.data && incident.value) {
    incident.value = {
      ...res.data,
      impacts: incident.value.impacts,
      impacts_unavailable: incident.value.impacts_unavailable,
    } as Incident;
  }
  posting.value = false;
}

async function load() {
  const ticket = ++loadTicket;
  const incidentID = id.value;
  loading.value = true;
  const [inc, ups, pm] = await Promise.all([
    api.GET("/api/v1/incidents/{incidentID}", { params: { path: { incidentID } } }),
    api.GET("/api/v1/incidents/{incidentID}/updates", { params: { path: { incidentID } } }),
    api.GET("/api/v1/incidents/{incidentID}/postmortem", { params: { path: { incidentID } } }),
  ]);
  if (ticket !== loadTicket) return;
  incident.value = inc.data ?? null;
  updates.value = ups.data ?? [];
  postmortem.value = pm.error ? null : (pm.data ?? null);
  loading.value = false;
  loadSubjectName().catch(() => {});
}

// Best-effort by design: without the name the chip still states the KIND, and an incident whose
// subject was DELETED must keep rendering — its timeline is a record of something that happened,
// and the anchor being gone does not unhappen it.
async function loadSubjectName() {
  const inc = incident.value;
  const projectID = inc?.project_id;
  if (!inc || !projectID) return;
  if (inc.service_id) {
    const res = await api.GET("/api/v1/projects/{projectID}/services/{serviceID}", {
      params: { path: { projectID, serviceID: inc.service_id } },
    });
    subjectName.value = res.data?.service?.slug || res.data?.service?.name || "";
  } else if (inc.monitor_id) {
    const res = await api.GET("/api/v1/monitors/{monitorID}", {
      params: { path: { monitorID: inc.monitor_id } },
    });
    subjectName.value = res.data?.name || "";
  }
}

// Add-update composer.
const composer = reactive({ status: "" as string, body: "" });
const posting = ref(false);
const updateError = ref("");
// Selected target status (defaults to the incident's current one), and whether
// posting will actually change it — drives the segmented picker + button label.
const nextStatus = computed(() => composer.status || incident.value?.status || "investigating");
const willChangeStatus = computed(() => !!composer.status && composer.status !== incident.value?.status);

async function addUpdate(forceResolve = false) {
  posting.value = true;
  updateError.value = "";
  // A plain comment sends NO status. Echoing the status this screen last loaded is how a comment
  // written while somebody else moved the incident forward used to revert it; the server resolves
  // "keep whatever it is" against the row it locks, and a screen minutes out of date can no longer
  // publish a stale opinion about the lifecycle.
  const status = forceResolve ? "resolved" : composer.status;
  const body = forceResolve ? (composer.body || "Resolved.") : composer.body;
  try {
    const res = await api.POST("/api/v1/incidents/{incidentID}/updates", {
      params: { path: { incidentID: id.value } },
      body: { ...(status ? { status: status as IncidentUpdate["status"] } : {}), body },
    });
    if (res.error) {
      updateError.value = (res.error as { error?: string })?.error || "Could not post the update.";
      return;
    }
    composer.body = "";
    composer.status = "";
    await load();
  } finally {
    posting.value = false;
  }
}

// Structured postmortem composer (serialized to the single markdown body).
const pm = reactive(emptySections());
const editingPm = ref(false);
const pmPosting = ref(false);
const pmError = ref("");
const pmSections = computed(() => PM_SECTIONS);
const publishedSections = computed(() => renderSections(postmortem.value?.body ?? ""));
const pmHasContent = computed(() => PM_SECTIONS.some((s) => pm[s.key].trim()));

function startEditPm() {
  Object.assign(pm, postmortem.value ? parsePostmortem(postmortem.value.body) : emptySections());
  pmError.value = "";
  editingPm.value = true;
}
function cancelEditPm() {
  editingPm.value = false;
  pmError.value = "";
}

async function publishPostmortem() {
  if (!pmHasContent.value) return;
  pmPosting.value = true;
  pmError.value = "";
  try {
    const res = await api.PUT("/api/v1/incidents/{incidentID}/postmortem", {
      params: { path: { incidentID: id.value } },
      body: { body: serializePostmortem(pm) },
    });
    if (res.error || !res.data) {
      pmError.value = (res.error as { error?: string })?.error || "Could not publish the postmortem.";
      return;
    }
    postmortem.value = res.data;
    editingPm.value = false;
  } finally {
    pmPosting.value = false;
  }
}

onMounted(() => {
  ws.init();
  load();
});

// The route identity and the workspace, the same pair ServiceDetail watches.
watch(() => [id.value, ws.projectId], load);
</script>

<template>
  <AppShell active="incidents" :crumbs="['incidents', incident?.title || '…']">
    <template #actions>
      <button
        v-if="incident && !isResolved && !isAcked && canWrite"
        type="button"
        class="h-[34px] rounded-sm border border-border px-[13px] text-[13px] text-ink-2 hover:border-accent/60 hover:text-accent disabled:opacity-50"
        :disabled="posting"
        title="Stop on-call escalation for this incident"
        @click="acknowledge"
      >
        Acknowledge
      </button>
      <button
        v-if="incident && !isResolved && canWrite"
        type="button"
        class="h-[34px] rounded-sm border border-border px-[13px] text-[13px] text-ink-2 hover:border-up/60 hover:text-up disabled:opacity-50"
        :disabled="posting"
        @click="addUpdate(true)"
      >
        Resolve
      </button>
    </template>

    <div class="mx-auto max-w-[900px] px-[22px] pb-16 pt-6">
      <div v-if="incident" class="mb-5">
        <div class="flex flex-wrap items-center gap-[10px]">
          <h1 class="text-[22px] font-semibold tracking-tight">{{ incident.title }}</h1>
          <span class="rounded-full px-[9px] py-[2px] text-[11.5px] font-medium" :class="statusBadge(incident.status).cls">{{ statusBadge(incident.status).label }}</span>
          <span class="rounded-full px-[9px] py-[2px] text-[11.5px] font-medium" :class="impactBadge(incident.impact).cls">{{ impactBadge(incident.impact).label }}</span>
          <span class="rounded-xs border border-border px-[6px] py-px font-mono text-[10.5px] uppercase tracking-[0.04em] text-ink-3">{{ incident.source }}</span>
          <IncidentSubjectChip
            :service-id="incident.service_id"
            :service-slug="subjectName"
            :monitor-id="incident.monitor_id"
            :monitor-name="subjectName"
          />
          <span
            v-if="(incident.escalation_step ?? 0) > 0 && !incident.acknowledged_at && incident.status !== 'resolved'"
            class="rounded-full bg-degraded-weak px-[9px] py-[2px] text-[11.5px] font-medium text-degraded"
            :title="'The escalation engine has notified ' + incident.escalation_step + ' step(s) of the policy'"
          >⛑ escalated to step {{ incident.escalation_step }}<template v-if="incident.last_escalated_at"> · {{ relTime(incident.last_escalated_at) }}</template></span>
        </div>
        <div class="mt-[7px] flex flex-wrap gap-x-4 gap-y-1 text-[13px] text-ink-3">
          <span>started <span class="font-mono text-ink-2">{{ relTime(incident.started_at) }}</span></span>
          <span v-if="incident.acknowledged_at">acknowledged
            <template v-if="incident.acknowledged_by_name">by <b class="font-medium text-ink-2">{{ incident.acknowledged_by_name }}</b> ·</template>
            <span class="font-mono text-ink-2">{{ relTime(incident.acknowledged_at) }}</span></span>
          <span v-if="incident.resolved_at">resolved <span class="font-mono text-ink-2">{{ relTime(incident.resolved_at) }}</span></span>
          <span v-if="incident.external_key" class="rounded-xs border border-border bg-inset px-[6px] py-px font-mono text-[11px]" :title="'External correlation key'">{{ incident.external_key }}</span>
        </div>
      </div>

      <!-- Impact chips (FR-021 §14.4): every impacted upstream service is a candidate —
           the relation never elects one culprit; the "via" path is the stored array. -->
      <div v-if="impacts.length || impactsUnavailable" class="mb-5 flex flex-wrap gap-2" data-testid="incident-impacts">
        <RouterLink
          v-for="l in probableRoots"
          :key="'r-' + l.service_id"
          :to="{ name: 'service', params: { id: l.service_id } }"
          class="inline-flex items-center gap-[7px] rounded-sm border border-border bg-surface-2 px-[9px] py-[4px] text-[12.5px] hover:border-border-strong"
          :title="'via ' + (l.path ?? []).join(' → ')"
          data-testid="impact-root"
        >
          <span class="text-[10.5px] font-semibold uppercase tracking-[0.06em] text-down">🕸 probable root</span>
          <span class="font-medium">{{ l.slug }}</span>
          <span class="font-mono text-[11px] text-ink-3">via {{ (l.path ?? []).join(" → ") }}</span>
        </RouterLink>
        <RouterLink
          v-for="l in affected"
          :key="'a-' + l.service_id"
          :to="{ name: 'service', params: { id: l.service_id } }"
          class="inline-flex items-center gap-[7px] rounded-sm border border-border bg-surface-2 px-[9px] py-[4px] text-[12.5px] hover:border-border-strong"
          :title="'via ' + (l.path ?? []).join(' → ')"
          data-testid="impact-affected"
        >
          <span class="text-[10.5px] font-semibold uppercase tracking-[0.06em] text-degraded">affected</span>
          <span class="font-medium">{{ l.slug }}</span>
        </RouterLink>
        <span
          v-if="impactsUnavailable"
          class="inline-flex items-center gap-[6px] rounded-sm border border-dashed border-border-strong px-[9px] py-[4px] text-[12.5px] text-ink-3"
          data-testid="impact-unavailable"
        >
          🕸 impact links unavailable — the read failed, so this is not “no impact”
        </span>
      </div>

      <!-- timeline -->
      <section class="mb-5 rounded border border-border bg-surface shadow-card">
        <div class="border-b border-border px-4 py-[13px] text-[13px] font-semibold">Timeline</div>
        <ol class="flex flex-col">
          <li v-for="(u, i) in updates" :key="i" class="flex gap-3 border-b border-border px-4 py-[13px] last:border-b-0">
            <span class="mt-[3px] h-[9px] w-[9px] shrink-0 rounded-full" :class="statusBadge(u.status).cls"></span>
            <div class="min-w-0">
              <div class="flex items-center gap-2">
                <span class="text-[12.5px] font-medium">{{ statusBadge(u.status).label }}</span>
                <span class="font-mono text-[11px] text-ink-3">{{ relTime(u.created_at) }}</span>
                <span v-if="u.author" class="font-mono text-[11px] text-ink-3">· {{ u.author }}</span>
              </div>
              <p v-if="u.body" class="mt-1 whitespace-pre-wrap text-[13px] text-ink-2">{{ u.body }}</p>
            </div>
          </li>
          <li v-if="!updates.length && !loading" class="px-4 py-6 text-center text-[13px] text-ink-3">No updates yet.</li>
        </ol>
      </section>

      <!-- add update -->
      <section v-if="incident && !isResolved && canWrite" class="mb-5 flex flex-col gap-3 rounded border border-border bg-surface p-4 shadow-card">
        <div class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Post an update</div>
        <!-- status as a one-click segmented picker (current is highlighted) -->
        <div class="flex flex-wrap items-center gap-[6px]">
          <span class="mr-1 text-[12px] text-ink-3">Status</span>
          <button
            v-for="s in forwardStatuses(incident?.status)"
            :key="s"
            type="button"
            class="rounded-sm border px-[11px] py-[6px] text-[12.5px] font-medium transition-colors"
            :class="nextStatus === s ? 'border-accent bg-accent-weak text-accent' : 'border-border text-ink-2 hover:border-border-strong hover:text-ink'"
            :data-testid="`update-status-${s}`"
            @click="composer.status = s"
          >{{ statusBadge(s).label }}</button>
        </div>
        <textarea v-model="composer.body" rows="3" data-testid="update-body" placeholder="What changed (optional, markdown)." class="rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13.5px] outline-none focus:border-accent"></textarea>
        <div v-if="updateError" class="text-[12.5px] text-down">{{ updateError }}</div>
        <div>
          <button type="button" :disabled="posting" data-testid="update-post" class="h-[34px] rounded-sm bg-accent px-4 text-[13px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-50" @click="addUpdate(false)">
            {{ posting ? "Posting…" : willChangeStatus ? `Change to ${statusBadge(composer.status).label}` : "Post update" }}
          </button>
        </div>
      </section>

      <!-- postmortem -->
      <section class="rounded border border-border bg-surface p-4 shadow-card">
        <div class="mb-3 flex items-center gap-[10px]">
          <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Postmortem</span>
          <button
            v-if="postmortem && !editingPm && canWrite"
            type="button"
            class="ml-auto inline-flex h-[28px] items-center gap-[6px] rounded-sm border border-border px-[10px] text-[12.5px] text-ink-2 hover:border-border-strong"
            @click="startEditPm"
          >
            <svg viewBox="0 0 24 24" class="h-[13px] w-[13px]" fill="none" stroke="currentColor" stroke-width="2"><path d="M4 20h4L18.5 9.5a2.1 2.1 0 0 0-3-3L5 17v3z" /></svg>
            Edit
          </button>
        </div>

        <!-- published view (structured sections) -->
        <div v-if="postmortem && !editingPm">
          <div class="mb-3 font-mono text-[11px] text-ink-3">published {{ relTime(postmortem.published_at) }} · {{ postmortem.author }}</div>
          <div class="flex flex-col gap-3">
            <div v-for="sec in publishedSections" :key="sec.heading">
              <h4 class="mb-1 text-[12px] font-semibold uppercase tracking-[0.05em] text-ink-3">{{ sec.heading }}</h4>
              <p class="whitespace-pre-wrap text-[13.5px] leading-relaxed text-ink-2">{{ sec.content }}</p>
            </div>
            <p v-if="!publishedSections.length" class="text-[13px] text-ink-3">No content.</p>
          </div>
        </div>

        <!-- structured editor (new, or editing an existing one) -->
        <div v-else-if="canWrite && (editingPm || isResolved)" class="flex flex-col gap-4">
          <label v-for="sec in pmSections" :key="sec.key" class="flex flex-col gap-[6px]">
            <span class="text-[12px] font-semibold text-ink-2">{{ sec.heading }}</span>
            <textarea v-model="pm[sec.key]" rows="3" :placeholder="sec.placeholder" class="rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13.5px] outline-none focus:border-accent"></textarea>
          </label>
          <div v-if="pmError" class="text-[12.5px] text-down">{{ pmError }}</div>
          <div class="flex items-center gap-2">
            <button type="button" :disabled="pmPosting || !pmHasContent" class="h-[34px] rounded-sm bg-accent px-4 text-[13px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-50" @click="publishPostmortem">
              {{ pmPosting ? "Saving…" : postmortem ? "Save postmortem" : "Publish postmortem" }}
            </button>
            <button v-if="editingPm && postmortem" type="button" class="h-[34px] rounded-sm border border-border px-4 text-[13px] text-ink-2 hover:border-border-strong" @click="cancelEditPm">Cancel</button>
          </div>
        </div>

        <p v-else class="text-[13px] text-ink-3">A postmortem can be published once the incident is resolved.</p>
      </section>
    </div>
  </AppShell>
</template>
