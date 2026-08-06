<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";
import { api } from "@/api/client";
import type { components } from "@/api/schema";
import AppShell from "@/components/AppShell.vue";
import { useSession } from "@/stores/session";
import { useWorkspace } from "@/stores/workspace";

type Channel = components["schemas"]["NotificationChannel"];
type Policy = components["schemas"]["EscalationPolicy"];
type Schedule = components["schemas"]["OnCallSchedule"];
type Target = components["schemas"]["EscalationTarget"];

const ws = useWorkspace();
const session = useSession();
const loading = ref(true);
const error = ref("");

const channels = ref<Channel[]>([]);
const policies = ref<Policy[]>([]);
const schedules = ref<Schedule[]>([]);

const canWrite = computed(() => session.canProjectWrite(ws.orgId, ws.projectId));
const channelName = (id: string) => channels.value.find((c) => c.id === id)?.name ?? id;
const scheduleName = (id: string) => schedules.value.find((s) => s.id === id)?.name ?? id;
const targetLabel = (t: Target) =>
  t.type === "schedule" ? `on-call: ${scheduleName(t.id)}` : channelName(t.id);

async function loadAll() {
  loading.value = true;
  if (ws.projectId) {
    const [ch, pol, sch, mons] = await Promise.all([
      api.GET("/api/v1/projects/{projectID}/notification-channels", { params: { path: { projectID: ws.projectId } } }),
      api.GET("/api/v1/projects/{projectID}/escalation-policies", { params: { path: { projectID: ws.projectId } } }),
      api.GET("/api/v1/projects/{projectID}/oncall-schedules", { params: { path: { projectID: ws.projectId } } }),
      api.GET("/api/v1/projects/{projectID}/monitors", { params: { path: { projectID: ws.projectId } } }),
    ]);
    channels.value = ch.data ?? [];
    policies.value = pol.data ?? [];
    schedules.value = sch.data ?? [];
    projectMonitors.value = mons.data ?? [];
    await loadScheduleExtras();
  }
  loading.value = false;
}

// Overrides + current on-call per schedule (vacation cover).
type Override = components["schemas"]["OnCallOverride"];
const overrides = ref<Record<string, Override[]>>({});
const onCallNow = ref<Record<string, string>>({});
const ovForm = reactive<Record<string, { channel_id: string; starts_at: string; ends_at: string }>>({});
function ovDraft(id: string) {
  if (!ovForm[id]) ovForm[id] = { channel_id: "", starts_at: "", ends_at: "" };
  return ovForm[id];
}
async function loadScheduleExtras() {
  const ov: Record<string, Override[]> = {};
  const cur: Record<string, string> = {};
  await Promise.all(
    schedules.value.map(async (s) => {
      const id = s.id ?? "";
      if (!id) return;
      const [o, c] = await Promise.all([
        api.GET("/api/v1/oncall-schedules/{scheduleID}/overrides", { params: { path: { scheduleID: id } } }),
        api.GET("/api/v1/oncall-schedules/{scheduleID}/current", { params: { path: { scheduleID: id } } }),
      ]);
      ov[id] = o.data ?? [];
      cur[id] = c.data?.channel_id ?? "";
    }),
  );
  overrides.value = ov;
  onCallNow.value = cur;
}
async function addOverride(id: string) {
  const f = ovDraft(id);
  if (!f.channel_id || !f.starts_at || !f.ends_at) return;
  await api.POST("/api/v1/oncall-schedules/{scheduleID}/overrides", {
    params: { path: { scheduleID: id } },
    body: { channel_id: f.channel_id, starts_at: new Date(f.starts_at).toISOString(), ends_at: new Date(f.ends_at).toISOString() },
  });
  ovForm[id] = { channel_id: "", starts_at: "", ends_at: "" };
  await loadAll();
}
async function deleteOverride(oid: string) {
  await api.DELETE("/api/v1/oncall-overrides/{overrideID}", { params: { path: { overrideID: oid } } });
  await loadAll();
}
const fmtRange = (a?: string, b?: string) =>
  `${a ? new Date(a).toLocaleString() : "?"} → ${b ? new Date(b).toLocaleString() : "?"}`;

// --- Policy composer ---
type DraftStep = { after_seconds: number; targets: Target[] };
const policyForm = reactive<{ name: string; repeat_last: boolean; steps: DraftStep[] }>({
  name: "",
  repeat_last: false,
  steps: [{ after_seconds: 0, targets: [] }],
});
const savingPolicy = ref(false);
const policyError = ref("");

