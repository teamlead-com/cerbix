<script setup lang="ts">
// FR-021 phase 3, changeset 3: the impact-graph surface, 1:1 with the APPROVED mock
// (docs/design/mock-service-impact.html, screens 1–2). Lists and badges only — no visual
// graph in this phase.
//
// The two rules this component exists to keep:
//   * the EDGE SET has its own concurrency token. Edges live outside the declaration axes,
//     so `graph_generation` — not the declaration revision — is what a save echoes back,
//     and a 409 means someone else's set is now the truth: the editor reloads rather than
//     re-submitting over it.
//   * every failure renders as ITSELF. A stale token, a cycle, a foreign service, a depth
//     or count bound and a file-owned service are five DIFFERENT answers, and the payload's
//     own message is what the operator reads — a generic "could not save" would hide which
//     one happened.
//
// Transport discipline follows ServiceReliability.vue: every request is caught at its own
// boundary (openapi-fetch rethrows network failures), and every async assignment is gated
// on a load generation captured before the first await, so a delayed response from another
// service or project can never land here.
import { computed, onBeforeUnmount, onMounted, ref, watch } from "vue";

import { api } from "@/api/client";
import type { components } from "@/api/schema";

type Graph = components["schemas"]["ServiceGraph"];
type Edge = components["schemas"]["ServiceGraphEdge"];
// The list payload nests the service; the editor only needs id/slug/name.
type Candidate = { id: string; slug: string; name: string };
type Res<T> = { data?: T; error?: unknown };

const props = defineProps<{
  projectId: string;
  serviceId: string;
  canWrite: boolean;
  /** The file provider owning this service, or "" when the UI owns it. */
  managedBy?: string;
}>();

const graph = ref<Graph | null>(null);
const loading = ref(true);
const error = ref("");

// The editor is a separate state: opening it snapshots the CURRENT set, so cancelling
// leaves nothing behind and saving compares against the token that was read with it.
const editing = ref(false);
const candidates = ref<Candidate[]>([]);
// Candidate discovery has its OWN states ([298] P1-2): a failed list is not the honest
// "this project has no other services", and an editor that never learned the candidates
// must not be able to submit a set.
const candidatesLoading = ref(false);
const candidatesError = ref("");
const selected = ref<Set<string>>(new Set());
const search = ref("");
const saving = ref(false);
const saveError = ref("");
const staleReload = ref(false);

let generation = 0;
// Editor OPERATIONS get their own sequence ([301] P2-1): the context generation cannot
// tell a cancelled open from the reopen that followed it in the same service, so a
// deferred candidate list from the first open would overwrite the second's.
let editorOp = 0;

/** resetEditor drops every draft/sensitive piece of editor state synchronously. */
function resetEditor() {
  editorOp++;
  editing.value = false;
  candidates.value = [];
  candidatesLoading.value = false;
  candidatesError.value = "";
  selected.value = new Set();
  search.value = "";
  saving.value = false;
  saveError.value = "";
  staleReload.value = false;
}

/** guarded normalizes a rejected fetch into the same { error } shape an HTTP error has. */
async function guarded<T>(p: Promise<Res<T>>): Promise<Res<T>> {
  try {
    return await p;
  } catch (e) {
    return { error: { error: e instanceof Error ? e.message : "request failed" } };
  }
}

function messageOf(err: unknown, fallback: string): string {
  const m = (err as { error?: string })?.error;
  return typeof m === "string" && m ? m : fallback;
}

async function load() {
  const gen = ++generation;
  // The draft belongs to the context it was typed in ([298] P2-1): a service or project
  // change discards it BEFORE the read, so an in-flight save can never land its old
  // selection on the new service, and no stale draft is left on screen.
  resetEditor();
  loading.value = true;
  error.value = "";
  const res = await guarded(
    api.GET("/api/v1/projects/{projectID}/services/{serviceID}/dependencies", {
      params: { path: { projectID: props.projectId, serviceID: props.serviceId } },
    }) as Promise<Res<Graph>>,
  );
  if (gen !== generation) return;
  if (res.error || !res.data) {
    error.value = messageOf(res.error, "Could not read the dependencies.");
    graph.value = null;
  } else {
    graph.value = res.data;
  }
  loading.value = false;
}

const upstream = computed<Edge[]>(() => graph.value?.depends_on ?? []);
const downstream = computed<Edge[]>(() => graph.value?.depended_on_by ?? []);
const edgeCount = computed(() => upstream.value.length);

