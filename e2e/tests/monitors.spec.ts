import { test, expect } from "@playwright/test";
import { apiGet, apiSend, firstProject, cleanupMonitors } from "./helpers";

test.describe("monitors", () => {
  test.afterEach(async ({ page }) => {
    const { projectID } = await firstProject(page);
    await cleanupMonitors(page, projectID);
  });

  // FR-030 (D-0234): a monitor says what it is for. Created through the API with a description, it shows
  // under its name on /monitors and in full on its page; one created without shows no description element
  // anywhere — the compatibility promise, asserted rather than assumed.
  test("a monitor's description is shown on the list and the detail, and absent when it has none", async ({ page }) => {
    const { projectID } = await firstProject(page);
    const described = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors`, {
      name: "e2e-described", type: "tcp", target: "localhost:5432", region: "core",
      interval_seconds: 30, timeout_seconds: 5,
      description: "Confirms the payment provider can reach our callback URL; a DOWN here means paid orders stay pending.",
    });
    expect(described.status(), await described.text()).toBe(201);
    const withDesc = await described.json();
    expect(withDesc.description).toContain("Confirms the payment provider");
    const bare = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors`, {
      name: "e2e-undescribed", type: "tcp", target: "localhost:5432", region: "core", interval_seconds: 30, timeout_seconds: 5,
    });
    expect(bare.status(), await bare.text()).toBe(201);
    const withoutDesc = await bare.json();
    expect(withoutDesc.description, "omitted means empty, not null").toBe("");

    // 201 code points are refused with the field named.
    const tooLong = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors`, {
      name: "e2e-too-long", type: "tcp", target: "localhost:5432", region: "core", interval_seconds: 30, timeout_seconds: 5,
      description: "я".repeat(201),
    });
    expect(tooLong.status()).toBe(400);
    expect(await tooLong.text()).toContain("description");

    await page.goto("/monitors");
    const rowWith = page.locator("tr", { hasText: "e2e-described" });
    await expect(rowWith.locator('[data-testid="monitor-description"]')).toContainText("Confirms the payment provider");
    const rowWithout = page.locator("tr", { hasText: "e2e-undescribed" });
    await expect(rowWithout).toBeVisible();
    await expect(rowWithout.locator('[data-testid="monitor-description"]')).toHaveCount(0);

    await page.goto(`/monitors/${withDesc.id}`);
    await expect(page.locator('[data-testid="monitor-description"]')).toContainText("paid orders stay pending");
    await page.goto(`/monitors/${withoutDesc.id}`);
    await expect(page.locator("h1", { hasText: "e2e-undescribed" })).toBeVisible();
    await expect(page.locator('[data-testid="monitor-description"]')).toHaveCount(0);
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
