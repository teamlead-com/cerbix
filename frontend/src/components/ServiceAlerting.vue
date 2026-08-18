<script setup lang="ts">
// FR-021 phase 5 (§16.6a/§16.1), against the APPROVED mock (docs/design/mock-alerting-ownership.html,
// "Paging ownership" and "What this service pages for").
//
// Two things this component must not do, both of which would make it a liar:
//
//   * it must never present the DECLARATION as coverage. `owns_paging: true` says what an operator
//     asked for; whether members' alerts are actually being replaced is a separate answer with its
//     own reason, and the server owns it. The badge renders `alerting_state`, never the switch.
//   * it must never send a field the operator did not touch. The PATCH carries exactly the edited
//     keys, because a body that restates everything would silently overwrite a change somebody else
//     made between load and save — the server merges under its own lock, and this form is what tells
//     it what to merge.
//
// Transport discipline follows ServiceDependencies.vue: every request is caught at its own boundary,
// and every async assignment is gated on a load generation captured before the first await.
import { computed, onBeforeUnmount, onMounted, ref, watch } from "vue";

import { api } from "@/api/client";
import type { components } from "@/api/schema";

type Alerting = components["schemas"]["ServiceAlerting"];
type AlertingState = components["schemas"]["ServiceAlertingState"];
type SignalState = components["schemas"]["ServiceSignalState"];

const props = defineProps<{
  projectId: string;
  serviceId: string;
  canWrite: boolean;
  /** The declaration as the server holds it, from the service detail. */
  alerting?: Alerting | null;
  /** First paint only: the detail already carried it, so the panel does not blink on mount. */
  state?: AlertingState | null;
  /** The file provider owning this service, or "" when the UI owns it. */
  managedBy?: string;
}>();

const emit = defineEmits<{ (e: "saved", value: Alerting): void }>();

const saving = ref(false);
const error = ref("");
const draft = ref<Alerting | null>(null);
let generation = 0;

// Coverage is the SERVER's answer and it expires: a lease runs out, an evaluation fails, a PATCH
// bumps the generation and dis-arms the service on the spot. A badge rendered once at page load
// would keep saying ARMED while delivery had already stopped suppressing anything — the precise
// disagreement §16 exists to prevent — so it is re-read on a cadence and immediately after every
// successful save.
const REFRESH_MS = 15_000;
const liveState = ref<AlertingState | null>(props.state ?? null);
let stateGeneration = 0;
let timer: ReturnType<typeof setInterval> | undefined;

async function refreshState() {
  const mine = ++stateGeneration;
  try {
    const res = (await api.GET("/api/v1/projects/{projectID}/services/{serviceID}/alerting/state", {
      params: { path: { projectID: props.projectId, serviceID: props.serviceId } },
    })) as { data?: AlertingState };
    // A stale response from a service the operator has already navigated away from must never land.
    if (mine !== stateGeneration) return;
    if (res.data) liveState.value = res.data;
  } catch {
    // A failed refresh keeps the last SERVER answer rather than inventing one: "we could not ask"
    // is not "not armed", and neither is it "armed".
  }
}

onMounted(() => {
  void refreshState();
  timer = setInterval(() => void refreshState(), REFRESH_MS);
});
onBeforeUnmount(() => {
  stateGeneration++;
  if (timer) clearInterval(timer);
});
watch(
  () => [props.projectId, props.serviceId],
  () => {
    liveState.value = null;
    void refreshState();
  },
);

/** The file owns these fields (§16.6a); the UI renders them and refuses to send. */
const readOnly = computed(() => !props.canWrite || !!props.managedBy);

watch(
  () => props.alerting,
  (value) => {
    draft.value = value ? { ...value, page_on: [...value.page_on] } : null;
    error.value = "";
  },
  { immediate: true },
);

/** Why coverage is not armed, in the operator's words rather than the enum's. */
const REASONS: Record<string, string> = {
  not_owned: "this service does not own paging",
  policy_pages_nothing: "the policy pages for no state",
  never_evaluated: "no evaluation yet",
  generation_changed: "the configuration changed — re-arming on the next evaluation",
  revision_changed: "the definition changed — re-arming on the next evaluation",
  evaluation_error: "the last evaluation failed",
  stale_lease: "no recent evaluation",
  no_enabled_target: "no enabled burn target",
  held: "a window cannot be quoted, so no rule can fire",
  rule_unevaluated: "a declared rule has no verdict yet",
  unroutable: "nothing to notify — no schedule and no enabled channel",
};

