import { test, expect } from "@playwright/test";
import { apiSend } from "./helpers";

// FR-019 — deleting an organization from Settings → Danger Zone (global admin). The dev
// bootstrap admin is a global admin, so the org Danger Zone tab is visible. Non-admin
// authorization is covered by the api unit test (TestDeleteOrganizationAuthz).
test.describe("organization deletion (FR-019)", () => {
  test("global admin deletes an org from Danger Zone; the cascade removes its projects & monitors", async ({ page }) => {
    const slug = `e2e-org-${Date.now()}`;

    // Arrange: an e2e org with a project + monitor (to prove the two-level cascade).
    const orgRes = await apiSend(page, "post", "/api/v1/organizations", { slug, name: "e2e delete-me org" });
    expect(orgRes.ok(), "create org").toBeTruthy();
    const org = await orgRes.json();
    const projRes = await apiSend(page, "post", `/api/v1/organizations/${org.id}/projects`, { slug: "svc", name: "svc" });
    expect(projRes.ok(), "create project").toBeTruthy();
    const project = await projRes.json();
    const monRes = await apiSend(page, "post", `/api/v1/projects/${project.id}/monitors`, {
      name: `e2e-${slug}`, type: "tcp", target: "localhost:5432", region: "core",
      interval_seconds: 60, timeout_seconds: 5, retries: 1, failure_threshold: 2, renotify_seconds: 3600,
    });
    expect(monRes.ok(), "create monitor").toBeTruthy();
    const monitor = await monRes.json();

    let deleted = false;
    try {
      // Select the new org in the workspace, then open its Danger Zone.
      await page.addInitScript(
        ([o, p]) => {
          localStorage.setItem("cerbix.org", o);
          localStorage.setItem("cerbix.project", p);
        },
        [org.id, project.id] as [string, string],
      );
      await page.goto("/settings?tab=orgdanger");

      // Open the confirm modal; the delete button is gated on typing the org slug.
      await page.getByRole("button", { name: "Delete organization" }).click();
      const confirm = page.getByRole("button", { name: "Delete organization", exact: true });
      await expect(confirm).toBeVisible();
      await expect(confirm).toBeDisabled();
      await page.getByPlaceholder(slug).fill(slug);
      await expect(confirm).toBeEnabled();
      await confirm.click();

      // Redirects to the dashboard; the org and (via cascade) its project + monitor are gone.
      await expect(page).toHaveURL(/\/$/, { timeout: 15_000 });
      expect((await page.request.get(`/api/v1/organizations/${org.id}`)).status(), "org gone").toBe(404);
      expect((await page.request.get(`/api/v1/projects/${project.id}`)).status(), "project cascaded").toBe(404);
      expect((await page.request.get(`/api/v1/monitors/${monitor.id}`)).status(), "monitor cascaded").toBe(404);
      deleted = true;
    } finally {
      if (!deleted) await apiSend(page, "delete", `/api/v1/organizations/${org.id}`);
    }
  });
});
