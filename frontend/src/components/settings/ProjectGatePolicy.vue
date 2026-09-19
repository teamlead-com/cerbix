<script setup lang="ts">
import { computed, onMounted, ref, watch } from "vue";

import { api } from "@/api/client";
import type { components } from "@/api/schema";
import {
  ASSIGNMENTS,
  CHIP_ACC,
  CHIP_BASE,
  CHIP_DORM,
  CHIP_PLAIN,
  CLAUSE_DESCRIPTIONS,
  GATE_CLAUSES,
  GATE_SCHEMA_VERSION,
  GATE_WINDOWS,
  UNKNOWN_BEHAVIOR_LABELS,
  assignmentClass,
  createTemplate,
  describeFailure,
  draftFromPolicy,
  draftToBody,
  durationShort,
  failureOf,
  isConflict,
  validateDraft,
  type GateFailure,
  type PolicyDraft,
} from "@/lib/gate";
import { sealedLabel } from "@/lib/services";

type GatePolicy = components["schemas"]["GatePolicy"];
type Res<T> = { data?: T; error?: unknown; response?: { status?: number } };

const props = defineProps<{ projectId: string; canManage: boolean }>();
const path = computed(() => ({ projectID: props.projectId }));
const policy = ref<GatePolicy | null>(null);
const loading = ref(true);
const unavailable = ref("");
const editing = ref(false);
const saving = ref(false);
const deleting = ref(false);
const error = ref("");
const conflict = ref(false);
const draft = ref<PolicyDraft>(createTemplate(GATE_WINDOWS));
const baseRevision = ref<number | null>(null);

const chipBase = CHIP_BASE;
const chipPlain = `${CHIP_BASE} ${CHIP_PLAIN}`;
const chipAcc = `${CHIP_BASE} ${CHIP_ACC}`;
const chipDorm = `${CHIP_BASE} ${CHIP_DORM}`;
const draftErrors = computed(() => validateDraft(draft.value, GATE_WINDOWS));
const canSave = computed(() => !saving.value && !conflict.value && Object.keys(draftErrors.value).length === 0);

function failure(res: Res<unknown>): GateFailure {
  return failureOf(res);
}

async function load() {
  loading.value = true;
  unavailable.value = "";
  error.value = "";
  conflict.value = false;
  const res = await api.GET("/api/v1/projects/{projectID}/gate/policy", { params: { path: path.value } }) as Res<GatePolicy>;
  loading.value = false;
  if (res.error !== undefined || (res.response?.status ?? 0) >= 400) {
    if (res.response?.status === 404) {
      policy.value = null;
      return;
    }
    unavailable.value = describeFailure(failure(res), { context: "read", fallback: "Could not load the project gate policy." });
    return;
  }
  policy.value = res.data ?? null;
}

function openEditor() {
  if (!props.canManage || conflict.value) return;
  draft.value = policy.value ? draftFromPolicy(policy.value) : createTemplate(GATE_WINDOWS);
  baseRevision.value = policy.value?.revision ?? null;
  error.value = "";
  editing.value = true;
}

function discard() {
  editing.value = false;
  if (!conflict.value) error.value = "";
}

async function save() {
  if (!canSave.value) return;
  saving.value = true;
  error.value = "";
  const res = await api.PUT("/api/v1/projects/{projectID}/gate/policy", {
    params: { path: path.value },
    body: draftToBody(draft.value, baseRevision.value),
  }) as Res<{ revision: number }>;
  saving.value = false;
  if (res.error !== undefined || (res.response?.status ?? 0) >= 400) {
    const item = failure(res);
    error.value = describeFailure(item, { context: "save", fallback: "Could not save the project gate policy." });
    conflict.value = isConflict(item);
    return;
  }
  editing.value = false;
  await load();
}

async function remove() {
  if (!policy.value || deleting.value || conflict.value) return;
  if (!window.confirm("Delete the project gate policy? Services without their own policy will become not configured.")) return;
  deleting.value = true;
  error.value = "";
  const res = await api.DELETE("/api/v1/projects/{projectID}/gate/policy", {
    params: { path: path.value, query: { expected_revision: policy.value.revision } },
  }) as Res<unknown>;
  deleting.value = false;
  if (res.error !== undefined || (res.response?.status ?? 0) >= 400) {
    const item = failure(res);
    error.value = describeFailure(item, { context: "delete", fallback: "Could not delete the project gate policy." });
    conflict.value = isConflict(item);
    return;
  }
  await load();
}

watch(() => props.projectId, load);
onMounted(load);
</script>

