<script setup lang="ts">
import { computed, onMounted, ref, watch } from "vue";
import { api } from "@/api/client";
import type { components } from "@/api/schema";
import AppShell from "@/components/AppShell.vue";
import StatusPill from "@/components/StatusPill.vue";
import { useSession } from "@/stores/session";
import { useWorkspace } from "@/stores/workspace";

type Monitor = components["schemas"]["Monitor"];

const ws = useWorkspace();
const session = useSession();
const loading = ref(true);
const monitors = ref<Monitor[]>([]);
const error = ref("");

async function load() {
  loading.value = true;
  error.value = "";
  try {
    await ws.init();
    if (!ws.projectId) {
      monitors.value = [];
      return;
    }
    const res = await api.GET("/api/v1/projects/{projectID}/monitors", {
      params: { path: { projectID: ws.projectId } },
    });
    monitors.value = res.data ?? [];
  } catch {
    error.value = "Could not load monitors.";
  } finally {
    loading.value = false;
  }
}

function statusOf(m: Monitor): "up" | "down" | "pending" {
  return (m.status as "up" | "down" | "pending") ?? "pending";
}

// Dependency suppression: a down monitor whose (transitive) parent is also down
// is a cascade victim — its alerts are muted, the badge names the root.
function suppressedBy(m: Monitor, visited = new Set<string>()): string | undefined {
  for (const pid of m.depends_on ?? []) {
    if (visited.has(pid)) continue;
    visited.add(pid);
    const p = monitors.value.find((x) => x.id === pid);
    if (!p) continue;
    if (p.status === "down") return p.name ?? undefined;
    const deeper = suppressedBy(p, visited);
    if (deeper) return deeper;
  }
  return undefined;
}

// While a failure is being confirmed (up, but fails counted) the pill switches
// to the degraded style with a live "Confirming N/M…" label.
function confirmingLabel(m: Monitor): string | undefined {
  const fails = m.consecutive_failures ?? 0;
  const threshold = m.failure_threshold ?? 1;
  if (m.status === "up" && fails > 0 && fails < threshold) return `Confirming ${fails}/${threshold}…`;
  return undefined;
}

// Tag filter (AND: a monitor must carry every selected tag).
const activeTags = ref<Set<string>>(new Set());
const allTags = computed(() => {
  const s = new Set<string>();
  for (const m of monitors.value) for (const t of m.tags ?? []) s.add(t);
  return [...s].sort();
});
function toggleTag(t: string) {
  const s = new Set(activeTags.value);
  if (s.has(t)) s.delete(t);
  else s.add(t);
  activeTags.value = s;
}
// Source filter (FR-017): All / UI-managed / file-managed.
const sourceFilter = ref<"all" | "ui" | "file">("all");
const hasFileManaged = computed(() => monitors.value.some((m) => m.management?.source === "file"));
const shown = computed(() => {
  return monitors.value.filter((m) => {
    if (sourceFilter.value !== "all" && (m.management?.source ?? "ui") !== sourceFilter.value) return false;
    if (activeTags.value.size) {
      const tags = m.tags ?? [];
      for (const t of activeTags.value) if (!tags.includes(t)) return false;
    }
    return true;
  });
});

onMounted(load);
watch(() => ws.projectId, load);
</script>

