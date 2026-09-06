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


// FR-029 invariant 6 on BOTH remote transports, which only this stack has. The single stack proves
// the refusal (`no_capable_runner` in a pull region with no agent); what it cannot prove is the
// positive half: that a canary is CLAIMED by a remote executor that announced it — the AMQP worker in
// geo1 consuming `checks.canary.<token>.geo1`, and the pull agent in geo2 claiming with
// `X-Cerbix-Workflow-Kinds` — and RUN there. The target is unreachable by design (every address in a
// compose network is private, and the canary's URL policy has no override), so the proof is a
// heartbeat whose message is a STAGE: the job left the queue, an executor executed it, and the result
// came back through the region's own transport. A `dispatch:` message would mean the scheduler refused
// it; no message at all would mean the queue nobody consumes.
test("a canary is claimed and run by the AMQP worker in geo1 and by the pull agent in geo2", async ({ page }) => {
  await waitForLiveRegions(page, ["geo1", "geo2"]);
  const { projectID } = await firstProject(page);
  const workflow = (host: string) => ({
    kind: "async_transaction_v1",
    submit: {
      kind: "http_json",
      method: "POST",
      url: `https://${host}/files/upload`,
      submit_timeout: 5,
      accepted_status: [202],
      body: { tenant: "e2e" },
    },
    correlate: { source: "response_json", path: "task_id" },
    completion: {
      kind: "poll_json",
      url: `https://${host}/tasks/{{ correlation_id }}`,
      timeout: 20,
      poll: { interval: 5, max_attempts: 4, success_path: "status", success_value: "completed" },
    },
    result: { max_latency: 20, required_json_fields: ["s3_path"], lifecycle_path: "s3_path" },
    cleanup: { kind: "none", acknowledged: true },
  });

  for (const region of ["geo1", "geo2"]) {
    const created = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors`, {
      name: `e2e-canary-${region}`,
      type: "async_canary",
      region,
      interval_seconds: 30,
      timeout_seconds: 30,
      failure_threshold: 1,
      config: { workflow: JSON.stringify(workflow(`canary-${region}.internal.invalid`)) },
    });
    expect(created.status(), await created.text()).toBe(201);
    const monitor = await created.json();

    let msg = "";
    await expect
      .poll(
        async () => {
          const beats = await apiGet(page, `/api/v1/monitors/${monitor.id}/heartbeats?limit=5`);
          msg = (beats as any[])[0]?.msg ?? "";
          return msg !== "";
        },
        { timeout: 120_000, message: `${region}: the canary produced no heartbeat — dispatched into a queue nobody consumes, or never dispatched` },
      )
      .toBe(true);
    // A stage, not a dispatch refusal: the executor in the region took the job and ran it.
    expect(msg, `${region} heartbeat: ${msg}`).toMatch(/^(submit|correlate|await_result|assert_result|cleanup_validation):/);
    expect(msg, `${region}: the scheduler refused the run instead of dispatching it`).not.toMatch(/^dispatch:/);
    expect(msg, `${region}: heartbeat leaked the target`).not.toContain("internal.invalid");
  }
});

// FR-020's credentialed dispatch, on BOTH remote transports — the half no geo run exercised until
// 2026-09-06.
//
// The single stack proves the mechanism in one process; `secret-smoke` proves it for the core
// region and one pull agent. What neither can prove is what THIS stack is for: that a credential
// sealed for a region reaches the executor in that region, over the transport that region actually
// uses, and that the executor can open it with a key it holds alone. Until this test existed the
// geo stack ran entirely on generation-1 carriers — the topology that reproduces a real geo
// boundary proved nothing about the mechanism that boundary exists to protect. It was recorded as
// an open coverage gap in `D-0246` rather than left unsaid.
//
// The target is a password-protected redis INSIDE each region (`redis-geo1` on the geo1 network,
// `redis-geo2` on geo2), reachable by that region's prober and by nothing else. A shared target on
// the central network would have shown that a credential travels, not that it travels to the right
// place. UP is the assertion: redis answers an unauthenticated PING with an error, so a monitor
// that reports UP was authenticated, and a credential that was stripped, mis-sealed or opened with
// the wrong key cannot produce it.
test("a credential sealed for a region is opened by that region's executor, on both transports", async ({ page }) => {
  await waitForLiveRegions(page, ["geo1", "geo2"]);
  const { projectID } = await firstProject(page);
  const base = `/api/v1/projects/${projectID}/secrets`;

  const cases = [
    { region: "geo1", secret: "e2e-geo1-redis", value: "geo1-redis-password", target: "redis-geo1:6379" },
    { region: "geo2", secret: "e2e-geo2-redis", value: "geo2-redis-password", target: "redis-geo2:6379" },
  ];

  for (const c of cases) {
    // Start clean even if a previous run died mid-way.
    await apiSend(page, "delete", `${base}/${c.secret}`);
    const secret = await apiSend(page, "post", base, { name: c.secret, value: c.value });
    expect(secret.ok(), `secret create for ${c.region}: ${secret.status()} ${await secret.text()}`).toBeTruthy();
  }

  const ids: Record<string, string> = {};
  for (const c of cases) {
    const created = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors`, {
      name: `e2e-geo-credentialed-${c.region}`,
      type: "redis",
      target: c.target,
      region: c.region,
      interval_seconds: 5,
      timeout_seconds: 4,
      config: { password_ref: c.secret, tls: "false" },
    });
    expect(created.status(), await created.text()).toBe(201);
    ids[c.region] = (await created.json()).id;
  }

  for (const c of cases) {
    await waitForStatus(page, ids[c.region], "up");
  }

  // The credential is never echoed back by a read surface — the same rule as the single stack, and
  // worth asserting HERE too because this is the only place the value crossed a region boundary.
  for (const c of cases) {
    const monitor = await apiGet(page, `/api/v1/monitors/${ids[c.region]}`);
    expect(JSON.stringify(monitor), `${c.region} monitor echoed its credential`).not.toContain(c.value);
    expect(monitor.config.password_ref, "the monitor keeps a REFERENCE, never a value").toBe(c.secret);
  }

  for (const c of cases) {
    await apiSend(page, "delete", `/api/v1/monitors/${ids[c.region]}`);
    await apiSend(page, "delete", `${base}/${c.secret}`);
  }
});
