<script setup lang="ts">
import { computed, nextTick, ref } from "vue";
import { RouterLink } from "vue-router";
import { instantLabel } from "@/lib/wallclock";
import type { Monitor, OnboardingSnapshot, OnboardingStep } from "@/lib/onboarding";

const props = defineProps<{
  snapshot: OnboardingSnapshot;
  monitors: Monitor[];
  orgName: string;
  projectName: string;
}>();

const emit = defineEmits<{
  close: [];
  dismiss: [];
  retry: [];
  createOrg: [];
  createProject: [];
  chooseMonitor: [id: string];
}>();

const steps: { key: OnboardingStep; label: string }[] = [
  { key: "org", label: "Organization" },
  { key: "project", label: "Project" },
  { key: "monitor", label: "Monitor" },
  { key: "result", label: "First result" },
];

const compact = computed(() => props.snapshot.kind === "complete_existing");
const monitor = computed(() => props.snapshot.candidate);
const heartbeat = computed(() => props.snapshot.heartbeat);
const monitorType = computed(() => monitor.value?.type?.toUpperCase() || "monitor");
const region = computed(() => monitor.value?.region || "core");
const handoff = computed(() => props.snapshot.handoffStep);
const heading = ref<HTMLElement | null>(null);

async function focusHeading() {
  await nextTick();
  heading.value?.focus();
}

function closeFromKeyboard() {
  emit("close");
}

defineExpose({ focusHeading });

const badge = computed(() => {
  switch (props.snapshot.kind) {
    case "failed_read": return "verification failed";
    case "worker_unavailable": return "worker unavailable";
    case "scheduler_unknown": return "no result yet";
    case "first_result_up": return "first result received";
    case "first_result_down": return "real evidence · target failed";
    case "complete_existing": return "opened manually · setup already complete";
    case "waiting_for_result": return monitor.value?.type === "push" ? "push branch" : "waiting · not healthy yet";
    default: return "canonical state";
  }
});

const title = computed(() => {
  switch (props.snapshot.kind) {
    case "loading": return "Checking your Cerbix workspace…";
    case "failed_read": return "We couldn't verify setup";
    case "fresh_no_org": return "Create your first organization";
    case "org_no_project": return `Create a project in ${props.orgName || "this organization"}`;
    case "project_no_monitor": return "Choose the first real monitor";
    case "forbidden_handoff":
      if (handoff.value === "org") return "An administrator needs to create an organization";
      if (handoff.value === "project") return "An organization admin needs to create or grant access to a project";
      return "An editor or admin needs to create the first monitor";
    case "worker_unavailable": return `No worker is connected in ${region.value}`;
    case "scheduler_unknown": return "No result yet";
    case "first_result_up": return `${monitor.value?.name || "The monitor"} returned UP`;
    case "first_result_down": return `${monitor.value?.name || "The monitor"} returned DOWN`;
    case "complete_existing": return "First useful result already exists";
    default:
      return monitor.value?.type === "push"
        ? "Send the first heartbeat from your real job"
        : "Monitor saved. Waiting for its first real result";
  }
});

const lead = computed(() => {
  switch (props.snapshot.kind) {
    case "loading": return "Organization, project, monitor and heartbeat facts are being read from the selected scope.";
    case "failed_read": return "Nothing has been marked incomplete or complete, and this failure is not rendered as an empty project.";
    case "fresh_no_org": return "Organizations are the tenant boundary. The existing create dialog remains the owner of validation and creation.";
    case "org_no_project": return "Projects hold monitors and evidence. Create one without leaving the Dashboard journey.";
    case "project_no_monitor": return "HTTP is the recommended start; TCP and DNS are equally valid. The full typed form remains available.";
    case "forbidden_handoff": return "You can view setup progress, but your current access cannot perform this step.";
    case "worker_unavailable": return `Cerbix cannot run ${monitor.value?.name || "this monitor"} in ${region.value} until a worker connects there.`;
    case "scheduler_unknown": return "A connected worker does not prove that the scheduler issued a run. Check deployment readiness and scheduler metrics.";
    case "first_result_up": return "Onboarding is complete because Cerbix stored a real terminal observation. Optional setup stays optional.";
    case "first_result_down": return "The journey is complete, but the target is not healthy. Inspect and correct the real failure.";
    case "complete_existing": return `${props.monitors.length} monitor${props.monitors.length === 1 ? "" : "s"} and persisted heartbeat evidence were found. Automatic onboarding stays closed for this project.`;
    default:
      return monitor.value?.type === "push"
        ? "The push monitor exists, but Cerbix has not received a heartbeat. Its token remains only on the monitor detail page."
        : `Cerbix has not persisted a heartbeat for ${monitor.value?.name || "this monitor"} yet. Creation alone does not prove the target is healthy.`;
  }
});

