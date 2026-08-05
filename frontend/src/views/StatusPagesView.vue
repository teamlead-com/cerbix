<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from "vue";
import { api } from "@/api/client";
import type { components } from "@/api/schema";
import AppShell from "@/components/AppShell.vue";
import { useSession } from "@/stores/session";
import { useWorkspace } from "@/stores/workspace";
import { componentMeta } from "@/lib/statuspage";

type StatusPage = components["schemas"]["StatusPage"];
type Component = components["schemas"]["Component"];
type Monitor = components["schemas"]["Monitor"];
type Visibility = "public" | "internal" | "unlisted";

const ws = useWorkspace();
const session = useSession();
// Status pages are org-managed.
const canManage = computed(() => session.isOrgAdmin(ws.orgId));
const loading = ref(true);
const error = ref("");
const pages = ref<StatusPage[]>([]);
const monitors = ref<Monitor[]>([]);

const selected = ref<StatusPage | null>(null);
const componentsList = ref<Component[]>([]);

async function loadPages() {
  loading.value = true;
  error.value = "";
  try {
    await ws.init();
    if (!ws.orgId) {
      pages.value = [];
      return;
    }
    const [pg, mon] = await Promise.all([
      api.GET("/api/v1/organizations/{orgID}/status-pages", { params: { path: { orgID: ws.orgId } } }),
      ws.projectId
        ? api.GET("/api/v1/projects/{projectID}/monitors", { params: { path: { projectID: ws.projectId } } })
        : Promise.resolve({ data: [] as Monitor[] }),
    ]);
    pages.value = pg.data ?? [];
    monitors.value = mon.data ?? [];
    if (selected.value) select(pages.value.find((p) => p.id === selected.value?.id) ?? null);
  } catch {
    error.value = "Could not load status pages.";
  } finally {
    loading.value = false;
  }
}

async function select(page: StatusPage | null) {
  selected.value = page;
  componentsList.value = [];
  editing.value = false;
  confirmDelete.value = false;
  if (!page) return;
  const res = await api.GET("/api/v1/status-pages/{pageID}/components", { params: { path: { pageID: page.id! } } });
  componentsList.value = res.data ?? [];
}

// Create page.
const showCreate = ref(false);
const pageForm = reactive({ slug: "", title: "", visibility: "internal" as Visibility });
const creating = ref(false);
const createError = ref("");
const canCreatePage = computed(() => !!pageForm.slug.trim() && !!pageForm.title.trim() && !creating.value);

async function createPage() {
  if (!canCreatePage.value || !ws.orgId) return;
  creating.value = true;
  createError.value = "";
  try {
    const res = await api.POST("/api/v1/organizations/{orgID}/status-pages", {
      params: { path: { orgID: ws.orgId } },
      body: { slug: pageForm.slug.trim(), title: pageForm.title.trim(), visibility: pageForm.visibility },
    });
    if (res.error || !res.data) {
      createError.value = (res.error as { error?: string })?.error || "Could not create the page.";
      return;
    }
    pages.value.unshift(res.data);
    showCreate.value = false;
    pageForm.slug = "";
    pageForm.title = "";
    pageForm.visibility = "internal";
    await select(res.data);
  } finally {
    creating.value = false;
  }
}

// Add component.
const compForm = reactive({ name: "", monitor_id: "", manual_status: "" });
const addingComp = ref(false);
const compError = ref("");

async function addComponent() {
  if (!selected.value || !compForm.name.trim()) return;
  addingComp.value = true;
  compError.value = "";
  const body: components["schemas"]["CreateComponent"] = { name: compForm.name.trim() };
  if (compForm.monitor_id) body.monitor_id = compForm.monitor_id;
  if (compForm.manual_status) body.manual_status = compForm.manual_status as Component["manual_status"];
  try {
    const res = await api.POST("/api/v1/status-pages/{pageID}/components", {
      params: { path: { pageID: selected.value.id! } },
      body,
    });
    if (res.error || !res.data) {
      compError.value = (res.error as { error?: string })?.error || "Could not add the component.";
      return;
    }
    componentsList.value.push(res.data);
    compForm.name = "";
    compForm.monitor_id = "";
    compForm.manual_status = "";
  } finally {
    addingComp.value = false;
  }
}

