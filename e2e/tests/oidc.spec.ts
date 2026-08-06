import { test, expect } from "@playwright/test";

// Requires the sso compose profile (Keycloak with the cerbix realm). Skips
// cleanly when the SSO button is absent.
test.use({ storageState: { cookies: [], origins: [] } });

test.describe("OIDC login (sso profile)", () => {
  test("full authorization-code round-trip via Keycloak", async ({ page }) => {
    await page.goto("/login");
    const sso = page.locator("button", { hasText: /SSO|Keycloak/ });
    test.skip(!(await sso.count()), "OIDC is not configured on this instance");
    await sso.click();
    await page.waitForURL(/realms\/cerbix/, { timeout: 15_000 });
    await page.fill("#username", "testuser@example.com");
    await page.fill("#password", "password");
    await page.click("#kc-login");
    await page.waitForURL((u) => !u.href.includes("realms"), { timeout: 15_000 });
    const me = await page.request.get("/api/v1/me");
    expect(me.ok()).toBeTruthy();
    const body = await me.json();
    expect(body.user.email).toBe("testuser@example.com");
  });
});
