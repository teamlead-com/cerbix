import { test, expect } from "./fixtures";
import { apiGet, apiSend, ensureE2EWorkspace } from "./helpers";

// FR-042 / NFR-035 browser gate: a service that declares `down → critical`, forced down by its own
// monitor, opens an incident whose impact is critical, and BOTH surfaces say why — the public status
// page headlines "Critical impact", and the incident page states the basis ("declared by service X
// for state down"). Everything is `e2e-` prefixed and removed in finally.

test.describe("incident impact", () => {
  test("a service mapped down → critical publishes Critical impact and its incident shows the basis line", async ({ page }) => {
    test.setTimeout(300_000);
    await page.goto("/");
    const { orgID, projectID } = await ensureE2EWorkspace(page);
    const stamp = Date.now();
    const SLUG = `e2e-impact-svc-${stamp}`;
    const SVC_NAME = "E2E Impact Service";
    const PAGE_SLUG = `e2e-impact-status-${stamp}`;
    const MON = "e2e-impact-down";
    let svcID = "";
    let pageID = "";
    let monID = "";
    let chanID = "";

    try {
      // A TCP monitor aimed at a closed port: refused at once, down after one failure.
      const mon = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors`, {
        name: MON, type: "tcp", target: "127.0.0.1:1", region: "core",
        interval_seconds: 30, timeout_seconds: 3, retries: 1, failure_threshold: 1, auto_incident: false,
      });
      expect(mon.status(), "create monitor").toBe(201);
      monID = (await mon.json()).id;

      // A route: without an enabled project channel the onset is withheld as unroutable and no
      // incident opens. The URL is never contacted by this spec's assertions.
      const ch = await apiSend(page, "post", `/api/v1/projects/${projectID}/notification-channels`, {
        type: "webhook", name: "e2e-impact-hook", config: { url: "https://example.com/e2e-impact" }, enabled: true,
      });
      expect(ch.status(), "create channel").toBe(201);
      chanID = (await ch.json()).id;

      const svc = await apiSend(page, "post", `/api/v1/projects/${projectID}/services`, { slug: SLUG, name: SVC_NAME });
      expect(svc.status(), "create service").toBe(201);
      svcID = (await svc.json()).id;
      const decl = await apiSend(page, "put", `/api/v1/projects/${projectID}/services/${svcID}/declaration`, {
        expected_revision: 0, monitors: [monID], sli: [monID],
      });
      expect(decl.status(), "declare").toBe(200);

      // The mapping under test, all three keys, and the service owns its paging.
      const patched = await apiSend(page, "patch", `/api/v1/projects/${projectID}/services/${svcID}/alerting`, {
        owns_paging: true,
        confirm_evaluations: 1,
        impact_by_state: { degraded: "minor", down: "critical", unknown: "none" },
      });
      expect(patched.status(), "alerting PATCH").toBe(200);

      // A status page on the project, so the page surfaces its active incidents.
      const sp = await apiSend(page, "post", `/api/v1/organizations/${orgID}/status-pages`, {
        slug: PAGE_SLUG, title: "E2E Impact Status", visibility: "public", project_id: projectID,
      });
      expect(sp.status(), "create status page").toBe(201);
      pageID = (await sp.json()).id;

      // Wait (bounded, polling) for the service's own incident to open.
      let incident: any = null;
      await expect.poll(async () => {
        const list = await apiGet(page, `/api/v1/projects/${projectID}/incidents`);
        incident = (Array.isArray(list) ? list : list.incidents ?? []).find(
          (i: any) => i.service_id === svcID && !i.resolved_at && i.status !== "resolved",
        ) ?? null;
        return incident ? incident.impact : null;
      }, { timeout: 170_000, intervals: [3_000], message: "the service's incident opens with the mapped impact" }).toBe("critical");

      // Public page: the active-incident headline carries the worst impact.
      await page.goto(`/status/${PAGE_SLUG}`);
      await expect(page.getByTestId("overall-status-supporting")).toContainText("Critical impact", { timeout: 30_000 });
      // The hero line could be coloured by another open incident, so the claim is checked on THIS
      // incident's own card: located by its title, its impact badge reads "Critical".
      const card = page.getByTestId("active-incident").filter({ hasText: incident.title });
      await expect(card).toHaveCount(1, { timeout: 30_000 });
      await expect(card.getByTestId("impact-badge")).toHaveText(/^\s*Critical\s*$/);

      // Incident page: badge and the basis sentence.
      await page.goto(`/incidents/${incident.id}`);
      await expect(page.getByTestId("incident-impact-badge")).toContainText(/critical/i);
      const basis = page.getByTestId("incident-impact-basis");
      await expect(basis).toContainText(`Critical impact — declared by service ${SVC_NAME} for state down`);
      await expect(page.getByTestId("incident-impact-basis-mapping")).toBeVisible();
    } finally {
      // Resolve whatever this run opened (the service's incident, or any on its monitor) before the
      // service and monitor go; cleanup tolerates errors, as in the sibling specs.
      try {
        const list = await apiGet(page, `/api/v1/projects/${projectID}/incidents`);
        for (const i of Array.isArray(list) ? list : list.incidents ?? []) {
          if (i.resolved_at || i.status === "resolved") continue;
          if ((svcID && i.service_id === svcID) || (monID && i.monitor_id === monID)) {
            await apiSend(page, "post", `/api/v1/incidents/${i.id}/updates`, { status: "resolved", body: "e2e cleanup" });
          }
        }
      } catch {
        /* best-effort cleanup */
      }
      if (pageID) await apiSend(page, "delete", `/api/v1/status-pages/${pageID}`);
      if (svcID) await apiSend(page, "delete", `/api/v1/projects/${projectID}/services/${svcID}`);
      if (chanID) await apiSend(page, "delete", `/api/v1/notification-channels/${chanID}`);
      if (monID) await apiSend(page, "delete", `/api/v1/monitors/${monID}`);
    }
  });
});
