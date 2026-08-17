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

  test("the reliability surface states its honesty and the objective rule holds end to end", async ({ page }) => {
    await page.goto("/");
    const { projectID } = await ensureE2EWorkspace(page);
    const created = await apiSend(page, "post", `/api/v1/projects/${projectID}/services`, {
      slug: `${SLUG}-report`, name: "E2E Report",
    });
    expect(created.status()).toBe(201);
    const svc = await created.json();

    // A fresh service: the live signal is honestly unknown, and the report is a REASON with
    // a dash — never a number.
    await page.goto(`/services/${svc.id}`);
    await expect(page.getByTestId("svc-health-sli")).toHaveText("unknown");
    await expect(page.getByTestId("svc-report-reason")).toContainText("No reliability inputs");
    await expect(page.getByTestId("svc-kpi-availability")).toContainText("—");

    // Declare one SLI member so an objective becomes meaningful; the report moves to the
    // nothing-sealed reason (still a dash, still never 100%).
    const mon = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors`, {
      name: `${SLUG}-report-http`, type: "http", target: "http://cerbix:8080/healthz",
      interval_seconds: 30, region: "core", enabled: false,
    });
    expect(mon.status()).toBe(201);
    const monitor = await mon.json();
    const decl = await apiSend(page, "put", `/api/v1/projects/${projectID}/services/${svc.id}/declaration`,
      { expected_revision: 0, monitors: [monitor.id], sli: [monitor.id] });
    expect(decl.status()).toBe(200);

    await page.reload();
    await expect(page.getByTestId("svc-report-reason")).toContainText("Nothing is sealed yet");

    // The objective editor enforces the D-0165 rule in the browser (no request leaves for
    // an inadmissible value) and the server round-trip stores the canonical number.
    await page.getByTestId("svc-objective-open").click();
    await page.getByTestId("svc-objective-input").fill("100");
    await page.getByTestId("svc-objective-save").click();
    await expect(page.getByTestId("svc-objective-error")).toContainText("below 100");

    await page.getByTestId("svc-objective-input").fill("99.9");
    await page.getByTestId("svc-objective-save").click();
    await expect(page.getByTestId("svc-objective-editor")).toHaveCount(0);
    await expect(page.getByTestId("svc-kpi-availability")).toContainText("objective 99.9%");

    // A STORED objective stays mutable (§11.3): the editor reopens prefilled with the
    // canonical current value and the update round-trips.
    await page.getByTestId("svc-objective-open").click();
    await expect(page.getByTestId("svc-objective-input")).toHaveValue("99.9");
    await page.getByTestId("svc-objective-input").fill("99.99");
    await page.getByTestId("svc-objective-save").click();
    await expect(page.getByTestId("svc-objective-editor")).toHaveCount(0);
    await expect(page.getByTestId("svc-kpi-availability")).toContainText("objective 99.99%");

    // Cleanup: the service and its monitor.
    await apiSend(page, "delete", `/api/v1/projects/${projectID}/services/${svc.id}`);
    await apiSend(page, "delete", `/api/v1/monitors/${monitor.id}`);
  });

  // FR-021 phase 3 (§14.1-14.4): the impact graph on the surface an operator touches.
  // The Go suites prove the transactional contract; this proves the two things a screen
  // can still get wrong — that a cycle is refused as a CYCLE (not merged, not a generic
  // failure), and that the dependency block renders both directions with real health.
  test("the impact graph refuses a cycle and renders both directions", async ({ page }) => {
    await page.goto("/");
    const { projectID } = await ensureE2EWorkspace(page);

    const mk = async (slug: string) => {
      const r = await apiSend(page, "post", `/api/v1/projects/${projectID}/services`, { slug, name: slug });
      expect(r.status()).toBe(201);
      return (await r.json()).id as string;
    };
    const parent = await mk(`${SLUG}-graph-parent`);
    const child = await mk(`${SLUG}-graph-child`);

    // child depends on parent, through the UI's own editor.
    await page.goto(`/services/${child}`);
    await expect(page.getByTestId("service-dependencies")).toBeVisible();
    await page.getByTestId("svc-dep-edit").click();
    await page.locator(`[data-testid="svc-dep-option"] input[data-slug="${SLUG}-graph-parent"]`).check();
    await page.getByTestId("svc-dep-save").click();
    await expect(page.getByTestId("svc-dep-upstream")).toContainText(`${SLUG}-graph-parent`);
    await expect(page.getByTestId("svc-dep-count")).toContainText("1 / 20");

    // The parent's screen shows the reverse direction — the same edge, read from the
    // other end, with a REAL health signal rather than a blank ([298] P2-2: the claim is
    // now asserted, not merely commented).
    await page.goto(`/services/${parent}`);
    await expect(page.getByTestId("svc-dep-downstream")).toContainText(`${SLUG}-graph-child`);
    // A service with no declaration has no SLI, so the honest signal is "unknown" — the
    // point is that SOMETHING categorical is rendered, never an empty cell.
    await expect(page.getByTestId("svc-dep-downstream-health")).toHaveText(/Operational|Degraded|Down|Unknown|unknown/);
    // Both services are UI-owned, so neither row carries a file-pin chip.
    await expect(page.getByTestId("svc-dep-managed-chip")).toHaveCount(0);

    // Closing the loop is refused AS A CYCLE, with the editor still open so the operator
    // can fix their own edit.
    await page.getByTestId("svc-dep-edit").click();
    await page.locator(`[data-testid="svc-dep-option"] input[data-slug="${SLUG}-graph-child"]`).check();
    await page.getByTestId("svc-dep-save").click();
    await expect(page.getByTestId("svc-dep-save-error")).toContainText("dependency_cycle");
    await expect(page.getByTestId("svc-dep-editor")).toBeVisible();

    // And the graph did not move: the parent still has no upstream edges.
    const graph = await apiGet(page, `/api/v1/projects/${projectID}/services/${parent}/dependencies`);
    expect(graph.depends_on ?? []).toEqual([]);
  });
});
