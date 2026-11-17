<script setup lang="ts">
// FR-042 / NFR-035 (D4, D19), against the APPROVED mock (docs/design/mock-incident-impact.html §2):
// Settings → Project → Incidents. One setting today — the impact a project's MONITOR auto-incidents
// open with — in a tab named for the subject rather than the field, so later incident settings have
// a home (D-8). The card follows the instance "Monitor defaults" card.
//
// Three things it must not do:
//   * send the server's no-op. Save stays disabled until the draft differs from what is STORED, and
//     "was <Old>" says what it differs from while it does.
//   * show a value it did not read. A failed read says so; it never falls back to `major` (the
//     server's default), because a reader would take that for this project's setting.
//   * hand a reader a disabled control. Without `project:manage` the value is the incident page's
//     own impact badge — the word they will meet on incidents — and who can change it (D-9).
import { computed, onMounted, ref, watch } from "vue";

import { api } from "@/api/client";
import type { components } from "@/api/schema";
import { IMPACT_ORDER, impactBadge } from "@/lib/incident";

type Project = components["schemas"]["Project"];
type IncidentImpact = components["schemas"]["IncidentImpact"];

const props = defineProps<{ projectId: string; canManage: boolean }>();

const loading = ref(true);
const unavailable = ref(false);
const stored = ref<IncidentImpact | null>(null);
const draft = ref<IncidentImpact | null>(null);
const saving = ref(false);
const saved = ref(false);
const error = ref("");
// A response for the project the operator has already switched away from must never land.
let generation = 0;

const options = IMPACT_ORDER.map((value) => ({ value, label: impactBadge(value).label }));
const dirty = computed(() => draft.value !== null && draft.value !== stored.value);

async function load() {
  const mine = ++generation;
  loading.value = true;
  unavailable.value = false;
  error.value = "";
  saved.value = false;
  // A save still in flight belongs to the project being left; its answer is dropped by generation.
  saving.value = false;
  stored.value = null;
  draft.value = null;
  // No project yet (an organization without projects, or the workspace still resolving): there is
  // nothing to read, so no request and no error. The watch below loads once an id appears.
  if (!props.projectId) {
    loading.value = false;
    return;
  }
  try {
    const res = (await api.GET("/api/v1/projects/{projectID}", {
      params: { path: { projectID: props.projectId } },
    })) as { data?: Project; error?: unknown };
    if (mine !== generation) return;
    const value = res.data?.default_incident_impact;
    if (res.error || !value) {
      unavailable.value = true;
      return;
    }
    stored.value = value;
    draft.value = value;
  } catch {
    if (mine === generation) unavailable.value = true;
  } finally {
    if (mine === generation) loading.value = false;
  }
}

function pick(value: string) {
  draft.value = value as IncidentImpact;
  // A further change is a new edit: the last save's confirmation no longer describes the form.
  saved.value = false;
  error.value = "";
}

async function save() {
  if (!props.canManage || !dirty.value || saving.value || !draft.value) return;
  const mine = generation;
  saving.value = true;
  error.value = "";
  saved.value = false;
  try {
    const res = (await api.PATCH("/api/v1/projects/{projectID}", {
      params: { path: { projectID: props.projectId } },
      body: { default_incident_impact: draft.value },
    })) as { data?: Project; error?: { error?: string } };
    if (mine !== generation) return;
    const echoed = res.data?.default_incident_impact;
    if (res.error || !echoed) {
      // The refusal renders as itself — a 403 when the role was revoked while the page was open
      // is a different answer from a transport failure.
      error.value = res.error?.error ?? "Could not save the project's incident settings.";
      return;
    }
    // The ECHO is what the row holds now, so the form adopts it: what was just saved is no longer
    // a change, and "was <Old>" gives way to "✓ Saved".
    stored.value = echoed;
    draft.value = echoed;
    saved.value = true;
  } catch {
    if (mine === generation) error.value = "Could not reach the server.";
  } finally {
    if (mine === generation) saving.value = false;
  }
}

watch(() => props.projectId, load);
onMounted(load);
</script>

<template>
  <section class="flex flex-col gap-4 rounded border border-border bg-surface p-[18px] shadow-card" data-testid="project-incident-settings">
    <h2 class="text-[14px] font-semibold">Incidents</h2>

    <p v-if="loading" class="text-[13px] text-ink-3">Loading…</p>
    <p v-else-if="!projectId" class="text-[13px] text-ink-3" data-testid="project-incident-no-project">
      Select a project to manage its incident settings.
    </p>
    <p v-else-if="unavailable" class="text-[13px] text-ink-3" data-testid="project-incident-unavailable">
      Could not load the project's incident settings.
    </p>

    <template v-else>
      <label v-if="canManage" class="flex flex-col gap-[6px]">
        <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Impact of monitor incidents</span>
        <select
          class="min-w-[200px] self-start rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent"
          :value="draft ?? undefined"
          :disabled="saving"
          data-testid="project-incident-impact-select"
          @change="pick(($event.target as HTMLSelectElement).value)"
        >
          <option v-for="o in options" :key="o.value" :value="o.value">{{ o.label }}</option>
        </select>
      </label>
      <div v-else class="flex flex-col gap-[6px]">
        <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Impact of monitor incidents</span>
        <span>
          <span
            class="rounded-full px-[9px] py-[2px] text-[11.5px] font-medium"
            :class="impactBadge(stored ?? undefined).cls"
            data-testid="project-incident-impact-badge"
          >{{ impactBadge(stored ?? undefined).label }}</span>
        </span>
      </div>

      <p class="max-w-[72ch] text-[12.5px] leading-[1.55] text-ink-3" data-testid="project-incident-explanation">
        The impact a monitor's automatic incident opens with. It is shown on status pages and in incident lists, and it does not change while the incident is open. Changing it here does not touch incidents that are already open — it applies from the next one. Incidents a service opens take their impact from that service's alerting settings instead.
      </p>

      <div v-if="canManage" class="flex items-center gap-3 border-t border-border pt-[14px]">
        <button
          type="button"
          :disabled="!dirty || saving"
          class="h-[36px] rounded-sm bg-accent px-[16px] text-[13px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-50"
          data-testid="project-incident-save"
          @click="save"
        >{{ saving ? "Saving…" : "Save" }}</button>
        <span v-if="dirty" class="text-[12.5px] text-ink-3" data-testid="project-incident-was">was {{ impactBadge(stored ?? undefined).label }}</span>
        <span v-else-if="saved" class="text-[12.5px] text-up" data-testid="project-incident-saved">✓ Saved</span>
        <span v-if="error" class="text-[12.5px] text-down" data-testid="project-incident-error">{{ error }}</span>
      </div>
      <p v-else class="text-[12.5px] text-ink-3" data-testid="project-incident-read-only">Only a project admin can change this.</p>
    </template>
  </section>
</template>
