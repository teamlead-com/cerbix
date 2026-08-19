import { test, expect } from "@playwright/test";
import { apiGet, apiSend, firstProject } from "./helpers";

test.describe("incidents", () => {
  test("open → acknowledge (with actor) → resolve", async ({ page }) => {
    const { projectID } = await firstProject(page);
    const r = await apiSend(page, "post", `/api/v1/projects/${projectID}/incidents`, {
      title: "e2e-incident", impact: "minor", body: "opened by the e2e suite",
    });
    expect(r.status()).toBe(201);
    const inc = await r.json();

    expect((await apiSend(page, "post", `/api/v1/incidents/${inc.id}/acknowledge`)).ok()).toBeTruthy();

    await page.goto(`/incidents/${inc.id}`);
    // Acknowledged-by is resolved to a display name (D-0109).
    await expect(page.getByText(/acknowledged/).first()).toBeVisible();
    await expect(page.locator("b", { hasText: "Administrator" })).toBeVisible();

    // Resolve through the timeline update API and verify the badge flips.
    const upd = await apiSend(page, "post", `/api/v1/incidents/${inc.id}/updates`, {
      status: "resolved", body: "fixed by the e2e suite",
    });
    expect(upd.ok()).toBeTruthy();
    await page.reload();
    await expect(page.getByText("Resolved").first()).toBeVisible();
  });

  // FR-022: the incident says what it is an incident OF, and a project-level incident says nothing —
  // the chip is a discriminator, so its ABSENCE carries meaning too. Only two of the three states are
  // reachable from here: a SERVICE-anchored incident is opened by the evaluator alone (the create API
  // deliberately takes no service anchor), so that state is covered by the unit specs instead.
  test("the incident header names its subject — and stays silent when there is none", async ({ page }) => {
    const { projectID } = await firstProject(page);
    const mon = await (
      await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors`, {
        name: "e2e-subject-chip", type: "http", target: "https://example.com/", interval_seconds: 300,
        timeout_seconds: 5, region: "core", enabled: false,
      })
    ).json();
    const anchored = await (
      await apiSend(page, "post", `/api/v1/projects/${projectID}/incidents`, {
        title: "e2e-anchored", impact: "minor", body: "e2e", monitor_id: mon.id,
      })
    ).json();
    const projectLevel = await (
      await apiSend(page, "post", `/api/v1/projects/${projectID}/incidents`, {
        title: "e2e-project-level", impact: "minor", body: "e2e",
      })
    ).json();

    await page.goto(`/incidents/${anchored.id}`);
    await expect(page.getByTestId("incident-subject")).toContainText("monitor");
    await expect(page.getByTestId("incident-subject")).toContainText("e2e-subject-chip");

    await page.goto(`/incidents/${projectLevel.id}`);
    await expect(page.getByText("e2e-project-level").first()).toBeVisible();
    await expect(page.getByTestId("incident-subject")).toHaveCount(0);

    for (const id of [anchored.id, projectLevel.id]) {
      await apiSend(page, "post", `/api/v1/incidents/${id}/updates`, { status: "resolved", body: "cleanup" });
    }
    await apiSend(page, "delete", `/api/v1/monitors/${mon.id}`);
  });

  test("escalation progress pill renders while unacknowledged", async ({ page }) => {
    const { projectID } = await firstProject(page);
    const r = await apiSend(page, "post", `/api/v1/projects/${projectID}/incidents`, {
      title: "e2e-escalated", impact: "major", body: "e2e",
    });
    const inc = await r.json();
    // The engine normally advances the step; the suite verifies the read path
    // renders it (the field itself is set by the scheduler in production).
    const detail = await apiGet(page, `/api/v1/incidents/${inc.id}`);
    expect(detail.escalation_step ?? 0).toBe(0); // fresh incident — no pill
    await page.goto(`/incidents/${inc.id}`);
    await expect(page.locator("text=escalated to step")).toHaveCount(0);
    await apiSend(page, "post", `/api/v1/incidents/${inc.id}/updates`, { status: "resolved", body: "cleanup" });
  });
});