function addStep() {
  const last = policyForm.steps[policyForm.steps.length - 1];
  policyForm.steps.push({ after_seconds: (last?.after_seconds ?? 0) + 600, targets: [] });
}
function removeStep(i: number) {
  policyForm.steps.splice(i, 1);
  if (policyForm.steps.length === 0) policyForm.steps.push({ after_seconds: 0, targets: [] });
}
function addTarget(step: DraftStep) {
  // Default to the first available channel, else the first schedule, else unselected.
  if (channels.value[0]?.id) step.targets.push({ type: "channel", id: channels.value[0].id });
  else if (schedules.value[0]?.id) step.targets.push({ type: "schedule", id: schedules.value[0].id });
  else step.targets.push({ type: "channel", id: "" });
}
function removeTarget(step: DraftStep, i: number) {
  step.targets.splice(i, 1);
}
// A target is edited as "channel:<id>" / "schedule:<id>" so one <select> covers both.
// An unselected target maps to "" so the disabled placeholder option shows.
function targetValue(t: Target) {
  return t.id ? `${t.type}:${t.id}` : "";
}
function setTargetValue(t: Target, v: string) {
  const [type, id] = v.split(/:(.*)/s);
  t.type = type as Target["type"];
  t.id = id;
}

const projectMonitors = ref<components["schemas"]["Monitor"][]>([]);
const monitorsUsingPolicy = (id?: string) => projectMonitors.value.filter((m) => m.escalation_policy_id === id).length;

// Edit mode: the same composer, prefilled; save switches POST → PUT.
const editingPolicyId = ref("");
const editingPolicyName = ref("");
function startEditPolicy(p: Policy) {
  editingPolicyId.value = p.id ?? "";
  editingPolicyName.value = p.name ?? "";
  policyForm.name = p.name ?? "";
  policyForm.repeat_last = !!p.repeat_last;
  policyForm.steps = (p.steps ?? []).map((st) => ({
    after_seconds: st.after_seconds ?? 0,
    targets: (st.targets ?? []).map((t) => ({ type: t.type as Target["type"], id: t.id ?? "" })),
  }));
  if (!policyForm.steps.length) policyForm.steps = [{ after_seconds: 0, targets: [] }];
  policyError.value = "";
}
function resetPolicyForm() {
  editingPolicyId.value = "";
  editingPolicyName.value = "";
  policyForm.name = "";
  policyForm.repeat_last = false;
  policyForm.steps = [{ after_seconds: 0, targets: [] }];
  policyError.value = "";
}

async function createPolicy() {
  policyError.value = "";
  if (!policyForm.name.trim()) {
    policyError.value = "Name is required.";
    return;
  }
  if (policyForm.steps.some((s) => s.targets.length === 0 || s.targets.some((t) => !t.id))) {
    policyError.value = "Every step needs at least one target.";
    return;
  }
  savingPolicy.value = true;
  const body = { name: policyForm.name.trim(), repeat_last: policyForm.repeat_last, steps: policyForm.steps };
  const res = editingPolicyId.value
    ? await api.PUT("/api/v1/escalation-policies/{policyID}", { params: { path: { policyID: editingPolicyId.value } }, body })
    : await api.POST("/api/v1/projects/{projectID}/escalation-policies", { params: { path: { projectID: ws.projectId } }, body });
  savingPolicy.value = false;
  if (res.error) {
    policyError.value = (res.error as { error?: string })?.error || "Could not save the policy.";
    return;
  }
  resetPolicyForm();
  await loadAll();
}

const confirmDeletePolicyId = ref("");
async function deletePolicy(id: string) {
  await api.DELETE("/api/v1/escalation-policies/{policyID}", { params: { path: { policyID: id } } });
  confirmDeletePolicyId.value = "";
  if (editingPolicyId.value === id) resetPolicyForm();
  await loadAll();
}