// The approved grammar: `armed` borrows the UP hue, `pending` the PENDING hue with a dashed ring
// (the phase-4 dormant grammar), `degraded` the DEGRADED hue. Alerting introduces no colour of its
// own, because who is paged is not a new state of a thing.
function badge(signal?: SignalState | null): { label: string; tone: string; detail: string } {
  if (!signal) return { label: "unknown", tone: "pending", detail: "coverage could not be read" };
  if (signal.armed) return { label: "armed", tone: "up", detail: "members' alerts are delegated" };
  // NOT armed is the safe state, not an error state: the members are paging for themselves. The two
  // labels separate "it will arm by itself" from "somebody has to do something about it".
  const transient = ["never_evaluated", "generation_changed", "revision_changed", "stale_lease", "held", "rule_unevaluated"];
  const pending = transient.includes(signal.reason ?? "");
  return {
    label: pending ? "pending" : "degraded",
    tone: pending ? "pending" : "degraded",
    detail: REASONS[signal.reason ?? ""] ?? signal.reason ?? "not armed",
  };
}

const live = computed(() => badge(liveState.value?.live));
const burn = computed(() => badge(liveState.value?.burn));
const liveSignal = computed(() => liveState.value?.live ?? null);
const burnSignal = computed(() => liveState.value?.burn ?? null);

function togglePageOn(value: "down" | "degraded") {
  if (!draft.value) return;
  const set = new Set(draft.value.page_on);
  if (set.has(value)) set.delete(value);
  else set.add(value);
  draft.value.page_on = [...set].sort();
}

/** Only what actually changed travels, so a concurrent edit to another field survives. */
function changedFields(): Record<string, unknown> {
  const before = props.alerting;
  const now = draft.value;
  if (!before || !now) return {};
  const out: Record<string, unknown> = {};
  if (before.owns_paging !== now.owns_paging) out.owns_paging = now.owns_paging;
  if (before.page_on_unknown !== now.page_on_unknown) out.page_on_unknown = now.page_on_unknown;
  if (before.confirm_evaluations !== now.confirm_evaluations) out.confirm_evaluations = now.confirm_evaluations;
  if (before.page_on.join(",") !== now.page_on.join(",")) out.page_on = now.page_on;
  return out;
}

const dirty = computed(() => Object.keys(changedFields()).length > 0);

async function save() {
  if (!draft.value || readOnly.value || !dirty.value) return;
  const mine = ++generation;
  saving.value = true;
  error.value = "";
  try {
    const res = (await api.PATCH("/api/v1/projects/{projectID}/services/{serviceID}/alerting", {
      params: { path: { projectID: props.projectId, serviceID: props.serviceId } },
      body: changedFields(),
    })) as { data?: Alerting; error?: { error?: string } };
    if (mine !== generation) return;
    if (res.error || !res.data) {
      // Every refusal renders as ITSELF, from the payload's own message: a file-managed service, an
      // out-of-range confirmation and an unknown state are three different answers.
      error.value = res.error?.error ?? "could not save";
      return;
    }
    emit("saved", res.data);
    // A paging edit bumps the configuration generation, which dis-arms the service until the next
    // evaluation. Re-reading here is what stops the panel from showing the pre-save green badge
    // beside a declaration that has just dis-armed everything.
    await refreshState();
  } catch {
    if (mine === generation) error.value = "could not reach the server";
  } finally {
    if (mine === generation) saving.value = false;
  }
}
</script>

