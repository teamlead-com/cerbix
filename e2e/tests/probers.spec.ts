import { test, expect } from "@playwright/test";
import { apiGet, apiSend, firstProject, cleanupMonitors } from "./helpers";

// Polls a monitor until it reaches the wanted status (the scheduler probes on
// its own rhythm; 5s intervals keep this under a minute).
async function waitForStatus(page: any, id: string, want: string, timeoutMs = 60_000) {
  const deadline = Date.now() + timeoutMs;
  let last = "";
  while (Date.now() < deadline) {
    const m = await apiGet(page, `/api/v1/monitors/${id}`);
    last = m.status;
    if (last === want) return;
    await page.waitForTimeout(2000);
  }
  throw new Error(`monitor ${id} stuck in "${last}", wanted "${want}"`);
}

test.describe("probers against the stack itself", () => {
  test.afterEach(async ({ page }) => {
    const { projectID } = await firstProject(page);
    await cleanupMonitors(page, projectID);
  });

  test("http monitor with a conditions expression goes up", async ({ page }) => {
    const { projectID } = await firstProject(page);
    const r = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors`, {
      name: "e2e-http-ok", type: "http", target: "http://localhost:8080/healthz",
      region: "core", interval_seconds: 5, timeout_seconds: 4,
      conditions: ["[STATUS] == 200"],
    });
    expect(r.status(), await r.text()).toBe(201);
    const mon = await r.json();
    await waitForStatus(page, mon.id, "up");
  });

  test("a failing condition takes the monitor down", async ({ page }) => {
    const { projectID } = await firstProject(page);
    const r = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors`, {
      name: "e2e-http-bad", type: "http", target: "http://localhost:8080/healthz",
      region: "core", interval_seconds: 5, timeout_seconds: 4, failure_threshold: 1,
      confirm_interval_seconds: 0, auto_incident: false,
      conditions: ["[STATUS] == 500"],
    });
    expect(r.status(), await r.text()).toBe(201);
    const mon = await r.json();
    await waitForStatus(page, mon.id, "down");
  });

  test("test-connection probes postgres/tcp instantly; unknown region → 502", async ({ page }) => {
    const { projectID } = await firstProject(page);
    const pg = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors/test`, {
      type: "postgres", target: "postgres:5432", region: "core", timeout_seconds: 5, interval_seconds: 30,
      config: { username: "cerbix", password: "cerbix", database: "cerbix" },
    });
    expect(pg.ok(), await pg.text()).toBeTruthy();
    expect((await pg.json()).up).toBeTruthy();

    const tcp = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors/test`, {
      type: "tcp", target: "rabbitmq:5672", region: "core", timeout_seconds: 5, interval_seconds: 30,
    });
    expect((await tcp.json()).up).toBeTruthy();

    // Region affinity is a transport property: over AMQP/pull a region without
    // workers answers 502; the in-process dispatcher probes everything locally.
    const nowhere = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors/test`, {
      type: "tcp", target: "rabbitmq:5672", region: "e2e-ghost-region", timeout_seconds: 5, interval_seconds: 30,
    });
    const expected = (process.env.CERBIX_TOPOLOGY ?? "single") === "single" ? 200 : 502;
    expect(nowhere.status()).toBe(expected);
  });

  test("composite quorum flips with M", async ({ page }) => {
    const { projectID } = await firstProject(page);
    const mk = async (name: string, target: string) => {
      const r = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors`, {
        name, type: "tcp", target, region: "core", interval_seconds: 5, timeout_seconds: 3,
        failure_threshold: 1, confirm_interval_seconds: 0, auto_incident: false,
      });
      expect(r.status(), await r.text()).toBe(201);
      return r.json();
    };
    const up = await mk("e2e-child-up", "postgres:5432");
    const down = await mk("e2e-child-down", "localhost:9"); // discard port — closed
    await waitForStatus(page, up.id, "up");
    await waitForStatus(page, down.id, "down");

    // quorum 2 of 2: one down vote < 2 → composite stays up
    const q2 = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors`, {
      name: "e2e-quorum2", type: "composite", target: "", region: "core",
      interval_seconds: 5, timeout_seconds: 5, failure_threshold: 1, confirm_interval_seconds: 0, auto_incident: false,
      config: { children: `${up.id},${down.id}`, mode: "quorum", quorum: "2" },
    });
    expect(q2.status(), await q2.text()).toBe(201);
    await waitForStatus(page, (await q2.json()).id, "up");

    // quorum 1 of 2: one down vote ≥ 1 → composite down
    const q1 = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors`, {
      name: "e2e-quorum1", type: "composite", target: "", region: "core",
      interval_seconds: 5, timeout_seconds: 5, failure_threshold: 1, confirm_interval_seconds: 0, auto_incident: false,
      config: { children: `${up.id},${down.id}`, mode: "quorum", quorum: "1" },
    });
    expect(q1.status(), await q1.text()).toBe(201);
    await waitForStatus(page, (await q1.json()).id, "down");
  });
});
