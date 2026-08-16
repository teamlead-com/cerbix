import { test, expect } from "@playwright/test";
import { apiGet, apiSend, ensureE2EWorkspace } from "./helpers";

// The browser gate for FR-021 phase 1. The Go suite proves the transactional contract; this
// proves the surface an operator actually touches, and specifically the three things this
// feature exists to make impossible to misread:
//
//   1. a service with no declaration reports NO availability — never 100%;
//   2. the context list and the SLI list are separately declared, and adding to the first
//      never adds to the second;
//   3. no availability number is shown anywhere in this release, because nothing computes one.
//
// Everything is prefixed `e2e-` and cleaned up, per the suite's contract with dev stacks.

// Phase 1 computes no availability, so nothing may RENDER one. Scanning the text for "100%"
// does not express that — the screens legitimately say "reports no availability, not 100%",
// and a naive substring check fails on the very sentence that proves the point. What is
// forbidden is a percentage in the position of a VALUE: a leaf element whose whole text is a
// number and a percent sign, which is the shape every KPI in this product has.
async function expectNoRenderedPercentage(page: import("@playwright/test").Page) {
  const values = await page.locator("body *").evaluateAll((els) =>
    els
      .filter((e) => e.children.length === 0 && /^\s*\d+(\.\d+)?\s*%\s*$/.test(e.textContent || ""))
      .map((e) => (e.textContent || "").trim()),
  );
  expect(values, "phase 1 must render no percentage as a value").toEqual([]);
}

