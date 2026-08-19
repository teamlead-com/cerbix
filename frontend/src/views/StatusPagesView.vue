<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from "vue";
import { api } from "@/api/client";
import type { components } from "@/api/schema";
import AppShell from "@/components/AppShell.vue";
import { useSession } from "@/stores/session";
import { useWorkspace } from "@/stores/workspace";
import { componentMeta, reasonText, sourceLabel, summaryHeadline } from "@/lib/statuspage";

type StatusPage = components["schemas"]["StatusPage"];
type Component = components["schemas"]["Component"];
type Monitor = components["schemas"]["Monitor"];
type Service = components["schemas"]["Service"];
type ConversionPreview = components["schemas"]["ConversionPreview"];
type ComponentSource = "monitor" | "service" | "manual";
type Visibility = "public" | "internal" | "unlisted";

const ws = useWorkspace();
const session = useSession();
// Status pages are org-managed.
const canManage = computed(() => session.isOrgAdmin(ws.orgId));
const loading = ref(true);
const error = ref("");
const pages = ref<StatusPage[]>([]);
const monitors = ref<Monitor[]>([]);
const services = ref<Service[]>([]);

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
    const [pg, mon, svc] = await Promise.all([
      api.GET("/api/v1/organizations/{orgID}/status-pages", { params: { path: { orgID: ws.orgId } } }),
      ws.projectId
        ? api.GET("/api/v1/projects/{projectID}/monitors", { params: { path: { projectID: ws.projectId } } })
        : Promise.resolve({ data: [] as Monitor[] }),
      // Services are the third component source (FR-021 §15.0). Loaded from the SELECTED project:
      // a service component binds a project, and offering another project's services here would
      // build a component the page-scope rule then refuses at write time.
      ws.projectId
        ? api.GET("/api/v1/projects/{projectID}/services", { params: { path: { projectID: ws.projectId } } })
        : Promise.resolve({ data: [] as Service[] }),
    ]);
    pages.value = pg.data ?? [];
    monitors.value = mon.data ?? [];
    services.value = (svc.data as Service[] | undefined) ?? [];
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
  subscribers.value = [];
  editing.value = false;
  confirmDelete.value = false;
  if (!page) return;
  const res = await api.GET("/api/v1/status-pages/{pageID}/components", { params: { path: { pageID: page.id! } } });
  componentsList.value = res.data ?? [];
  if (canManage.value) {
    const subs = await api.GET("/api/v1/status-pages/{pageID}/subscribers", { params: { path: { pageID: page.id! } } });
    subscribers.value = subs.data ?? [];
  }
}

// Subscribers (org admin): who receives incident emails for this page.
const subscribers = ref<components["schemas"]["Subscriber"][]>([]);
const confirmRemoveSub = ref("");
const confirmedCount = computed(() => subscribers.value.filter((s) => s.confirmed_at).length);
async function removeSubscriber(id: string) {
  if (!selected.value) return;
  const res = await api.DELETE("/api/v1/status-pages/{pageID}/subscribers/{subscriberID}", {
    params: { path: { pageID: selected.value.id!, subscriberID: id } },
  });
  if (!res.error) subscribers.value = subscribers.value.filter((s) => s.id !== id);
  confirmRemoveSub.value = "";
}
const fmtSubDate = (ts?: string) => (ts ? new Date(ts).toISOString().slice(0, 10) : "—");

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
const compForm = reactive({
  source: "monitor" as ComponentSource,
  name: "",
  monitor_id: "",
  service_id: "",
  manual_status: "",
  group: "",
  description: "",
  position: 0,
});
// Groups already in use on this page — offered as datalist suggestions.
const usedGroups = computed(() => [...new Set(componentsList.value.map((c) => c.group).filter(Boolean))] as string[]);
const addingComp = ref(false);
const compError = ref("");

