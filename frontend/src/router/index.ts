import { createRouter, createWebHistory, type RouteRecordRaw } from "vue-router";

import { useSession } from "@/stores/session";

const routes: RouteRecordRaw[] = [
  {
    path: "/login",
    name: "login",
    component: () => import("@/views/LoginView.vue"),
    meta: { public: true },
  },
  {
    path: "/forgot",
    name: "forgot-password",
    component: () => import("@/views/ForgotPasswordView.vue"),
    meta: { public: true },
  },
  {
    path: "/reset",
    name: "reset-password",
    component: () => import("@/views/ResetPasswordView.vue"),
    meta: { public: true },
  },
  {
    path: "/",
    name: "dashboard",
    component: () => import("@/views/DashboardView.vue"),
  },
  {
    path: "/monitors",
    name: "monitors",
    component: () => import("@/views/MonitorsView.vue"),
  },
  {
    path: "/monitors/new",
    name: "monitor-new",
    component: () => import("@/views/NewMonitorView.vue"),
  },
  {
    path: "/monitors/:id",
    name: "monitor",
    component: () => import("@/views/MonitorDetailView.vue"),
  },
  {
    path: "/monitors/:id/edit",
    name: "monitor-edit",
    component: () => import("@/views/NewMonitorView.vue"),
  },
  {
    path: "/sla",
    name: "sla",
    component: () => import("@/views/SlaView.vue"),
  },
  {
    // Members moved into Settings → Organization; keep old bookmarks working.
    path: "/members",
    redirect: { path: "/settings", query: { tab: "members" } },
  },
  {
    path: "/settings",
    name: "settings",
    component: () => import("@/views/SettingsView.vue"),
  },
  {
    path: "/admin/outbox",
    name: "admin-outbox",
    component: () => import("@/views/AdminOutboxView.vue"),
    meta: { globalAdmin: true },
  },
  {
    path: "/incidents",
    name: "incidents",
    component: () => import("@/views/IncidentsView.vue"),
  },
  {
    path: "/escalation",
    name: "escalation",
    component: () => import("@/views/EscalationView.vue"),
  },
  {
    path: "/incidents/new",
    name: "incident-new",
    component: () => import("@/views/NewIncidentView.vue"),
  },
  {
    path: "/incidents/:id",
    name: "incident",
    component: () => import("@/views/IncidentDetailView.vue"),
  },
  {
    path: "/status",
    name: "status",
    component: () => import("@/views/StatusPagesView.vue"),
  },
  {
    path: "/status/:slug",
    name: "public-status",
    component: () => import("@/views/PublicStatusView.vue"),
    meta: { public: true },
  },
  { path: "/:pathMatch(.*)*", redirect: "/" },
];

export const router = createRouter({
  history: createWebHistory(),
  routes,
});

// Resolve the session once, then gate routes.
router.beforeEach(async (to) => {
  const session = useSession();
  if (!session.loaded) {
    try {
      await session.fetchMe();
    } catch {
      /* treated as unauthenticated */
    }
  }
  if (to.meta.public) {
    // Public routes are open to anyone; only the login page bounces authed users.
    return session.isAuthed && to.name === "login" ? { name: "dashboard" } : true;
  }
  if (!session.isAuthed) {
    return { name: "login", query: { redirect: to.fullPath } };
  }
  // Global-admin-only routes bounce non-admins to the dashboard.
  if (to.meta.globalAdmin && !session.isGlobalAdmin) {
    return { name: "dashboard" };
  }
  return true;
});