// --- Schedule composer ---
const shiftPresets = [
  { label: "Daily", seconds: 86400 },
  { label: "Weekly", seconds: 604800 },
  { label: "Bi-weekly", seconds: 1209600 },
];
const scheduleForm = reactive<{ name: string; shift_seconds: number; anchor_at: string; participants: string[] }>({
  name: "",
  shift_seconds: 604800,
  anchor_at: "",
  participants: [],
});
const savingSchedule = ref(false);
const scheduleError = ref("");
function toggleParticipant(id: string) {
  const i = scheduleForm.participants.indexOf(id);
  if (i >= 0) scheduleForm.participants.splice(i, 1);
  else scheduleForm.participants.push(id);
}
const editingScheduleId = ref("");
const editingScheduleName = ref("");
function startEditSchedule(sc: Schedule) {
  editingScheduleId.value = sc.id ?? "";
  editingScheduleName.value = sc.name ?? "";
  scheduleForm.name = sc.name ?? "";
  scheduleForm.shift_seconds = sc.shift_seconds ?? 604800;
  scheduleForm.anchor_at = sc.anchor_at ? sc.anchor_at.slice(0, 16) : "";
  scheduleForm.participants = [...(sc.participants ?? [])];
  scheduleError.value = "";
}
function resetScheduleForm() {
  editingScheduleId.value = "";
  editingScheduleName.value = "";
  scheduleForm.name = "";
  scheduleForm.participants = [];
  scheduleForm.anchor_at = "";
  scheduleError.value = "";
}

async function createSchedule() {
  scheduleError.value = "";
  if (!scheduleForm.name.trim() || scheduleForm.participants.length === 0) {
    scheduleError.value = "Name and at least one participant are required.";
    return;
  }
  const anchor = scheduleForm.anchor_at ? new Date(scheduleForm.anchor_at).toISOString() : new Date().toISOString();
  savingSchedule.value = true;
  const body = {
    name: scheduleForm.name.trim(),
    shift_seconds: scheduleForm.shift_seconds,
    anchor_at: anchor,
    participants: scheduleForm.participants,
  };
  const res = editingScheduleId.value
    ? await api.PUT("/api/v1/oncall-schedules/{scheduleID}", { params: { path: { scheduleID: editingScheduleId.value } }, body })
    : await api.POST("/api/v1/projects/{projectID}/oncall-schedules", { params: { path: { projectID: ws.projectId } }, body });
  savingSchedule.value = false;
  if (res.error) {
    scheduleError.value = (res.error as { error?: string })?.error || "Could not save the schedule.";
    return;
  }
  resetScheduleForm();
  await loadAll();
}
const confirmDeleteScheduleId = ref("");
async function deleteSchedule(id: string) {
  await api.DELETE("/api/v1/oncall-schedules/{scheduleID}", { params: { path: { scheduleID: id } } });
  confirmDeleteScheduleId.value = "";
  if (editingScheduleId.value === id) resetScheduleForm();
  await loadAll();
}

const shiftLabel = (s: number) => shiftPresets.find((p) => p.seconds === s)?.label ?? `${Math.round(s / 86400)}d`;
const fmtOffset = (s: number) => (s === 0 ? "immediately" : s % 3600 === 0 ? `${s / 3600}h` : `${Math.round(s / 60)}m`);

onMounted(async () => {
  await ws.init();
  await loadAll();
});

const inputCls =
  "h-[36px] w-full rounded-sm border border-border bg-surface-2 px-[11px] text-[13.5px] text-ink outline-none focus:border-accent focus:bg-surface";
const selectCls =
  "h-[34px] rounded-sm border border-border bg-surface-2 px-2 text-[13px] text-ink outline-none focus:border-accent";
</script>

