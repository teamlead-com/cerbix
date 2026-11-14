import { flushPromises, mount } from "@vue/test-utils";
import { beforeEach, describe, expect, it, vi } from "vitest";

import StatusPagesView from "@/views/StatusPagesView.vue";

// sec-hardening-2 H-3 fix 4 (mock-sec-hardening-2-iter3.html, approved 2026-11-14): the unlisted
// token reaches org admins only. A member who reads an unlisted page without it opens the page and
// its feeds the way an internal page opens — the authed preview and the authed feed — and sees the
// muted "Secret link" line instead of the token. An org admin's links keep the token.

const apiMock = vi.hoisted(() => ({ GET: vi.fn(), POST: vi.fn(), PATCH: vi.fn(), DELETE: vi.fn() }));
vi.mock("@/api/client", () => ({ api: apiMock }));
vi.mock("@/components/AppShell.vue", () => ({
  default: { name: "AppShell", props: { crumbs: { type: Array, default: () => [] } }, template: "<div><slot name=\"actions\" /><slot /></div>" },
}));
const admin = vi.hoisted(() => ({ value: false }));
vi.mock("@/stores/session", () => ({ useSession: () => ({ isOrgAdmin: () => admin.value }) }));
vi.mock("@/stores/workspace", () => ({
  useWorkspace: () => ({ init: () => Promise.resolve(), orgId: "o1", projectId: "p1", orgName: "Acme", projectName: "Payments" }),
}));

const RouterLink = { props: ["to"], template: "<a :data-to=\"to\"><slot /></a>" };

async function mountWith(page: Record<string, unknown>) {
  apiMock.GET.mockImplementation((path: string) => {
    if (path.includes("organizations")) return Promise.resolve({ data: [page] }); // the page list
    return Promise.resolve({ data: [] });
  });
  const wrapper = mount(StatusPagesView, { global: { stubs: { RouterLink } } });
  await flushPromises();
  const button = wrapper.findAll("button").find((b) => b.text().includes("Customer status"));
  await button!.trigger("click");
  await flushPromises();
  return wrapper;
}

const base = { id: "sp9", org_id: "o1", slug: "customer", title: "Customer status", visibility: "unlisted" };

describe("StatusPagesView — links to an unlisted page", () => {
  beforeEach(() => {
    apiMock.GET.mockReset();
  });

  it("without the token (a member without org:manage): the authed preview and feed, and the secret-link line", async () => {
    admin.value = false;
    const w = await mountWith({ ...base, has_unlisted_token: true });
    expect(w.find("[data-to]").attributes("data-to")).toBe("/status/customer?preview=sp9");
    const feeds = w.findAll("a").filter((a) => ["rss", "atom", "json"].includes(a.text())).map((a) => a.attributes("href"));
    expect(feeds).toEqual([
      "/api/v1/status-pages/sp9/feed?format=rss",
      "/api/v1/status-pages/sp9/feed?format=atom",
      "/api/v1/status-pages/sp9/feed?format=json",
    ]);
    expect(w.find("[data-testid=secret-link-note]").text()).toBe("Secret link · only organization admins see the token");
    expect(w.text()).not.toContain("unlisted token:");
  });

  it("with the token (an org admin): unchanged — the public page and feeds carry it, and the token is shown", async () => {
    admin.value = true;
    const w = await mountWith({ ...base, unlisted_token: "tok123", has_unlisted_token: true });
    expect(w.find("[data-to]").attributes("data-to")).toBe("/status/customer?token=tok123");
    const rss = w.findAll("a").find((a) => a.text() === "rss");
    expect(rss!.attributes("href")).toBe("/api/v1/public/status-pages/customer/feed?format=rss&token=tok123");
    expect(w.text()).toContain("unlisted token: tok123");
    expect(w.find("[data-testid=secret-link-note]").exists()).toBe(false);
  });
});