test.describe("services", () => {
  const SLUG = "e2e-checkout";
  const MON_A = "e2e-svc-http";
  const MON_B = "e2e-svc-db";

  test.afterEach(async ({ page }) => {
    const { projectID } = await ensureE2EWorkspace(page);
    for (const s of await apiGet(page, `/api/v1/projects/${projectID}/services`)) {
      if ((s.service.slug as string).startsWith("e2e-")) {
        await apiSend(page, "delete", `/api/v1/projects/${projectID}/services/${s.service.id}`);
      }
    }
    for (const m of await apiGet(page, `/api/v1/projects/${projectID}/monitors`)) {
      if ((m.name as string).startsWith("e2e-svc-")) await apiSend(page, "delete", `/api/v1/monitors/${m.id}`);
    }
  });

  test("declare a service: two lists, separately declared, and no invented number", async ({ page }) => {
    await page.goto("/");
    const { projectID } = await ensureE2EWorkspace(page);

    // Two monitors to declare against. Created via API — the point of this spec is the
    // service surface, and monitors.spec.ts already covers monitor creation.
    for (const [name, target] of [[MON_A, "localhost:5432"], [MON_B, "localhost:6379"]]) {
      const r = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors`, {
        name, type: "tcp", target, region: "core",
        interval_seconds: 30, timeout_seconds: 5, retries: 1, failure_threshold: 1,
      });
      expect(r.status(), `create ${name}`).toBe(201);
    }
    const monitors = await apiGet(page, `/api/v1/projects/${projectID}/monitors`);
    const slugOf = (name: string) => monitors.find((m: any) => m.name === name)!.slug as string;

    await page.goto("/services");
    await expect(page.getByTestId("service-create-open")).toBeVisible();

    // Create through the UI: a service with no declaration is a complete state, so this is a
    // finished operation and not the first step of a wizard.
    await page.getByTestId("service-create-open").click();
    await page.getByTestId("service-create-slug").fill(SLUG);
    await page.getByTestId("service-create-name").fill("E2E Checkout");
    await page.getByTestId("service-create-submit").click();
    await page.waitForURL(/\/services\/[0-9a-f-]{36}$/);
    const svcID = page.url().split("/").pop()!;

    // …and it lands on the detail screen, which says so out loud rather than showing 100%.
    await expect(page.getByTestId("service-no-declaration")).toBeVisible();
    await expect(page.getByTestId("service-no-declaration")).toContainText("no availability");
    await expectNoRenderedPercentage(page);

    // Declare: both monitors as context, ONE of them as a reliability input.
    await page.getByTestId("service-edit-declaration").click();
    await expect(page.getByTestId(`service-context-${slugOf(MON_A)}`)).toBeVisible();

    await page.getByTestId(`service-context-${slugOf(MON_A)}`).check();
    await page.getByTestId(`service-context-${slugOf(MON_B)}`).check();

    // The load-bearing assertion of the whole screen: adding to the context does NOT make a
    // monitor count toward availability.
    await expect(page.getByTestId(`service-sli-${slugOf(MON_A)}`)).not.toBeChecked();
    await expect(page.getByTestId(`service-sli-${slugOf(MON_B)}`)).not.toBeChecked();

    await page.getByTestId(`service-sli-${slugOf(MON_A)}`).check();
    await page.getByTestId("service-declaration-save").click();

    // Back on the detail screen: one input, one diagnostic-only member.
    await expect(page.getByTestId("service-sli-member")).toHaveCount(1);
    await expect(page.getByTestId("service-diagnostic-member")).toHaveCount(1);
    await expect(page.getByTestId("service-sli-member")).toContainText(slugOf(MON_A));
    await expect(page.getByTestId("service-diagnostic-member")).toContainText(slugOf(MON_B));

    // The two counts reach the list as two independent numbers.
    await page.goto("/services");
    const row = page.locator('[data-testid="service-row"]').filter({ has: page.locator(`[data-slug="${SLUG}"]`) })
      .or(page.locator(`[data-testid="service-row"][data-slug="${SLUG}"]`));
    await expect(row).toBeVisible();
    await expect(row.getByTestId("service-sli-count")).toHaveText("1");
    await expect(row.getByTestId("service-context-count")).toHaveText("2");

    // The declaration must put the service ON the materialization path. This is the assertion
    // whose absence let a subsystem that produced nothing in production pass every gate: the
    // first version of this spec accepted "not materialized yet" as a resting state, when it
    // was the only state the system could ever reach.
    //
    // The watermark itself is proven in Go (store.TestDeclaringAServiceMakesItMaterialize…),
    // because sealing waits out a 60s bucket plus the late-arrival grace and a browser suite
    // has no business sitting through that.
    const detail = await apiGet(page, `/api/v1/projects/${projectID}/services/${svcID}`);
    expect(detail.materialization.materialization_start,
      "declaring reliability inputs did not start materialization").toBeTruthy();

    // No release-2 number leaks onto the screen. This is the invariant the whole feature is
    // for: the absence of a plausible figure nothing computed.
    await expectNoRenderedPercentage(page);
  });

  test("a stale edit is refused rather than merged", async ({ page }) => {
    await page.goto("/");
    const { projectID } = await ensureE2EWorkspace(page);
    const created = await apiSend(page, "post", `/api/v1/projects/${projectID}/services`, {
      slug: `${SLUG}-conflict`, name: "E2E Conflict",
    });
    expect(created.status()).toBe(201);
    const svc = await created.json();

    // Open the editor on revision 0…
    await page.goto(`/services/${svc.id}/declaration`);
    await expect(page.getByTestId("service-declaration-save")).toBeVisible();

    // …while somebody else declares revision 1 out from under it.
    const other = await apiSend(page, "put",
      `/api/v1/projects/${projectID}/services/${svc.id}/declaration`,
      { expected_revision: 0, monitors: [], sli: [] });
    expect(other.status()).toBe(200);

    // Saving the stale editor is a refusal the operator has to resolve — two people have made
    // two different statements about what availability means, and they are not merged.
    await page.getByTestId("service-declaration-save").click();
    await expect(page.getByTestId("service-save-error")).toBeVisible();
    await expect(page.getByTestId("service-save-error")).toContainText("changed this declaration");
  });
});