<template>
  <section class="rounded border border-border bg-surface shadow-card" data-testid="project-gate-policy">
    <div class="flex flex-wrap items-center gap-2 border-b border-border px-4 py-3">
      <div>
        <h2 class="text-[14px] font-semibold">Release gate policy</h2>
        <p class="mt-0.5 text-[12px] text-ink-3">Default policy inherited by services without an explicit service policy.</p>
      </div>
      <div class="flex-1"></div>
      <span v-if="policy" :class="chipAcc" data-testid="project-gate-source">project policy</span>
      <span v-else :class="chipDorm" data-testid="project-gate-source">no project policy</span>
      <button v-if="canManage && !editing" type="button" class="h-[32px] rounded-sm border border-border px-3 text-[12.5px] hover:border-border-strong" :disabled="loading || conflict" data-testid="project-gate-configure" @click="openEditor">
        {{ policy ? "Edit policy" : "Configure policy" }}
      </button>
    </div>

    <p v-if="loading" class="px-4 py-7 text-[13px] text-ink-3">Loading policy…</p>
    <p v-else-if="unavailable" class="px-4 py-7 text-[13px] text-ink-3">{{ unavailable }}</p>

    <div v-else class="p-4">
      <div v-if="error" class="mb-4 flex items-center gap-2 rounded border border-down bg-down-weak px-3 py-2 text-[12.5px] text-down" role="alert">
        <span class="flex-1">{{ error }}</span>
        <button v-if="conflict" type="button" class="rounded border border-down bg-surface px-2 py-0.5" @click="load">Reload</button>
      </div>

      <div v-if="!policy && !editing" class="max-w-[70ch] text-[13px] text-ink-2">
        <p>No project default is configured. A service can still define its own release gate policy; services without either policy return <span class="font-mono">NOT_CONFIGURED</span>.</p>
        <p class="mt-2 text-ink-3">Project changes revoke only active overrides that inherited this project policy. Explicit service policies and their overrides remain unchanged.</p>
      </div>

      <div v-else-if="policy && !editing" class="flex flex-col gap-4" data-testid="project-gate-readonly">
        <div class="grid grid-cols-2 gap-4 max-[760px]:grid-cols-1">
          <div><span class="text-[11px] font-medium uppercase tracking-wide text-ink-3">Window evaluation</span><p class="mt-1 text-[13px]">{{ (policy.window_mode ?? "one") === "all" ? "Worst of all configured windows" : "One window" }}</p></div>
          <div v-if="(policy.window_mode ?? 'one') === 'one'"><span class="text-[11px] font-medium uppercase tracking-wide text-ink-3">SLO window</span><p class="mt-1 font-mono text-[13px]">{{ policy.window }}</p></div>
          <div><span class="text-[11px] font-medium uppercase tracking-wide text-ink-3">Unavailable facts</span><p class="mt-1 text-[13px]">{{ UNKNOWN_BEHAVIOR_LABELS[policy.unknown_behavior] }}</p></div>
        </div>
        <div class="overflow-hidden rounded border border-border">
          <div v-for="clause in GATE_CLAUSES" :key="clause" class="flex flex-wrap items-center gap-2 border-b border-border px-3 py-2.5 last:border-b-0">
            <span class="min-w-[210px] font-mono text-[12.5px]">{{ clause }}</span>
            <span v-if="clause === 'budget_consumed'" class="font-mono text-[12px] text-ink-2">≥ {{ policy.budget_consumed_percent }}% burned</span>
            <span class="flex-1"></span>
            <span v-for="assignment in ASSIGNMENTS" :key="assignment" :class="[chipBase, assignmentClass(assignment, policy.clauses[clause] === assignment)]">{{ assignment }}</span>
            <span class="basis-full text-[12px] text-ink-3">{{ CLAUSE_DESCRIPTIONS[clause] }}</span>
          </div>
        </div>
        <div class="grid grid-cols-2 gap-4 max-[760px]:grid-cols-1 text-[12.5px]">
          <p><span class="text-ink-3">Max seal lag:</span> <span class="font-mono">{{ durationShort(policy.max_seal_lag_seconds) }}</span></p>
          <p><span class="text-ink-3">Revision:</span> <span class="font-mono">{{ policy.revision }} · {{ sealedLabel(policy.updated_at) }} · {{ policy.updated_by }}</span></p>
        </div>
        <div v-if="canManage" class="flex gap-2 border-t border-border pt-4">
          <button type="button" class="h-[32px] rounded-sm border border-down px-3 text-[12.5px] text-down hover:bg-down-weak" :disabled="deleting || conflict" @click="remove">{{ deleting ? "Deleting…" : "Delete project policy" }}</button>
        </div>
      </div>

      <form v-else class="flex flex-col gap-4" novalidate @submit.prevent="save">
        <div class="flex flex-wrap items-center gap-2"><span :class="chipPlain">schema_version {{ policy?.schema_version ?? GATE_SCHEMA_VERSION }}</span><span :class="chipPlain">{{ policy ? `revision ${policy.revision}` : "new policy" }}</span></div>
        <div class="flex flex-col gap-1 text-[12.5px] font-medium text-ink-2">
          <span>Window evaluation</span>
          <div class="flex flex-wrap gap-2" data-testid="project-gate-window-mode">
            <button type="button" class="h-[32px] rounded-sm border px-3 text-[12.5px]" :class="draft.window_mode === 'one' ? 'border-accent bg-accent text-accent-ink' : 'border-border bg-surface'" :disabled="saving" data-testid="project-gate-window-mode-one" @click="draft.window_mode = 'one'">One window</button>
            <button type="button" class="h-[32px] rounded-sm border px-3 text-[12.5px]" :class="draft.window_mode === 'all' ? 'border-accent bg-accent text-accent-ink' : 'border-border bg-surface'" :disabled="saving" data-testid="project-gate-window-mode-all" @click="draft.window_mode = 'all'">Worst of all configured windows</button>
          </div>
          <span class="text-[12px] font-normal text-ink-3">Each inheriting service resolves its own configured target inventory inside the decision snapshot.</span>
        </div>
        <div class="grid grid-cols-2 gap-4 max-[760px]:grid-cols-1">
          <label v-if="draft.window_mode === 'one'" class="flex flex-col gap-1 text-[12.5px] font-medium text-ink-2">SLO window
            <select v-model="draft.window" class="h-[34px] rounded-sm border border-border bg-surface px-2 font-mono text-[13px]" :class="draftErrors.window ? 'border-down' : ''" :disabled="saving" data-testid="project-gate-window"><option v-for="window in GATE_WINDOWS" :key="window" :value="window">{{ window }}</option></select>
            <span v-if="draftErrors.window" class="text-[12px] text-down">{{ draftErrors.window }}</span>
          </label>
          <label class="flex flex-col gap-1 text-[12.5px] font-medium text-ink-2">When a fact is unavailable
            <select v-model="draft.unknown_behavior" class="h-[34px] rounded-sm border border-border bg-surface px-2 text-[13px]" :disabled="saving"><option value="warn">warn — proceed, say why</option><option value="block">block — hold the release</option></select>
          </label>
        </div>
        <div class="overflow-hidden rounded border border-border">
          <div v-for="clause in GATE_CLAUSES" :key="clause" class="grid grid-cols-[minmax(0,1fr)_auto] gap-3 border-b border-border px-3 py-3 last:border-b-0 max-[600px]:grid-cols-1">
            <div><p class="font-mono text-[12.5px]">{{ clause }}</p><p class="mt-0.5 text-[12px] text-ink-3">{{ CLAUSE_DESCRIPTIONS[clause] }}</p></div>
            <div class="flex items-start gap-1"><label v-for="assignment in ASSIGNMENTS" :key="assignment" class="cursor-pointer rounded px-2 py-1 text-[12px]" :class="assignmentClass(assignment, draft.clauses[clause] === assignment)"><input v-model="draft.clauses[clause]" class="sr-only" type="radio" :name="`project-gate-${clause}`" :value="assignment" :disabled="saving"><span>{{ assignment }}</span></label></div>
          </div>
        </div>
        <div class="grid grid-cols-2 gap-4 max-[760px]:grid-cols-1">
          <label class="flex flex-col gap-1 text-[12.5px] font-medium text-ink-2">Budget consumed threshold, %<input v-model="draft.threshold" type="number" min="1" max="100" class="h-[34px] rounded-sm border border-border bg-surface px-2 font-mono text-[13px]" :class="draftErrors.threshold ? 'border-down' : ''" :disabled="saving"><span v-if="draftErrors.threshold" class="text-[12px] text-down">{{ draftErrors.threshold }}</span></label>
          <label class="flex flex-col gap-1 text-[12.5px] font-medium text-ink-2">Maximum seal lag, minutes<input v-model="draft.sealLagMinutes" type="number" min="1" class="h-[34px] rounded-sm border border-border bg-surface px-2 font-mono text-[13px]" :class="draftErrors['seal-lag'] ? 'border-down' : ''" :disabled="saving"><span v-if="draftErrors['seal-lag']" class="text-[12px] text-down">{{ draftErrors['seal-lag'] }}</span></label>
        </div>
        <div class="flex gap-2"><button type="submit" class="h-[34px] rounded-sm bg-accent px-4 text-[12.5px] font-medium text-white disabled:opacity-50" :disabled="!canSave" data-testid="project-gate-save">{{ saving ? "Saving…" : "Save policy" }}</button><button type="button" class="h-[34px] rounded-sm border border-border px-4 text-[12.5px]" :disabled="saving" @click="discard">Cancel</button></div>
      </form>
    </div>
  </section>
</template>
