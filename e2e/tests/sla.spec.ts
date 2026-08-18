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

  // The project objective (iter-0155, mock approved): a promise about the WHOLE project, distinct from
  // the mean across its monitors. Round-trip through the browser, then Clear — because "clearing takes
  // the budget with it" is the decision that is easiest to implement halfway.
  test("project objective round-trips and clearing takes the budget with it", async ({ page }) => {
    const { projectID } = await firstProject(page);

    // Start clean: an unset window answers 404, and Clear on it must too.
    await apiSend(page, "delete", `/api/v1/projects/${projectID}/sla-target?window=30d`);

    await page.goto("/sla");
    await expect(page.getByTestId("project-objective")).toBeVisible();
    await expect(page.getByTestId("project-objective-unset")).toHaveText("not set");

    await page.getByTestId("project-objective-input").fill("99.9");
    await page.getByTestId("project-objective-save").click();
    await expect(page.getByTestId("project-objective-value")).toContainText("99.9");
    // The report is what states the budget, so its presence proves the server derived it.
    await expect(page.getByTestId("project-objective-budget")).toBeVisible();

    const stated = await apiGet(page, `/api/v1/projects/${projectID}/sla`);
    const w30 = (stated as { windows: { window: string; objective?: number; error_budget?: unknown }[] }).windows.find(
      (w) => w.window === "30d",
    );
    expect(w30?.objective).toBe(99.9);
    expect(w30?.error_budget).toBeTruthy();

    await page.getByTestId("project-objective-clear").click();
    await expect(page.getByTestId("project-objective-unset")).toHaveText("not set");
    const after = await apiGet(page, `/api/v1/projects/${projectID}/sla`);
    const w30after = (after as { windows: { window: string; objective?: number; error_budget?: unknown }[] }).windows.find(
      (w) => w.window === "30d",
    );
    expect(w30after?.objective).toBeUndefined();
    expect(w30after?.error_budget).toBeUndefined();
  });
});
