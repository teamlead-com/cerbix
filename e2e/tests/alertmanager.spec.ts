import { test, expect } from "@playwright/test";
import { apiGet, apiSend, firstProject } from "./helpers";

// Alertmanager webhook ingest: firing opens an incident correlated by the
// fingerprint (idempotent), resolved closes it; the incident detail renders
// the external key chip.
test.describe("alertmanager ingest", () => {
  test("firing → incident (idempotent) → resolved", async ({ page }) => {
    const { projectID } = await firstProject(page);
    const fp = `e2e-fp-${Date.now()}`;
    const alert = (status: string) => ({
      alerts: [{
        status,
        labels: { alertname: "e2eHighLatency", severity: "warning" },
        annotations: { summary: "e2e alert from the suite" },
        fingerprint: fp,
      }],
    });

    const fire = await apiSend(page, "post", `/api/v1/projects/${projectID}/alerts/alertmanager`, alert("firing"));
    expect(fire.ok(), `fire -> ${fire.status()}: ${await fire.text()}`).toBeTruthy();

    const incidents = await apiGet(page, `/api/v1/projects/${projectID}/incidents`);
    const inc = incidents.find((i: any) => i.external_key?.includes(fp));
    expect(inc, "incident with the fingerprint external_key").toBeTruthy();
    expect(inc.status).not.toBe("resolved");

    // Re-firing the same fingerprint must not open a second incident.
    await apiSend(page, "post", `/api/v1/projects/${projectID}/alerts/alertmanager`, alert("firing"));
    const again = await apiGet(page, `/api/v1/projects/${projectID}/incidents`);
    expect(again.filter((i: any) => i.external_key?.includes(fp)).length).toBe(1);

    // The detail page shows the correlation chip (D-0109).
    await page.goto(`/incidents/${inc.id}`);
    await expect(page.getByText(fp).first()).toBeVisible();

    const resolve = await apiSend(page, "post", `/api/v1/projects/${projectID}/alerts/alertmanager`, alert("resolved"));
    expect(resolve.ok()).toBeTruthy();
    const closed = await apiGet(page, `/api/v1/incidents/${inc.id}`);
    expect(closed.status).toBe("resolved");
  });
});
