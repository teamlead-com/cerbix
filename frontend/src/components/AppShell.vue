<script setup lang="ts">
import { computed, onMounted, ref, watch } from "vue";
import { RouterLink, useRouter } from "vue-router";
import BrandMark from "@/components/BrandMark.vue";
import CreateDialog from "@/components/CreateDialog.vue";
import SearchBox from "@/components/SearchBox.vue";
import { useTheme } from "@/composables/useTheme";
import { useBranding } from "@/stores/branding";
import { useLive } from "@/stores/live";
import { useSession } from "@/stores/session";
import { useUi } from "@/stores/ui";
import { useWorkspace } from "@/stores/workspace";

withDefaults(
  defineProps<{ active?: string; crumbs?: string[] }>(),
  { active: "dashboard", crumbs: () => [] },
);

const { toggle } = useTheme();
const session = useSession();
const ws = useWorkspace();
const ui = useUi();
const branding = useBranding();
const live = useLive();
const router = useRouter();

const announceCls: Record<string, string> = {
  info: "bg-accent-weak text-ink-2",
  warning: "bg-degraded-weak text-degraded",
  critical: "bg-down-weak text-down",
};
const initials = computed(() => session.initials || "··");
onMounted(() => session.fetchVersion()); // cached in the store — one request per session

// The Services badge is a temporary adoption affordance: it points at a screen the operator
// has not met yet, and it must disappear the moment the project HAS a service — including one
// a bundle created, which is why the nav probes rather than waiting for a visit.
onMounted(() => ws.ensureServiceCount());
watch(() => ws.projectId, () => ws.ensureServiceCount());
const showServicesBadge = computed(() => ws.serviceCounts[ws.projectId] === 0);
const canManageOrg = computed(() => !!ws.orgId && session.isOrgAdmin(ws.orgId));

function openCreate(kind: "org" | "project") {
  switcherOpen.value = false;
  ui.openCreate(kind);
}

async function signOut() {
  await session.logout();
  router.push({ name: "login" });
}

const switcherOpen = ref(false);
const userMenuOpen = ref(false);
async function pickOrg(id: string) {
  await ws.selectOrg(id);
}
function pickProject(id: string) {
  ws.selectProject(id);
  switcherOpen.value = false;
  router.push({ name: "dashboard" });
}

// Icons replicated 1:1 from the design artifact. Each icon is a list of SVG
// primitives (path/rect/circle) drawn as stroke.
type Shape =
  | { t: "path"; d: string }
  | { t: "rect"; x: number; y: number; w: number; h: number; rx: number }
  | { t: "circle"; cx: number; cy: number; r: number };
type NavItem = { key: string; label: string; to: { name: string }; icon: Shape[] };

const sections: { label: string; items: NavItem[] }[] = [
  {
    label: "Project",
    items: [
      { key: "dashboard", label: "Dashboard", to: { name: "dashboard" }, icon: [
        { t: "rect", x: 3, y: 3, w: 7, h: 9, rx: 1.5 }, { t: "rect", x: 14, y: 3, w: 7, h: 5, rx: 1.5 },
        { t: "rect", x: 14, y: 12, w: 7, h: 9, rx: 1.5 }, { t: "rect", x: 3, y: 16, w: 7, h: 5, rx: 1.5 },
      ] },
      // Order is level of abstraction: the unit of reliability, then what measures it.
      // Nesting Monitors under Services would assert containment, which the model rejects.
      { key: "services", label: "Services", to: { name: "services" }, icon: [
        { t: "rect", x: 3, y: 4, w: 18, h: 6, rx: 1.6 }, { t: "rect", x: 3, y: 14, w: 18, h: 6, rx: 1.6 },
        { t: "path", d: "M7 7h.01M7 17h.01" },
      ] },
      { key: "monitors", label: "Monitors", to: { name: "monitors" }, icon: [{ t: "path", d: "M3 12h4l2 6 4-14 2 8h6" }] },
      { key: "sla", label: "SLA & SLO", to: { name: "sla" }, icon: [{ t: "path", d: "M3 3v18h18" }, { t: "path", d: "M7 14l3-4 3 3 5-7" }] },
      // FR-024 (D-0207): the project's decision LEDGER — project-scoped like SLA, because a
      // decision outlives the service it was about (D10). Read-only for viewer+.
      { key: "gate-decisions", label: "Gate decisions", to: { name: "gate-decisions" }, icon: [
        { t: "path", d: "M12 3l7 3v5c0 5-3.5 8.5-7 10-3.5-1.5-7-5-7-10V6l7-3z" }, { t: "path", d: "M9 12l2 2 4-4" },
      ] },
      { key: "escalation", label: "Escalation", to: { name: "escalation" }, icon: [{ t: "path", d: "M12 3v10" }, { t: "path", d: "M8 9l4 4 4-4" }, { t: "path", d: "M4 21h16" }] },
      { key: "incidents", label: "Incidents", to: { name: "incidents" }, icon: [
        { t: "path", d: "M12 9v4M12 17h.01" },
        { t: "path", d: "M10.3 3.9L2 18a2 2 0 0 0 1.7 3h16.6A2 2 0 0 0 22 18L13.7 3.9a2 2 0 0 0-3.4 0z" },
      ] },
      { key: "status", label: "Status pages", to: { name: "status" }, icon: [{ t: "rect", x: 3, y: 4, w: 18, h: 16, rx: 2 }, { t: "path", d: "M3 9h18" }] },
    ],
  },
];
const settingsIcon: Shape[] = [
  { t: "circle", cx: 12, cy: 12, r: 3.2 },
  { t: "path", d: "M19 12a7 7 0 0 0-.1-1.2l2-1.6-2-3.4-2.4 1a7 7 0 0 0-2-1.2l-.4-2.6H9.9l-.4 2.6a7 7 0 0 0-2 1.2l-2.4-1-2 3.4 2 1.6A7 7 0 0 0 5 12c0 .4 0 .8.1 1.2l-2 1.6 2 3.4 2.4-1a7 7 0 0 0 2 1.2l.4 2.6h4.2l.4-2.6a7 7 0 0 0 2-1.2l2.4 1 2-3.4-2-1.6c.1-.4.1-.8.1-1.2z" },
];
</script>