async function addComponent() {
  if (!selected.value || !compForm.name.trim()) return;
  addingComp.value = true;
  compError.value = "";
  const body: components["schemas"]["CreateComponent"] = { name: compForm.name.trim() };
  // Exactly the binding the chosen source needs. The server DERIVES `source` from what it
  // receives, so sending a leftover id from another source would describe a different component
  // than the form shows.
  if (compForm.source === "monitor" && compForm.monitor_id) body.monitor_id = compForm.monitor_id;
  if (compForm.source === "service" && compForm.service_id) body.service_id = compForm.service_id;
  if (compForm.source === "manual" && compForm.manual_status) {
    body.manual_status = compForm.manual_status as Component["manual_status"];
  }
  if (compForm.group.trim()) body.group = compForm.group.trim();
  if (compForm.description.trim()) body.description = compForm.description.trim();
  if (compForm.position) body.position = compForm.position;
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
    compForm.service_id = "";
    compForm.manual_status = "";
    compForm.group = "";
    compForm.description = "";
    compForm.position = 0;
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

// Feed link that actually opens, mirroring publicPath: unlisted carries its
// token, internal uses the authed feed endpoint (members only).
function feedHref(fmt: string): string {
  const p = selected.value;
  if (!p) return "";
  if (p.visibility === "internal") return `/api/v1/status-pages/${p.id}/feed?format=${fmt}`;
  const tok = p.visibility === "unlisted" && p.unlisted_token ? `&token=${p.unlisted_token}` : "";
  return `/api/v1/public/status-pages/${p.slug}/feed?format=${fmt}${tok}`;
}

// Preview path that actually opens: unlisted carries its token, internal falls
// back to the authed render endpoint via ?preview=<pageID> (members only).
const publicPath = computed(() => {
  const p = selected.value;
  if (!p) return "";
  const base = `/status/${p.slug}`;
  if (p.visibility === "unlisted" && p.unlisted_token) return `${base}?token=${p.unlisted_token}`;
  if (p.visibility === "internal") return `${base}?preview=${p.id}`;
  return base;
});
const monitorName = (id?: string) => monitors.value.find((m) => m.id === id)?.name ?? "—";
const serviceName = (id?: string) => services.value.find((sv) => sv.id === id)?.name ?? "—";

// What the line REPORTS, from the active source only. The dormant binding is shown separately
// below it, never mixed in: a component that renders a service while still holding a monitor id is
// the normal state after a conversion, and conflating the two is how an operator ends up reading
// the wrong fact.
function activeBinding(c: Component): string {
  switch (c.source) {
    case "monitor":
      return "monitor: " + monitorName(c.monitor_id);
    case "service":
      return "service: " + serviceName(c.service_id);
    default:
      return c.manual_status ? "manual: " + componentMeta(c.manual_status).label : "manual: no status set";
  }
}

// The binding a revert would restore without the operator choosing it again.
function dormantBinding(c: Component): string {
  if (c.source !== "monitor" && c.monitor_id) return "monitor: " + monitorName(c.monitor_id);
  if (c.source !== "service" && c.service_id) return "service: " + serviceName(c.service_id);
  if (c.source !== "manual" && c.manual_status) return "manual: " + componentMeta(c.manual_status).label;
  return "";
}

// ── Conversion (FR-021 §15.0): preview, consent, confirm ──────────────────────────────────
//
// The two CAS tokens live INSIDE the preview object, never in separate refs: they are only
// meaningful as the pair the server issued, and a stale token that survives a re-preview in a
// stray ref is exactly the bug the fence exists to catch.
const convertFor = ref<Component | null>(null);
const convertTarget = reactive({ source: "service" as ComponentSource, service_id: "", monitor_id: "", manual_status: "" });
const convertPreview = ref<ConversionPreview | null>(null);
const convertBusy = ref(false);
const convertError = ref("");

function startConvert(c: Component) {
  convertFor.value = c;
  convertPreview.value = null;
  convertError.value = "";
  // Default to a source the component is NOT already on, so the dialog opens on a real choice.
  convertTarget.source = c.source === "service" ? "manual" : "service";
  convertTarget.service_id = c.service_id ?? "";
  convertTarget.monitor_id = c.monitor_id ?? "";
  convertTarget.manual_status = (c.manual_status as string) ?? "";
}

function cancelConvert() {
  convertFor.value = null;
  convertPreview.value = null;
  convertError.value = "";
}

function convertBody() {
  const body: Record<string, unknown> = { source: convertTarget.source };
  // An empty id is sent as OMITTED, not as "": the server reads an absent id as "the dormant
  // binding is the target", which is what makes a revert a single click.
  if (convertTarget.source === "service" && convertTarget.service_id) body.service_id = convertTarget.service_id;
  if (convertTarget.source === "monitor" && convertTarget.monitor_id) body.monitor_id = convertTarget.monitor_id;
  if (convertTarget.source === "manual" && convertTarget.manual_status) body.manual_status = convertTarget.manual_status;
  return body;
}

async function runPreview() {
  if (!convertFor.value) return;
  convertBusy.value = true;
  convertError.value = "";
  convertPreview.value = null;
  try {
    const res = await api.POST("/api/v1/components/{componentID}/conversion/preview", {
      params: { path: { componentID: convertFor.value.id! } },
      body: convertBody() as never,
    });
    if (res.error || !res.data) {
      convertError.value = (res.error as { error?: string })?.error || "Could not preview the change.";
      return;
    }
    convertPreview.value = res.data;
  } finally {
    convertBusy.value = false;
  }
}

async function confirmConvert() {
  const preview = convertPreview.value;
  const target = convertFor.value;
  // No preview, no confirmation: the button is disabled too, but the guard is here as well
  // because a confirmation without the issued tokens is an unpreviewed conversion.
  if (!preview || !target) return;
  convertBusy.value = true;
  convertError.value = "";
  try {
    const res = await api.POST("/api/v1/components/{componentID}/conversion", {
      params: { path: { componentID: target.id! } },
      body: { ...convertBody(), revision: preview.revision, page_generation: preview.page_generation } as never,
    });
    if (res.error || !res.data) {
      const msg = (res.error as { error?: string })?.error ?? "";
      convertError.value = msg.includes("page_configuration_stale")
        ? "This page changed while you were looking at the preview. Preview again to see the current state."
        : msg || "Could not apply the change.";
      // A stale preview is DISCARDED rather than kept for a retry: re-confirming with the same
      // tokens would fail identically, and leaving them on screen invites exactly that.
      convertPreview.value = null;
      return;
    }
    const i = componentsList.value.findIndex((c) => c.id === res.data!.id);
    if (i >= 0) componentsList.value[i] = res.data;
    cancelConvert();
  } finally {
    convertBusy.value = false;
  }
}

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
              <a :href="feedHref('rss')" class="font-mono text-ink-3 hover:text-accent">rss</a>
              <a :href="feedHref('atom')" class="font-mono text-ink-3 hover:text-accent">atom</a>
              <a :href="feedHref('json')" class="font-mono text-ink-3 hover:text-accent">json</a>
            </div>
            <p v-if="selected.unlisted_token" class="mt-2 font-mono text-[11.5px] text-ink-3">unlisted token: {{ selected.unlisted_token }}</p>
          </div>

          <!-- components -->
          <div class="rounded border border-border bg-surface shadow-card">
            <div class="border-b border-border px-4 py-[11px] text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Components</div>
            <ul>
              <li v-for="c in componentsList" :key="c.id" class="flex items-center gap-3 border-b border-border px-4 py-[11px] last:border-b-0" data-testid="component-row">
                <div class="min-w-0">
                  <div class="flex items-center gap-2 text-[13px] font-medium">
                    {{ c.name }}
                    <span class="rounded-full border border-border px-[7px] py-[1px] font-mono text-[10px] uppercase tracking-[0.06em] text-ink-3" data-testid="component-source">{{ sourceLabel(c.source) }}</span>
                  </div>
                  <div class="font-mono text-[11px] text-ink-3" data-testid="component-binding">{{ activeBinding(c) }}</div>
                  <!-- The dormant binding, stated as dormant. It is what a revert restores, and
                       leaving it unlabelled would read as a second live source. -->
                  <div v-if="dormantBinding(c)" class="font-mono text-[11px] text-ink-3" data-testid="component-dormant">
                    kept for revert · {{ dormantBinding(c) }}
                  </div>
                </div>
                <div v-if="canManage" class="ml-auto flex items-center gap-3">
                  <button type="button" class="text-[12px] text-ink-3 hover:text-accent" data-testid="convert-component" @click="startConvert(c)">Change source</button>
                  <button type="button" class="text-ink-3 hover:text-down" aria-label="Delete component" @click="deleteComponent(c.id!)">
                    <svg viewBox="0 0 24 24" class="h-[15px] w-[15px]" fill="none" stroke="currentColor" stroke-width="2"><path d="M6 6l12 12M18 6L6 18" /></svg>
                  </button>
                </div>
              </li>
              <li v-if="!componentsList.length" class="px-4 py-5 text-center text-[13px] text-ink-3">No components yet.</li>
            </ul>

            <!-- Conversion (FR-021 §15.0): the operator consents to the page as it WILL read,
                 not to a form. The confirm button stays disabled until a preview exists, because
                 the two CAS tokens come from the preview and nothing else. -->
            <div v-if="convertFor" class="border-t border-border bg-surface-2 p-4" data-testid="conversion-dialog">
              <div class="mb-3 text-[13px] font-medium">Change what “{{ convertFor.name }}” reports</div>
              <div class="flex flex-wrap items-end gap-3">
                <label class="flex flex-col gap-[6px]">
                  <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">New source</span>
                  <select v-model="convertTarget.source" class="w-[130px] rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent" data-testid="conversion-source" @change="convertPreview = null">
                    <option value="monitor">Monitor</option>
                    <option value="service">Service</option>
                    <option value="manual">Manual</option>
                  </select>
                </label>
                <label v-if="convertTarget.source === 'service'" class="flex flex-col gap-[6px]">
                  <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Service</span>
                  <select v-model="convertTarget.service_id" class="w-[190px] rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent" @change="convertPreview = null">
                    <option value="">{{ convertFor.service_id ? "— keep " + serviceName(convertFor.service_id) + " —" : "— choose —" }}</option>
                    <option v-for="sv in services" :key="sv.id" :value="sv.id">{{ sv.name }}</option>
                  </select>
                </label>
                <label v-else-if="convertTarget.source === 'monitor'" class="flex flex-col gap-[6px]">
                  <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Monitor</span>
                  <select v-model="convertTarget.monitor_id" class="w-[190px] rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent" @change="convertPreview = null">
                    <option value="">{{ convertFor.monitor_id ? "— keep " + monitorName(convertFor.monitor_id) + " —" : "— choose —" }}</option>
                    <option v-for="m in monitors" :key="m.id" :value="m.id">{{ m.name }}</option>
                  </select>
                </label>
                <label v-else class="flex flex-col gap-[6px]">
                  <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Manual status</span>
                  <select v-model="convertTarget.manual_status" class="w-[170px] rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent" @change="convertPreview = null">
                    <option value="">{{ convertFor.manual_status ? "— keep " + componentMeta(convertFor.manual_status).label + " —" : "— not set —" }}</option>
                    <option value="operational">{{ componentMeta("operational").label }}</option>
                    <option value="degraded">{{ componentMeta("degraded").label }}</option>
                    <option value="partial_outage">{{ componentMeta("partial_outage").label }}</option>
                    <option value="major_outage">{{ componentMeta("major_outage").label }}</option>
                    <option value="maintenance">{{ componentMeta("maintenance").label }}</option>
                  </select>
                </label>
                <button type="button" :disabled="convertBusy" class="h-[38px] rounded-sm border border-border px-4 text-[13px] hover:border-accent hover:text-accent disabled:opacity-50" data-testid="conversion-preview" @click="runPreview">Preview</button>
                <button type="button" class="h-[38px] px-2 text-[13px] text-ink-3 hover:text-ink" @click="cancelConvert">Cancel</button>
              </div>

              <p v-if="convertError" class="mt-3 text-[12.5px] text-down" data-testid="conversion-error">{{ convertError }}</p>

              <div v-if="convertPreview" class="mt-4 rounded border border-border bg-surface p-4" data-testid="conversion-result">
                <div class="grid gap-4 sm:grid-cols-2">
                  <div>
                    <div class="mb-1 text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Now</div>
                    <div class="flex items-center gap-2 text-[13px]">
                      <span class="h-[8px] w-[8px] rounded-full" :class="componentMeta(convertPreview.component?.status).dot"></span>
                      <span :class="componentMeta(convertPreview.component?.status).text">{{ componentMeta(convertPreview.component?.status).label }}</span>
                      <span class="font-mono text-[11px] text-ink-3">{{ sourceLabel(convertPreview.component?.source) }}</span>
                    </div>
                    <div v-if="reasonText(convertPreview.component?.reason)" class="mt-1 text-[11.5px] text-ink-3">{{ reasonText(convertPreview.component?.reason) }}</div>
                    <div class="mt-2 text-[12px] text-ink-2" data-testid="summary-before">
                      Page: {{ summaryHeadline(convertPreview.summary?.summary, convertPreview.summary?.summary_state, convertPreview.summary?.unmeasured_count) }}
                    </div>
                  </div>
                  <div>
                    <div class="mb-1 text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">After this change</div>
                    <div class="flex items-center gap-2 text-[13px]">
                      <span class="h-[8px] w-[8px] rounded-full" :class="componentMeta(convertPreview.proposed?.status).dot"></span>
                      <span :class="componentMeta(convertPreview.proposed?.status).text" data-testid="proposed-status">{{ componentMeta(convertPreview.proposed?.status).label }}</span>
                      <span class="font-mono text-[11px] text-ink-3">{{ sourceLabel(convertPreview.proposed?.source) }}</span>
                    </div>
                    <div v-if="reasonText(convertPreview.proposed?.reason)" class="mt-1 text-[11.5px] text-ink-3">{{ reasonText(convertPreview.proposed?.reason) }}</div>
                    <div class="mt-2 text-[12px] text-ink-2" data-testid="summary-after">
                      Page: {{ summaryHeadline(convertPreview.proposed_summary?.summary, convertPreview.proposed_summary?.summary_state, convertPreview.proposed_summary?.unmeasured_count) }}
                    </div>
                  </div>
                </div>
                <ul v-if="convertPreview.notes?.length" class="mt-3 flex flex-col gap-1 pl-4 text-[12.5px] text-ink-2">
                  <li v-for="(n, i) in convertPreview.notes" :key="i" class="list-disc">{{ n }}</li>
                </ul>
                <div class="mt-4 flex items-center gap-3">
                  <button type="button" :disabled="convertBusy || convertPreview.no_op" class="h-[36px] rounded-sm border border-accent px-4 text-[13px] text-accent hover:bg-accent-weak disabled:opacity-50" data-testid="conversion-confirm" @click="confirmConvert">Apply this change</button>
                  <span class="font-mono text-[11px] text-ink-3">rev {{ convertPreview.revision }} · page {{ convertPreview.page_generation }}</span>
                </div>
              </div>
            </div>
            <div v-if="canManage" class="flex flex-wrap items-end gap-3 border-t border-border p-4">
              <label class="flex flex-col gap-[6px]">
                <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Name</span>
                <input v-model="compForm.name" type="text" placeholder="API" class="w-[160px] rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent" />
              </label>
              <label class="flex flex-col gap-[6px]">
                <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Source</span>
                <select v-model="compForm.source" class="w-[130px] rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent" data-testid="new-component-source">
                  <option value="monitor">Monitor</option>
                  <option value="service">Service</option>
                  <option value="manual">Manual</option>
                </select>
              </label>
              <label v-if="compForm.source === 'monitor'" class="flex flex-col gap-[6px]">
                <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Monitor</span>
                <select v-model="compForm.monitor_id" class="w-[180px] rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent">
                  <option value="">— choose —</option>
                  <option v-for="m in monitors" :key="m.id" :value="m.id">{{ m.name }}</option>
                </select>
              </label>
              <label v-else-if="compForm.source === 'service'" class="flex flex-col gap-[6px]">
                <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Service</span>
                <select v-model="compForm.service_id" class="w-[180px] rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent" data-testid="new-component-service">
                  <option value="">— choose —</option>
                  <option v-for="sv in services" :key="sv.id" :value="sv.id">{{ sv.name }}</option>
                </select>
                <span v-if="!services.length" class="text-[11px] text-ink-3">No services in this project yet.</span>
              </label>
              <label v-else class="flex flex-col gap-[6px]">
                <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Manual status</span>
                <select v-model="compForm.manual_status" class="w-[170px] rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent">
                  <!-- No "no data" option: it is COMPUTED when measurement is absent, and an
                       operator choosing it would be typing an unknown as if it were a statement. -->
                  <option value="">— not set yet —</option>
                  <option value="operational">{{ componentMeta("operational").label }}</option>
                  <option value="degraded">{{ componentMeta("degraded").label }}</option>
                  <option value="partial_outage">{{ componentMeta("partial_outage").label }}</option>
                  <option value="major_outage">{{ componentMeta("major_outage").label }}</option>
                  <option value="maintenance">{{ componentMeta("maintenance").label }}</option>
                </select>
              </label>
              <label class="flex flex-col gap-[6px]">
                <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Group <span class="font-normal normal-case tracking-normal">— optional</span></span>
                <input v-model="compForm.group" type="text" placeholder="Services" list="comp-groups" class="w-[150px] rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent" />
                <datalist id="comp-groups"><option v-for="g in usedGroups" :key="g" :value="g" /></datalist>
              </label>
              <label class="flex flex-col gap-[6px]">
                <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Description <span class="font-normal normal-case tracking-normal">— optional</span></span>
                <input v-model="compForm.description" type="text" placeholder="Public REST API" class="w-[190px] rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent" />
              </label>
              <label class="flex flex-col gap-[6px]">
                <span class="text-[11px] font-semibold uppercase tracking-[0.07em] text-ink-3">Position</span>
                <input v-model.number="compForm.position" type="number" min="0" class="w-[80px] rounded-sm border border-border bg-surface-2 px-3 py-2 text-[13px] outline-none focus:border-accent" />
              </label>
              <button type="button" :disabled="addingComp || !compForm.name.trim()" class="h-[38px] rounded-sm border border-border px-4 text-[13px] hover:border-accent hover:text-accent disabled:opacity-50" @click="addComponent">Add</button>
            </div>
            <div v-if="compError" class="px-4 pb-3 text-[12.5px] text-down">{{ compError }}</div>
          </div>

          <!-- subscribers (org admin) -->
          <div v-if="canManage" class="mt-4 rounded border border-border bg-surface shadow-card">
            <div class="flex items-center gap-2 border-b border-border px-4 py-[13px]">
              <h3 class="text-[13px] font-semibold">Subscribers</h3>
              <span class="font-mono text-[12px] text-ink-3">{{ subscribers.length }} · {{ confirmedCount }} confirmed</span>
            </div>
            <table class="w-full text-[13px]">
              <thead>
                <tr class="text-[10.5px] uppercase tracking-[0.06em] text-ink-3">
                  <th class="border-b border-border px-4 py-[9px] text-left">Email</th>
                  <th class="border-b border-border px-4 py-[9px] text-left">Status</th>
                  <th class="border-b border-border px-4 py-[9px] text-left">Subscribed</th>
                  <th class="border-b border-border px-4 py-[9px]"></th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="s in subscribers" :key="s.id" class="hover:bg-surface-2">
                  <td class="border-b border-border px-4 py-[10px] font-mono text-[12.5px]">{{ s.email }}</td>
                  <td class="border-b border-border px-4 py-[10px]">
                    <span v-if="s.confirmed_at" class="rounded-full bg-up-weak px-[9px] py-[2px] text-[11px] font-semibold text-up">confirmed</span>
                    <span v-else class="rounded-full bg-degraded-weak px-[9px] py-[2px] text-[11px] font-semibold text-degraded" title="The confirmation email was sent but its link has not been clicked">pending confirm</span>
                  </td>
                  <td class="border-b border-border px-4 py-[10px] font-mono text-[12px] text-ink-3">{{ fmtSubDate(s.created_at) }}</td>
                  <td class="border-b border-border px-4 py-[10px] text-right">
                    <template v-if="confirmRemoveSub === s.id">
                      <span class="mr-2 text-[12px] text-ink-3">Remove?</span>
                      <button type="button" class="mr-1 h-[26px] rounded-sm bg-down px-[9px] text-[12px] font-medium text-white hover:opacity-90" @click="removeSubscriber(s.id!)">Confirm</button>
                      <button type="button" class="h-[26px] rounded-sm border border-border px-[9px] text-[12px] text-ink-2 hover:border-border-strong" @click="confirmRemoveSub = ''">Cancel</button>
                    </template>
                    <button v-else type="button" class="text-[12.5px] text-down hover:underline" @click="confirmRemoveSub = s.id ?? ''">Remove</button>
                  </td>
                </tr>
                <tr v-if="!subscribers.length"><td colspan="4" class="px-4 py-6 text-center text-[13px] text-ink-3">No subscribers yet — the subscribe form lives on the public page.</td></tr>
              </tbody>
            </table>
          </div>
        </section>

        <section v-else class="grid place-items-center rounded border border-border bg-surface p-10 text-[13px] text-ink-3 shadow-card">
          Select a page to manage its components.
        </section>
      </div>
    </div>
  </AppShell>
</template>