<template>
  <AppShell active="monitors" :crumbs="[ws.orgName || 'cerbix', ws.projectName || '…', 'Monitors']">
    <template #actions>
      <RouterLink
        v-if="session.canProjectWrite(ws.orgId, ws.projectId)"
        :to="{ name: 'monitor-new' }"
        class="flex h-[34px] items-center gap-[7px] rounded-sm bg-accent px-[13px] text-[13px] font-medium text-accent-ink hover:bg-accent-2"
      >
        <svg viewBox="0 0 24 24" class="h-[15px] w-[15px]" fill="none" stroke="currentColor" stroke-width="2.2"><path d="M12 5v14M5 12h14" /></svg>
        New monitor
      </RouterLink>
    </template>

    <div class="mx-auto max-w-[1180px] px-[22px] pb-16 pt-[26px]">
      <div class="mb-[22px]">
        <h1 class="text-[21px] font-semibold tracking-tight">Monitors</h1>
        <p class="mt-[3px] text-[13px] text-ink-3">
          <span v-if="loading">Loading…</span>
          <span v-else>{{ monitors.length }} monitors · {{ ws.projectName }}</span>
        </p>
      </div>

      <div v-if="error" class="rounded border border-down/40 bg-down-weak p-4 text-[13px] text-down">{{ error }}</div>

      <!-- source filter (FR-017): coexisting UI/file-managed monitors -->
      <div v-if="!error && hasFileManaged" class="mb-3 flex items-center gap-[6px]">
        <span class="mr-1 text-[11px] font-semibold uppercase tracking-[0.06em] text-ink-3">Source</span>
        <button
          v-for="opt in (['all', 'ui', 'file'] as const)"
          :key="opt"
          type="button"
          class="rounded-full border px-[10px] py-[3px] text-[11.5px] capitalize transition-colors"
          :class="sourceFilter === opt ? 'border-accent bg-accent-weak text-accent' : 'border-border text-ink-2 hover:border-border-strong hover:text-ink'"
          @click="sourceFilter = opt"
        >{{ opt === "ui" ? "UI" : opt }}</button>
      </div>

      <!-- tag filter -->
      <div v-if="!error && allTags.length" class="mb-4 flex flex-wrap items-center gap-[6px]">
        <span class="mr-1 text-[11px] font-semibold uppercase tracking-[0.06em] text-ink-3">Filter</span>
        <button
          v-for="t in allTags"
          :key="t"
          type="button"
          class="rounded-full border px-[10px] py-[3px] font-mono text-[11.5px] transition-colors"
          :class="activeTags.has(t) ? 'border-accent bg-accent-weak text-accent' : 'border-border text-ink-2 hover:border-border-strong hover:text-ink'"
          @click="toggleTag(t)"
        >{{ t }}</button>
        <button v-if="activeTags.size" type="button" class="ml-1 text-[12px] text-ink-3 hover:text-ink" @click="activeTags = new Set()">clear</button>
        <span class="ml-auto font-mono text-[12px] text-ink-3">{{ shown.length }} / {{ monitors.length }}</span>
      </div>

      <section v-if="!error" class="overflow-hidden rounded border border-border bg-surface shadow-card">
        <table class="w-full text-[13px]">
          <thead>
            <tr class="text-[10.5px] uppercase tracking-[0.06em] text-ink-3">
              <th class="border-b border-border px-4 py-[10px] text-left">Monitor</th>
              <th class="border-b border-border px-4 py-[10px] text-left">Type</th>
              <th class="border-b border-border px-4 py-[10px] text-left">Target</th>
              <th class="border-b border-border px-4 py-[10px] text-left">Interval</th>
              <th class="border-b border-border px-4 py-[10px] text-left">Status</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="m in shown" :key="m.id" class="group hover:bg-surface-2">
              <td class="border-b border-border px-4 py-[11px]">
                <RouterLink :to="{ name: 'monitor', params: { id: m.id } }" class="font-medium text-ink hover:text-accent">
                  {{ m.name }}
                </RouterLink>
                <span v-if="m.management?.source === 'file'" class="ml-[6px] inline-flex items-center gap-[4px] rounded-full bg-inset px-[7px] py-px font-mono text-[10.5px] text-ink-2" :title="'Managed by file provider ' + m.management.provider + ' (' + m.management.path + ') — read-only'">
                  <svg viewBox="0 0 24 24" class="h-[11px] w-[11px]" fill="none" stroke="currentColor" stroke-width="2"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><path d="M14 2v6h6"/></svg>
                  file
                </span>
                <span v-for="t in m.tags || []" :key="t" class="ml-[6px] rounded-full bg-inset px-[7px] py-px font-mono text-[10.5px] text-ink-3">{{ t }}</span>
              </td>
              <td class="border-b border-border px-4 py-[11px]">
                <span class="rounded-xs border border-border px-[6px] py-px font-mono text-[10.5px] uppercase tracking-[0.04em] text-ink-3">{{ m.type }}</span>
              </td>
              <td class="border-b border-border px-4 py-[11px] font-mono text-ink-2">{{ m.target || "—" }}</td>
              <td class="border-b border-border px-4 py-[11px] font-mono text-ink-3">{{ m.type === "push" ? "—" : m.interval_seconds + "s" }}</td>
              <td class="border-b border-border px-4 py-[11px]">
                  <span class="inline-flex flex-wrap items-center gap-[6px]">
                    <StatusPill :status="confirmingLabel(m) ? 'degraded' : statusOf(m)" :label="confirmingLabel(m)" />
                    <span v-if="m.type === 'composite' && m.config?.mode === 'quorum'" class="inline-flex h-6 items-center rounded-full bg-accent-weak px-[9px] text-[11px] font-medium text-accent" :title="'Down when ≥ ' + m.config?.quorum + ' members report down'">quorum {{ m.config?.quorum }}/{{ (m.config?.children ?? '').split(',').filter(Boolean).length }}</span>
                    <span v-if="m.status === 'down' && suppressedBy(m)" class="inline-flex h-6 items-center gap-[5px] rounded-full bg-inset px-[9px] text-[11px] font-medium text-ink-2" :title="'Alerts suppressed: depends on ' + suppressedBy(m) + ', which is down'">⏸ suppressed by {{ suppressedBy(m) }}</span>
                  </span>
                </td>
            </tr>
            <tr v-if="!shown.length && monitors.length && !loading">
              <td colspan="5" class="px-4 py-10 text-center text-[13px] text-ink-3">No monitors match the selected tags.</td>
            </tr>
            <tr v-if="!monitors.length && !loading">
              <td colspan="5" class="px-4 py-10 text-center text-[13px] text-ink-3">
                No monitors yet.
                <RouterLink v-if="session.canProjectWrite(ws.orgId, ws.projectId)" :to="{ name: 'monitor-new' }" class="text-accent hover:underline">Add the first one.</RouterLink>
              </td>
            </tr>
          </tbody>
        </table>
      </section>
    </div>
  </AppShell>
</template>
