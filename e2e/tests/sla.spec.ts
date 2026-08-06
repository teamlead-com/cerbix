import { test, expect } from "@playwright/test";
import { apiGet, apiSend, firstProject, cleanupMonitors } from "./helpers";

test.describe("SLA editor", () => {
  test.afterEach(async ({ page }) => {
    const { projectID } = await firstProject(page);
    await cleanupMonitors(page, projectID);
  });

  test("objective set inline; burn rules round-trip", async ({ page }) => {
    const { projectID } = await firstProject(page);
    const created = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors`, {
      name: "e2e-slo", type: "tcp", target: "localhost:5432", region: "core",
    });
    const mon = await created.json();

    // Inline objective editing on the SLA page.
    await page.goto("/sla");
    const row = page.locator("tr", { hasText: "e2e-slo" }).first();
    await expect(row).toBeVisible();
    await row.locator("button", { hasText: "Set" }).click();
    await row.locator('input[type="number"]').fill("99.5");
    await row.locator("button", { hasText: "Save" }).click();
    await page.waitForTimeout(800);

    const sla = await apiGet(page, `/api/v1/monitors/${mon.id}/sla`);
    const w30 = (sla.windows ?? []).find((w: any) => w.window === "30d");
    expect(w30?.objective).toBeCloseTo(99.5);

    // Burn rules persist through the API contract the editor uses (D-0098).
    const rules = [
      { severity: "page", threshold: 14.4, long_window_seconds: 3600, short_window_seconds: 300 },
      { severity: "ticket", threshold: 3, long_window_seconds: 21600, short_window_seconds: 1800 },
    ];
    const put = await apiSend(page, "put", `/api/v1/monitors/${mon.id}/sla-target`, {
      objective: 99.5, window: "30d", burn_alert: true, burn_rules: rules,
    });
    expect(put.ok(), await put.text()).toBeTruthy();
    const rt = await apiGet(page, `/api/v1/monitors/${mon.id}/sla`);
    const rtw = (rt.windows ?? []).find((w: any) => w.window === "30d");
    expect(rtw?.burn_rules?.length).toBe(2);
    expect(rtw?.burn_alert).toBeTruthy();
  });
});