async function deleteComponent(id: string) {
  const res = await api.DELETE("/api/v1/components/{componentID}", { params: { path: { componentID: id } } });
  if (!res.error) componentsList.value = componentsList.value.filter((c) => c.id !== id);
}

// Edit page (title + visibility; slug/org immutable).
const editing = ref(false);
const editForm = reactive({ title: "", visibility: "internal" as Visibility });
const savingEdit = ref(false);
const editError = ref("");

function startEdit() {
  if (!selected.value) return;
  editForm.title = selected.value.title ?? "";
  editForm.visibility = (selected.value.visibility as Visibility) ?? "internal";
  editError.value = "";
  editing.value = true;
}

async function saveEdit() {
  if (!selected.value || !editForm.title.trim()) return;
  savingEdit.value = true;
  editError.value = "";
  try {
    const res = await api.PATCH("/api/v1/status-pages/{pageID}", {
      params: { path: { pageID: selected.value.id! } },
      body: { title: editForm.title.trim(), visibility: editForm.visibility },
    });
    if (res.error || !res.data) {
      editError.value = (res.error as { error?: string })?.error || "Could not save the page.";
      return;
    }
    const idx = pages.value.findIndex((p) => p.id === res.data!.id);
    if (idx >= 0) pages.value[idx] = res.data;
    selected.value = res.data;
    editing.value = false;
  } finally {
    savingEdit.value = false;
  }
}

// Delete page (components cascade server-side).
const confirmDelete = ref(false);
const deleting = ref(false);

async function removePage() {
  if (!selected.value) return;
  deleting.value = true;
  try {
    const id = selected.value.id!;
    const res = await api.DELETE("/api/v1/status-pages/{pageID}", { params: { path: { pageID: id } } });
    if (!res.error) {
      pages.value = pages.value.filter((p) => p.id !== id);
      selected.value = null;
      componentsList.value = [];
      editing.value = false;
    }
  } finally {
    deleting.value = false;
    confirmDelete.value = false;
  }
}

const publicPath = computed(() => (selected.value ? `/status/${selected.value.slug}` : ""));
const monitorName = (id?: string) => monitors.value.find((m) => m.id === id)?.name ?? "—";

onMounted(loadPages);
watch(() => ws.orgId, loadPages);
</script>

