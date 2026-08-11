import { test, expect } from "@playwright/test";
import { apiSend, firstProject } from "./helpers";

// FR-018 — deleting a project from Settings → Danger Zone. Non-admin visibility is
// covered by the api unit test (TestDeleteProjectAuthz); the shared admin session here
// can't cheaply assume a second role, so this spec exercises the happy path + cascade.
test.describe("project deletion (FR-018)", () => {
  test("org admin deletes a project from Danger Zone; the cascade removes its monitors", async ({ page }) => {
    const { orgID } = await firstProject(page);
    const slug = `e2e-del-${Date.now()}`;

    // Arrange: an e2e project with a monitor in it (to prove the cascade).
    const createdRes = await apiSend(page, "post", `/api/v1/organizations/${orgID}/projects`, { slug, name: "e2e delete me" });
    expect(createdRes.ok(), "create project").toBeTruthy();
    const project = await createdRes.json();
    const monRes = await apiSend(page, "post", `/api/v1/projects/${project.id}/monitors`, {
      name: `e2e-${slug}`, type: "tcp", target: "localhost:5432", region: "core",
      interval_seconds: 60, timeout_seconds: 5, retries: 1, failure_threshold: 2, renotify_seconds: 3600,
    });
    expect(monRes.ok(), "create monitor").toBeTruthy();
    const monitor = await monRes.json();

    let deleted = false;
    try {
      // Pre-select the new project in the workspace, then open its Danger Zone.
      await page.addInitScript(
        ([o, p]) => {
          localStorage.setItem("cerbix.org", o);
          localStorage.setItem("cerbix.project", p);
        },
        [orgID, project.id] as [string, string],
      );
      await page.goto("/settings?tab=danger");

      // Open the confirm modal; the delete button is gated on typing the slug.
      await page.getByRole("button", { name: "Delete project" }).click();
      const confirm = page.getByRole("button", { name: "Delete project", exact: true });
      await expect(confirm).toBeVisible();
      await expect(confirm).toBeDisabled();
      await page.getByPlaceholder(slug).fill(slug);
      await expect(confirm).toBeEnabled();
      await confirm.click();

      // Redirects to the dashboard; the project and (via cascade) its monitor are gone.
      await expect(page).toHaveURL(/\/$/, { timeout: 15_000 });
      expect((await page.request.get(`/api/v1/projects/${project.id}`)).status(), "project gone").toBe(404);
      expect((await page.request.get(`/api/v1/monitors/${monitor.id}`)).status(), "monitor cascaded").toBe(404);
      deleted = true;
    } finally {
      if (!deleted) await apiSend(page, "delete", `/api/v1/projects/${project.id}`);
    }
  });
});
