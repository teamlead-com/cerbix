<script setup lang="ts">
import { computed, onBeforeUnmount, ref } from "vue";
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
const error = ref("");
const active = ref(-1);
const inputRef = ref<HTMLInputElement | null>(null);
const resultListRef = ref<HTMLElement | null>(null);
const liveStatusRef = ref<HTMLElement | null>(null);
const liveMessage = ref("");
let timer: ReturnType<typeof setTimeout> | undefined;
let inputGeneration = 0;
let requestGeneration = 0;
let inflight: AbortController | undefined;
let suppressFocusOpen = false;

function announce(message: string) {
  liveMessage.value = message;
  if (liveStatusRef.value) liveStatusRef.value.textContent = message;
}

function cancelPending() {
  if (timer) {
    clearTimeout(timer);
    timer = undefined;
  }
  inflight?.abort();
  inflight = undefined;
}

function onInput() {
  const mine = ++inputGeneration;
  cancelPending();
  active.value = -1;
  const term = q.value.trim();
  if (term.length < 2) {
    hits.value = [];
    open.value = false;
    loading.value = false;
    error.value = "";
    announce("");
    return;
  }
  hits.value = [];
  error.value = "";
  open.value = true;
  loading.value = true;
  announce("Searching…");
  timer = setTimeout(() => void runSearch(term, mine), 220);
}

function isCurrent(mine: number, request: number) {
  return mine === inputGeneration && request === requestGeneration;
}

function isAbort(error: unknown) {
  return (error as { name?: string } | null)?.name === "AbortError";
}

async function runSearch(term: string, mine: number) {
  if (mine !== inputGeneration || term.length < 2) return;
  const request = ++requestGeneration;
  const controller = new AbortController();
  inflight?.abort();
  inflight = controller;
  loading.value = true;
  error.value = "";
  announce("Searching…");
  try {
    const res = await api.GET("/api/v1/search", {
      params: { query: { q: term } },
      signal: controller.signal,
    });
    if (res.error || (res.response && !res.response.ok)) throw new Error("Search request failed");
    if (!isCurrent(mine, request)) return;
    hits.value = res.data?.hits ?? [];
    announce(
      hits.value.length
        ? `${hits.value.length} ${hits.value.length === 1 ? "match" : "matches"}`
        : `No matches for “${term}”.`,
    );
  } catch (cause) {
    if (!isCurrent(mine, request) || isAbort(cause)) return;
    hits.value = [];
    error.value = "Search failed. Please try again.";
    announce(error.value);
  } finally {
    if (isCurrent(mine, request)) {
      loading.value = false;
      inflight = undefined;
    }
  }
}

function close() {
  open.value = false;
  active.value = -1;
  if (resultListRef.value) resultListRef.value.scrollTop = 0;
}

function reset() {
  q.value = "";
  hits.value = [];
  loading.value = false;
  error.value = "";
  announce("");
  close();
}

function focusInput() {
  suppressFocusOpen = true;
  inputRef.value?.focus();
  queueMicrotask(() => {
    suppressFocusOpen = false;
  });
}

function restoreInputIfCurrent(mine: number) {
  if (mine === inputGeneration) focusInput();
}

function onFocus() {
  if (suppressFocusOpen) return;
  if (q.value.trim().length >= 2) open.value = true;
}

function onEscape() {
  if (open.value) {
    close();
    focusInput();
    return;
  }
  cancelPending();
  ++inputGeneration;
  reset();
  focusInput();
}

