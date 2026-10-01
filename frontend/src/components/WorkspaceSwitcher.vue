<script setup lang="ts">
import { computed } from "vue";

import OverlaySurface from "@/components/OverlaySurface.vue";
import { useSession } from "@/stores/session";
import { useWorkspace } from "@/stores/workspace";

const props = withDefaults(
  defineProps<{
    open: boolean;
    canManageOrg?: boolean;
    menuId?: string;
  }>(),
  {
    canManageOrg: false,
    menuId: "workspace-switcher-menu",
  },
);

const emit = defineEmits<{
  "update:open": [open: boolean];
  create: [kind: "org" | "project"];
  "project-selected": [id: string];
  "organization-selected": [id: string];
}>();

const ws = useWorkspace();
const session = useSession();
const menuId = computed(() => props.menuId);
const triggerId = computed(() => `${menuId.value}-trigger`);
const transitionPending = computed(() => ws.transitionPending);
const transitionError = computed(() => ws.transitionError);
const failedOrgId = computed(() => ws.transition.failedOrgId);

function close() {
  emit("update:open", false);
}

async function pickOrg(id: string) {
  if (transitionPending.value) return;
  if (await ws.selectOrg(id)) {
    close();
    emit("organization-selected", id);
  }
}

function pickProject(id: string) {
  if (ws.selectProject(id)) {
    close();
    emit("project-selected", id);
  }
}

async function retry() {
  if (!failedOrgId.value) return;
  if (await ws.selectOrg(failedOrgId.value)) {
    close();
    emit("organization-selected", failedOrgId.value);
  }
}

function openCreate(kind: "org" | "project") {
  emit("create", kind);
}
</script>

<template>
  <div class="relative mb-[6px]">
    <button
      :id="triggerId"
      class="flex w-full items-center gap-2 rounded border border-border bg-surface-2 px-[10px] py-2 text-left hover:border-border-strong disabled:cursor-wait disabled:opacity-80"
      type="button"
      :aria-expanded="open"
      :aria-controls="menuId"
      :disabled="transitionPending"
      @click="emit('update:open', !open)"
    >
      <span class="grid h-[22px] w-[22px] place-items-center rounded-[6px] bg-accent text-[11px] font-bold text-accent-ink">
        {{ (ws.orgName[0] || "·").toUpperCase() }}
      </span>
      <span v-if="ws.loading && !ws.orgId" class="flex min-w-0 flex-1 flex-col gap-[5px] py-[2px]" aria-label="Loading workspace">
        <span class="h-[10px] w-[70%] animate-pulse rounded bg-inset motion-reduce:animate-none"></span>
        <span class="h-[9px] w-[45%] animate-pulse rounded bg-inset motion-reduce:animate-none"></span>
      </span>
      <span v-else-if="transitionPending" class="flex min-w-0 flex-1 flex-col leading-tight" aria-live="polite">
        <b class="truncate text-[13px] font-semibold">Switching organization…</b>
        <span class="truncate text-[11px] text-ink-2">Loading projects…</span>
      </span>
      <span v-else class="flex min-w-0 flex-1 flex-col leading-tight">
        <b class="truncate text-[13px] font-semibold">{{ ws.orgName || "—" }}</b>
        <span class="truncate text-[11px] text-ink-2">{{ ws.projectName || "organization" }}</span>
      </span>
      <span class="ml-auto text-ink-3">
        <svg viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor" stroke-width="2"><path d="M8 9l4 4 4-4" /></svg>
      </span>
    </button>

    <OverlaySurface
      :open="open"
      :id="menuId"
      role="menu"
      :modal="false"
      :close-on-outside="true"
      :labelledby="triggerId"
      class="contents"
      @close="close"
    >
      <div
        class="absolute left-0 right-0 top-[calc(100%+4px)] z-30 max-h-[70vh] overflow-y-auto rounded border border-border-strong bg-surface p-1 shadow-lg"
        role="presentation"
      >
        <div class="px-[9px] pb-1 pt-2 text-[10px] font-semibold uppercase tracking-[0.09em] text-ink-2">Organization</div>
        <button
          v-for="o in ws.orgs"
          :key="o.id"
          class="flex w-full items-center gap-2 rounded-sm px-[9px] py-[6px] text-left text-[13px] hover:bg-surface-2 disabled:cursor-wait disabled:opacity-60"
          :class="o.id === ws.orgId ? 'font-medium text-accent' : 'text-ink-2'"
          type="button"
          role="menuitem"
          :disabled="transitionPending"
          @click="pickOrg(o.id!)"
        >
          <span class="truncate">{{ o.name || o.slug }}</span>
          <svg v-if="o.id === ws.orgId" class="ml-auto h-[14px] w-[14px]" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4"><path d="M5 12l5 5 9-11" /></svg>
        </button>
        <button
          v-if="session.isGlobalAdmin"
          class="flex w-full items-center gap-2 rounded-sm px-[9px] py-[6px] text-left text-[13px] font-medium text-accent hover:bg-surface-2 disabled:opacity-60"
          type="button"
          role="menuitem"
          :disabled="transitionPending"
          @click="openCreate('org')"
        >
          <svg viewBox="0 0 24 24" class="h-[15px] w-[15px]" fill="none" stroke="currentColor" stroke-width="2.2"><path d="M12 5v14M5 12h14" /></svg>
          New organization
        </button>

        <div class="mx-1 my-1 border-t border-border"></div>
        <div class="px-[9px] pb-1 pt-1 text-[10px] font-semibold uppercase tracking-[0.09em] text-ink-2">Project</div>
        <template v-if="!transitionPending && !transitionError">
          <button
            v-for="p in ws.projects"
            :key="p.id"
            class="flex w-full items-center gap-2 rounded-sm px-[9px] py-[6px] text-left text-[13px] hover:bg-surface-2"
            :class="p.id === ws.projectId ? 'font-medium text-accent' : 'text-ink-2'"
            type="button"
            role="menuitem"
            @click="pickProject(p.id!)"
          >
            <span class="truncate">{{ p.name || p.slug }}</span>
            <svg v-if="p.id === ws.projectId" class="ml-auto h-[14px] w-[14px]" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4"><path d="M5 12l5 5 9-11" /></svg>
          </button>
          <p v-if="ws.projectsEmpty" class="px-[9px] py-2 text-[12px] text-ink-2">No projects in this organization.</p>
        </template>
        <div v-if="transitionPending" class="px-[9px] py-2 text-[12px] text-ink-2" aria-live="polite">Loading projects…</div>
        <div v-else-if="transitionError" class="px-[9px] py-2 text-[12px] text-down" role="alert">
          <p>{{ transitionError }}</p>
          <button class="mt-1 font-medium underline hover:no-underline" type="button" @click="retry">Retry</button>
        </div>
        <button
          v-if="canManageOrg && !transitionPending && !transitionError"
          class="flex w-full items-center gap-2 rounded-sm px-[9px] py-[6px] text-left text-[13px] font-medium text-accent hover:bg-surface-2"
          type="button"
          role="menuitem"
          @click="openCreate('project')"
        >
          <svg viewBox="0 0 24 24" class="h-[15px] w-[15px]" fill="none" stroke="currentColor" stroke-width="2.2"><path d="M12 5v14M5 12h14" /></svg>
          New project
        </button>
      </div>
    </OverlaySurface>
  </div>
</template>
