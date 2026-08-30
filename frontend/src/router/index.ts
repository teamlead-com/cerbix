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
    // Services sits ABOVE Monitors in the nav as a PEER, never nested: a monitor may be in
    // the SLI of several services, or of none.
    path: "/services",
    name: "services",
    component: () => import("@/views/ServicesView.vue"),
  },
  {
    path: "/services/:id",
    name: "service",
    component: () => import("@/views/ServiceDetailView.vue"),
  },
  {
    path: "/services/:id/declaration",
    name: "service-declaration",
    component: () => import("@/views/ServiceDeclarationView.vue"),
  },
  {
    // FR-024 (D-0207): the override history is per SERVICE — an override is an operator's act on
    // one service's gate — so it lives under the service, read-only; revocation is on the card.
    path: "/services/:id/gate/overrides",
    name: "service-gate-overrides",
    component: () => import("@/views/GateOverridesView.vue"),
  },
  {
    // FR-025 (D-0210 item 5, D6/D10): the change timeline is per SERVICE and goes with it — a
    // change is the service's fact, not a ledger that outlives its subject. Read-only; the record
    // is the pipeline's (`cerbix change record`). `?kind=`/`?source=` pre-filter.
    path: "/services/:id/changes",
    name: "service-changes",
    component: () => import("@/views/ServiceChangesView.vue"),
  },
  {
    // FR-025 (D-0210 item 3, D8): the before/after of ONE change, addressed by its identity
    // `?source&external_id` and a `horizon` — there is no by-identity group route, so the
    // comparison is the only per-group read and lives under the service, read-only.
    path: "/services/:id/changes/compare",
    name: "service-change-compare",
    component: () => import("@/views/ChangeCompareView.vue"),
  },
  {
    // FR-024 (D-0207, D10): the decision ledger is PROJECT-scoped, never service-nested — a
    // decision outlives its service, and a route under the service would answer 404 at exactly
    // the moment the evidence is wanted. `?service=<id>` pre-filters.
    path: "/gate/decisions",
    name: "gate-decisions",
    component: () => import("@/views/GateDecisionsView.vue"),
  },
  {
    path: "/gate/decisions/:id",
    name: "gate-decision",
    component: () => import("@/views/GateDecisionView.vue"),
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
