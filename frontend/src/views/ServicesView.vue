<script setup lang="ts">
import { computed, onMounted, ref, watch } from "vue";
import { useRouter } from "vue-router";

import { api } from "@/api/client";
import type { components } from "@/api/schema";
import AppShell from "@/components/AppShell.vue";
import { useSession } from "@/stores/session";
import { useWorkspace } from "@/stores/workspace";
import { lagLabel, sealedLabel } from "@/lib/services";

type Summary = components["schemas"]["ServiceSummary"];

const ws = useWorkspace();
const session = useSession();
const router = useRouter();

const loading = ref(true);
const rows = ref<Summary[]>([]);
const error = ref("");

const canWrite = computed(() => session.canProjectWrite(ws.orgId, ws.projectId));

async function load() {
  loading.value = true;
  error.value = "";
  try {
    await ws.init();
    if (!ws.projectId) {
      rows.value = [];
      return;
    }
    const res = await api.GET("/api/v1/projects/{projectID}/services", {
      params: { path: { projectID: ws.projectId } },
    });
    rows.value = res.data ?? [];
    ws.noteServiceCount(ws.projectId, rows.value.length);
  } catch {
    error.value = "Could not load services.";
  } finally {
    loading.value = false;
  }
}

// Create is a slug + name, nothing else: a service with no declaration is a complete state,
// so there is no wizard to walk through before the row exists.
const creating = ref(false);
const newSlug = ref("");
const newName = ref("");
const createError = ref("");
const saving = ref(false);

function openCreate() {
  newSlug.value = "";
  newName.value = "";
  createError.value = "";
  creating.value = true;
}

async function create() {
  if (saving.value) return;
  saving.value = true;
  createError.value = "";
  const res = await api.POST("/api/v1/projects/{projectID}/services", {
    params: { path: { projectID: ws.projectId } },
    body: { slug: newSlug.value.trim(), name: newName.value.trim() || undefined },
  });
  saving.value = false;
  if (res.error || !res.data) {
    createError.value = (res.error as { error?: string })?.error || "Could not create the service.";
    return;
  }
  creating.value = false;
  await router.push({ name: "service", params: { id: res.data.id! } });
}

onMounted(load);
watch(() => ws.projectId, load);
</script>

