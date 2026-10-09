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

  // iter-0205 (func-admin-users.md "Last active"): signing out must not erase "last active". The
  // fixture user signs in through SSO in a context of its own (never the shared admin session),
  // signs out, and the admin's user list must still show THIS sign-in — not "never", and not the
  // older sign-in auth.setup left behind (which is all the old max(sessions.created_at) could see).
  test("users page: last active survives the user's sign-out", async ({ page, browser }) => {
    test.skip((process.env.CERBIX_TOPOLOGY ?? "single") !== "single", "needs the Keycloak profile of the single stack");
    const lastActiveOf = async () =>
      (await apiGet(page, "/api/v1/admin/users")).find((u: any) => u.email === "testuser@example.com")?.last_active_at ?? null;
    // Read before signing in: on the old code, after the sign-out this is all that is left (the
    // session auth.setup created), so the strict "later than before" below fails whatever the timing.
    const before = await lastActiveOf();
    const signInStarted = Date.now() - 2_000; // slack for clock skew between the runner and the API
    // An EMPTY storage state is required: inside a test, browser.newContext() inherits the
    // project's `use` options — the admin's stored session — and the logout below would then end
    // the session every other spec shares.
    const context = await browser.newContext({
      baseURL: process.env.CERBIX_URL || "http://localhost:8080",
      storageState: { cookies: [], origins: [] },
    });
    try {
      const oidcPage = await context.newPage();
      await oidcPage.goto("/login");
      // A fresh context has no IdP cookie, so Keycloak always shows its form (as in auth.setup).
      await oidcPage.locator("button", { hasText: /SSO|Keycloak/ }).click();
      await oidcPage.waitForURL(/realms\/cerbix/, { timeout: 15_000 });
      await oidcPage.fill("#username", "testuser@example.com");
      await oidcPage.fill("#password", "password");
      await oidcPage.click("#kc-login");
      await oidcPage.waitForURL((url) => !url.href.includes("realms"), { timeout: 15_000 });
      const me = await oidcPage.request.get("/api/v1/me");
      expect(me.ok(), "fixture user signed in").toBeTruthy();
      // Never sign out anyone but the fixture user.
      expect(JSON.stringify(await me.json()), "this context is the fixture user, not the shared admin").toContain("testuser@example.com");
      const out = await oidcPage.request.post("/auth/logout", { maxRedirects: 0 });
      expect([200, 204, 302, 303], `logout status ${out.status()}`).toContain(out.status());
      expect((await oidcPage.request.get("/api/v1/me")).status(), "fixture user signed out").toBe(401);
    } finally {
      await context.close();
    }

    const users = await apiGet(page, "/api/v1/admin/users");
    const guinea = users.find((u: any) => u.email === "testuser@example.com");
    expect(guinea, "fixture user listed").toBeTruthy();
    expect(guinea.last_active_at, "last active after sign-out").toBeTruthy();
    // Soft, so both report: the strict one is the timing-proof check, the other the readable one.
    if (before !== null) {
      expect.soft(Date.parse(guinea.last_active_at), "last active moved past the earlier reading").toBeGreaterThan(Date.parse(before));
    }
    expect.soft(Date.parse(guinea.last_active_at), "last active is this test's sign-in").toBeGreaterThanOrEqual(signInStarted);

    await page.goto("/settings?tab=users");
    const row = page.locator("tr", { hasText: "testuser@example.com" }).first();
    await expect(row).toBeVisible();
    await expect(row.locator("td", { hasText: /^never$/ })).toHaveCount(0);
  });

  test("guards: self-demote and self-delete are 400", async ({ page }) => {
    const me = await apiGet(page, "/api/v1/me");
    const patch = await apiSend(page, "patch", `/api/v1/admin/users/${me.user.id}`, { is_global_admin: false });
    expect(patch.status()).toBe(400);
    const del = await apiSend(page, "delete", `/api/v1/admin/users/${me.user.id}`);
    expect(del.status()).toBe(400);
  });
});