const announcement = computed(() => `${title.value}. ${lead.value}`);

function stepClass(step: OnboardingStep): string {
  if (props.snapshot.done.includes(step)) return "done";
  if (props.snapshot.current === step) return "current";
  return "";
}
</script>

<template>
  <section
    v-if="compact"
    class="mb-3 grid grid-cols-[minmax(0,1fr)_auto] items-center gap-4 rounded border border-border bg-surface p-4 shadow-card max-[760px]:grid-cols-1"
    aria-label="Onboarding guide opened manually"
    data-testid="onboarding-guide"
    :data-state="snapshot.kind"
    @keydown.esc.stop="closeFromKeyboard"
  >
    <div>
      <span class="inline-flex h-6 items-center rounded-full bg-inset px-[9px] text-[12px] font-medium text-ink-3">{{ badge }}</span>
      <h2 ref="heading" tabindex="-1" class="mt-[7px] text-[15px] font-semibold">{{ title }}</h2>
      <p class="mt-[3px] text-[12.5px] text-ink-2">{{ lead }}</p>
      <ol class="mt-[10px] flex list-none flex-wrap gap-[7px] p-0">
        <li v-for="step in steps" :key="step.key" class="inline-flex h-6 items-center rounded-full bg-up-weak px-[9px] text-[12px] font-medium text-up">✓ {{ step.label }}</li>
      </ol>
    </div>
    <div class="flex flex-wrap justify-end gap-2 max-[760px]:justify-start">
      <RouterLink v-if="monitor?.id" :to="{ name: 'monitor', params: { id: monitor.id } }" class="inline-flex h-[34px] items-center rounded-sm bg-accent px-[13px] text-[13px] font-medium text-accent-ink hover:bg-accent-2">Review setup</RouterLink>
      <button type="button" class="h-[34px] rounded-sm border border-border px-[13px] text-[13px] hover:border-border-strong" @click="emit('close')">Collapse guide</button>
    </div>
  </section>

  <section
    v-else
    class="mb-3 grid grid-cols-[252px_minmax(0,1fr)] overflow-hidden rounded border border-border bg-surface shadow-card max-[760px]:grid-cols-1"
    aria-label="Onboarding guide"
    data-testid="onboarding-guide"
    :data-state="snapshot.kind"
    @keydown.esc.stop="closeFromKeyboard"
  >
    <aside class="border-r border-border bg-surface-2 p-4 max-[760px]:border-b max-[760px]:border-r-0">
      <h2 class="text-[14px] font-semibold">Get to your first useful result</h2>
      <p class="mb-[13px] mt-[3px] text-[12px] text-ink-3">Recomputed from the selected Cerbix scope.</p>
      <ol class="flex list-none flex-col gap-[3px] p-0">
        <li
          v-for="(step, index) in steps"
          :key="step.key"
          class="grid grid-cols-[22px_minmax(0,1fr)] gap-2 rounded-sm p-[7px]"
          :class="stepClass(step.key) === 'current' ? 'bg-surface' : ''"
          :aria-current="snapshot.current === step.key ? 'step' : undefined"
        >
          <span
            class="grid h-[22px] w-[22px] place-items-center rounded-full border border-border-strong bg-surface text-[11px] text-ink-3"
            :class="snapshot.done.includes(step.key) ? '!border-transparent bg-up-weak text-up' : snapshot.current === step.key ? '!border-transparent bg-accent-weak text-accent' : ''"
          >{{ snapshot.done.includes(step.key) ? "✓" : index + 1 }}</span>
          <span>
            <b class="block text-[12.5px] font-semibold">{{ step.label }}</b>
            <small class="mt-px block text-[11px] text-ink-3">{{ snapshot.done.includes(step.key) ? "Verified from Cerbix" : snapshot.current === step.key ? "Current step" : "Not reached" }}</small>
          </span>
        </li>
      </ol>
    </aside>

    <div class="p-5">
      <div class="mb-[9px] flex items-center gap-2">
        <span
          class="inline-flex h-6 items-center rounded-full px-[9px] text-[12px] font-medium"
          :class="snapshot.kind === 'first_result_up' ? 'bg-up-weak text-up' : snapshot.kind === 'first_result_down' || snapshot.kind === 'failed_read' ? 'bg-down-weak text-down' : snapshot.kind === 'worker_unavailable' || snapshot.kind === 'scheduler_unknown' ? 'bg-degraded-weak text-degraded' : 'bg-inset text-ink-3'"
        >{{ badge }}</span>
      </div>
      <h2 ref="heading" tabindex="-1" class="text-[19px] font-semibold tracking-[-0.025em]">{{ title }}</h2>
      <p class="mt-[5px] max-w-[650px] text-[13.5px] text-ink-2">{{ lead }}</p>
      <p class="sr-only" aria-live="polite">{{ announcement }}</p>

      <div v-if="snapshot.kind === 'loading'" class="mt-4 flex items-center gap-[11px] rounded border border-border bg-surface-2 p-[13px] text-[12.5px]">
        <span class="h-[18px] w-[18px] animate-spin rounded-full border-2 border-border-strong border-t-accent motion-reduce:animate-none"></span>
        Checking the selected scope…
      </div>

      <div v-else-if="snapshot.kind === 'failed_read'" class="mt-4 flex gap-[10px] rounded bg-down-weak px-[13px] py-3 text-[12.5px] text-ink-2">
        <span class="font-semibold text-down">!</span>
        <div><b class="text-ink">Could not load the facts required for onboarding.</b><br><span class="font-mono text-[11.5px]">{{ snapshot.error }}</span></div>
      </div>

      <div v-else-if="snapshot.kind === 'worker_unavailable' || snapshot.kind === 'scheduler_unknown'" class="mt-4 flex gap-[10px] rounded bg-degraded-weak px-[13px] py-3 text-[12.5px] text-ink-2">
        <span class="font-semibold text-degraded">!</span>
        <div>
          <b class="text-ink">{{ snapshot.kind === "worker_unavailable" ? `Region ${region} has no connected worker.` : "Scheduler dispatch is not observable from the SPA." }}</b><br>
          <span>{{ snapshot.kind === "worker_unavailable" ? "Start a worker in the configured region or edit the monitor deliberately." : "Use /readyz and cerbix_scheduler_* metrics to diagnose the deployment." }}</span>
        </div>
      </div>

      <div v-else-if="snapshot.kind === 'waiting_for_result'" class="mt-4 flex items-center gap-[11px] rounded border border-border bg-surface-2 p-[13px] text-[12.5px]">
        <span v-if="monitor?.type !== 'push'" class="h-[18px] w-[18px] animate-spin rounded-full border-2 border-border-strong border-t-accent motion-reduce:animate-none"></span>
        <span v-else class="font-semibold text-accent">→</span>
        <div>
          <b>{{ monitor?.type === "push" ? "Open Push endpoint instructions." : `Waiting for a result from ${region}` }}</b><br>
          <span class="text-ink-3">{{ monitor?.type === "push" ? "The guide never repeats or stores the push token." : "Expected after the monitor's first scheduled run." }}</span>
        </div>
      </div>

      <div v-if="monitor && ['waiting_for_result', 'worker_unavailable', 'scheduler_unknown', 'first_result_up', 'first_result_down'].includes(snapshot.kind)" class="mt-4 grid grid-cols-[max-content_1fr] gap-x-[14px] gap-y-[7px] rounded border border-border bg-surface-2 p-[13px] text-[12.5px]">
        <span class="text-ink-3">Monitor</span><span>{{ monitor.name }}</span>
        <span class="text-ink-3">Type</span><span class="font-mono">{{ monitorType }}</span>
        <span v-if="monitor.type !== 'push'" class="text-ink-3">Region</span><span v-if="monitor.type !== 'push'" class="font-mono">{{ region }}</span>
        <template v-if="heartbeat">
          <span class="text-ink-3">Observed</span><span class="font-mono">{{ instantLabel(heartbeat.ts) }}</span>
          <span class="text-ink-3">Result</span><span :class="heartbeat.up ? 'text-up' : 'text-down'">● {{ heartbeat.up ? "UP" : "DOWN" }}</span>
          <span v-if="heartbeat.code !== undefined" class="text-ink-3">Code</span><span v-if="heartbeat.code !== undefined" class="font-mono">{{ heartbeat.code }}</span>
          <span v-if="heartbeat.latency_ms !== undefined" class="text-ink-3">Latency</span><span v-if="heartbeat.latency_ms !== undefined" class="font-mono">{{ heartbeat.latency_ms }} ms</span>
          <span v-if="heartbeat.msg" class="text-ink-3">Message</span><span v-if="heartbeat.msg" class="font-mono">{{ heartbeat.msg }}</span>
        </template>
      </div>

      <div v-if="snapshot.kind === 'project_no_monitor'" class="mt-4 grid grid-cols-3 gap-2 max-[620px]:grid-cols-1">
        <RouterLink v-for="choice in [{ type: 'http', label: 'HTTP', sub: 'Recommended' }, { type: 'tcp', label: 'TCP', sub: 'Port reachability' }, { type: 'dns', label: 'DNS', sub: 'Resolution' }]" :key="choice.type" :to="{ name: 'monitor-new', query: { onboarding: '1', type: choice.type } }" class="rounded border border-border bg-surface-2 p-3 hover:border-accent">
          <b class="block text-[13px]">{{ choice.label }}</b><span class="text-[11.5px] text-ink-3">{{ choice.sub }}</span>
        </RouterLink>
      </div>

      <label v-if="snapshot.kind === 'waiting_for_result' && monitors.length > 1" class="mt-4 flex max-w-[420px] flex-col gap-[6px] text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">
        Resume candidate
        <select class="rounded-sm border border-border bg-surface-2 px-3 py-2 font-sans text-[13px] font-normal normal-case tracking-normal text-ink" :value="monitor?.id" @change="emit('chooseMonitor', ($event.target as HTMLSelectElement).value)">
          <option v-for="item in monitors" :key="item.id" :value="item.id">{{ item.name }}</option>
        </select>
      </label>

      <div class="mt-[18px] flex flex-wrap items-center gap-2">
        <button v-if="snapshot.kind === 'failed_read'" type="button" class="h-[34px] rounded-sm bg-accent px-[13px] text-[13px] font-medium text-accent-ink hover:bg-accent-2" @click="emit('retry')">Retry verification</button>
        <button v-else-if="snapshot.kind === 'fresh_no_org'" type="button" class="h-[34px] rounded-sm bg-accent px-[13px] text-[13px] font-medium text-accent-ink hover:bg-accent-2" @click="emit('createOrg')">Create organization</button>
        <button v-else-if="snapshot.kind === 'org_no_project'" type="button" class="h-[34px] rounded-sm bg-accent px-[13px] text-[13px] font-medium text-accent-ink hover:bg-accent-2" @click="emit('createProject')">Create project</button>
        <RouterLink v-else-if="snapshot.kind === 'project_no_monitor'" :to="{ name: 'monitor-new', query: { onboarding: '1' } }" class="inline-flex h-[34px] items-center rounded-sm bg-accent px-[13px] text-[13px] font-medium text-accent-ink hover:bg-accent-2">Open full monitor form</RouterLink>
        <RouterLink v-else-if="monitor?.id" :to="{ name: 'monitor', params: { id: monitor.id }, query: monitor.type === 'push' ? { onboarding: '1' } : {} }" class="inline-flex h-[34px] items-center rounded-sm bg-accent px-[13px] text-[13px] font-medium text-accent-ink hover:bg-accent-2">{{ monitor.type === "push" ? "Open push instructions" : "Open monitor" }}</RouterLink>
        <RouterLink v-if="monitor?.id && (snapshot.kind === 'first_result_down' || snapshot.kind === 'worker_unavailable')" :to="{ name: 'monitor-edit', params: { id: monitor.id } }" class="inline-flex h-[34px] items-center rounded-sm border border-border px-[13px] text-[13px] hover:border-border-strong">Edit monitor</RouterLink>
      </div>

      <div v-if="snapshot.kind === 'first_result_up'" class="mt-4 border-t border-border pt-4">
        <h3 class="text-[13px] font-semibold">Optional next actions</h3>
        <div class="mt-[9px] flex flex-wrap gap-2">
          <RouterLink :to="{ name: 'services' }" class="rounded-sm border border-border px-[10px] py-[7px] text-[12px] text-ink-2">Create a service</RouterLink>
          <RouterLink :to="{ name: 'settings', query: { tab: 'channels' } }" class="rounded-sm border border-border px-[10px] py-[7px] text-[12px] text-ink-2">Configure notifications</RouterLink>
          <RouterLink :to="{ name: 'status' }" class="rounded-sm border border-border px-[10px] py-[7px] text-[12px] text-ink-2">Publish a status page</RouterLink>
          <RouterLink :to="{ name: 'sla' }" class="rounded-sm border border-border px-[10px] py-[7px] text-[12px] text-ink-2">Set reliability objectives</RouterLink>
        </div>
      </div>

      <div class="mt-[18px] flex items-center gap-[10px] text-[12px] text-ink-3 max-[620px]:flex-col max-[620px]:items-stretch">
        <button type="button" class="text-left underline" @click="emit('close')">Skip for now</button>
        <button type="button" class="text-left underline" @click="emit('dismiss')">Do not show automatically</button>
        <span class="flex-1"></span>
        <span>Normal navigation stays available.</span>
      </div>
    </div>
  </section>
</template>