<template>
  <AppShell active="escalation" :crumbs="['escalation']">
    <div class="mx-auto max-w-[980px] px-[22px] pb-16 pt-6">
      <div class="mb-5">
        <h1 class="text-[22px] font-semibold tracking-tight">On-call &amp; escalation</h1>
        <p class="mt-[3px] text-[13px] text-ink-3">
          Route a monitor's down alerts through an ordered ladder instead of notifying every channel at once. Acknowledge an incident to stop escalation.
        </p>
      </div>

      <div v-if="loading" class="text-[13px] text-ink-3">Loading…</div>
      <div v-else class="grid gap-5 lg:grid-cols-2">
        <!-- Escalation policies -->
        <section class="rounded border border-border bg-surface shadow-card">
          <div class="border-b border-border px-4 py-[13px] text-[13px] font-semibold">Escalation policies</div>
          <div class="flex flex-col divide-y divide-border">
            <div v-for="p in policies" :key="p.id" class="px-4 py-3">
              <div class="flex items-center gap-2">
                <b class="text-[13.5px]">{{ p.name }}</b>
                <span v-if="p.repeat_last" class="rounded-xs border border-border px-[6px] py-px text-[10.5px] text-ink-3">repeats last</span>
                <template v-if="canWrite">
                  <button class="ml-auto text-[12px] text-ink-3 hover:text-accent" @click="startEditPolicy(p)">Edit</button>
                  <button class="text-[12px] text-ink-3 hover:text-down" @click="confirmDeletePolicyId = p.id ?? ''">Delete</button>
                </template>
              </div>
              <div v-if="confirmDeletePolicyId === p.id" class="mt-2 rounded-sm bg-down-weak px-3 py-2 text-[12.5px] text-down">
                Delete "{{ p.name }}"?
                <template v-if="monitorsUsingPolicy(p.id)"><b>{{ monitorsUsingPolicy(p.id) }} monitor(s)</b> use this policy — they will silently fall back to flat notifications.</template>
                <template v-else>No monitors currently use it.</template>
                <button type="button" class="ml-2 h-[24px] rounded-sm bg-down px-[8px] text-[11.5px] font-medium text-white hover:opacity-90" @click="deletePolicy(p.id ?? '')">Confirm delete</button>
                <button type="button" class="ml-1 h-[24px] rounded-sm border border-border px-[8px] text-[11.5px] text-ink-2" @click="confirmDeletePolicyId = ''">Cancel</button>
              </div>
              <ol class="mt-1 flex flex-col gap-[3px] text-[12.5px] text-ink-2">
                <li v-for="(s, i) in p.steps" :key="i" class="flex gap-2">
                  <span class="font-mono text-ink-3">{{ fmtOffset(s.after_seconds ?? 0) }}</span>
                  <span>→ {{ (s.targets ?? []).map(targetLabel).join(", ") }}</span>
                </li>
              </ol>
            </div>
            <p v-if="!policies.length" class="px-4 py-3 text-[12.5px] text-ink-3">No policies yet.</p>
          </div>

          <!-- create policy -->
          <form v-if="canWrite" class="flex flex-col gap-3 border-t border-border bg-surface-2/40 p-4" @submit.prevent="createPolicy">
            <div class="flex items-center gap-2 text-[12px] font-semibold uppercase tracking-[0.05em] text-ink-3">
              {{ editingPolicyId ? "Edit policy: " + editingPolicyName : "New policy" }}
              <span v-if="editingPolicyId" class="rounded-full bg-accent-weak px-[8px] py-px text-[10px] font-bold text-accent">editing</span>
            </div>
            <div v-if="!channels.length && !schedules.length" class="rounded-sm border border-degraded/40 bg-degraded-weak px-3 py-2 text-[12.5px] text-ink-2">
              No notification channels yet — a step's target is a channel or an on-call schedule, and schedules are built from channels too. Add channels in
              <RouterLink :to="{ name: 'settings' }" class="text-accent hover:underline">Settings</RouterLink> first.
            </div>
            <input v-model="policyForm.name" :class="inputCls" placeholder="Policy name (e.g. payments on-call)" />
            <div v-for="(step, si) in policyForm.steps" :key="si" class="rounded-sm border border-border bg-surface p-[10px]">
              <div class="flex items-center gap-2 text-[12px]">
                <span class="text-ink-3">After</span>
                <input v-model.number="step.after_seconds" type="number" min="0" class="h-[30px] w-[90px] rounded-sm border border-border bg-surface-2 px-2 text-[13px]" />
                <span class="text-ink-3">seconds</span>
                <button type="button" class="ml-auto text-[11.5px] text-ink-3 hover:text-down" @click="removeStep(si)">remove step</button>
              </div>
              <div class="mt-2 flex flex-col gap-[6px]">
                <div v-for="(t, ti) in step.targets" :key="ti" class="flex items-center gap-2">
                  <select :value="targetValue(t)" :class="selectCls" class="flex-1" @change="setTargetValue(t, ($event.target as HTMLSelectElement).value)">
                    <option value="" disabled>Select a channel or schedule…</option>
                    <optgroup v-if="channels.length" label="Channels">
                      <option v-for="c in channels" :key="c.id" :value="`channel:${c.id}`">{{ c.name }}</option>
                    </optgroup>
                    <optgroup v-if="schedules.length" label="On-call schedules">
                      <option v-for="s in schedules" :key="s.id" :value="`schedule:${s.id}`">on-call: {{ s.name }}</option>
                    </optgroup>
                  </select>
                  <button type="button" class="text-[11.5px] text-ink-3 hover:text-down" @click="removeTarget(step, ti)">×</button>
                </div>
                <button type="button" class="self-start text-[12px] text-accent hover:underline" @click="addTarget(step)">+ target</button>
              </div>
            </div>
            <div class="flex items-center gap-3">
              <button type="button" class="text-[12.5px] text-accent hover:underline" @click="addStep">+ step</button>
              <label class="ml-auto flex items-center gap-2 text-[12.5px] text-ink-2">
                <input v-model="policyForm.repeat_last" type="checkbox" /> repeat last step (renotify)
              </label>
            </div>
            <p v-if="policyError" class="text-[12.5px] text-down">{{ policyError }}</p>
            <div class="flex items-center gap-2">
              <button type="submit" :disabled="savingPolicy" class="h-[34px] rounded-sm bg-accent px-4 text-[13px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-50">
                {{ savingPolicy ? "Saving…" : editingPolicyId ? "Save changes" : "Create policy" }}
              </button>
              <button v-if="editingPolicyId" type="button" class="h-[34px] rounded-sm border border-border px-3 text-[13px] text-ink-2 hover:border-border-strong" @click="resetPolicyForm">Cancel</button>
            </div>
          </form>
        </section>

        <!-- On-call schedules -->
        <section class="rounded border border-border bg-surface shadow-card">
          <div class="border-b border-border px-4 py-[13px] text-[13px] font-semibold">On-call schedules</div>
          <div class="flex flex-col divide-y divide-border">
            <div v-for="s in schedules" :key="s.id" class="px-4 py-3">
              <div class="flex items-center gap-2">
                <b class="text-[13.5px]">{{ s.name }}</b>
                <span class="rounded-xs border border-border px-[6px] py-px text-[10.5px] text-ink-3">{{ shiftLabel(s.shift_seconds ?? 0) }} rotation</span>
                <template v-if="canWrite">
                  <button class="ml-auto text-[12px] text-ink-3 hover:text-accent" @click="startEditSchedule(s)">Edit</button>
                  <button class="text-[12px] text-ink-3 hover:text-down" @click="confirmDeleteScheduleId = s.id ?? ''">Delete</button>
                </template>
              </div>
              <div v-if="confirmDeleteScheduleId === s.id" class="mt-2 rounded-sm bg-down-weak px-3 py-2 text-[12.5px] text-down">
                Delete "{{ s.name }}"?
                <template v-if="(overrides[s.id ?? ''] ?? []).length"><b>{{ (overrides[s.id ?? ''] ?? []).length }} override(s)</b> (vacation cover) will be deleted with it.</template>
                Policies targeting it will skip this step.
                <button type="button" class="ml-2 h-[24px] rounded-sm bg-down px-[8px] text-[11.5px] font-medium text-white hover:opacity-90" @click="deleteSchedule(s.id ?? '')">Confirm delete</button>
                <button type="button" class="ml-1 h-[24px] rounded-sm border border-border px-[8px] text-[11.5px] text-ink-2" @click="confirmDeleteScheduleId = ''">Cancel</button>
              </div>
              <div class="mt-1 text-[12.5px] text-ink-2">{{ (s.participants ?? []).map(channelName).join(" → ") }}</div>
              <div v-if="onCallNow[s.id ?? '']" class="mt-[6px] text-[12px]">
                <span class="text-ink-3">On call now:</span>
                <span class="ml-1 rounded-xs bg-up-weak px-[6px] py-px font-medium text-up">{{ channelName(onCallNow[s.id ?? '']) }}</span>
              </div>
              <!-- overrides (vacation cover) -->
              <div v-if="(overrides[s.id ?? ''] ?? []).length" class="mt-2 flex flex-col gap-1">
                <div v-for="o in overrides[s.id ?? '']" :key="o.id" class="flex items-center gap-2 text-[12px] text-ink-2">
                  <span class="rounded-xs border border-degraded/40 bg-degraded-weak px-[6px] py-px text-degraded">cover</span>
                  <b>{{ channelName(o.channel_id ?? '') }}</b>
                  <span class="text-ink-3">{{ fmtRange(o.starts_at, o.ends_at) }}</span>
                  <button v-if="canWrite" class="ml-auto text-[11.5px] text-ink-3 hover:text-down" @click="deleteOverride(o.id ?? '')">×</button>
                </div>
              </div>
              <details v-if="canWrite && channels.length" class="mt-2 text-[12px]">
                <summary class="cursor-pointer text-accent hover:underline">+ add vacation cover</summary>
                <div class="mt-2 flex flex-wrap items-center gap-2">
                  <select v-model="ovDraft(s.id ?? '').channel_id" :class="selectCls">
                    <option value="" disabled>cover channel…</option>
                    <option v-for="c in channels" :key="c.id" :value="c.id">{{ c.name }}</option>
                  </select>
                  <input v-model="ovDraft(s.id ?? '').starts_at" type="datetime-local" class="h-[32px] rounded-sm border border-border bg-surface-2 px-2 text-[12.5px]" />
                  <span class="text-ink-3">→</span>
                  <input v-model="ovDraft(s.id ?? '').ends_at" type="datetime-local" class="h-[32px] rounded-sm border border-border bg-surface-2 px-2 text-[12.5px]" />
                  <button type="button" class="h-[32px] rounded-sm bg-accent px-3 text-[12.5px] font-medium text-accent-ink hover:bg-accent-2" @click="addOverride(s.id ?? '')">Add</button>
                </div>
              </details>
            </div>
            <p v-if="!schedules.length" class="px-4 py-3 text-[12.5px] text-ink-3">No schedules yet.</p>
          </div>

          <!-- create schedule -->
          <form v-if="canWrite" class="flex flex-col gap-3 border-t border-border bg-surface-2/40 p-4" @submit.prevent="createSchedule">
            <div class="flex items-center gap-2 text-[12px] font-semibold uppercase tracking-[0.05em] text-ink-3">
              {{ editingScheduleId ? "Edit schedule: " + editingScheduleName : "New schedule" }}
              <span v-if="editingScheduleId" class="rounded-full bg-accent-weak px-[8px] py-px text-[10px] font-bold text-accent">editing</span>
            </div>
            <input v-model="scheduleForm.name" :class="inputCls" placeholder="Schedule name (e.g. primary rotation)" />
            <div class="flex items-center gap-2 text-[12.5px]">
              <span class="text-ink-3">Rotate</span>
              <select v-model.number="scheduleForm.shift_seconds" :class="selectCls">
                <option v-for="p in shiftPresets" :key="p.seconds" :value="p.seconds">{{ p.label }}</option>
              </select>
              <span class="text-ink-3">from</span>
              <input v-model="scheduleForm.anchor_at" type="datetime-local" class="h-[34px] rounded-sm border border-border bg-surface-2 px-2 text-[13px]" />
            </div>
            <div>
              <div class="mb-1 text-[12px] text-ink-3">Rotation order (checked channels rotate in listed order):</div>
              <div class="flex flex-col gap-1">
                <label v-for="c in channels" :key="c.id" class="flex items-center gap-2 text-[13px] text-ink-2">
                  <input type="checkbox" :checked="scheduleForm.participants.includes(c.id ?? '')" @change="toggleParticipant(c.id ?? '')" />
                  {{ c.name }}
                  <span v-if="scheduleForm.participants.includes(c.id ?? '')" class="text-[11px] text-ink-3">#{{ scheduleForm.participants.indexOf(c.id ?? '') + 1 }}</span>
                </label>
                <p v-if="!channels.length" class="text-[12.5px] text-ink-3">
                  No channels in this project yet — add them in
                  <RouterLink :to="{ name: 'settings' }" class="text-accent hover:underline">Settings</RouterLink>.
                </p>
              </div>
            </div>
            <p v-if="scheduleError" class="text-[12.5px] text-down">{{ scheduleError }}</p>
            <div class="flex items-center gap-2">
              <button type="submit" :disabled="savingSchedule" class="h-[34px] rounded-sm bg-accent px-4 text-[13px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-50">
                {{ savingSchedule ? "Saving…" : editingScheduleId ? "Save changes" : "Create schedule" }}
              </button>
              <button v-if="editingScheduleId" type="button" class="h-[34px] rounded-sm border border-border px-3 text-[13px] text-ink-2 hover:border-border-strong" @click="resetScheduleForm">Cancel</button>
            </div>
          </form>
        </section>
      </div>
      <p v-if="error" class="mt-4 text-[13px] text-down">{{ error }}</p>
    </div>
  </AppShell>
</template>
