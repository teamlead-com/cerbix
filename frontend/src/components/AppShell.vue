<script setup lang="ts">
import { computed, onMounted, ref, watch } from "vue";
import { RouterLink, type RouteLocationRaw, useRouter } from "vue-router";

import BrandMark from "@/components/BrandMark.vue";
import CreateDialog from "@/components/CreateDialog.vue";
import NavigationLinks from "@/components/NavigationLinks.vue";
import OverlaySurface from "@/components/OverlaySurface.vue";
import SearchBox from "@/components/SearchBox.vue";
import WorkspaceSwitcher from "@/components/WorkspaceSwitcher.vue";
import { useTheme } from "@/composables/useTheme";
import { useBranding } from "@/stores/branding";
import { useLive } from "@/stores/live";
import { useSession } from "@/stores/session";
import { useUi } from "@/stores/ui";
import { useWorkspace } from "@/stores/workspace";

export interface BreadcrumbItem {
  label: string;
  to?: RouteLocationRaw;
}

withDefaults(
  defineProps<{ active?: string; crumbs?: BreadcrumbItem[] }>(),
  { active: "dashboard", crumbs: () => [] },
);

const { theme, toggle } = useTheme();
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
const themeLabel = computed(() => (theme.value === "dark" ? "Switch to light theme" : "Switch to dark theme"));
const themePressed = computed(() => theme.value === "dark");
const announceLive = computed(() => (branding.announcement.level === "critical" ? "assertive" : "polite"));

onMounted(() => session.fetchVersion()); // cached in the store — one request per session

// The Services badge is a temporary adoption affordance: it points at a screen the operator
// has not met yet, and it must disappear the moment the project HAS a service — including one
// a bundle created, which is why the nav probes rather than waiting for a visit.
onMounted(() => ws.ensureServiceCount());
watch(() => ws.projectId, () => ws.ensureServiceCount());
const canManageOrg = computed(() => !!ws.orgId && session.isOrgAdmin(ws.orgId));

const switcherOpen = ref(false);
const drawerSwitcherOpen = ref(false);
const userMenuOpen = ref(false);
const drawerOpen = ref(false);

function openCreate(kind: "org" | "project") {
  switcherOpen.value = false;
  drawerSwitcherOpen.value = false;
  ui.openCreate(kind);
}

async function signOut() {
  userMenuOpen.value = false;
  await session.logout();
  router.push({ name: "login" });
}

function projectSelected(inDrawer = false) {
  if (inDrawer) drawerOpen.value = false;
  router.push({ name: "dashboard" });
}

function organizationSelected(inDrawer = false) {
  if (inDrawer) drawerOpen.value = false;
  router.push({ name: "dashboard" });
}

function closeDrawer() {
  drawerSwitcherOpen.value = false;
  drawerOpen.value = false;
}

function closeUserMenu() {
  userMenuOpen.value = false;
}
</script>