async function openEditor() {
  saveError.value = "";
  staleReload.value = false;
  candidatesError.value = "";
  candidatesLoading.value = true;
  candidates.value = [];
  selected.value = new Set(upstream.value.map((e) => e.id));
  editing.value = true;
  const gen = generation;
  const op = ++editorOp;
  const res = await guarded(
    api.GET("/api/v1/projects/{projectID}/services", {
      params: { path: { projectID: props.projectId } },
    }) as Promise<Res<components["schemas"]["ServiceSummary"][]>>,
  );
  if (gen !== generation || op !== editorOp) return;
  candidatesLoading.value = false;
  if (res.error || !res.data) {
    candidatesError.value = messageOf(res.error, "Could not list this project's services.");
    return;
  }
  candidates.value = res.data
    .map((row) => ({ id: row.service.id, slug: row.service.slug, name: row.service.name }))
    .filter((c) => c.id !== props.serviceId);
}

/** The editor may only submit a set it could actually see. */
const canSubmit = computed(() => !saving.value && !candidatesLoading.value && !candidatesError.value);

const visibleCandidates = computed(() => {
  const q = search.value.trim().toLowerCase();
  const list = q
    ? candidates.value.filter((s) => (s.slug + " " + s.name).toLowerCase().includes(q))
    : candidates.value;
  // Selected first, then alphabetical — the current set is what the operator is editing.
  return [...list].sort((a, b) => {
    const sa = selected.value.has(a.id) ? 0 : 1;
    const sb = selected.value.has(b.id) ? 0 : 1;
    return sa - sb || a.slug.localeCompare(b.slug);
  });
});

function toggle(id: string) {
  const next = new Set(selected.value);
  if (next.has(id)) next.delete(id);
  else next.add(id);
  selected.value = next;
}

async function save() {
  if (!graph.value || !canSubmit.value) return;
  saving.value = true;
  saveError.value = "";
  staleReload.value = false;
  const gen = generation;
  const res = await guarded(
    api.PUT("/api/v1/projects/{projectID}/services/{serviceID}/dependencies", {
      params: { path: { projectID: props.projectId, serviceID: props.serviceId } },
      body: {
        depends_on: [...selected.value],
        graph_generation: graph.value.graph_generation,
      },
    }) as Promise<Res<Graph>>,
  );
  if (gen !== generation) return;
  saving.value = false;
  if (res.error || !res.data) {
    saveError.value = messageOf(res.error, "Could not save the dependencies.");
    // A stale token means someone else's set is the truth now: offer the reload rather
    // than letting a retry silently overwrite it.
    staleReload.value = saveError.value.includes("graph_generation_stale");
    return;
  }
  graph.value = res.data;
  editing.value = false;
}

async function reloadIntoEditor() {
  await load();
  selected.value = new Set(upstream.value.map((e) => e.id));
  saveError.value = "";
  staleReload.value = false;
}

// The phase-2 signal has TWO layers and they are never merged ([298] P1-3): `sli` is what
// availability is declared to mean, `diagnostics` is the operational context. A neighbour
// whose SLI is healthy while its diagnostics are failing must SAY so — collapsing the row
// to one word is exactly the hiding the two-layer card exists to prevent.
function sliClass(e: Edge): string {
  switch (e.health?.sli) {
    case "down":
      return "text-down";
    case "degraded":
      return "text-degraded";
    case "healthy":
      return "text-up";
    default:
      return "text-ink-3";
  }
}
function sliLabel(e: Edge): string {
  if (!e.health) return "unknown";
  switch (e.health.sli) {
    case "down":
      return "Down";
    case "degraded":
      return "Degraded";
    case "healthy":
      return "Operational";
    default:
      return "Unknown";
  }
}
function diagClass(e: Edge): string {
  switch (e.health?.diagnostics) {
    case "failing":
      return "text-down";
    case "ok":
      return "text-ink-3";
    default:
      return "text-ink-3";
  }
}
function diagLabel(e: Edge): string {
  // An absent health payload is the API's DISCLOSED degraded read ([301] P1-2): both
  // layers say "unknown" — a blank second line would look like a rendered fact, not a
  // missing one.
  if (!e.health) return "diagnostics unknown";
  switch (e.health.diagnostics) {
    case "failing":
      return `diagnostics failing${e.health.failing_monitors?.length ? ": " + e.health.failing_monitors.join(", ") : ""}`;
    case "ok":
      return "diagnostics ok";
    default:
      return "diagnostics unknown";
  }
}

