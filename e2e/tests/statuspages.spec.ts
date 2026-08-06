import { test, expect } from "@playwright/test";
import { apiGet, apiSend, firstProject } from "./helpers";

test.describe("status pages", () => {
  test("full lifecycle: page, grouped component, preview, feeds, subscribers", async ({ page }) => {
    const { orgID } = await firstProject(page);
    const slug = "e2e-status";
    // Fresh start (idempotent re-runs).
    const existing = await apiGet(page, `/api/v1/organizations/${orgID}/status-pages`);
    for (const p of existing) if (p.slug === slug) await apiSend(page, "delete", `/api/v1/status-pages/${p.id}`);

    const created = await apiSend(page, "post", `/api/v1/organizations/${orgID}/status-pages`, {
      slug, title: "E2E Status", visibility: "public",
    });
    expect(created.status()).toBe(201);
    const sp = await created.json();
    try {
      // Component with the once-curl-only fields (D-0114).
      const comp = await apiSend(page, "post", `/api/v1/status-pages/${sp.id}/components`, {
        name: "e2e-comp", group: "E2E group", description: "component description", position: 5, manual_status: "operational",
      });
      expect(comp.status()).toBe(201);

      // Public render shows the group heading and the description.
      await page.goto(`/status/${slug}`);
      await expect(page.locator("text=E2E group")).toBeVisible();
      await expect(page.locator("text=component description")).toBeVisible();

      // Feeds answer for a public page.
      const rss = await page.request.get(`/api/v1/public/status-pages/${slug}/feed?format=rss`);
      expect(rss.status()).toBe(200);

      // Internal visibility: public 404s, ?preview renders with the banner (D-0114),
      // and the authed feed answers while the public one refuses.
      expect((await apiSend(page, "patch", `/api/v1/status-pages/${sp.id}`, { title: "E2E Status", visibility: "internal" })).ok()).toBeTruthy();
      const pub = await page.request.get(`/api/v1/public/status-pages/${slug}`);
      expect(pub.status()).toBe(404);
      await page.goto(`/status/${slug}?preview=${sp.id}`);
      await expect(page.getByText(/Internal page/)).toBeVisible();
      expect((await page.request.get(`/api/v1/status-pages/${sp.id}/feed?format=rss`)).status()).toBe(200);

      // Back to public: the anonymous subscribe form creates a pending subscriber
      // the owner sees in the editor (D-0116).
      await apiSend(page, "patch", `/api/v1/status-pages/${sp.id}`, { title: "E2E Status", visibility: "public" });
      const sub = await page.request.post(`/api/v1/public/status-pages/${slug}/subscribers`, { data: { email: "e2e-sub@example.com" } });
      expect(sub.ok()).toBeTruthy();
      const subs = await apiGet(page, `/api/v1/status-pages/${sp.id}/subscribers`);
      const mine = subs.find((s: any) => s.email === "e2e-sub@example.com");
      expect(mine).toBeTruthy();
      expect(mine.confirmed_at ?? null).toBeNull();
      expect((await apiSend(page, "delete", `/api/v1/status-pages/${sp.id}/subscribers/${mine.id}`)).ok()).toBeTruthy();
    } finally {
      await apiSend(page, "delete", `/api/v1/status-pages/${sp.id}`);
    }
  });
});
