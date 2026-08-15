import { test, expect } from "@playwright/test";
import { apiGet, apiSend, ensureE2EWorkspace } from "./helpers";

// The §9 browser gate for FR-020. The shell smoke proves the API and dispatch path; this
// proves the surface an operator actually touches — and the guards that only exist because
// a secret is referenced by something: a rename or delete that would strand a monitor must
// be refused, not silently applied.
//
// Everything is prefixed `e2e-` and cleaned up, per the suite's contract with dev stacks.
test.describe("secrets", () => {
  const NAME = "e2e-db-password";

  test("the panel manages a secret's whole life and refuses to strand a monitor", async ({ page }) => {
    // The workspace helper talks to the API through the PAGE, so the page has to be on an
    // http origin first — from about:blank a relative fetch has no base to resolve against.
    await page.goto("/");
    const { projectID } = await ensureE2EWorkspace(page);
    const base = `/api/v1/projects/${projectID}/secrets`;

    // Start clean even if a previous run died mid-way.
    await apiSend(page, "delete", `${base}/${NAME}`);
    await apiSend(page, "delete", `${base}/${NAME}-renamed`);

    await page.goto(`/settings?tab=secrets&project=${projectID}`);
    await expect(page.getByTestId("secret-add-name")).toBeVisible();

    // Create through the UI, not the API: the point is that the panel works.
    await page.getByTestId("secret-add-name").fill(NAME);
    await page.getByTestId("secret-add-value").fill("e2e-initial-value");
    await page.getByTestId("secret-add-submit").click();
    await expect(page.locator(`text=${NAME}`).first()).toBeVisible({ timeout: 10_000 });

    // The value is write-only: it must not come back from any read surface, and the
    // rendered page must not contain it either.
    const listed = await apiGet(page, base);
    const entry = (listed as any[]).find((s) => s.name === NAME);
    expect(entry, "created secret is listed").toBeTruthy();
    expect(JSON.stringify(listed)).not.toContain("e2e-initial-value");
    expect(await page.content()).not.toContain("e2e-initial-value");

    let monitorID = "";
    try {
      // Reference it from a monitor, which is what turns the guards on.
      const created = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors`, {
        name: "e2e-secret-consumer",
        type: "redis",
        target: "127.0.0.1:6379",
        interval_seconds: 3600,
        timeout_seconds: 5,
        enabled: false,
        config: { password_ref: NAME, tls: "false" },
      });
      expect(created.ok(), `monitor create: ${created.status()} ${await created.text()}`).toBeTruthy();
      monitorID = (await created.json()).id;

      // used_by reflects the reference — the operator can see what a delete would break.
      await expect
        .poll(async () => {
          const rows = await apiGet(page, base);
          return (rows as any[]).find((s) => s.name === NAME)?.used_by?.total ?? 0;
        }, { timeout: 10_000 })
        .toBeGreaterThan(0);

      // Rotation keeps the reference intact: same name, same consumers, new value.
      const rotated = await apiSend(page, "patch", `${base}/${NAME}`, { value: "e2e-rotated-value" });
      expect(rotated.ok(), `rotate: ${rotated.status()}`).toBeTruthy();
      const afterRotate = (await apiGet(page, base)) as any[];
      expect(afterRotate.find((s) => s.name === NAME)?.used_by?.total).toBeGreaterThan(0);
      expect(JSON.stringify(afterRotate)).not.toContain("e2e-rotated-value");

      // Deleting a referenced secret is refused — stranding a monitor is the failure this
      // guard exists to prevent, and 409 is how the panel learns to say so.
      const blocked = await apiSend(page, "delete", `${base}/${NAME}`);
      expect(blocked.status(), "delete of a referenced secret").toBe(409);
      expect(await blocked.text()).toContain("secret_in_use");

      // A UI-managed reference re-points atomically on rename: the monitor follows.
      const renamed = await apiSend(page, "patch", `${base}/${NAME}`, { name: `${NAME}-renamed` });
      expect(renamed.ok(), `rename: ${renamed.status()} ${await renamed.text()}`).toBeTruthy();
      const monitor = await apiGet(page, `/api/v1/monitors/${monitorID}`);
      expect(monitor.config.password_ref, "reference followed the rename").toBe(`${NAME}-renamed`);

      // The panel shows the new name and no longer the old one.
      await page.goto(`/settings?tab=secrets&project=${projectID}`);
      await expect(page.locator(`text=${NAME}-renamed`).first()).toBeVisible({ timeout: 10_000 });
    } finally {
      if (monitorID) {
        await apiSend(page, "delete", `/api/v1/monitors/${monitorID}`);
      }
    }

    // With the consumer gone the delete succeeds — the guard was about the reference, not
    // about the secret being undeletable.
    await expect
      .poll(async () => (await apiSend(page, "delete", `${base}/${NAME}-renamed`)).status(), { timeout: 10_000 })
      .toBeLessThan(300);
    const remaining = (await apiGet(page, base)) as any[];
    expect(remaining.find((s) => s.name === `${NAME}-renamed`)).toBeFalsy();
  });
});
