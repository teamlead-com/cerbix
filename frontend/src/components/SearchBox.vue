<script setup lang="ts">
import { onBeforeUnmount, ref } from "vue";
import { useRouter } from "vue-router";
import { api } from "@/api/client";
import type { components } from "@/api/schema";
import { useWorkspace } from "@/stores/workspace";

type Hit = components["schemas"]["SearchHit"];

const router = useRouter();
const ws = useWorkspace();

const q = ref("");
const hits = ref<Hit[]>([]);
const open = ref(false);
const loading = ref(false);
const active = ref(-1);
let timer: ReturnType<typeof setTimeout> | undefined;

function onInput() {
  active.value = -1;
  if (timer) clearTimeout(timer);
  const term = q.value.trim();
  if (term.length < 2) {
    hits.value = [];
    open.value = false;
    return;
  }
  open.value = true;
  timer = setTimeout(runSearch, 220);
}

async function runSearch() {
  const term = q.value.trim();
  if (term.length < 2) return;
  loading.value = true;
  try {
    const res = await api.GET("/api/v1/search", { params: { query: { q: term } } });
    hits.value = res.data?.hits ?? [];
  } catch {
    hits.value = [];
  } finally {
    loading.value = false;
  }
}

function close() {
  open.value = false;
  active.value = -1;
}
function reset() {
  q.value = "";
  hits.value = [];
  close();
}

async function go(hit: Hit) {
  reset();
  if (hit.type === "monitor") {
    router.push({ name: "monitor", params: { id: hit.id } });
  } else if (hit.type === "incident") {
    router.push({ name: "incident", params: { id: hit.id } });
  } else {
    // project — switch workspace to it, then land on its dashboard
    if (hit.org_id && hit.org_id !== ws.orgId) await ws.selectOrg(hit.org_id);
    if (hit.project_id) ws.selectProject(hit.project_id);
    router.push({ name: "dashboard" });
  }
}

function onKeydown(e: KeyboardEvent) {
  if (!open.value || !hits.value.length) return;
  if (e.key === "ArrowDown") {
    e.preventDefault();
    active.value = (active.value + 1) % hits.value.length;
  } else if (e.key === "ArrowUp") {
    e.preventDefault();
    active.value = (active.value - 1 + hits.value.length) % hits.value.length;
  } else if (e.key === "Enter" && active.value >= 0) {
    e.preventDefault();
    go(hits.value[active.value]);
  }
}

const typeLabel: Record<string, string> = { monitor: "Monitor", project: "Project", incident: "Incident" };

onBeforeUnmount(() => timer && clearTimeout(timer));
</script>

<template>
  <div class="relative">
    <div class="flex h-[34px] w-[240px] items-center gap-2 rounded-sm border border-border bg-surface px-[10px] text-ink-3 focus-within:border-accent max-[1100px]:w-[150px]">
      <svg viewBox="0 0 24 24" class="h-4 w-4 shrink-0" fill="none" stroke="currentColor" stroke-width="2"><circle cx="11" cy="11" r="7" /><path d="M21 21l-4-4" /></svg>
      <input
        v-model="q"
        type="text"
        placeholder="Search…"
        class="w-full bg-transparent text-[13px] text-ink outline-none placeholder:text-ink-3"
        @input="onInput"
        @focus="q.trim().length >= 2 && (open = true)"
        @keydown="onKeydown"
        @keydown.esc="close"
      />
      <kbd v-if="!q" class="rounded-[3px] border border-border px-[4px] font-mono text-[10px] text-ink-3 max-[1100px]:hidden">/</kbd>
    </div>

    <template v-if="open">
      <div class="fixed inset-0 z-30" @click="close"></div>
      <div class="absolute right-0 top-[calc(100%+6px)] z-40 max-h-[70vh] w-[340px] overflow-y-auto rounded border border-border-strong bg-surface p-1 shadow-lg">
        <p v-if="loading" class="px-3 py-3 text-[12.5px] text-ink-3">Searching…</p>
        <p v-else-if="!hits.length" class="px-3 py-3 text-[12.5px] text-ink-3">No matches for “{{ q.trim() }}”.</p>
        <button
          v-for="(h, i) in hits"
          :key="i"
          type="button"
          class="flex w-full items-center gap-[10px] rounded-sm px-[9px] py-[7px] text-left"
          :class="i === active ? 'bg-surface-2' : 'hover:bg-surface-2'"
          @click="go(h)"
          @mouseenter="active = i"
        >
          <svg class="h-[15px] w-[15px] shrink-0 text-ink-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9">
            <path v-if="h.type === 'monitor'" d="M3 12h4l2 6 4-14 2 8h6" />
            <template v-else-if="h.type === 'incident'"><path d="M12 9v4M12 17h.01" /><path d="M10.3 3.9L2 18a2 2 0 0 0 1.7 3h16.6A2 2 0 0 0 22 18L13.7 3.9a2 2 0 0 0-3.4 0z" /></template>
            <template v-else><rect x="3" y="4" width="18" height="16" rx="2" /><path d="M3 9h18" /></template>
          </svg>
          <span class="min-w-0 flex-1">
            <span class="block truncate text-[13px]">{{ h.label }}</span>
            <span v-if="h.sub" class="block truncate font-mono text-[11px] text-ink-3">{{ h.sub }}</span>
          </span>
          <span class="rounded-full border border-border px-[7px] py-px text-[10px] uppercase tracking-[0.04em] text-ink-3">{{ typeLabel[h.type ?? ""] || h.type }}</span>
        </button>
      </div>
    </template>
  </div>
</template>
