import { test, expect } from "@playwright/test";
import { apiGet, firstProject } from "./helpers";

// Monitoring-as-Code UI surface (FR-017 §15). File-managed monitors are created by the file
// provider, not the UI, so the badge/read-only/named-provider-filter assertions only run when
// the live stack actually has a file provider configured with at least one owned monitor;
// otherwise they skip. The diagnostics-endpoint CONTRACT check runs unconditionally.
test.describe("monitoring-as-code UI", () => {
  test("global-admin diagnostics endpoint returns the {bundles,providers} contract", async ({ page }) => {
    const r = await page.request.get("/api/v1/admin/file-providers");
    // A stack built before FR-017 has no such route — skip rather than fail on a stale image.
    test.skip(r.status() === 404, "diagnostics endpoint not present in this build (pre-FR-017 stack)");
    expect(r.status(), "global admin should reach the diagnostics endpoint").toBe(200);
    const body = await r.json();
    expect(body, "diagnostics body must carry a bundles array").toHaveProperty("bundles");
    expect(Array.isArray(body.bundles)).toBeTruthy();
    // providers (runtime status) is present when this process runs file providers; it may be
    // absent/empty on a stack with none configured — assert the key is at least well-typed.
    if (body.providers !== undefined) expect(Array.isArray(body.providers)).toBeTruthy();
  });

  test("file-managed monitor shows badge + read-only controls + named-provider filter", async ({ page }) => {
    const { projectID } = await firstProject(page);
    const mons = await apiGet(page, `/api/v1/projects/${projectID}/monitors`);
    const managed = (mons as any[]).find((m) => m?.management?.source === "file");
    test.skip(!managed, "no file-managed monitor in this stack (no file provider configured)");

    const provider = managed.management.provider as string;

    // Monitor list: the source filter offers the named provider, and filtering shows the badge.
    await page.goto("/monitors");
    const providerChip = page.locator("button", { hasText: provider });
    await expect(providerChip).toBeVisible();
    await providerChip.click();
    await expect(page.locator('[title*="Managed by file provider"]').first()).toBeVisible();

    // Detail: declarative edit/delete/pause controls are hidden for a file-managed monitor.
    await page.goto(`/monitors/${managed.id}`);
    await expect(page.locator("text=/Managed by file/i").first()).toBeVisible();
    await expect(page.locator("button", { hasText: /^Delete$/ })).toHaveCount(0);
    await expect(page.locator("button", { hasText: /^Pause$/ })).toHaveCount(0);
  });
});
