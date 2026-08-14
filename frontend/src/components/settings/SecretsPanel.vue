<script setup lang="ts">
import { computed, onBeforeUnmount, reactive, ref, watch } from "vue";
import { api } from "@/api/client";
import type { components } from "@/api/schema";
import { useSession } from "@/stores/session";
import { useWorkspace } from "@/stores/workspace";

type ProjectSecret = components["schemas"]["ProjectSecret"];

const ws = useWorkspace();
const session = useSession();

const loading = ref(true);
const secrets = ref<ProjectSecret[]>([]);
const error = ref("");
// The instance-wide feature toggle (secrets.enabled) — 404 feature_disabled on any call.
const featureDisabled = ref(false);
const actionError = ref("");

const form = reactive({ name: "", value: "" });
const creating = ref(false);
const canWrite = computed(() => session.canProjectWrite(ws.orgId, ws.projectId));

// ^[a-z][a-z0-9-]{0,62}$ — same slug rule the API enforces.
const nameOk = computed(() => /^[a-z][a-z0-9-]{0,62}$/.test(form.name.trim()));
const canAdd = computed(() => canWrite.value && nameOk.value && !!form.value && !creating.value);

// Inline editor state — one row at a time, keyed by the secret's current name.
const editing = ref<string | null>(null);
const editName = ref("");
const editValue = ref("");
const savingEdit = ref(false);

type RequestContext = {
  projectID: string;
  generation: number;
  controller: AbortController;
};

let projectGeneration = 0;
let loadController: AbortController | null = null;
const activeControllers = new Set<AbortController>();

function beginRequest(projectID: string): RequestContext {
  const controller = new AbortController();
  activeControllers.add(controller);
  return { projectID, generation: projectGeneration, controller };
}

function requestIsCurrent(req: RequestContext): boolean {
  return !req.controller.signal.aborted && req.generation === projectGeneration && ws.projectId === req.projectID;
}

function finishRequest(req: RequestContext) {
  activeControllers.delete(req.controller);
}

// Project selection is a tenant boundary. Drop every plaintext-bearing field,
// invalidate every in-flight response and clear project-scoped errors before the
// next tenant can render or mutate anything.
function resetSensitiveState() {
  projectGeneration += 1;
  for (const controller of activeControllers) controller.abort();
  activeControllers.clear();
  loadController = null;

  form.name = "";
  form.value = "";
  editing.value = null;
  editName.value = "";
  editValue.value = "";
  actionError.value = "";
  creating.value = false;
  savingEdit.value = false;

  secrets.value = [];
  error.value = "";
  featureDisabled.value = false;
  loading.value = false;
}

const monitors = (n: number) => `${n} monitor${n === 1 ? "" : "s"}`;
const fmtDate = (ts?: string | null) => (ts ? new Date(ts).toISOString().slice(0, 10) : "—");
const usedTotal = (s: ProjectSecret) => s.used_by?.total ?? 0;
// Rename is locked while a Monitoring-as-Code file references the name — cerbix never rewrites bundles.
const renameLocked = (s: ProjectSecret) => (s.used_by?.file_managed ?? 0) > 0;

async function load() {
  const projectID = ws.projectId;
  loadController?.abort();
  if (!projectID) {
    secrets.value = [];
    loading.value = false;
    return;
  }
  const req = beginRequest(projectID);
  loadController = req.controller;
  loading.value = true;
  error.value = "";
  featureDisabled.value = false;
  try {
    const res = await api.GET("/api/v1/projects/{projectID}/secrets", {
      params: { path: { projectID } },
      signal: req.controller.signal,
    });
    if (!requestIsCurrent(req)) return;
    if (res.error) {
      if ((res.error as { error?: string })?.error === "feature_disabled") {
        featureDisabled.value = true;
      } else {
        error.value = "You need access to this project to view its secrets.";
      }
      secrets.value = [];
      return;
    }
    secrets.value = res.data ?? [];
  } catch {
    if (!requestIsCurrent(req)) return;
    error.value = "Could not load the project's secrets.";
  } finally {
    if (requestIsCurrent(req)) loading.value = false;
    if (loadController === req.controller) loadController = null;
    finishRequest(req);
  }
}

