import { beforeEach, describe, expect, it, vi } from "vitest";

// FR-041a D21, mock 5 / 5c: `/i/:ref` is a PROTECTED route — the existing guard sends a visitor
// who is not signed in to `/login?redirect=/i/<ref>`, and the login page says nothing about the
// incident. The view modules are replaced: only the route table and the guard are under test.

const session = vi.hoisted(() => ({
  loaded: true,
  isAuthed: false,
  isGlobalAdmin: false,
  fetchMe: vi.fn(),
}));
vi.mock("@/stores/session", () => ({ useSession: () => session }));
vi.mock("@/views/LoginView.vue", () => ({ default: { name: "LoginView", template: "<div />" } }));
vi.mock("@/views/DashboardView.vue", () => ({ default: { name: "DashboardView", template: "<div />" } }));
vi.mock("@/views/IncidentRefView.vue", () => ({ default: { name: "IncidentRefView", template: "<div />" } }));

import { router } from "@/router";

describe("router — /i/:ref", () => {
  beforeEach(() => {
    session.isAuthed = false;
  });

  it("/i/X7K2P9QM resolves to incident-ref and is not public", () => {
    const r = router.resolve("/i/X7K2P9QM");
    expect(r.name).toBe("incident-ref");
    expect(r.params.ref).toBe("X7K2P9QM");
    expect(r.meta.public).toBeFalsy();
    const record = router.getRoutes().find((x) => x.name === "incident-ref");
    expect(record?.path).toBe("/i/:ref");
    expect(record?.meta.public).toBeFalsy();
  });

  it("5c: a visitor who is not signed in is sent to the login page with the link as its redirect", async () => {
    await router.push("/i/x7k2p9qm");
    expect(router.currentRoute.value.name).toBe("login");
    expect(router.currentRoute.value.query.redirect).toBe("/i/x7k2p9qm");
  });

  it("a signed-in user reaches the view", async () => {
    session.isAuthed = true;
    await router.push("/i/X7K2P9QM");
    expect(router.currentRoute.value.name).toBe("incident-ref");
  });

  // NOT a test of LoginView's password path, and not the off-site refusal itself. That refusal is
  // structural: createWebHistory always builds the new URL as this origin + base + the location, and
  // history.pushState refuses a cross-origin URL anyway, so no string handed to router.push leaves
  // the site. This test pins only WHERE such a value lands: the catch-all route sends it to "/".
  it("router.push of a //host or /\\host string lands on / through the catch-all route", async () => {
    session.isAuthed = true;
    await router.push("/i/X7K2P9QM");
    await router.push("//evil.example/i/X");
    expect(router.currentRoute.value.fullPath).toBe("/");
    await router.push("/i/X7K2P9QM");
    await router.push("/\\evil.example");
    expect(router.currentRoute.value.fullPath).toBe("/");
  });
});