<template>
  <div class="grid min-h-screen grid-cols-[240px_1fr] max-[900px]:grid-cols-1">
    <aside class="sticky top-0 flex h-screen flex-col gap-1 border-r border-border bg-surface p-3 max-[900px]:hidden">
      <div class="flex items-center gap-[9px] px-2 pb-3 pt-[6px]">
        <BrandMark :tile="26" :glyph="15" />
        <span class="font-mono text-[15px] font-semibold tracking-tight">{{ branding.productName }}</span>
      </div>

      <!-- org / project switcher -->
      <div class="relative mb-[6px]">
        <button
          class="flex w-full items-center gap-2 rounded border border-border bg-surface-2 px-[10px] py-2 text-left hover:border-border-strong"
          type="button"
          @click="switcherOpen = !switcherOpen"
        >
          <span class="grid h-[22px] w-[22px] place-items-center rounded-[6px] bg-gradient-to-br from-accent to-accent-2 text-[11px] font-bold text-white">
            {{ (ws.orgName[0] || "·").toUpperCase() }}
          </span>
          <span v-if="ws.loading && !ws.orgId" class="flex min-w-0 flex-1 flex-col gap-[5px] py-[2px]" aria-label="Loading workspace">
            <span class="h-[10px] w-[70%] animate-pulse rounded bg-inset motion-reduce:animate-none"></span>
            <span class="h-[9px] w-[45%] animate-pulse rounded bg-inset motion-reduce:animate-none"></span>
          </span>
          <span v-else class="flex min-w-0 flex-col leading-tight">
            <b class="truncate text-[13px] font-semibold">{{ ws.orgName || "—" }}</b>
            <span class="truncate text-[11px] text-ink-3">{{ ws.projectName || "organization" }}</span>
          </span>
          <span class="ml-auto text-ink-3">
            <svg viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor" stroke-width="2"><path d="M8 9l4 4 4-4" /></svg>
          </span>
        </button>

        <template v-if="switcherOpen">
          <div class="fixed inset-0 z-20" @click="switcherOpen = false"></div>
          <div class="absolute left-0 right-0 top-[calc(100%+4px)] z-30 max-h-[70vh] overflow-y-auto rounded border border-border-strong bg-surface p-1 shadow-lg">
            <div class="px-[9px] pb-1 pt-2 text-[10px] font-semibold uppercase tracking-[0.09em] text-ink-3">Organization</div>
            <button
              v-for="o in ws.orgs"
              :key="o.id"
              class="flex w-full items-center gap-2 rounded-sm px-[9px] py-[6px] text-left text-[13px] hover:bg-surface-2"
              :class="o.id === ws.orgId ? 'font-medium text-accent' : 'text-ink-2'"
              type="button"
              @click="pickOrg(o.id!)"
            >
              <span class="truncate">{{ o.name || o.slug }}</span>
              <svg v-if="o.id === ws.orgId" class="ml-auto h-[14px] w-[14px]" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4"><path d="M5 12l5 5 9-11" /></svg>
            </button>
            <button
              v-if="session.isGlobalAdmin"
              class="flex w-full items-center gap-2 rounded-sm px-[9px] py-[6px] text-left text-[13px] font-medium text-accent hover:bg-surface-2"
              type="button"
              @click="openCreate('org')"
            >
              <svg viewBox="0 0 24 24" class="h-[15px] w-[15px]" fill="none" stroke="currentColor" stroke-width="2.2"><path d="M12 5v14M5 12h14" /></svg>
              New organization
            </button>

            <div class="mx-1 my-1 border-t border-border"></div>
            <div class="px-[9px] pb-1 pt-1 text-[10px] font-semibold uppercase tracking-[0.09em] text-ink-3">Project</div>
            <button
              v-for="p in ws.projects"
              :key="p.id"
              class="flex w-full items-center gap-2 rounded-sm px-[9px] py-[6px] text-left text-[13px] hover:bg-surface-2"
              :class="p.id === ws.projectId ? 'font-medium text-accent' : 'text-ink-2'"
              type="button"
              @click="pickProject(p.id!)"
            >
              <span class="truncate">{{ p.name || p.slug }}</span>
              <svg v-if="p.id === ws.projectId" class="ml-auto h-[14px] w-[14px]" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4"><path d="M5 12l5 5 9-11" /></svg>
            </button>
            <p v-if="!ws.projects.length" class="px-[9px] py-2 text-[12px] text-ink-3">No projects in this org.</p>
            <button
              v-if="canManageOrg"
              class="flex w-full items-center gap-2 rounded-sm px-[9px] py-[6px] text-left text-[13px] font-medium text-accent hover:bg-surface-2"
              type="button"
              @click="openCreate('project')"
            >
              <svg viewBox="0 0 24 24" class="h-[15px] w-[15px]" fill="none" stroke="currentColor" stroke-width="2.2"><path d="M12 5v14M5 12h14" /></svg>
              New project
            </button>
          </div>
        </template>
      </div>

      <template v-for="sec in sections" :key="sec.label">
        <div class="px-[10px] pb-1 pt-3 text-[10.5px] font-semibold uppercase tracking-[0.09em] text-ink-3">{{ sec.label }}</div>
        <RouterLink
          v-for="item in sec.items"
          :key="item.key"
          :to="item.to"
          class="flex items-center gap-[10px] rounded-sm px-[10px] py-[7px] text-[13.5px]"
          :class="active === item.key ? 'bg-accent-weak font-medium text-accent' : 'text-ink-2 hover:bg-surface-2 hover:text-ink'"
        >
          <svg viewBox="0 0 24 24" class="h-[16px] w-[16px] shrink-0" :class="active === item.key ? 'opacity-100' : 'opacity-80'" fill="none" stroke="currentColor" stroke-width="1.9">
            <template v-for="(s, i) in item.icon" :key="i">
              <path v-if="s.t === 'path'" :d="s.d" />
              <rect v-else-if="s.t === 'rect'" :x="s.x" :y="s.y" :width="s.w" :height="s.h" :rx="s.rx" />
              <circle v-else :cx="s.cx" :cy="s.cy" :r="s.r" />
            </template>
          </svg>
          {{ item.label }}
          <span
            v-if="item.key === 'services' && showServicesBadge"
            class="ml-auto rounded-full bg-accent-weak px-[6px] py-px text-[9.5px] font-semibold uppercase tracking-[0.06em] text-accent"
          >new</span>
        </RouterLink>
      </template>

      <div class="flex-1"></div>
      <RouterLink
        v-if="session.isGlobalAdmin"
        :to="{ name: 'admin-outbox' }"
        class="flex items-center gap-[10px] rounded-sm px-[10px] py-[7px] text-[13.5px]"
        :class="active === 'admin-outbox' ? 'bg-accent-weak font-medium text-accent' : 'text-ink-2 hover:bg-surface-2 hover:text-ink'"
      >
        <svg viewBox="0 0 24 24" class="h-[16px] w-[16px] shrink-0" :class="active === 'admin-outbox' ? 'opacity-100' : 'opacity-80'" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
          <path d="M4 4h16v12H5.2L4 18z" /><path d="M8 9h8M8 12h5" />
        </svg>
        Dead-letter
      </RouterLink>
      <RouterLink
        :to="{ name: 'settings' }"
        class="flex items-center gap-[10px] rounded-sm px-[10px] py-[7px] text-[13.5px]"
        :class="active === 'settings' ? 'bg-accent-weak font-medium text-accent' : 'text-ink-2 hover:bg-surface-2 hover:text-ink'"
      >
        <svg viewBox="0 0 24 24" class="h-[16px] w-[16px] shrink-0" :class="active === 'settings' ? 'opacity-100' : 'opacity-80'" fill="none" stroke="currentColor" stroke-width="1.9">
          <template v-for="(s, i) in settingsIcon" :key="i">
            <path v-if="s.t === 'path'" :d="s.d" />
            <rect v-else-if="s.t === 'rect'" :x="s.x" :y="s.y" :width="s.w" :height="s.h" :rx="s.rx" />
            <circle v-else :cx="s.cx" :cy="s.cy" :r="s.r" />
          </template>
        </svg>
        Settings
      </RouterLink>
      <div
        v-if="session.version"
        class="px-[10px] pb-[2px] pt-[6px] font-mono text-[10.5px] text-ink-3"
        :title="session.commit && session.commit !== 'unknown' ? 'commit ' + session.commit : ''"
      >cerbix {{ session.version }}</div>
    </aside>

    <main class="min-w-0">
      <div
        v-if="branding.announcement.enabled && branding.announcement.text"
        class="flex items-center gap-2 px-[22px] py-[7px] text-[12.5px] font-medium"
        :class="announceCls[branding.announcement.level || 'info']"
      >
        <span>📣</span><span>{{ branding.announcement.text }}</span>
      </div>
      <header class="sticky top-0 z-10 flex h-14 items-center gap-3 border-b border-border bg-surface px-[22px]">
        <nav class="flex items-center gap-2 text-[13.5px] text-ink-3">
          <template v-for="(c, i) in crumbs" :key="i">
            <span v-if="i" class="text-border-strong">/</span>
            <span :class="i === crumbs.length - 1 ? 'font-semibold text-ink' : ''">{{ c }}</span>
          </template>
        </nav>
        <div class="ml-auto flex items-center gap-2">
          <span
            v-if="live.started && !live.connected"
            class="inline-flex items-center gap-[7px] rounded-full bg-degraded-weak px-[11px] py-[3px] text-[12px] font-medium text-degraded"
            title="The live update stream dropped — statuses may be stale until it reconnects"
          >
            <span class="h-2 w-2 animate-pulse rounded-full bg-degraded motion-reduce:animate-none"></span>
            Live updates reconnecting…
          </span>
          <SearchBox />
          <slot name="actions" />
          <button class="grid h-[34px] w-[34px] place-items-center rounded-sm border border-border bg-surface text-ink-2 hover:border-border-strong hover:text-ink" type="button" aria-label="Toggle theme" @click="toggle">
            <svg viewBox="0 0 24 24" class="h-4 w-4" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="4" /><path d="M12 2v2M12 20v2M2 12h2M20 12h2M5 5l1.5 1.5M17.5 17.5L19 19M19 5l-1.5 1.5M6.5 17.5L5 19" /></svg>
          </button>
          <div class="relative">
            <button
              class="grid h-[30px] w-[30px] place-items-center rounded-full bg-gradient-to-br from-accent to-accent-2 text-xs font-bold text-white"
              type="button"
              aria-label="Account menu"
              :title="session.user?.email"
              @click="userMenuOpen = !userMenuOpen"
            >
              {{ initials }}
            </button>
            <template v-if="userMenuOpen">
              <div class="fixed inset-0 z-20" @click="userMenuOpen = false"></div>
              <div class="absolute right-0 top-[calc(100%+6px)] z-30 w-60 rounded border border-border-strong bg-surface p-1 shadow-lg">
                <div class="px-3 py-2">
                  <div class="truncate text-[13px] font-semibold">{{ session.user?.display_name || session.user?.email }}</div>
                  <div class="truncate text-[11.5px] text-ink-3">{{ session.user?.email }}</div>
                  <div v-if="session.isGlobalAdmin" class="mt-[3px] inline-block rounded-full border border-border px-[7px] py-[1px] text-[10px] font-medium uppercase tracking-[0.05em] text-ink-3">Global admin</div>
                </div>
                <div class="mx-1 my-1 border-t border-border"></div>
                <RouterLink
                  :to="{ name: 'settings' }"
                  class="flex items-center gap-[10px] rounded-sm px-3 py-2 text-[13px] text-ink-2 hover:bg-surface-2 hover:text-ink"
                  @click="userMenuOpen = false"
                >
                  <svg viewBox="0 0 24 24" class="h-[15px] w-[15px] shrink-0" fill="none" stroke="currentColor" stroke-width="1.9"><template v-for="(s, i) in settingsIcon" :key="i"><path v-if="s.t === 'path'" :d="s.d" /><circle v-else-if="s.t === 'circle'" :cx="s.cx" :cy="s.cy" :r="s.r" /></template></svg>
                  Settings
                </RouterLink>
                <button
                  type="button"
                  class="flex w-full items-center gap-[10px] rounded-sm px-3 py-2 text-left text-[13px] text-down hover:bg-surface-2"
                  @click="signOut"
                >
                  <svg viewBox="0 0 24 24" class="h-[15px] w-[15px] shrink-0" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4" /><path d="M16 17l5-5-5-5" /><path d="M21 12H9" /></svg>
                  Sign out
                </button>
              </div>
            </template>
          </div>
        </div>
      </header>
      <slot />
    </main>

    <CreateDialog />
  </div>
</template>