async function add() {
  const projectID = ws.projectId;
  if (!canAdd.value || !projectID || !canWrite.value) return;
  const req = beginRequest(projectID);
  const submittedName = form.name.trim();
  const submittedValue = form.value;
  creating.value = true;
  actionError.value = "";
  try {
    const res = await api.POST("/api/v1/projects/{projectID}/secrets", {
      params: { path: { projectID } },
      body: { name: submittedName, value: submittedValue },
      signal: req.controller.signal,
    });
    if (!requestIsCurrent(req)) return;
    if (res.error) {
      const code = (res.error as { error?: string })?.error;
      actionError.value =
        code === "secret_exists"
          ? `A secret named "${submittedName}" already exists in this project (secret_exists).`
          : code === "feature_disabled"
            ? "The secret inventory is disabled on this instance (secrets.enabled)."
            : code || "Could not add the secret.";
      return;
    }
    form.name = "";
    form.value = ""; // write-only: drop the plaintext as soon as it is stored
    await load();
  } catch {
    if (requestIsCurrent(req)) actionError.value = "Could not add the secret.";
  } finally {
    if (requestIsCurrent(req)) creating.value = false;
    finishRequest(req);
  }
}

function startEdit(s: ProjectSecret) {
  actionError.value = "";
  editing.value = s.name ?? null;
  editName.value = s.name ?? "";
  editValue.value = "";
}

function cancelEdit() {
  editing.value = null;
  editValue.value = "";
}

async function saveEdit(s: ProjectSecret) {
  const projectID = ws.projectId;
  if (savingEdit.value || !projectID || !s.name || !canWrite.value) return;
  const req = beginRequest(projectID);
  const body: { name?: string; value?: string } = {};
  const newName = editName.value.trim();
  if (newName && newName !== s.name) body.name = newName;
  if (editValue.value) body.value = editValue.value;
  if (!body.name && !body.value) {
    cancelEdit(); // nothing to change
    return;
  }
  savingEdit.value = true;
  actionError.value = "";
  try {
    const res = await api.PATCH("/api/v1/projects/{projectID}/secrets/{name}", {
      params: { path: { projectID, name: s.name } },
      body,
      signal: req.controller.signal,
    });
    if (!requestIsCurrent(req)) return;
    if (res.error) {
      const e = res.error as { error?: string; count?: number };
      actionError.value =
        e.error === "secret_renamed_in_use"
          ? `Cannot rename "${s.name}": ${monitors(e.count ?? 0)} reference it from file-managed bundles (secret_renamed_in_use). Rename it in the file source instead.`
          : e.error === "secret_exists"
            ? `A secret named "${newName}" already exists in this project (secret_exists).`
            : e.error === "feature_disabled"
              ? "The secret inventory is disabled on this instance (secrets.enabled)."
              : e.error || "Could not update the secret.";
      return;
    }
    editValue.value = ""; // write-only: drop the plaintext once stored
    editing.value = null;
    await load();
  } catch {
    if (requestIsCurrent(req)) actionError.value = "Could not update the secret.";
  } finally {
    if (requestIsCurrent(req)) savingEdit.value = false;
    finishRequest(req);
  }
}

