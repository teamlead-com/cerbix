import { test, expect } from "@playwright/test";
import { apiGet, apiSend, firstProject, cleanupMonitors } from "./helpers";

test.describe("monitors", () => {
  test.afterEach(async ({ page }) => {
    const { projectID } = await firstProject(page);
    await cleanupMonitors(page, projectID);
  });

  test("http monitor: create, detail card, pause, delete", async ({ page }) => {
    const { projectID } = await firstProject(page);
    const r = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors`, {
      name: "e2e-http", type: "tcp", target: "localhost:5432", region: "core",
      interval_seconds: 30, timeout_seconds: 5, retries: 1, failure_threshold: 2, renotify_seconds: 3600,
    });
    expect(r.status()).toBe(201);
    const mon = await r.json();

    await page.goto(`/monitors/${mon.id}`);
    // The Configuration card shows the full alerting config (D-0109).
    await expect(page.locator("dt", { hasText: "Failure threshold" })).toBeVisible();
    await expect(page.locator("dt", { hasText: "Re-notify" })).toBeVisible();
    await expect(page.locator("dt", { hasText: "Updated" })).toBeVisible();

    await page.locator("button", { hasText: "Pause" }).click();
    await expect(page.locator("text=Paused")).toBeVisible();

    await page.locator("button", { hasText: "Delete" }).click();
    // Delete flow may confirm — accept either an inline confirm or direct removal.
    const confirm = page.locator("button", { hasText: /Confirm|Delete/ }).last();
    if (await confirm.isVisible().catch(() => false)) await confirm.click().catch(() => {});
    await page.waitForTimeout(800);
    const left = await apiGet(page, `/api/v1/projects/${projectID}/monitors`);
    expect(left.some((m: any) => m.id === mon.id)).toBeFalsy();
  });

  test("push monitor: endpoint panel and a live heartbeat", async ({ page }) => {
    const { projectID } = await firstProject(page);
    const r = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors`, {
      name: "e2e-push", type: "push", target: "", region: "core",
      interval_seconds: 60, timeout_seconds: 10, grace_seconds: 120,
    });
    expect(r.status()).toBe(201);
    const mon = await r.json();
    expect(mon.push_token).toBeTruthy();

    // The public heartbeat endpoint answers without auth (dead-man's switch contract).
    const hb = await page.request.post(`/api/v1/public/push/${mon.push_token}`);
    expect(hb.status()).toBe(200);

    await page.goto(`/monitors/${mon.id}`);
    await expect(page.locator("h3", { hasText: "Push endpoint" })).toBeVisible();
    await expect(page.locator("code", { hasText: mon.push_token })).toBeVisible();
    await expect(page.locator("dt", { hasText: "Grace period" })).toBeVisible();
  });

  test("instance monitor defaults prefill the new-monitor form", async ({ page }) => {
    const before = await apiGet(page, "/api/v1/settings/monitor-defaults");
    const set = { ...before, interval_seconds: 120, timeout_seconds: 15, retries: 2 };
    expect((await apiSend(page, "put", "/api/v1/settings/monitor-defaults", set)).ok()).toBeTruthy();
    try {
      await page.goto("/monitors/new");
      await page.waitForTimeout(1200); // defaults fetch
      await expect(page.locator('input[type="number"]').first()).toHaveValue("120");
    } finally {
      await apiSend(page, "put", "/api/v1/settings/monitor-defaults", before);
    }
  });

  test("explicit zero retries survives the create (pointer semantics)", async ({ page }) => {
    const { projectID } = await firstProject(page);
    const r = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors`, {
      name: "e2e-zero", type: "tcp", target: "localhost:5432", region: "core", retries: 0, renotify_seconds: 0,
    });
    const mon = await r.json();
    expect(mon.retries).toBe(0);
    expect(mon.renotify_seconds).toBe(0);
  });
});
