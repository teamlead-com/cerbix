import { test as setup, expect } from "@playwright/test";
import { ADMIN, ensureE2EWorkspace } from "./helpers";

// One real sign-in for the whole run; specs reuse the stored session.
setup("authenticate as the bootstrap admin", async ({ page, browser }) => {
  await page.goto("/login");
  await page.fill('input[type="email"]', ADMIN.email);
  await page.fill('input[type="password"]', ADMIN.password);
  const loginResponse = page.waitForResponse(
    (response) => response.url().includes("/auth/local/login") && response.request().method() === "POST",
  );
  await page.click('button[type="submit"]');
  expect((await loginResponse).status(), "local bootstrap login").toBe(200);
  await expect(page.locator("aside")).toBeVisible({ timeout: 15_000 });
  const workspace = await ensureE2EWorkspace(page);
  await page.evaluate(
    ([orgID, projectID]) => {
      localStorage.setItem("cerbix.org", orgID);
      localStorage.setItem("cerbix.project", projectID);
    },
    [workspace.orgID, workspace.projectID] as [string, string],
  );
  await page.reload();
  await expect(page.locator("aside")).toBeVisible({ timeout: 15_000 });
  await page.context().storageState({ path: ".auth/admin.json" });

  // The full single-stack suite includes admin flows that need a second user.
  // Provision the deterministic Keycloak fixture up front so a fresh database
  // has the same coverage as a retained developer database.
  if ((process.env.CERBIX_TOPOLOGY ?? "single") === "single") {
    const context = await browser.newContext({
      baseURL: process.env.CERBIX_URL || "http://localhost:8080",
    });
    try {
      const oidcPage = await context.newPage();
      await oidcPage.goto("/login");
      const sso = oidcPage.locator("button", { hasText: /SSO|Keycloak/ });
      await expect(sso, "single-stack E2E requires the configured Keycloak profile").toBeVisible({ timeout: 15_000 });
      await sso.click();
      await oidcPage.waitForURL(/realms\/cerbix/, { timeout: 15_000 });
      await oidcPage.fill("#username", "testuser@example.com");
      await oidcPage.fill("#password", "password");
      await oidcPage.click("#kc-login");
      await oidcPage.waitForURL((url) => !url.href.includes("realms"), { timeout: 15_000 });
      const me = await oidcPage.request.get("/api/v1/me");
      expect(me.ok(), "OIDC fixture user JIT provisioning").toBeTruthy();
    } finally {
      await context.close();
    }
  }
});
