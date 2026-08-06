import { test as setup, expect } from "@playwright/test";
import { ADMIN } from "./helpers";

// One real sign-in for the whole run; specs reuse the stored session.
setup("authenticate as the bootstrap admin", async ({ page }) => {
  await page.goto("/login");
  await page.fill('input[type="email"]', ADMIN.email);
  await page.fill('input[type="password"]', ADMIN.password);
  await page.click('button[type="submit"]');
  await expect(page.locator("aside")).toBeVisible({ timeout: 15_000 });
  await page.context().storageState({ path: ".auth/admin.json" });
});