async function remove(s: ProjectSecret) {
  const projectID = ws.projectId;
  if (!projectID || !s.name || !canWrite.value) return;
  actionError.value = "";
  if (!confirm(`Delete secret "${s.name}"? The value cannot be recovered.`)) return;
  const req = beginRequest(projectID);
  try {
    const res = await api.DELETE("/api/v1/projects/{projectID}/secrets/{name}", {
      params: { path: { projectID, name: s.name } },
      signal: req.controller.signal,
    });
    if (!requestIsCurrent(req)) return;
    if (res.error) {
      const e = res.error as { error?: string; count?: number };
      actionError.value =
        e.error === "secret_in_use"
          ? `Cannot delete "${s.name}": it is referenced by ${monitors(e.count ?? 0)} (secret_in_use). Re-point or remove those monitors first.`
          : e.error === "feature_disabled"
            ? "The secret inventory is disabled on this instance (secrets.enabled)."
            : e.error || "Could not delete the secret.";
      return;
    }
    await load();
  } catch {
    if (requestIsCurrent(req)) actionError.value = "Could not delete the secret.";
  } finally {
    finishRequest(req);
  }
}

watch(
  () => ws.projectId,
  () => {
    resetSensitiveState();
    void load();
  },
  { immediate: true },
);
onBeforeUnmount(resetSensitiveState);
</script>