<template>
  <div data-testid="app-shell" class="grid min-h-screen grid-cols-[240px_1fr] max-[900px]:grid-cols-1">
    <aside class="sticky top-0 flex h-screen flex-col gap-1 border-r border-border bg-surface p-3 max-[900px]:hidden">
      <div class="flex items-center gap-[9px] px-2 pb-3 pt-[6px]">
        <BrandMark :tile="26" :glyph="15" />
        <span class="font-mono text-[15px] font-semibold tracking-tight">{{ branding.productName }}</span>
      </div>

      <WorkspaceSwitcher
        v-model:open="switcherOpen"
        :can-manage-org="canManageOrg"
        @create="openCreate"
        @project-selected="projectSelected()"
        @organization-selected="organizationSelected()"
      />
      <NavigationLinks :active="active" />

      <div
        v-if="session.version"
        class="mt-auto px-[10px] pb-[2px] pt-[6px] font-mono text-[10.5px] text-ink-3"
        :title="session.commit && session.commit !== 'unknown' ? 'commit ' + session.commit : ''"
      >cerbix {{ session.version }}</div>
    </aside>

    <main class="min-w-0">
      <div
        v-if="branding.announcement.enabled && branding.announcement.text"
        role="status"
        aria-atomic="true"
        :aria-live="announceLive"
        class="flex items-center gap-2 px-[22px] py-[7px] text-[12.5px] font-medium"
        :class="announceCls[branding.announcement.level || 'info']"
      >
        <span>{{ branding.announcement.text }}</span>
      </div>
      <header data-testid="app-topbar" class="sticky top-0 z-10 flex h-14 items-center gap-3 border-b border-border bg-surface px-[22px] max-[900px]:h-auto max-[900px]:min-h-14 max-[900px]:flex-wrap max-[900px]:gap-2 max-[900px]:px-3 max-[900px]:py-2">
        <button
          data-testid="navigation-trigger"
          class="hidden h-[34px] w-[34px] place-items-center rounded-sm border border-border bg-surface text-ink-2 hover:border-border-strong hover:text-ink max-[900px]:grid"
          type="button"
          aria-label="Open navigation"
          aria-controls="navigation-drawer"
          :aria-expanded="drawerOpen"
          @click="drawerOpen = true"
        >
          <svg viewBox="0 0 24 24" class="h-[18px] w-[18px]" fill="none" stroke="currentColor" stroke-width="2"><path d="M4 6h16M4 12h16M4 18h16" /></svg>
        </button>

        <nav aria-label="Breadcrumb" class="flex min-w-0 items-center gap-2 overflow-hidden text-[13.5px] text-ink-2 max-[900px]:order-1 max-[900px]:flex-1">
          <template v-for="(crumb, index) in crumbs" :key="index">
            <span v-if="index" aria-hidden="true" class="text-border-strong">/</span>
            <RouterLink
              v-if="crumb.to"
              :to="crumb.to"
              class="truncate text-ink-2 hover:text-accent"
            >{{ crumb.label }}</RouterLink>
            <span
              v-else
              class="truncate"
              :class="index === crumbs.length - 1 ? 'font-semibold text-ink' : ''"
              :aria-current="index === crumbs.length - 1 ? 'page' : undefined"
            >{{ crumb.label }}</span>
          </template>
        </nav>

        <div class="ml-auto flex items-center gap-2 max-[900px]:order-2 max-[900px]:ml-0 max-[900px]:w-full max-[900px]:flex-wrap">
          <span
            v-if="live.started && !live.connected"
            class="inline-flex items-center gap-[7px] rounded-full bg-degraded-weak px-[11px] py-[3px] text-[12px] font-medium text-degraded"
            title="The live update stream dropped — statuses may be stale until it reconnects"
          >
            <span class="h-2 w-2 animate-pulse rounded-full bg-degraded motion-reduce:animate-none"></span>
            Live updates reconnecting…
          </span>
          <div class="max-[900px]:min-w-0 max-[900px]:flex-1"><SearchBox /></div>
          <slot name="actions" />
          <button
            data-testid="theme-toggle"
            class="grid h-[34px] w-[34px] place-items-center rounded-sm border border-border bg-surface text-ink-2 hover:border-border-strong hover:text-ink"
            type="button"
            :aria-label="themeLabel"
            :aria-pressed="themePressed"
            @click="toggle"
          >
            <svg viewBox="0 0 24 24" class="h-4 w-4" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="4" /><path d="M12 2v2M12 20v2M2 12h2M20 12h2M5 5l1.5 1.5M17.5 17.5L19 19M19 5l-1.5 1.5M6.5 17.5L5 19" /></svg>
          </button>
          <div class="relative">
            <button
              id="account-menu-trigger"
              class="grid h-[30px] w-[30px] place-items-center rounded-full bg-accent text-xs font-bold text-accent-ink"
              type="button"
              aria-label="Account menu"
              aria-controls="account-menu"
              :aria-expanded="userMenuOpen"
              :title="session.user?.email"
              @click="userMenuOpen = !userMenuOpen"
            >
              {{ initials }}
            </button>
            <OverlaySurface
              :open="userMenuOpen"
              id="account-menu"
              role="menu"
              :modal="false"
              :close-on-outside="true"
              labelledby="account-menu-trigger"
              class="contents"
              @close="closeUserMenu"
            >
              <div
                class="absolute right-0 top-[calc(100%+6px)] z-30 w-60 rounded border border-border-strong bg-surface p-1 shadow-lg"
                role="presentation"
              >
                <div class="px-3 py-2">
                  <div class="truncate text-[13px] font-semibold">{{ session.user?.display_name || session.user?.email }}</div>
                  <div class="truncate text-[11.5px] text-ink-3">{{ session.user?.email }}</div>
                  <div v-if="session.isGlobalAdmin" class="mt-[3px] inline-block rounded-full border border-border px-[7px] py-[1px] text-[10px] font-medium uppercase tracking-[0.05em] text-ink-2">Global admin</div>
                </div>
                <div class="mx-1 my-1 border-t border-border"></div>
                <RouterLink
                  :to="{ name: 'settings' }"
                  role="menuitem"
                  class="flex items-center gap-[10px] rounded-sm px-3 py-2 text-[13px] text-ink-2 hover:bg-surface-2 hover:text-ink"
                  @click="closeUserMenu"
                >
                  <svg viewBox="0 0 24 24" class="h-[15px] w-[15px] shrink-0" fill="none" stroke="currentColor" stroke-width="1.9"><circle cx="12" cy="12" r="3.2" /><path d="M19 12a7 7 0 0 0-.1-1.2l2-1.6-2-3.4-2.4 1a7 7 0 0 0-2-1.2l-.4-2.6H9.9l-.4 2.6a7 7 0 0 0-2 1.2l-2.4-1-2 3.4 2 1.6A7 7 0 0 0 5 12c0 .4 0 .8.1 1.2l-2 1.6 2 3.4 2.4-1a7 7 0 0 0 2 1.2l.4 2.6h4.2l.4-2.6a7 7 0 0 0 2-1.2l2.4 1 2-3.4-2-1.6c.1-.4.1-.8.1-1.2z" /></svg>
                  Settings
                </RouterLink>
                <button
                  type="button"
                  role="menuitem"
                  class="flex w-full items-center gap-[10px] rounded-sm px-3 py-2 text-left text-[13px] text-down hover:bg-surface-2"
                  @click="signOut"
                >
                  <svg viewBox="0 0 24 24" class="h-[15px] w-[15px] shrink-0" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4" /><path d="M16 17l5-5-5-5" /><path d="M21 12H9" /></svg>
                  Sign out
                </button>
              </div>
            </OverlaySurface>
          </div>
        </div>
      </header>
      <slot />
    </main>

    <OverlaySurface
      :open="drawerOpen"
      id="navigation-drawer"
      role="dialog"
      :modal="true"
      :close-on-outside="true"
      labelledby="navigation-drawer-title"
      :initial-focus="'#navigation-drawer-close'"
      class="fixed inset-0 z-50 bg-black/45"
      @close="closeDrawer"
    >
      <aside
        class="fixed inset-y-0 left-0 z-[60] flex w-[286px] max-w-[calc(100vw-24px)] flex-col gap-1 border-r border-border-strong bg-surface p-3 shadow-lg motion-reduce:transition-none"
      >
        <div class="flex items-center gap-[9px] px-2 pb-3 pt-[6px]">
          <BrandMark :tile="26" :glyph="15" />
          <span id="navigation-drawer-title" class="font-mono text-[15px] font-semibold tracking-tight">{{ branding.productName }} navigation</span>
          <button
            id="navigation-drawer-close"
            class="ml-auto grid h-[34px] w-[34px] place-items-center rounded-sm border border-border bg-surface text-lg text-ink-2 hover:border-border-strong hover:text-ink"
            type="button"
            aria-label="Close navigation"
            @click="closeDrawer"
          >×</button>
        </div>
        <WorkspaceSwitcher
          v-model:open="drawerSwitcherOpen"
          menu-id="drawer-workspace-menu"
          :can-manage-org="canManageOrg"
          @create="openCreate"
          @project-selected="projectSelected(true)"
          @organization-selected="organizationSelected(true)"
        />
        <NavigationLinks :active="active" @navigate="closeDrawer" />
        <p class="mt-auto px-[10px] py-2 text-[11.5px] leading-relaxed text-ink-2">Navigation stays available at narrow widths. Escape or outside activation closes this drawer.</p>
      </aside>
    </OverlaySurface>

    <CreateDialog />
  </div>
</template>
