<script setup lang="ts">
import { computed } from "vue";
import { isNavigationFailure, RouterLink, type RouteLocationRaw, useRouter } from "vue-router";

import { useSession } from "@/stores/session";
import { useWorkspace } from "@/stores/workspace";

type Shape =
  | { t: "path"; d: string }
  | { t: "rect"; x: number; y: number; w: number; h: number; rx: number }
  | { t: "circle"; cx: number; cy: number; r: number };
type NavItem = { key: string; label: string; to: RouteLocationRaw; icon: Shape[] };

const props = withDefaults(defineProps<{ active?: string }>(), { active: "dashboard" });
const emit = defineEmits<{ navigate: [] }>();

const session = useSession();
const ws = useWorkspace();
const router = useRouter();
const showServicesBadge = computed(() => ws.serviceCounts[ws.projectId] === 0);

async function navigate(event: MouseEvent, to: RouteLocationRaw) {
  if (event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
  event.preventDefault();
  try {
    const result = await router.push(to);
    if (!isNavigationFailure(result)) emit("navigate");
  } catch {
    // A rejected guard or router error leaves a modal drawer open so the operator can recover.
  }
}

const sections: { label: string; items: NavItem[] }[] = [
  {
    label: "Project",
    items: [
      {
        key: "dashboard",
        label: "Dashboard",
        to: { name: "dashboard" },
        icon: [
          { t: "rect", x: 3, y: 3, w: 7, h: 9, rx: 1.5 },
          { t: "rect", x: 14, y: 3, w: 7, h: 5, rx: 1.5 },
          { t: "rect", x: 14, y: 12, w: 7, h: 9, rx: 1.5 },
          { t: "rect", x: 3, y: 16, w: 7, h: 5, rx: 1.5 },
        ],
      },
      // Order is level of abstraction: the unit of reliability, then what measures it.
      // Nesting Monitors under Services would assert containment, which the model rejects.
      {
        key: "services",
        label: "Services",
        to: { name: "services" },
        icon: [
          { t: "rect", x: 3, y: 4, w: 18, h: 6, rx: 1.6 },
          { t: "rect", x: 3, y: 14, w: 18, h: 6, rx: 1.6 },
          { t: "path", d: "M7 7h.01M7 17h.01" },
        ],
      },
      {
        key: "monitors",
        label: "Monitors",
        to: { name: "monitors" },
        icon: [{ t: "path", d: "M3 12h4l2 6 4-14 2 8h6" }],
      },
      {
        key: "sla",
        label: "SLA & SLO",
        to: { name: "sla" },
        icon: [
          { t: "path", d: "M3 3v18h18" },
          { t: "path", d: "M7 14l3-4 3 3 5-7" },
        ],
      },
      // FR-024 (D-0207): the project's decision LEDGER — project-scoped like SLA, because a
      // decision outlives the service it was about (D10). Read-only for viewer+.
      {
        key: "gate-decisions",
        label: "Gate decisions",
        to: { name: "gate-decisions" },
        icon: [
          { t: "rect", x: 5, y: 3, w: 14, h: 18, rx: 1.5 },
          { t: "path", d: "M8 8h8M8 12h8M8 16h5" },
        ],
      },
      {
        key: "escalation",
        label: "Escalation",
        to: { name: "escalation" },
        icon: [
          { t: "path", d: "M12 3v10" },
          { t: "path", d: "M8 9l4 4 4-4" },
          { t: "path", d: "M4 21h16" },
        ],
      },
      {
        key: "incidents",
        label: "Incidents",
        to: { name: "incidents" },
        icon: [
          { t: "path", d: "M12 9v4M12 17h.01" },
          { t: "path", d: "M10.3 3.9L2 18a2 2 0 0 0 1.7 3h16.6A2 2 0 0 0 22 18L13.7 3.9a2 2 0 0 0-3.4 0z" },
        ],
      },
      {
        key: "status",
        label: "Status pages",
        to: { name: "status" },
        icon: [
          { t: "rect", x: 3, y: 4, w: 18, h: 16, rx: 2 },
          { t: "path", d: "M3 9h18" },
        ],
      },
    ],
  },
];

const settingsIcon: Shape[] = [
  { t: "circle", cx: 12, cy: 12, r: 3.2 },
  {
    t: "path",
    d: "M19 12a7 7 0 0 0-.1-1.2l2-1.6-2-3.4-2.4 1a7 7 0 0 0-2-1.2l-.4-2.6H9.9l-.4 2.6a7 7 0 0 0-2 1.2l-2.4-1-2 3.4 2 1.6A7 7 0 0 0 5 12c0 .4 0 .8.1 1.2l-2 1.6 2 3.4 2.4-1a7 7 0 0 0 2 1.2l.4 2.6h4.2l.4-2.6a7 7 0 0 0 2-1.2l2.4 1 2-3.4-2-1.6c.1-.4.1-.8.1-1.2z",
  },
];

function isActive(key: string) {
  return props.active === key;
}
</script>

<template>
  <nav data-testid="navigation-links" aria-label="Primary navigation" class="flex min-h-0 flex-1 flex-col gap-1">
    <template v-for="section in sections" :key="section.label">
      <div class="px-[10px] pb-1 pt-3 text-[10.5px] font-semibold uppercase tracking-[0.09em] text-ink-2">
        {{ section.label }}
      </div>
      <RouterLink
        v-for="item in section.items"
        :key="item.key"
        :to="item.to"
        :aria-current="isActive(item.key) ? 'page' : undefined"
        :data-nav-label="item.label"
        class="flex items-center gap-[10px] rounded-sm px-[10px] py-[7px] text-[13.5px]"
        :class="isActive(item.key) ? 'bg-accent-weak font-medium text-accent' : 'text-ink-2 hover:bg-surface-2 hover:text-ink'"
        @click.capture="navigate($event, item.to)"
      >
        <svg
          viewBox="0 0 24 24"
          class="h-[16px] w-[16px] shrink-0"
          :class="isActive(item.key) ? 'opacity-100' : 'opacity-80'"
          fill="none"
          stroke="currentColor"
          stroke-width="1.9"
        >
          <template v-for="(shape, index) in item.icon" :key="index">
            <path v-if="shape.t === 'path'" :d="shape.d" />
            <rect v-else-if="shape.t === 'rect'" :x="shape.x" :y="shape.y" :width="shape.w" :height="shape.h" :rx="shape.rx" />
            <circle v-else :cx="shape.cx" :cy="shape.cy" :r="shape.r" />
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
      aria-label="Dead-letter"
      :aria-current="isActive('admin-outbox') ? 'page' : undefined"
      data-nav-label="Dead-letter"
      class="flex items-center gap-[10px] rounded-sm px-[10px] py-[7px] text-[13.5px]"
      :class="isActive('admin-outbox') ? 'bg-accent-weak font-medium text-accent' : 'text-ink-2 hover:bg-surface-2 hover:text-ink'"
      @click.capture="navigate($event, { name: 'admin-outbox' })"
    >
      <svg
        viewBox="0 0 24 24"
        class="h-[16px] w-[16px] shrink-0"
        :class="isActive('admin-outbox') ? 'opacity-100' : 'opacity-80'"
        fill="none"
        stroke="currentColor"
        stroke-width="1.9"
        stroke-linecap="round"
        stroke-linejoin="round"
      >
        <path d="M4 4h16v12H5.2L4 18z" />
        <path d="M8 9h8M8 12h5" />
      </svg>
      Dead-letter
    </RouterLink>
    <RouterLink
      :to="{ name: 'settings' }"
      :aria-current="isActive('settings') ? 'page' : undefined"
      data-nav-label="Settings"
      class="flex items-center gap-[10px] rounded-sm px-[10px] py-[7px] text-[13.5px]"
      :class="isActive('settings') ? 'bg-accent-weak font-medium text-accent' : 'text-ink-2 hover:bg-surface-2 hover:text-ink'"
      @click.capture="navigate($event, { name: 'settings' })"
    >
      <svg
        viewBox="0 0 24 24"
        class="h-[16px] w-[16px] shrink-0"
        :class="isActive('settings') ? 'opacity-100' : 'opacity-80'"
        fill="none"
        stroke="currentColor"
        stroke-width="1.9"
      >
        <template v-for="(shape, index) in settingsIcon" :key="index">
          <path v-if="shape.t === 'path'" :d="shape.d" />
          <rect v-else-if="shape.t === 'rect'" :x="shape.x" :y="shape.y" :width="shape.w" :height="shape.h" :rx="shape.rx" />
          <circle v-else :cx="shape.cx" :cy="shape.cy" :r="shape.r" />
        </template>
      </svg>
      Settings
    </RouterLink>
  </nav>
</template>