<template>
  <AppShell active="status" :crumbs="[ws.orgName || 'cerbix', 'Status pages']">
    <template #actions>
      <button v-if="canManage" type="button" class="flex h-[34px] items-center gap-[7px] rounded-sm bg-accent px-[13px] text-[13px] font-medium text-accent-ink hover:bg-accent-2" @click="showCreate = !showCreate">
        <svg viewBox="0 0 24 24" class="h-[15px] w-[15px]" fill="none" stroke="currentColor" stroke-width="2.2"><path d="M12 5v14M5 12h14" /></svg>
        New page
      </button>
    </template>

    <div class="mx-auto max-w-[1180px] px-[22px] pb-16 pt-[26px]">
      <div class="mb-[22px]">
        <h1 class="text-[21px] font-semibold tracking-tight">Status pages</h1>
        <p class="mt-[3px] text-[13px] text-ink-3">{{ ws.orgName }} · {{ pages.length }} pages</p>
      </div>

      <div v-if="error" class="rounded border border-down/40 bg-down-weak p-4 text-[13px] text-down">{{ error }}</div>

      <!-- create -->
      <div v-if="showCreate && canManage" class="mb-5 flex flex-col gap-3 rounded border border-border bg-surface p-4 shadow-card">
        <div class="grid grid-cols-3 gap-3 max-[720px]:grid-cols-1">
          <label class="flex flex-col gap-[6px]">
            <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Slug</span>
            <input v-model="pageForm.slug" type="text" placeholder="acme-status" class="rounded-sm border border-border bg-surface-2 px-3 py-2 font-mono text-[13px] outline-none focus:border-accent" />
          </label>
          <label class="flex flex-col gap-[6px]">
            <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Title</span>
            <input v-model="pageForm.title" type="text" placeholder="Acme Status" class="rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent" />
          </label>
          <label class="flex flex-col gap-[6px]">
            <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Visibility</span>
            <select v-model="pageForm.visibility" class="rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent">
              <option value="internal">Internal (members only)</option>
              <option value="public">Public</option>
              <option value="unlisted">Unlisted (secret link)</option>
            </select>
          </label>
        </div>
        <div v-if="createError" class="text-[12.5px] text-down">{{ createError }}</div>
        <div>
          <button type="button" :disabled="!canCreatePage" class="h-[34px] rounded-sm bg-accent px-4 text-[13px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-50" @click="createPage">
            {{ creating ? "Creating…" : "Create" }}
          </button>
        </div>
      </div>

      <div class="grid grid-cols-[280px_1fr] gap-5 max-[900px]:grid-cols-1">
        <!-- page list -->
        <section class="h-fit overflow-hidden rounded border border-border bg-surface shadow-card">
          <div class="border-b border-border px-4 py-[11px] text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Pages</div>
          <button
            v-for="p in pages"
            :key="p.id"
            type="button"
            class="flex w-full flex-col gap-[2px] border-b border-border px-4 py-[11px] text-left last:border-b-0 hover:bg-surface-2"
            :class="selected?.id === p.id ? 'bg-accent-weak' : ''"
            @click="select(p)"
          >
            <span class="text-[13px] font-medium">{{ p.title }}</span>
            <span class="font-mono text-[11px] text-ink-3">/{{ p.slug }} · {{ p.visibility }}</span>
          </button>
          <p v-if="!pages.length && !loading" class="px-4 py-6 text-center text-[13px] text-ink-3">No status pages yet.</p>
        </section>

        <!-- selected page -->
        <section v-if="selected" class="flex flex-col gap-4">
          <div class="rounded border border-border bg-surface p-4 shadow-card">
            <div class="flex flex-wrap items-center gap-2">
              <h2 class="text-[16px] font-semibold">{{ selected.title }}</h2>
              <span class="rounded-full border border-border px-[8px] py-px text-[11px] text-ink-2">{{ selected.visibility }}</span>
              <div v-if="canManage && !confirmDelete" class="ml-auto flex items-center gap-2">
                <button type="button" class="inline-flex h-[30px] items-center gap-[6px] rounded-sm border border-border px-[10px] text-[12.5px] text-ink-2 hover:border-border-strong" @click="editing ? (editing = false) : startEdit()">
                  <svg viewBox="0 0 24 24" class="h-[14px] w-[14px]" fill="none" stroke="currentColor" stroke-width="2"><path d="M4 20h4L18.5 9.5a2.1 2.1 0 0 0-3-3L5 17v3z" /></svg>
                  {{ editing ? "Close" : "Edit" }}
                </button>
                <button type="button" class="inline-flex h-[30px] items-center rounded-sm border border-border px-[10px] text-[12.5px] text-ink-2 hover:border-down/60 hover:text-down" @click="confirmDelete = true">Delete</button>
              </div>
              <div v-else-if="canManage && confirmDelete" class="ml-auto flex items-center gap-2">
                <span class="text-[12px] text-ink-3">Delete this page?</span>
                <button type="button" class="h-[30px] rounded-sm bg-down px-[11px] text-[12.5px] font-medium text-white hover:opacity-90 disabled:opacity-50" :disabled="deleting" @click="removePage">{{ deleting ? "Deleting…" : "Confirm" }}</button>
                <button type="button" class="h-[30px] rounded-sm border border-border px-[11px] text-[12.5px] text-ink-2 hover:border-border-strong" @click="confirmDelete = false">Cancel</button>
              </div>
            </div>

            <!-- edit form -->
            <div v-if="editing && canManage" class="mt-3 flex flex-wrap items-end gap-3 rounded-sm border border-border bg-surface-2 p-3">
              <label class="flex flex-col gap-[6px]">
                <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Title</span>
                <input v-model="editForm.title" type="text" class="w-[200px] rounded-sm border border-border bg-surface px-3 py-2 text-[13px] outline-none focus:border-accent" />
              </label>
              <label class="flex flex-col gap-[6px]">
                <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Visibility</span>
                <select v-model="editForm.visibility" class="w-[210px] rounded-sm border border-border bg-surface px-3 py-2 text-[13px] outline-none focus:border-accent">
                  <option value="internal">Internal (members only)</option>
                  <option value="public">Public</option>
                  <option value="unlisted">Unlisted (secret link)</option>
                </select>
              </label>
              <button type="button" :disabled="savingEdit || !editForm.title.trim()" class="h-[38px] rounded-sm bg-accent px-4 text-[13px] font-medium text-accent-ink hover:bg-accent-2 disabled:opacity-50" @click="saveEdit">{{ savingEdit ? "Saving…" : "Save" }}</button>
              <span v-if="editError" class="self-center text-[12.5px] text-down">{{ editError }}</span>
              <p class="w-full text-[11.5px] text-ink-3">The slug (public URL) can't be changed.</p>
            </div>

            <div class="mt-2 flex flex-wrap gap-x-4 gap-y-1 text-[12.5px]">
              <RouterLink :to="publicPath" class="text-accent hover:underline">View public page ↗</RouterLink>
              <a :href="`/api/v1/public/status-pages/${selected.slug}/feed?format=rss`" class="font-mono text-ink-3 hover:text-accent">rss</a>
              <a :href="`/api/v1/public/status-pages/${selected.slug}/feed?format=atom`" class="font-mono text-ink-3 hover:text-accent">atom</a>
              <a :href="`/api/v1/public/status-pages/${selected.slug}/feed?format=json`" class="font-mono text-ink-3 hover:text-accent">json</a>
            </div>
            <p v-if="selected.unlisted_token" class="mt-2 font-mono text-[11.5px] text-ink-3">unlisted token: {{ selected.unlisted_token }}</p>
          </div>

          <!-- components -->
          <div class="rounded border border-border bg-surface shadow-card">
            <div class="border-b border-border px-4 py-[11px] text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Components</div>
            <ul>
              <li v-for="c in componentsList" :key="c.id" class="flex items-center gap-3 border-b border-border px-4 py-[11px] last:border-b-0">
                <div class="min-w-0">
                  <div class="text-[13px] font-medium">{{ c.name }}</div>
                  <div class="font-mono text-[11px] text-ink-3">{{ c.monitor_id ? "monitor: " + monitorName(c.monitor_id) : "manual: " + (c.manual_status || "operational") }}</div>
                </div>
                <button v-if="canManage" type="button" class="ml-auto text-ink-3 hover:text-down" aria-label="Delete component" @click="deleteComponent(c.id!)">
                  <svg viewBox="0 0 24 24" class="h-[15px] w-[15px]" fill="none" stroke="currentColor" stroke-width="2"><path d="M6 6l12 12M18 6L6 18" /></svg>
                </button>
              </li>
              <li v-if="!componentsList.length" class="px-4 py-5 text-center text-[13px] text-ink-3">No components yet.</li>
            </ul>
            <div v-if="canManage" class="flex flex-wrap items-end gap-3 border-t border-border p-4">
              <label class="flex flex-col gap-[6px]">
                <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Name</span>
                <input v-model="compForm.name" type="text" placeholder="API" class="w-[160px] rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent" />
              </label>
              <label class="flex flex-col gap-[6px]">
                <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Monitor</span>
                <select v-model="compForm.monitor_id" class="w-[180px] rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent">
                  <option value="">— manual —</option>
                  <option v-for="m in monitors" :key="m.id" :value="m.id">{{ m.name }}</option>
                </select>
              </label>
              <label v-if="!compForm.monitor_id" class="flex flex-col gap-[6px]">
                <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Manual status</span>
                <select v-model="compForm.manual_status" class="w-[170px] rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent">
                  <option value="">operational</option>
                  <option value="degraded">{{ componentMeta("degraded").label }}</option>
                  <option value="partial_outage">{{ componentMeta("partial_outage").label }}</option>
                  <option value="major_outage">{{ componentMeta("major_outage").label }}</option>
                  <option value="maintenance">{{ componentMeta("maintenance").label }}</option>
                </select>
              </label>
              <button type="button" :disabled="addingComp || !compForm.name.trim()" class="h-[38px] rounded-sm border border-border px-4 text-[13px] hover:border-accent hover:text-accent disabled:opacity-50" @click="addComponent">Add</button>
            </div>
            <div v-if="compError" class="px-4 pb-3 text-[12.5px] text-down">{{ compError }}</div>
          </div>
        </section>

        <section v-else class="grid place-items-center rounded border border-border bg-surface p-10 text-[13px] text-ink-3 shadow-card">
          Select a page to manage its components.
        </section>
      </div>
    </div>
  </AppShell>
</template>
