import { test, expect } from "@playwright/test";
import { apiGet } from "./helpers";

test.describe("dead-letter admin surface", () => {
  test("page renders and the list endpoint answers", async ({ page }) => {
    const dead = await apiGet(page, "/api/v1/admin/outbox/dead");
    expect(Array.isArray(dead)).toBeTruthy();
    await page.goto("/admin/outbox");
    await expect(page.getByText(/[Dd]ead/).first()).toBeVisible();
  });
});