<template>
  <section class="mb-4 overflow-hidden rounded border border-border bg-surface shadow-card" data-testid="service-alerting">
    <header class="flex flex-wrap items-center gap-2 border-b border-border px-4 py-[10px]">
      <h2 class="text-[13.5px] font-semibold">Paging ownership</h2>
      <span
        v-for="sig in [{ key: 'live', b: live }, { key: 'burn', b: burn }]"
        :key="sig.key"
        class="rounded px-1.5 py-0.5 font-mono text-[11px]"
        :class="{
          'bg-up/10 text-up': sig.b.tone === 'up',
          'border border-dashed border-pending bg-pending/10 text-pending': sig.b.tone === 'pending',
          'bg-degraded/10 text-degraded': sig.b.tone === 'degraded',
        }"
        :data-testid="`alerting-badge-${sig.key}`"
        :title="sig.b.detail"
      >{{ sig.key }}: {{ sig.b.label }}</span>
      <span v-if="managedBy" class="ml-auto rounded bg-ink-3/10 px-1.5 py-0.5 font-mono text-[11px] text-ink-3">
        declared in {{ managedBy }}
      </span>
    </header>

    <div v-if="!alerting" class="px-4 py-3 text-[12.5px] text-ink-3" data-testid="alerting-unavailable">
      The paging declaration could not be read.
    </div>

    <div v-else class="space-y-3 px-4 py-3 text-[12.5px]">
      <!-- Coverage, PER SIGNAL and always with its reason. One armed signal must never hide the
           other's explanation: a green ownership toggle beside a dis-armed burn signal is the exact
           confusion these badges exist to remove. -->
      <ul class="space-y-1" data-testid="alerting-coverage">
        <li
          v-for="sig in [
            { key: 'live', b: live, s: liveSignal, what: 'Live transitions' },
            { key: 'burn', b: burn, s: burnSignal, what: 'Sealed burn' },
          ]"
          :key="sig.key"
          class="flex flex-wrap items-baseline gap-x-1.5"
          :data-testid="`alerting-signal-${sig.key}`"
        >
          <span class="text-ink-2">{{ sig.what }}:</span>
          <span
            :class="{
              'text-up': sig.b.tone === 'up',
              'text-pending': sig.b.tone === 'pending',
              'text-degraded': sig.b.tone === 'degraded',
            }"
          >{{ sig.b.label }}</span>
          <span class="text-ink-3">— {{ sig.b.detail }}</span>
          <span
            v-if="sig.s?.last_error"
            class="font-mono text-[11.5px] text-bad"
            :data-testid="`alerting-error-${sig.key}`"
          >{{ sig.s.last_error }}</span>
        </li>
      </ul>
      <p
        v-if="draft?.owns_paging && burn.label !== 'armed' && burnSignal"
        class="rounded border border-dashed border-pending bg-pending/5 px-2 py-1 text-[12px] text-pending"
        data-testid="alerting-burn-warning"
      >
        This service owns paging, but its burn signal is not armed: {{ burn.detail }}. Members keep
        alerting on their own budgets until it is.
      </p>

      <label class="flex items-center gap-2">
        <input
          type="checkbox"
          :checked="draft?.owns_paging"
          :disabled="readOnly || saving"
          data-testid="alerting-owns-paging"
          @change="draft && (draft.owns_paging = ($event.target as HTMLInputElement).checked)"
        />
        <span>This service owns paging for its declared inputs</span>
      </label>

      <div class="flex flex-wrap items-center gap-2">
        <span class="text-ink-3">Pages for</span>
        <button
          v-for="s in (['down', 'degraded'] as const)"
          :key="s"
          type="button"
          class="rounded border px-1.5 py-0.5 font-mono text-[11px]"
          :class="draft?.page_on.includes(s) ? 'border-ok bg-ok/10 text-ok' : 'border-border text-ink-3'"
          :disabled="readOnly || saving"
          :data-testid="`alerting-page-on-${s}`"
          @click="togglePageOn(s)"
        >{{ s }}</button>
        <label class="ml-2 flex items-center gap-1.5">
          <input
            type="checkbox"
            :checked="draft?.page_on_unknown"
            :disabled="readOnly || saving"
            data-testid="alerting-page-on-unknown"
            @change="draft && (draft.page_on_unknown = ($event.target as HTMLInputElement).checked)"
          />
          <span>and when it cannot be seen (<span class="font-mono">unknown</span>)</span>
        </label>
      </div>
      <p v-if="draft && draft.page_on.length === 0 && !draft.page_on_unknown" class="text-[11.5px] text-warn" data-testid="alerting-pages-nothing">
        This declaration pages for no state at all, which leaves members alerting for themselves.
      </p>

      <label class="flex items-center gap-2">
        <span class="text-ink-3">Confirm over</span>
        <input
          type="number"
          min="1"
          max="10"
          class="w-16 rounded border border-border bg-surface px-1.5 py-0.5 font-mono text-[12px]"
          :value="draft?.confirm_evaluations"
          :disabled="readOnly || saving"
          data-testid="alerting-confirm"
          @input="draft && (draft.confirm_evaluations = Number(($event.target as HTMLInputElement).value))"
        />
        <span class="text-ink-3">consecutive evaluations</span>
      </label>

      <p v-if="error" class="text-[12px] text-bad" data-testid="alerting-error">{{ error }}</p>

      <div v-if="!readOnly" class="flex items-center gap-2 pt-1">
        <button
          type="button"
          class="rounded bg-accent px-2 py-1 text-[12px] text-white disabled:opacity-50"
          :disabled="!dirty || saving"
          data-testid="alerting-save"
          @click="save"
        >{{ saving ? "Saving…" : "Save" }}</button>
        <span v-if="dirty" class="text-[11.5px] text-ink-3">Only the fields you changed are sent.</span>
      </div>
      <p v-else-if="managedBy" class="text-[11.5px] text-ink-3" data-testid="alerting-read-only">
        These fields are part of the file's desired state, so they are edited there.
      </p>
    </div>
  </section>
</template>
