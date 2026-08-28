import { test, expect } from "@playwright/test";
import { apiGet, apiSend, ensureE2EWorkspace } from "./helpers";

// The browser gate for FR-021 phase 5. The Go suite proves the arming conjunction and the
// transactional contract; this proves the thing an operator actually has to be able to read, on a
// running system, through the surface they touch:
//
//   1. the declaration and the COVERAGE are two different answers. Turning `owns_paging` on does not
//      make the badge say armed — the server decides that, and it says why not;
//   2. a paging edit DIS-ARMS until the next evaluation, and the panel must show that rather than
//      the pre-save green it was rendered with. This is the case a badge cached at page load gets
//      wrong, and it is the one an operator would believe;
//   3. a monitor that is NOT covered says so, in its own words, on its own page — because "why did
//      nothing page me" is the question this feature has to survive.
//
// Everything is prefixed `e2e-` and cleaned up, per the suite's contract with dev stacks.

test.describe("alerting ownership", () => {
  const SLUG = "e2e-alerting";
  const MON = "e2e-alerting-http";

  test.afterEach(async ({ page }) => {
    const { projectID } = await ensureE2EWorkspace(page);
    for (const s of await apiGet(page, `/api/v1/projects/${projectID}/services`)) {
      if ((s.service.slug as string).startsWith("e2e-")) {
        await apiSend(page, "delete", `/api/v1/projects/${projectID}/services/${s.service.id}`);
      }
    }
    for (const m of await apiGet(page, `/api/v1/projects/${projectID}/monitors`)) {
      if ((m.name as string).startsWith("e2e-alerting-")) {
        await apiSend(page, "delete", `/api/v1/monitors/${m.id}`);
      }
    }
  });

  test("the declaration is not coverage, and an edit dis-arms until re-evaluated", async ({ page }) => {
    await page.goto("/");
    const { projectID } = await ensureE2EWorkspace(page);

    const created = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors`, {
      name: MON, type: "tcp", target: "localhost:5432", region: "core",
      interval_seconds: 30, timeout_seconds: 5, retries: 1, failure_threshold: 1,
    });
    expect(created.status(), "create monitor").toBe(201);
    const monitors = await apiGet(page, `/api/v1/projects/${projectID}/monitors`);
    const mon = monitors.find((m: any) => m.name === MON);

    const svc = await apiSend(page, "post", `/api/v1/projects/${projectID}/services`, {
      slug: SLUG, name: "E2E Alerting",
    });
    expect(svc.status(), "create service").toBe(201);
    const services = await apiGet(page, `/api/v1/projects/${projectID}/services`);
    const svcID = services.find((s: any) => s.service.slug === SLUG).service.id as string;

    // Declare the monitor as an SLI, so the service has something to cover. Members are monitor
    // IDs and the write is CAS'd on the revision it expects, exactly as the SPA does it.
    const decl = await apiSend(page, "put", `/api/v1/projects/${projectID}/services/${svcID}/declaration`, {
      expected_revision: 0, monitors: [mon.id], sli: [mon.id],
    });
    expect(decl.status(), "declare").toBe(200);

    await page.goto(`/services/${svcID}`);
    const panel = page.getByTestId("service-alerting");
    await expect(panel).toBeVisible();

    // Ownership OFF: the server's own reason, rendered — not a blank and not a green badge.
    await expect(page.getByTestId("alerting-badge-live")).not.toContainText("armed");
    await expect(page.getByTestId("alerting-signal-live")).toContainText("own paging");

    // Turn ownership ON through the UI. The declaration changes; the COVERAGE does not follow it,
    // because nothing has evaluated the new configuration yet. A UI that rendered the switch as
    // coverage would go green here, which is exactly the confusion the badges exist to remove.
    await page.getByTestId("alerting-owns-paging").check();
    await page.getByTestId("alerting-save").click();
    await expect(page.getByTestId("alerting-save")).toBeDisabled();
    await expect(page.getByTestId("alerting-badge-live")).not.toContainText("armed");

    // The server agrees, through its own endpoint: the declaration is stored, the coverage is not
    // armed, and it says why.
    const state = await apiGet(page, `/api/v1/projects/${projectID}/services/${svcID}/alerting/state`);
    expect(state.live.armed, "coverage must not follow the switch").toBe(false);
    expect(String(state.live.reason).length, "not armed always says why").toBeGreaterThan(0);
    const stored = await apiGet(page, `/api/v1/projects/${projectID}/services/${svcID}/alerting`);
    expect(stored.owns_paging, "the declaration WAS stored").toBe(true);

    // A partial edit leaves the rest alone — the promise §16.6a makes about an omitted field.
    const patched = await apiSend(page, "patch",
      `/api/v1/projects/${projectID}/services/${svcID}/alerting`, { confirm_evaluations: 4 });
    expect(patched.status()).toBe(200);
    const after = await apiGet(page, `/api/v1/projects/${projectID}/services/${svcID}/alerting`);
    expect(after.owns_paging, "a confirm-only edit must not disown the service").toBe(true);
    expect(after.confirm_evaluations).toBe(4);

    // The repeat cadence, THROUGH THE CONTROL an operator uses (D-0185). It shipped unable to save:
    // the field was missing from the store's diff, which is both the audit line and the no-op gate,
    // so the write returned 200 and committed nothing. Every unit test set the column with direct
    // SQL and none of them noticed. Typing it into the form and reading the server back is the shape
    // that would have.
    await page.reload();
    await expect(panel).toBeVisible();
    // A value DIFFERENT from whatever is stored, computed rather than hard-coded. A literal made the
    // assertion depend on what earlier runs had left behind: when the service already held it the
    // form was not dirty, Save stayed disabled, and the test passed or failed by accident of order.
    // It failed in the full suite and passed on its own, which is the signature of exactly that.
    const wantCadence = Number(after.renotify_seconds ?? 0) + 900;
    await page.getByTestId("alerting-renotify").fill(String(wantCadence));
    await expect(page.getByTestId("alerting-save"), "a changed cadence must make the form dirty")
      .toBeEnabled();
    await page.getByTestId("alerting-save").click();
    await expect(page.getByTestId("alerting-save")).toBeDisabled();
    const cadence = await apiGet(page, `/api/v1/projects/${projectID}/services/${svcID}/alerting`);
    expect(cadence.renotify_seconds, "the cadence typed into the form must reach the database")
      .toBe(wantCadence);
    expect(cadence.confirm_evaluations, "a cadence edit must not disturb the rest").toBe(4);

    // And the monitor's own page answers "why did nothing page me": not delegated, with a reason.
    await page.goto(`/monitors/${mon.id}`);
    await expect(page.getByTestId("monitor-delegation-live")).toContainText("still alerts for itself");
    await expect(page.getByTestId("monitor-delegated")).toHaveCount(0);
  });

  test("a server-owned field is refused on the wire", async ({ page }) => {
    await page.goto("/");
    const { projectID } = await ensureE2EWorkspace(page);
    const svc = await apiSend(page, "post", `/api/v1/projects/${projectID}/services`, {
      slug: SLUG, name: "E2E Alerting",
    });
    expect(svc.status()).toBe(201);
    const services = await apiGet(page, `/api/v1/projects/${projectID}/services`);
    const svcID = services.find((s: any) => s.service.slug === SLUG).service.id as string;

    // The generation is the server's, and a body carrying it is a 400 rather than a value that
    // silently wins or is silently dropped.
    const refused = await apiSend(page, "patch",
      `/api/v1/projects/${projectID}/services/${svcID}/alerting`,
      { owns_paging: true, alert_config_generation: 99 });
    expect(refused.status(), "a server-owned field must be refused").toBe(400);

    const stored = await apiGet(page, `/api/v1/projects/${projectID}/services/${svcID}/alerting`);
    expect(stored.owns_paging, "a refused request must write NOTHING").toBe(false);
  });
});
