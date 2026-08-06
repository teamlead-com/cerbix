import { test, expect } from "@playwright/test";
import { apiSend, firstProject } from "./helpers";

test.describe("escalation & on-call", () => {
  test("channel → policy create, edit via PUT, confirmed delete", async ({ page }) => {
    const { projectID } = await firstProject(page);
    const ch = await apiSend(page, "post", `/api/v1/projects/${projectID}/notification-channels`, {
      type: "webhook", name: "e2e-chan", config: { url: "https://ops.example.com/e2e" },
    });
    expect(ch.status()).toBe(201);
    const chan = await ch.json();
    try {
      await page.goto("/escalation");
      const pol = page.locator("section", { hasText: "Escalation policies" });
      await pol.locator("input[placeholder*='Policy name']").fill("e2e-policy");
      await pol.locator("button", { hasText: "+ target" }).first().click();
      await pol.locator("select").first().selectOption({ index: 1 });
      await pol.locator("button", { hasText: "Create policy" }).click();
      await expect(pol.locator("b", { hasText: "e2e-policy" })).toBeVisible();

      // Edit: composer switches to PUT mode (D-0122).
      await pol.locator("button", { hasText: "Edit" }).first().click();
      await expect(pol.locator("text=editing")).toBeVisible();
      await pol.locator("input[placeholder*='Policy name']").fill("e2e-policy-renamed");
      await pol.locator("button", { hasText: "Save changes" }).click();
      await expect(pol.locator("b", { hasText: "e2e-policy-renamed" })).toBeVisible();

      // Delete requires an inline confirm that states the blast radius.
      await pol.locator("button", { hasText: "Delete" }).first().click();
      await expect(pol.locator("text=Confirm delete")).toBeVisible();
      await pol.locator("button", { hasText: "Confirm delete" }).click();
      await expect(pol.locator("b", { hasText: "e2e-policy-renamed" })).toHaveCount(0);
    } finally {
      await apiSend(page, "delete", `/api/v1/notification-channels/${chan.id}`);
    }
  });
});
