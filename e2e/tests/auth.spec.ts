import { test, expect } from "@playwright/test";
import { ADMIN, login } from "./helpers";

test.use({ storageState: { cookies: [], origins: [] } });

test.describe("authentication", () => {
  test("rejects a wrong password", async ({ page }) => {
    await page.goto("/login");
    await page.fill('input[type="email"]', ADMIN.email);
    await page.fill('input[type="password"]', "definitely-wrong");
    await page.click('button[type="submit"]');
    await expect(page.locator("text=Invalid email or password")).toBeVisible();
  });

  test("signs in locally and shows the build version", async ({ page }) => {
    await login(page);
    await expect(page.locator("aside >> text=Settings")).toBeVisible();
    // Sidebar footer carries the running build (iter D-0105).
    await expect(page.locator("aside").getByText(/cerbix (dev|v)/)).toBeVisible();
  });

  test("logout ends the session", async ({ page }) => {
    await login(page);
    await page.request.post("/auth/logout");
    await page.goto("/");
    await expect(page).toHaveURL(/\/login/);
  });

  test("unauthenticated API access is rejected", async ({ page }) => {
    const r = await page.request.get("/api/v1/organizations");
    expect(r.status()).toBe(401);
  });
});
