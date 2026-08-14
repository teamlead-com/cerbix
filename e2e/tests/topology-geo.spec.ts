import { test, expect } from "@playwright/test";
import { apiGet, apiSend, cleanupMonitors, firstProject } from "./helpers";

type Region = { name: string; live: boolean };

async function waitForLiveRegions(page: any, names: string[], timeoutMs = 60_000) {
  const deadline = Date.now() + timeoutMs;
  let last: Region[] = [];
  while (Date.now() < deadline) {
    const body = await apiGet(page, "/api/v1/regions");
    last = body.regions ?? [];
    if (names.every((name) => last.some((region) => region.name === name && region.live))) return;
    await page.waitForTimeout(1000);
  }
  throw new Error(`regions did not become live: wanted ${names.join(", ")}, got ${JSON.stringify(last)}`);
}

async function waitForStatus(page: any, id: string, want: string, timeoutMs = 60_000) {
  const deadline = Date.now() + timeoutMs;
  let last = "";
  while (Date.now() < deadline) {
    const monitor = await apiGet(page, `/api/v1/monitors/${id}`);
    last = monitor.status;
    if (last === want) return;
    await page.waitForTimeout(1000);
  }
  throw new Error(`monitor ${id} stuck in ${last}, wanted ${want}`);
}

test.afterEach(async ({ page }) => {
  const { projectID } = await firstProject(page);
  await cleanupMonitors(page, projectID);
});

test("geo1 AMQP worker and geo2 pull agent execute in their own networks", async ({ page }) => {
  await waitForLiveRegions(page, ["core", "geo1", "geo2"]);
  const { projectID } = await firstProject(page);

  const amqpGeo = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors/test`, {
    type: "tcp",
    target: "rabbitmq:5672",
    region: "geo1",
    timeout_seconds: 5,
    interval_seconds: 30,
  });
  expect(amqpGeo.status(), await amqpGeo.text()).toBe(200);
  expect((await amqpGeo.json()).up, "geo1 worker reaches only its broker-side target").toBeTruthy();

  const pullGeo = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors/test`, {
    type: "http",
    target: "http://api:8080/healthz",
    region: "geo2",
    timeout_seconds: 5,
    interval_seconds: 30,
    conditions: ["[STATUS] == 200"],
  });
  expect(pullGeo.status(), await pullGeo.text()).toBe(200);
  expect((await pullGeo.json()).up, "geo2 pull agent reaches the API-side target").toBeTruthy();
});

test("scheduler dispatches and ingests scheduled checks through both remote transports", async ({ page }) => {
  await waitForLiveRegions(page, ["geo1", "geo2"]);
  const { projectID } = await firstProject(page);
  const cases = [
    { name: "e2e-geo1-scheduled", type: "tcp", target: "rabbitmq:5672", region: "geo1" },
    { name: "e2e-geo2-scheduled", type: "http", target: "http://api:8080/healthz", region: "geo2" },
  ];
  for (const monitor of cases) {
    const response = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors`, {
      ...monitor,
      interval_seconds: 5,
      timeout_seconds: 4,
      conditions: monitor.type === "http" ? ["[STATUS] == 200"] : [],
    });
    expect(response.status(), await response.text()).toBe(201);
    await waitForStatus(page, (await response.json()).id, "up");
  }
});
