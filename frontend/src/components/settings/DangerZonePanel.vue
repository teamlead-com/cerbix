<script setup lang="ts">
import { computed, ref } from "vue";
import { useRouter } from "vue-router";
import { useWorkspace } from "@/stores/workspace";

const ws = useWorkspace();
const router = useRouter();

const project = computed(() => ws.currentProject);
const open = ref(false);
const confirmText = ref("");
const busy = ref(false);
const error = ref("");

// The delete button unlocks only when the typed value matches the project slug — an
// irreversible, GitHub-style confirmation (spec func-project-deletion §4).
const canDelete = computed(
  () => !!project.value && confirmText.value.trim() === (project.value.slug ?? "") && !busy.value,
);

function start() {
  confirmText.value = "";
  error.value = "";
  open.value = true;
}
function close() {
  if (!busy.value) open.value = false;
}
async function confirmDelete() {
  if (!canDelete.value || !project.value) return;
  busy.value = true;
  error.value = "";
  const err = await ws.deleteProject(project.value.id!);
  busy.value = false;
  if (err) {
    error.value = err; // e.g. the 409 managed-by-file message, or 403/other
    return;
  }
  open.value = false;
  router.push({ name: "dashboard" });
}
</script>

<template>
  <div v-if="!project" class="text-[13px] text-ink-3">Select a project to manage its danger-zone actions.</div>

  <div v-else>
    <div class="overflow-hidden rounded border bg-surface shadow-card" style="border-color: color-mix(in srgb, var(--down) 40%, var(--border))">
      <h3 class="flex items-center gap-2 border-b px-4 py-[13px] text-[14px] font-semibold text-down" style="border-color: color-mix(in srgb, var(--down) 22%, var(--border))">
        <svg viewBox="0 0 24 24" class="h-[15px] w-[15px]" fill="none" stroke="currentColor" stroke-width="2"><path d="M10.3 3.9L1.8 18a2 2 0 0 0 1.7 3h17a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0z" /><path d="M12 9v4M12 17h.01" /></svg>
        Danger zone
      </h3>
      <div class="flex items-center justify-between gap-5 px-4 py-4">
        <div>
          <div class="mb-[2px] text-[13.5px] font-semibold text-ink">Delete this project</div>
          <div class="text-[12.5px] text-ink-3">
            Permanently removes <b class="font-mono text-ink-2">{{ project.slug }}</b> and everything it owns — monitors &amp;
            history, incidents, SLA, escalation, channels, project tokens. Cannot be undone.
          </div>
        </div>
        <button
          type="button"
          class="h-9 flex-none rounded-sm border px-4 text-[13px] font-semibold text-down hover:bg-down-weak"
          style="border-color: color-mix(in srgb, var(--down) 55%, var(--border))"
          @click="start"
        >
          Delete project…
        </button>
      </div>
    </div>

    <!-- confirm modal -->
    <div
      v-if="open"
      class="fixed inset-0 z-50 grid place-items-center bg-[rgba(10,10,20,0.5)] p-5"
      @click.self="close"
      @keydown.esc="close"
    >
      <div class="w-full max-w-[480px] overflow-hidden rounded border border-border-strong bg-surface shadow-lg" role="dialog" aria-modal="true">
        <div class="flex gap-3 px-[18px] pb-1 pt-[18px]">
          <span class="grid h-[34px] w-[34px] flex-none place-items-center rounded-sm bg-down-weak text-down">
            <svg viewBox="0 0 24 24" class="h-[19px] w-[19px]" fill="none" stroke="currentColor" stroke-width="2"><path d="M3 6h18M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2M19 6l-1 14a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2L5 6" /></svg>
          </span>
          <div>
            <h3 class="text-[15.5px] font-semibold tracking-tight">Delete project “{{ project.name || project.slug }}”?</h3>
            <p class="mt-[3px] text-[12.5px] text-ink-3">This permanently deletes the project and all of its data. This can’t be undone.</p>
          </div>
        </div>

        <div class="px-[18px] pb-1 pt-2">
          <ul class="my-3 list-none rounded-sm border border-border bg-inset px-[14px] py-3 text-[12.5px]">
            <li v-for="item in ['All monitors and their check history', 'Incidents & postmortems', 'SLA config, escalation policies & on-call schedules', 'Notification channels & project-scoped tokens']" :key="item" class="flex items-center gap-2 py-[2px] text-ink-2">
              <span class="h-[5px] w-[5px] flex-none rounded-full bg-down"></span>{{ item }}
            </li>
          </ul>
          <div class="mb-1 flex gap-[10px] rounded-sm border px-3 py-[11px] text-[12.5px] text-ink-2" style="background: var(--accent-weak); border-color: color-mix(in srgb, var(--accent) 30%, var(--border))">
            <svg viewBox="0 0 24 24" class="mt-[1px] h-4 w-4 flex-none text-accent" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="10" /><path d="M12 16v-4M12 8h.01" /></svg>
            <span>Org-level status pages &amp; org-scoped tokens are kept; status-page components pointing at deleted monitors become detached.</span>
          </div>

          <label class="mt-[10px] block">
            <div class="mb-[6px] text-[12px] text-ink-2">Type <span class="rounded-[4px] border border-border bg-surface-2 px-[6px] py-[1px] font-mono text-ink">{{ project.slug }}</span> to confirm</div>
            <input
              v-model="confirmText"
              :placeholder="project.slug ?? ''"
              autocomplete="off"
              spellcheck="false"
              class="h-[38px] w-full rounded-sm border bg-surface-2 px-[11px] font-mono text-[13px] text-ink outline-none focus:border-accent focus:bg-surface"
              :class="canDelete ? 'border-up' : 'border-border'"
              @keydown.enter="confirmDelete"
            />
          </label>

          <p v-if="error" class="mt-[10px] text-[12.5px] text-down">{{ error }}</p>
        </div>

        <div class="flex items-center gap-2 px-[18px] pb-[18px] pt-[14px]">
          <span class="flex-1"></span>
          <button type="button" class="h-9 rounded-sm border border-border px-4 text-[13px] text-ink-2 hover:border-border-strong" :disabled="busy" @click="close">Cancel</button>
          <button
            type="button"
            :disabled="!canDelete"
            class="h-9 rounded-sm bg-down px-4 text-[13px] font-semibold text-white hover:brightness-110 disabled:cursor-not-allowed disabled:opacity-45"
            @click="confirmDelete"
          >
            {{ busy ? "Deleting…" : "Delete project" }}
          </button>
        </div>
      </div>
    </div>
  </div>
</template>