onMounted(load);
watch(() => [props.projectId, props.serviceId], load);
onBeforeUnmount(resetEditor);
</script>

<template>
  <section class="mb-4 overflow-hidden rounded border border-border bg-surface shadow-card" data-testid="service-dependencies">
    <header class="flex items-center gap-2 border-b border-border px-4 py-[10px]">
      <h2 class="text-[13.5px] font-semibold">Dependencies</h2>
      <span class="rounded-xs border border-border px-[7px] py-px font-mono text-[11px] text-ink-3" data-testid="svc-dep-count">
        {{ edgeCount }} / 20 edges
      </span>
      <div class="flex-1"></div>
      <button
        v-if="canWrite && !editing"
        type="button"
        class="flex h-[28px] items-center rounded-sm border border-border px-[11px] text-[12.5px] text-ink-2 hover:border-border-strong hover:text-ink"
        data-testid="svc-dep-edit"
        @click="openEditor"
      >
        Edit
      </button>
    </header>

    <p v-if="error" class="m-4 rounded border border-down/40 bg-down-weak p-3 text-[12.5px] text-down" data-testid="svc-dep-error">
      {{ error }}
    </p>
    <p v-else-if="loading" class="px-4 py-6 text-[13px] text-ink-3" data-testid="svc-dep-loading">Loading…</p>

    <template v-else-if="graph">
      <!-- Ownership belongs on the READ surface: a managed service has no Edit button at
           all (the parent's canWrite already excludes it), so a notice living only inside
           the editor would be unreachable ([298] P2-2). -->
      <p
        v-if="managedBy"
        class="mx-4 mt-4 rounded border border-border bg-surface-2 p-3 text-[12.5px] text-ink-2"
        data-testid="svc-dep-managed"
      >
        Owned by the file provider <span class="font-mono">{{ managedBy }}</span>. Its dependencies are declared in the
        source file — the API refuses an edit made here.
      </p>

      <!-- The read surface: two lists, each neighbour with the two-layer health signal. -->
      <div v-if="!editing" class="grid grid-cols-2 gap-0 max-[760px]:grid-cols-1">
        <div class="border-r border-border p-4 max-[760px]:border-b max-[760px]:border-r-0">
          <div class="text-[11px] font-semibold uppercase tracking-[0.06em] text-ink-3">Depends on</div>
          <ul class="mt-3 flex flex-col gap-[6px]">
            <li
              v-for="e in upstream"
              :key="e.id"
              class="flex flex-wrap items-center gap-x-2 gap-y-1 rounded-sm border border-border px-[10px] py-[7px] text-[13px]"
              data-testid="svc-dep-upstream"
            >
              <RouterLink :to="{ name: 'service', params: { id: e.id } }" class="font-medium hover:text-accent">{{ e.name }}</RouterLink>
              <span class="font-mono text-[11.5px] text-ink-3">{{ e.slug }}</span>
              <span
                v-if="e.managed_by"
                class="rounded-xs border border-border bg-inset px-[6px] py-px font-mono text-[10.5px] text-ink-2"
                data-testid="svc-dep-managed-chip"
                :title="'Owned by the file provider ' + e.managed_by"
              >file · {{ e.managed_by }}</span>
              <span class="ml-auto text-[12px]" :class="sliClass(e)" data-testid="svc-dep-health">{{ sliLabel(e) }}</span>
              <span class="basis-full text-[11.5px]" :class="diagClass(e)" data-testid="svc-dep-diagnostics">{{ diagLabel(e) }}</span>
            </li>
            <li v-if="!upstream.length" class="text-[12.5px] text-ink-3" data-testid="svc-dep-upstream-empty">
              No upstream dependencies declared.
            </li>
          </ul>
        </div>

        <div class="p-4">
          <div class="text-[11px] font-semibold uppercase tracking-[0.06em] text-ink-3">Depended on by</div>
          <ul class="mt-3 flex flex-col gap-[6px]">
            <li
              v-for="e in downstream"
              :key="e.id"
              class="flex flex-wrap items-center gap-x-2 gap-y-1 rounded-sm border border-border px-[10px] py-[7px] text-[13px]"
              data-testid="svc-dep-downstream"
            >
              <RouterLink :to="{ name: 'service', params: { id: e.id } }" class="font-medium hover:text-accent">{{ e.name }}</RouterLink>
              <span class="font-mono text-[11.5px] text-ink-3">{{ e.slug }}</span>
              <!-- A file-owned DEPENDENT pins this service: deleting it will be a 409. -->
              <span
                v-if="e.managed_by"
                class="rounded-xs border border-border bg-inset px-[6px] py-px font-mono text-[10.5px] text-ink-2"
                data-testid="svc-dep-managed-chip"
                :title="'Declared by the file provider ' + e.managed_by + ' — it pins this service against deletion'"
              >file · {{ e.managed_by }}</span>
              <span class="ml-auto text-[12px]" :class="sliClass(e)" data-testid="svc-dep-downstream-health">{{ sliLabel(e) }}</span>
              <span class="basis-full text-[11.5px]" :class="diagClass(e)">{{ diagLabel(e) }}</span>
            </li>
            <li v-if="!downstream.length" class="text-[12.5px] text-ink-3">
              Edges here are declared by each dependent service's owner.
            </li>
          </ul>
        </div>
      </div>

      <!-- The editor: a replace-set over the token that was read with the list. -->
      <div v-else class="p-4" data-testid="svc-dep-editor">
        <input
          v-model="search"
          type="text"
          placeholder="Search services…"
          class="mb-2 w-full rounded-sm border border-border bg-surface px-[10px] py-[7px] text-[13px]"
          data-testid="svc-dep-search"
        />
        <p class="mb-3 text-[11.5px] text-ink-3">
          Project services, excluding this one. At most 20 direct dependencies; the graph stays acyclic.
        </p>

        <p v-if="candidatesLoading" class="text-[12.5px] text-ink-3" data-testid="svc-dep-candidates-loading">
          Loading this project's services…
        </p>
        <!-- A failed candidate list is NOT "no other services": saying so would invite a
             replace-set the operator could not see ([298] P1-2). -->
        <p
          v-else-if="candidatesError"
          class="rounded border border-down/40 bg-down-weak p-3 text-[12.5px] text-down"
          data-testid="svc-dep-candidates-error"
        >
          {{ candidatesError }} — the dependency set cannot be edited until the list loads.
        </p>

        <ul v-else class="flex max-h-[280px] flex-col gap-[6px] overflow-y-auto">
          <li v-for="c in visibleCandidates" :key="c.id">
            <label
              class="flex cursor-pointer items-center gap-[9px] rounded-sm border px-[10px] py-[7px] text-[13px]"
              :class="selected.has(c.id) ? 'border-accent bg-accent-weak' : 'border-border'"
              data-testid="svc-dep-option"
            >
              <input
                type="checkbox"
                :checked="selected.has(c.id)"
                class="h-[15px] w-[15px] accent-accent"
                :data-slug="c.slug"
                @change="toggle(c.id)"
              />
              <span>{{ c.name }}</span>
              <span class="font-mono text-[11.5px] text-ink-3">{{ c.slug }}</span>
            </label>
          </li>
          <li v-if="!visibleCandidates.length" class="text-[12.5px] text-ink-3">No other services in this project.</li>
        </ul>

        <!-- Every rejection renders as ITSELF, with the API's own message. -->
        <div
          v-if="saveError"
          class="mt-3 flex items-start gap-[9px] rounded border border-down/40 bg-down-weak p-3 text-[12.5px] text-down"
          data-testid="svc-dep-save-error"
        >
          <span>{{ saveError }}</span>
          <button
            v-if="staleReload"
            type="button"
            class="ml-auto shrink-0 rounded-sm border border-down px-[8px] py-px text-[11.5px]"
            data-testid="svc-dep-reload"
            @click="reloadIntoEditor"
          >
            Reload
          </button>
        </div>

        <div class="mt-3 flex items-center justify-end gap-2">
          <span class="mr-auto font-mono text-[11.5px] text-ink-3" data-testid="svc-dep-token">
            graph_generation {{ graph.graph_generation }}
          </span>
          <button
            type="button"
            class="flex h-[30px] items-center rounded-sm border border-border px-[12px] text-[13px] text-ink-2 hover:border-border-strong"
            data-testid="svc-dep-cancel"
            @click="resetEditor()"
          >
            Cancel
          </button>
          <button
            type="button"
            :disabled="!canSubmit"
            class="flex h-[30px] items-center rounded-sm border border-accent bg-accent px-[12px] text-[13px] font-medium text-accent-ink disabled:opacity-60"
            data-testid="svc-dep-save"
            @click="save"
          >
            {{ saving ? "Saving…" : "Save dependencies" }}
          </button>
        </div>
      </div>
    </template>
  </section>
</template>
