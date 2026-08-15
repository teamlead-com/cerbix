import { test, expect } from "@playwright/test";
import { apiGet, apiSend, firstProject } from "./helpers";

// Users administration (D-0103/D-0104) uses the deterministic Keycloak user
// provisioned by auth.setup, never an arbitrary account from retained data.
test.describe("users administration", () => {
  test("members: invite by email, change role, remove", async ({ page }) => {
    const { orgID } = await firstProject(page);
    const users = await apiGet(page, "/api/v1/admin/users");
    const guinea = users.find((u: any) => u.email === "testuser@example.com");
    expect(guinea, "auth.setup must provision a non-admin OIDC fixture user").toBeTruthy();

    // Ensure not already a member.
    const members = await apiGet(page, `/api/v1/organizations/${orgID}/members`);
    for (const m of members) if (m.user_id === guinea.id) await apiSend(page, "delete", `/api/v1/organizations/${orgID}/members/${m.id}`);

    try {
      await page.goto("/settings?tab=members");
      await page.locator("button", { hasText: "Invite member" }).click();
      await page.fill('input[type="email"]', guinea.email);
      await page.locator("button", { hasText: /Send invite/ }).click();
      const row = page.locator("tr", { hasText: guinea.email });
      await expect(row).toBeVisible();

      // Role change through the select persists.
      await row.locator("select").selectOption("viewer");
      await page.waitForTimeout(600);
      const after = await apiGet(page, `/api/v1/organizations/${orgID}/members`);
      expect(after.find((m: any) => m.user_id === guinea.id).role).toBe("viewer");

      // Remove: the confirm explains only the membership goes away.
      await row.locator('button[aria-label="Remove from the organization"]').click();
      await expect(row.getByText(/The account is kept/)).toBeVisible();
      await row.locator("button", { hasText: "Confirm" }).click();
      await expect(page.locator("tr", { hasText: guinea.email })).toHaveCount(0);
    } finally {
      const after = await apiGet(page, `/api/v1/organizations/${orgID}/members`);
      for (const member of after) {
        if (member.user_id === guinea.id) {
          await apiSend(page, "delete", `/api/v1/organizations/${orgID}/members/${member.id}`);
        }
      }
    }
  });

  test("users page: grant/revoke admin, add to orgs with per-org roles", async ({ page }) => {
    const users = await apiGet(page, "/api/v1/admin/users");
    const guinea = users.find((u: any) => u.email === "testuser@example.com");
    expect(guinea, "auth.setup must provision a non-admin OIDC fixture user").toBeTruthy();
    const { orgID } = await firstProject(page);
    const members = await apiGet(page, `/api/v1/organizations/${orgID}/members`);
    for (const m of members) if (m.user_id === guinea.id) await apiSend(page, "delete", `/api/v1/organizations/${orgID}/members/${m.id}`);

    try {
      await page.goto("/settings?tab=users");
      const row = page.locator("tr", { hasText: guinea.email }).first();
      await row.locator("button", { hasText: "Grant admin" }).click();
      await expect(row.getByText(/Global admin/)).toBeVisible();
      await row.locator("button", { hasText: "Revoke admin" }).click();
      await expect(row.getByText(/Global admin/)).toHaveCount(0);

      // Inline expansion with per-org role picks (D-0113 predecessor flow).
      await row.locator("button", { hasText: "Add to org" }).click();
      const exp = page.locator("td[colspan='5']", { hasText: "Add to organizations" });
      await expect(exp).toBeVisible();
      await exp.locator("button.rounded-full:not([disabled])").first().click();
      await exp.locator("button", { hasText: /^Add/ }).last().click();
      await page.waitForTimeout(1000);
      await expect(page.locator("tr", { hasText: guinea.email }).first().getByText("E2E Harness · editor")).toBeVisible();

      // Self-protection: the admin's own row is locked.
      const self = page.locator("tr", { hasText: "admin@cerbix.local" });
      await expect(self.locator("button[disabled]", { hasText: /admin/ })).toBeVisible();
    } finally {
      await apiSend(page, "patch", `/api/v1/admin/users/${guinea.id}`, { is_global_admin: false });
      const after = await apiGet(page, `/api/v1/organizations/${orgID}/members`);
      for (const member of after) {
        if (member.user_id === guinea.id) {
          await apiSend(page, "delete", `/api/v1/organizations/${orgID}/members/${member.id}`);
        }
      }
    }
  });

  test("guards: self-demote and self-delete are 400", async ({ page }) => {
    const me = await apiGet(page, "/api/v1/me");
    const patch = await apiSend(page, "patch", `/api/v1/admin/users/${me.user.id}`, { is_global_admin: false });
    expect(patch.status()).toBe(400);
    const del = await apiSend(page, "delete", `/api/v1/admin/users/${me.user.id}`);
    expect(del.status()).toBe(400);
  });
});