<template>
  <div>
    <div class="mb-4">
      <h2 class="text-[15px] font-semibold tracking-tight">Secrets</h2>
      <p class="mt-[3px] text-[13px] text-ink-3">Named credentials for this project's monitors. Values are write-only: they are encrypted at rest and never shown again. Bundles and monitor forms reference them by name (<code class="font-mono text-[12px]">password_ref: payments-db-ro</code>).</p>
    </div>

    <div v-if="!ws.projectId" class="text-[13px] text-ink-3">Select a project to manage its secrets.</div>

    <div v-else-if="featureDisabled" class="rounded border border-border bg-surface p-4 text-[13px] text-ink-3 shadow-card">The secret inventory is disabled on this instance (<code class="font-mono text-[12px]">secrets.enabled</code>).</div>

    <template v-else>
      <div v-if="error" class="mb-4 rounded border border-border bg-surface p-4 text-[13px] text-ink-3 shadow-card">{{ error }}</div>
      <div v-if="actionError" data-testid="secret-action-error" class="mb-4 rounded-sm border border-down/40 bg-down-weak px-4 py-2 text-[13px] text-down">{{ actionError }}</div>

      <section v-if="!error" class="overflow-hidden rounded border border-border bg-surface shadow-card">
        <table class="w-full text-[13px]">
          <thead>
            <tr class="text-[10.5px] uppercase tracking-[0.06em] text-ink-3">
              <th class="border-b border-border px-4 py-[10px] text-left">Name</th>
              <th class="border-b border-border px-4 py-[10px] text-left">Created</th>
              <th class="border-b border-border px-4 py-[10px] text-left">Rotated</th>
              <th class="border-b border-border px-4 py-[10px] text-left">Used by</th>
              <th class="border-b border-border px-4 py-[10px]"></th>
            </tr>
          </thead>
          <tbody>
            <template v-for="s in secrets" :key="s.id">
              <tr class="hover:bg-surface-2">
                <td class="border-b border-border px-4 py-[11px] font-mono text-[12.5px]">{{ s.name }}</td>
                <td class="border-b border-border px-4 py-[11px] font-mono text-[12px]">{{ fmtDate(s.created_at) }}</td>
                <td class="border-b border-border px-4 py-[11px] font-mono text-[12px]">{{ fmtDate(s.rotated_at) }}</td>
                <td class="border-b border-border px-4 py-[11px]">
                  <span v-if="usedTotal(s) > 0" class="rounded-full bg-accent-weak px-[9px] py-[2px] text-[11px] font-semibold text-accent">{{ monitors(usedTotal(s)) }}</span>
                  <span v-else class="text-[12px] text-ink-3">unused</span>
                </td>
                <td class="border-b border-border px-4 py-[11px] text-right">
                  <template v-if="canWrite">
                    <button type="button" class="text-[12.5px] text-accent hover:underline" @click="editing === s.name ? cancelEdit() : startEdit(s)">Edit</button>
                    <button
                    type="button"
                    class="ml-3 text-[12.5px] text-down hover:underline disabled:cursor-not-allowed disabled:opacity-40 disabled:no-underline"
                    :disabled="usedTotal(s) > 0"
                    :title="usedTotal(s) > 0 ? `Referenced by ${monitors(usedTotal(s))}` : undefined"
                    @click="remove(s)"
                    >Delete</button>
                  </template>
                </td>
              </tr>
              <!-- inline editor row -->
              <tr v-if="canWrite && editing === s.name" class="bg-surface-2">
                <td colspan="5" class="border-b border-border px-4 py-4">
                  <div class="flex flex-wrap items-end gap-3">
                    <label class="flex flex-col gap-[6px]">
                      <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Name</span>
                      <input
                        v-model="editName"
                        type="text"
                        :disabled="renameLocked(s)"
                        :title="renameLocked(s) ? 'Rename is locked: a file-managed monitor references this name (bundles are never rewritten by cerbix)' : undefined"
                        class="w-[220px] rounded-sm border border-border bg-surface px-3 py-2 font-mono text-[13px] outline-none focus:border-accent disabled:cursor-not-allowed disabled:opacity-50"
                      />
                    </label>
                    <label class="flex flex-col gap-[6px]">
                      <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">New value (optional)</span>
                      <input v-model="editValue" type="password" placeholder="Leave empty to keep" autocomplete="new-password" class="w-[220px] rounded-sm border border-border bg-surface px-3 py-2 text-[13px] outline-none focus:border-accent" />
                    </label>
                    <button type="button" :disabled="savingEdit" class="h-[38px] rounded-sm bg-accent px-[15px] text-[13px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-50" @click="saveEdit(s)">{{ savingEdit ? "Saving…" : "Save" }}</button>
                    <button type="button" :disabled="savingEdit" class="h-[38px] rounded-sm border border-border px-[15px] text-[13px] text-ink-2 hover:border-border-strong" @click="cancelEdit">Cancel</button>
                  </div>
                  <p v-if="renameLocked(s)" class="mt-2 text-[12px] text-ink-3">Rename is locked: a file-managed monitor references this name (bundles are never rewritten by cerbix).</p>
                  <p class="mt-2 text-[12px] text-ink-3">A new value takes effect on the next probe — no bundle change, no file edit; in-flight jobs with the old value are fenced (stale results rejected).</p>
                </td>
              </tr>
            </template>
            <tr v-if="!secrets.length && !loading"><td colspan="5" class="px-4 py-10 text-center text-[13px] text-ink-3">No secrets yet.</td></tr>
          </tbody>
        </table>
        <div v-if="canWrite" class="flex flex-wrap items-end gap-3 border-t border-border p-4">
          <label class="flex flex-col gap-[6px]">
            <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Name</span>
            <input v-model="form.name" data-testid="secret-add-name" type="text" placeholder="payments-db-ro" autocomplete="off" spellcheck="false" class="w-[220px] rounded-sm border border-border bg-surface-2 px-3 py-2 font-mono text-[13px] outline-none focus:border-accent" />
            <span class="text-[11px] text-ink-3">slug a-z 0-9 -, unique in this project</span>
          </label>
          <label class="flex flex-col gap-[6px]">
            <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Value</span>
            <input v-model="form.value" data-testid="secret-add-value" type="password" autocomplete="new-password" class="w-[220px] rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent" />
            <span class="text-[11px] text-ink-3">write-only · max 4 KiB</span>
          </label>
          <button type="button" data-testid="secret-add-submit" :disabled="!canAdd" class="mb-[22px] h-[38px] rounded-sm bg-accent px-[15px] text-[13px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-50" @click="add">{{ creating ? "Adding…" : "Add secret" }}</button>
        </div>
        <div v-else class="border-t border-border px-4 py-3 text-[12px] text-ink-3">Read-only access. Ask a project editor or administrator to manage secrets.</div>
      </section>
      <div v-if="loading && !error" class="mt-4 text-[13px] text-ink-3">Loading…</div>
    </template>
  </div>
</template>