<template>
  <AppShell active="services" :crumbs="[ws.orgName || 'cerbix', ws.projectName || '…', 'Services']">
    <template #actions>
      <button
        v-if="canWrite"
        type="button"
        class="flex h-[34px] items-center gap-[7px] rounded-sm bg-accent px-[13px] text-[13px] font-medium text-accent-ink hover:bg-accent-2"
        @click="openCreate"
      >
        <svg viewBox="0 0 24 24" class="h-[15px] w-[15px]" fill="none" stroke="currentColor" stroke-width="2.2"><path d="M12 5v14M5 12h14" /></svg>
        New service
      </button>
    </template>

    <div class="mx-auto max-w-[1180px] px-[22px] pb-16 pt-[26px]">
      <div class="mb-[22px]">
        <h1 class="text-[21px] font-semibold tracking-tight">Services</h1>
        <p class="mt-[3px] text-[13px] text-ink-3">
          <span v-if="loading">Loading…</span>
          <span v-else>
            {{ rows.length }} {{ rows.length === 1 ? "service" : "services" }} · {{ ws.projectName }} — a service
            is where it is declared what reliability <i>means</i> for one operational unit.
          </span>
        </p>
      </div>

      <div v-if="error" class="rounded border border-down/40 bg-down-weak p-4 text-[13px] text-down">{{ error }}</div>

      <section v-if="!error" class="overflow-hidden rounded border border-border bg-surface shadow-card">
        <div class="overflow-x-auto">
          <table class="w-full text-[13px]">
            <thead>
              <tr class="text-[10.5px] uppercase tracking-[0.06em] text-ink-3">
                <th class="border-b border-border px-4 py-[10px] text-left" style="width: 34%">Service</th>
                <th class="border-b border-border px-4 py-[10px] text-left">Declaration</th>
                <th class="border-b border-border px-4 py-[10px] text-left">Reliability inputs</th>
                <th class="border-b border-border px-4 py-[10px] text-left" style="width: 24%">Sealed through</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="r in rows" :key="r.service.id" class="group hover:bg-surface-2">
                <td class="border-b border-border px-4 py-[11px]">
                  <RouterLink :to="{ name: 'service', params: { id: r.service.id } }" class="font-medium text-ink hover:text-accent">
                    {{ r.service.name }}
                  </RouterLink>
                  <span
                    v-if="r.service.managed_by"
                    class="ml-[6px] inline-flex items-center gap-[4px] rounded-full bg-inset px-[7px] py-px font-mono text-[10.5px] text-ink-2"
                    :title="'Managed by file provider ' + r.service.managed_by + ' — read-only in the UI'"
                  >
                    <svg viewBox="0 0 24 24" class="h-[11px] w-[11px]" fill="none" stroke="currentColor" stroke-width="2"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><path d="M14 2v6h6"/></svg>
                    file
                  </span>
                  <div class="mt-[2px] font-mono text-[11.5px] text-ink-3">{{ r.service.slug }}</div>
                </td>

                <td class="border-b border-border px-4 py-[11px]">
                  <template v-if="r.revision">
                    <span class="font-mono text-[12.5px] text-ink-2">rev {{ r.revision }}</span>
                    <span v-if="r.epoch_seq" class="ml-[6px] rounded-xs border border-border px-[6px] py-px font-mono text-[10.5px] text-ink-3">epoch {{ r.epoch_seq }}</span>
                  </template>
                  <span v-else class="text-[12.5px] text-ink-3">not declared</span>
                </td>

                <!-- Two INDEPENDENT counts. Equal counts are legitimate; what matters is that
                     the reader can see they were declared separately. -->
                <td class="border-b border-border px-4 py-[11px]">
                  <template v-if="r.revision">
                    <span class="font-mono text-[12.5px]">{{ r.sli_members }}</span>
                    <span class="text-ink-3"> SLI of </span>
                    <span class="font-mono text-[12.5px]">{{ r.context_members }}</span>
                    <span class="text-ink-3"> monitors</span>
                    <div v-if="!r.sli_members" class="mt-[2px] text-[11.5px] text-ink-3">
                      operational context only — reports no availability
                    </div>
                  </template>
                  <span v-else class="text-[12.5px] text-ink-3">—</span>
                </td>

                <!-- The watermark, not a timeline. It is contiguity-defined, so a service that
                     fell behind shows a lagging timestamp instead of a plausible picture. -->
                <td class="border-b border-border px-4 py-[11px]">
                  <template v-if="r.sealed_through">
                    <div class="font-mono text-[12.5px]">{{ sealedLabel(r.sealed_through) }}</div>
                    <div v-if="lagLabel(r.sealed_through)" class="mt-[2px] text-[11.5px] text-degraded">
                      {{ lagLabel(r.sealed_through) }} behind
                    </div>
                  </template>
                  <span v-else class="text-[12.5px] text-ink-3">not materialized yet</span>
                  <div v-if="r.repairing_count" class="mt-[3px] text-[11.5px] text-degraded">
                    {{ r.repairing_count }} range{{ r.repairing_count === 1 ? "" : "s" }} repairing
                  </div>
                </td>
              </tr>

              <tr v-if="!rows.length && !loading">
                <td colspan="4" class="px-4 py-10 text-center text-[13px] text-ink-3">
                  No services yet.
                  <button v-if="canWrite" type="button" class="text-accent hover:underline" @click="openCreate">Declare the first one.</button>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </section>

      <p class="mt-3 text-[12px] text-ink-3">
        Availability, error budget and burn rate arrive with the next iteration. This release declares what they
        will measure and how far materialization has got — no number is shown that nothing has computed.
      </p>
    </div>

    <!-- create dialog -->
    <div v-if="creating" class="fixed inset-0 z-40 grid place-items-center bg-black/40 p-4" @click.self="creating = false">
      <div class="w-full max-w-[420px] rounded border border-border-strong bg-surface p-5 shadow-lg">
        <h2 class="text-[15px] font-semibold">New service</h2>
        <p class="mt-1 text-[12.5px] text-ink-3">
          The slug is project-unique and immutable — it is what a bundle and a dashboard both reference.
        </p>
        <form class="mt-4 flex flex-col gap-3" @submit.prevent="create">
          <label class="flex flex-col gap-[5px]">
            <span class="text-[11px] font-semibold uppercase tracking-[0.06em] text-ink-3">Slug</span>
            <input
              v-model="newSlug"
              class="h-[34px] rounded-sm border border-border bg-surface-2 px-[10px] font-mono text-[13px] outline-none focus:border-accent"
              placeholder="checkout"
              pattern="[a-z][a-z0-9-]{0,62}"
              required
              autofocus
            />
          </label>
          <label class="flex flex-col gap-[5px]">
            <span class="text-[11px] font-semibold uppercase tracking-[0.06em] text-ink-3">Name</span>
            <input
              v-model="newName"
              class="h-[34px] rounded-sm border border-border bg-surface-2 px-[10px] text-[13px] outline-none focus:border-accent"
              placeholder="Checkout"
            />
          </label>
          <p v-if="createError" class="text-[12.5px] text-down">{{ createError }}</p>
          <div class="mt-1 flex justify-end gap-2">
            <button type="button" class="h-[32px] rounded-sm border border-border px-3 text-[13px] text-ink-2 hover:border-border-strong" @click="creating = false">Cancel</button>
            <button type="submit" :disabled="saving" class="h-[32px] rounded-sm bg-accent px-3 text-[13px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-60">
              {{ saving ? "Creating…" : "Create service" }}
            </button>
          </div>
        </form>
      </div>
    </div>
  </AppShell>
</template>
