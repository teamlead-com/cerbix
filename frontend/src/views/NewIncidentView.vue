<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from "vue";
import { useRouter } from "vue-router";
import { api } from "@/api/client";
import type { components } from "@/api/schema";
import AppShell from "@/components/AppShell.vue";
import { useSession } from "@/stores/session";
import { useWorkspace } from "@/stores/workspace";
import { IMPACT_ORDER, STATUS_ORDER, impactBadge, statusBadge } from "@/lib/incident";

type CreateIncident = components["schemas"]["CreateIncident"];
type Monitor = components["schemas"]["Monitor"];

const ws = useWorkspace();
const router = useRouter();

const form = reactive({
  projectId: "",
  title: "",
  impact: "minor" as CreateIncident["impact"],
  status: "investigating" as CreateIncident["status"],
  monitorId: "",
  body: "",
});

const monitors = ref<Monitor[]>([]);
const submitting = ref(false);
const error = ref("");
const session = useSession();
const writeAllowed = computed(() => !!form.projectId && session.canProjectWrite(ws.orgId, form.projectId));
const canSubmit = computed(() => writeAllowed.value && !!form.title.trim() && !submitting.value);
const projectName = computed(() => ws.projects.find((p) => p.id === form.projectId)?.name || "…");

// The affected monitor list follows the chosen project.
async function loadMonitors() {
  form.monitorId = "";
  monitors.value = [];
  if (!form.projectId) return;
  const res = await api.GET("/api/v1/projects/{projectID}/monitors", { params: { path: { projectID: form.projectId } } });
  monitors.value = res.data ?? [];
}

async function submit() {
  if (!canSubmit.value) return;
  submitting.value = true;
  error.value = "";
  try {
    const body: CreateIncident = { title: form.title.trim(), impact: form.impact, status: form.status, body: form.body };
    if (form.monitorId) body.monitor_id = form.monitorId;
    const res = await api.POST("/api/v1/projects/{projectID}/incidents", {
      params: { path: { projectID: form.projectId } },
      body,
    });
    if (res.error || !res.data) {
      error.value = (res.error as { error?: string })?.error || "Could not open the incident.";
      return;
    }
    router.push({ name: "incident", params: { id: res.data.id } });
  } catch {
    error.value = "Could not open the incident.";
  } finally {
    submitting.value = false;
  }
}

onMounted(async () => {
  await ws.init();
  form.projectId = ws.projectId || ws.projects[0]?.id || "";
  await loadMonitors();
});
watch(() => form.projectId, loadMonitors);
</script>

<template>
  <AppShell active="incidents" :crumbs="[ws.orgName || 'cerbix', projectName, 'New incident']">
    <div class="mx-auto max-w-[760px] px-[22px] pb-16 pt-[26px]">
      <div class="mb-[22px]">
        <h1 class="text-[21px] font-semibold tracking-tight">Open an incident</h1>
        <p class="mt-[3px] text-[13px] text-ink-3">Attach it to an existing project (and optionally the affected monitor).</p>
      </div>

      <form class="flex flex-col gap-4 rounded border border-border bg-surface p-4 shadow-card" @submit.prevent="submit">
        <div class="grid grid-cols-2 gap-3 max-[560px]:grid-cols-1">
          <label class="flex flex-col gap-[6px]">
            <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Project</span>
            <select v-model="form.projectId" class="rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent">
              <option v-if="!ws.projects.length" value="">No projects</option>
              <option v-for="p in ws.projects" :key="p.id" :value="p.id">{{ p.name }}</option>
            </select>
          </label>
          <label class="flex flex-col gap-[6px]">
            <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Affected monitor <span class="text-ink-3/70">· optional</span></span>
            <select v-model="form.monitorId" class="rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent">
              <option value="">— none —</option>
              <option v-for="m in monitors" :key="m.id" :value="m.id">{{ m.name }}</option>
            </select>
          </label>
        </div>

        <label class="flex flex-col gap-[6px]">
          <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Title</span>
          <input v-model="form.title" type="text" placeholder="Elevated API latency" class="rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13.5px] outline-none focus:border-accent" />
        </label>

        <div class="grid grid-cols-2 gap-3 max-[560px]:grid-cols-1">
          <label class="flex flex-col gap-[6px]">
            <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Impact</span>
            <select v-model="form.impact" class="rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent">
              <option v-for="i in IMPACT_ORDER" :key="i" :value="i">{{ impactBadge(i).label }}</option>
            </select>
          </label>
          <label class="flex flex-col gap-[6px]">
            <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Initial status</span>
            <select v-model="form.status" class="rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent">
              <option v-for="s in STATUS_ORDER" :key="s" :value="s">{{ statusBadge(s).label }}</option>
            </select>
          </label>
        </div>

        <label class="flex flex-col gap-[6px]">
          <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Opening update</span>
          <textarea v-model="form.body" rows="4" placeholder="What's happening and what you're doing about it (markdown)." class="rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13.5px] outline-none focus:border-accent"></textarea>
        </label>

        <div v-if="form.projectId && !writeAllowed" class="rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] text-ink-3">You don't have permission to write in this project.</div>
        <div v-if="error" class="rounded-sm border border-down/40 bg-down-weak px-3 py-2 text-[13px] text-down">{{ error }}</div>

        <div class="flex items-center gap-2">
          <button type="submit" :disabled="!canSubmit" class="h-[36px] rounded-sm bg-accent px-4 text-[13px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-50">
            {{ submitting ? "Opening…" : "Open incident" }}
          </button>
          <RouterLink :to="{ name: 'incidents' }" class="h-[36px] rounded-sm border border-border px-4 text-[13px] leading-[36px] text-ink-2 hover:border-border-strong">Cancel</RouterLink>
        </div>
      </form>
    </div>
  </AppShell>
</template>