// Every hit carries its own org and project, so EVERY hit switches the workspace to them before
// navigating — not only the project hits.
//
// It used to switch for projects alone. A monitor or incident hit from another workspace then landed
// on a detail page whose reads and permission checks still ran against the PREVIOUS tenant: the
// successor and convert pickers listed a stranger's services, and `canWrite` compared the workspace's
// org against the loaded subject's project, so a legitimate editor of the target saw acknowledge,
// resolve and postmortem disappear. Both entrances were the same defect and are fixed in one place
// rather than per detail view.
async function go(hit: Hit) {
  const mine = ++inputGeneration;
  cancelPending();
  reset();
  if (hit.org_id && hit.org_id !== ws.orgId) {
    const selected = await ws.selectOrg(hit.org_id);
    if (!selected || mine !== inputGeneration) {
      restoreInputIfCurrent(mine);
      return;
    }
  }
  if (hit.project_id && hit.project_id !== ws.projectId) {
    if (mine !== inputGeneration || !ws.selectProject(hit.project_id)) {
      restoreInputIfCurrent(mine);
      return;
    }
  }
  if (mine !== inputGeneration) return;
  if (hit.type === "monitor") {
    router.push({ name: "monitor", params: { id: hit.id } });
  } else if (hit.type === "incident") {
    router.push({ name: "incident", params: { id: hit.id } });
  } else {
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
    active.value = active.value < 0 ? hits.value.length - 1 : (active.value - 1 + hits.value.length) % hits.value.length;
  } else if (e.key === "Enter" && active.value >= 0) {
    e.preventDefault();
    void go(hits.value[active.value]);
  }
}

function sanitizeId(value: string) {
  return value.replace(/[^A-Za-z0-9_-]+/g, "-").replace(/^-+|-+$/g, "") || "result";
}

function optionId(hit: Hit, index: number) {
  const type = sanitizeId(hit.type || "result");
  const identity = sanitizeId(hit.id || String(index));
  return `search-option-${type}-${identity}-${index}`;
}

const activeOptionId = computed(() => {
  const hit = hits.value[active.value];
  return hit ? optionId(hit, active.value) : undefined;
});

const typeLabel: Record<string, string> = { monitor: "Monitor", project: "Project", incident: "Incident" };

onBeforeUnmount(() => {
  ++inputGeneration;
  cancelPending();
});
</script>

<template>
  <div class="relative">
    <div class="flex h-[34px] w-[240px] items-center gap-2 rounded-sm border border-border bg-surface px-[10px] text-ink-3 focus-within:border-accent max-[1100px]:w-[150px]">
      <svg viewBox="0 0 24 24" class="h-4 w-4 shrink-0" fill="none" stroke="currentColor" stroke-width="2"><circle cx="11" cy="11" r="7" /><path d="M21 21l-4-4" /></svg>
      <input
        ref="inputRef"
        v-model="q"
        type="text"
        role="combobox"
        aria-label="Search"
        aria-autocomplete="list"
        aria-controls="search-results"
        :aria-expanded="open ? 'true' : 'false'"
        :aria-activedescendant="activeOptionId"
        placeholder="Search…"
        class="w-full bg-transparent text-[13px] text-ink outline-none placeholder:text-ink-3"
        @input="onInput"
        @focus="onFocus"
        @keydown="onKeydown"
        @keydown.esc="onEscape"
      />
      <kbd v-if="!q" class="rounded-[3px] border border-border px-[4px] font-mono text-[10px] text-ink-3 max-[1100px]:hidden">/</kbd>
    </div>

    <p ref="liveStatusRef" class="sr-only" role="status" aria-live="polite" aria-atomic="true">{{ liveMessage }}</p>

    <template v-if="open">
      <div class="fixed inset-0 z-30" @click="close"></div>
      <div
        id="search-results"
        ref="resultListRef"
        role="listbox"
        class="absolute right-0 top-[calc(100%+6px)] z-40 max-h-[70vh] w-[340px] overflow-y-auto rounded border border-border-strong bg-surface p-1 shadow-lg"
      >
        <p v-if="loading" class="px-3 py-3 text-[12.5px] text-ink-2">Searching…</p>
        <p v-else-if="error" class="px-3 py-3 text-[12.5px] text-ink-2">{{ error }}</p>
        <p v-else-if="!hits.length" class="px-3 py-3 text-[12.5px] text-ink-2">No matches for “{{ q.trim() }}”.</p>
        <button
          v-for="(h, i) in hits"
          :id="optionId(h, i)"
          :key="optionId(h, i)"
          type="button"
          role="option"
          tabindex="-1"
          :aria-selected="i === active ? 'true' : 'false'"
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
