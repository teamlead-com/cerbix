import { test, expect } from "@playwright/test";
import { apiGet, apiSend, ensureE2EWorkspace, cleanupMonitors } from "./helpers";

// FR-029 against a live stack. What can be proven here is deliberately NOT the happy path: the URL
// policy refuses loopback, link-local and private addresses after resolution, and everything inside a
// dev compose network is private. That refusal has no override — a flag reachable in production is
// the policy's own bypass — so the journey itself is proven by unit tests through a dialer injected
// at the seam, and what a live stack proves is the two things only it can:
//
//   1. the type exists end to end — created, scheduled, probed, and answered with a heartbeat whose
//      message is a STAGE plus a bounded class and carries no URL;
//   2. the policy holds on the REAL path, with no test seam anywhere near it.
test.describe("async canary", () => {
  test.afterEach(async ({ page }) => {
    const { projectID } = await ensureE2EWorkspace(page);
    await cleanupMonitors(page, projectID);
  });

  test("a canary is scheduled, probed, and reports a stage without leaking its target", async ({ page }) => {
    await page.goto("/");
    const { projectID } = await ensureE2EWorkspace(page);

    // The workflow in its canonical stored form: the API takes the same config the file provider
    // projects, which is what makes one validation path serve both surfaces.
    const target = "https://canary-target.internal.invalid";
    const workflow = {
      kind: "async_transaction_v1",
      submit: {
        kind: "http_json",
        method: "POST",
        url: `${target}/files/upload`,
        submit_timeout: 5,
        accepted_status: [202],
        body: { tenant: "e2e" },
      },
      correlate: { source: "response_json", path: "task_id" },
      completion: {
        kind: "poll_json",
        url: `${target}/tasks/{{ correlation_id }}`,
        timeout: 20,
        poll: { interval: 5, max_attempts: 4, success_path: "status", success_value: "completed" },
      },
      result: { max_latency: 20, required_json_fields: ["s3_path"], lifecycle_path: "s3_path" },
      cleanup: { kind: "none", acknowledged: true },
    };

    const created = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors`, {
      name: "e2e-async-canary",
      type: "async_canary",
      region: "core",
      interval_seconds: 30,
      timeout_seconds: 30,
      failure_threshold: 1,
      config: { workflow: JSON.stringify(workflow) },
    });
    expect(created.status(), await created.text()).toBe(201);
    const monitor = await created.json();

    // The probe runs against a name that does not resolve — or, in another environment, one that
    // resolves to a private address the policy refuses. Either way the outcome is the same shape:
    // a stage and a bounded class.
    let msg = "";
    await expect
      .poll(
        async () => {
          const beats = await apiGet(page, `/api/v1/monitors/${monitor.id}/heartbeats?limit=5`);
          const first = (beats as any[])[0];
          msg = first?.msg ?? "";
          return msg !== "";
        },
        { timeout: 90_000, message: "the canary produced no heartbeat" },
      )
      .toBe(true);

    // A heartbeat names its stage and a bounded class and NOTHING else (NFR-024).
    expect(msg, `heartbeat message: ${msg}`).toMatch(/^(submit|correlate|await_result|assert_result|cleanup_validation):/);
    for (const leaked of [target, "canary-target", "/files/upload", "tasks/"]) {
      expect(msg, `heartbeat leaked ${leaked}`).not.toContain(leaked);
    }

    // FR-029 invariant 13's last clause, and FR-026 §10 (D-0233): the acknowledged `cleanup.kind: none`
    // is visible in the AUDIT TRAIL, as a `monitor.create` row on the organization's listing that
    // names the monitor and the acknowledgement — and nothing of the workflow's contents.
    const { orgID } = await ensureE2EWorkspace(page);
    const audit = (await apiGet(page, `/api/v1/organizations/${orgID}/audit?limit=100`)) as any[];
    const row = audit.find((a) => a.action === "monitor.create" && String(a.target).includes(monitor.id));
    expect(row, `no monitor.create audit row for ${monitor.id}; actions seen: ${[...new Set(audit.map((a) => a.action))].join(",")}`).toBeTruthy();
    expect(row.target).toContain("cleanup=none acknowledged");
    expect(row.via_token, "the SPA session is a user principal, not a token").toBe(false);
    for (const leaked of [target, "/files/upload", "task_id", "s3_path"]) {
      expect(row.target, `audit target leaked ${leaked}`).not.toContain(leaked);
    }
  });

  test("a canary cannot be created with a workflow the schema refuses", async ({ page }) => {
    await page.goto("/");
    const { projectID } = await ensureE2EWorkspace(page);

    // A literal in a credential-bearing header: refused by the SCHEMA, on every surface, and the
    // refusal never echoes the value.
    const workflow = {
      kind: "async_transaction_v1",
      submit: {
        kind: "http_json",
        method: "POST",
        url: "https://example.invalid/upload",
        submit_timeout: 5,
        accepted_status: [202],
        headers: [{ name: "authorization", value: "Bearer e2e-literal-token" }],
        body: { tenant: "e2e" },
      },
      correlate: { source: "response_json", path: "task_id" },
      completion: {
        kind: "poll_json",
        url: "https://example.invalid/tasks/{{ correlation_id }}",
        timeout: 20,
        poll: { interval: 5, max_attempts: 4, success_path: "status", success_value: "completed" },
      },
      result: { max_latency: 20, required_json_fields: ["s3_path"], lifecycle_path: "s3_path" },
      cleanup: { kind: "none", acknowledged: true },
    };

    const res = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors`, {
      name: "e2e-canary-literal",
      type: "async_canary",
      region: "core",
      interval_seconds: 30,
      timeout_seconds: 30,
      config: { workflow: JSON.stringify(workflow) },
    });
    expect(res.status()).toBe(400);
    const body = await res.text();
    expect(body).toContain("credential-bearing");
    expect(body, "the refusal echoed the credential").not.toContain("e2e-literal-token");
  });

  // FR-029 invariant 6 on the REAL path. `pull1` is a declared pull region of the dev stack with no
  // agent running in it, so nothing there announces a canary runner — which is exactly the situation
  // an operator hits when they declare a canary before upgrading a region. The promise the runbook
  // now makes is that this is one bounded DOWN naming the fix, not an indefinite pending, and only a
  // live stack can prove the scheduler's own dispatch decision.
  test("a canary in a region with no capable runner reports one bounded DOWN, not silence", async ({ page }) => {
    await page.goto("/");
    const { projectID } = await ensureE2EWorkspace(page);

    const workflow = {
      kind: "async_transaction_v1",
      submit: {
        kind: "http_json",
        method: "POST",
        url: "https://canary-uncovered.internal.invalid/files/upload",
        submit_timeout: 5,
        accepted_status: [202],
        body: { tenant: "e2e" },
      },
      correlate: { source: "response_json", path: "task_id" },
      completion: {
        kind: "poll_json",
        url: "https://canary-uncovered.internal.invalid/tasks/{{ correlation_id }}",
        timeout: 20,
        poll: { interval: 5, max_attempts: 4, success_path: "status", success_value: "completed" },
      },
      result: { max_latency: 20, required_json_fields: ["s3_path"], lifecycle_path: "s3_path" },
      cleanup: { kind: "none", acknowledged: true },
    };

    const created = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors`, {
      name: "e2e-canary-uncovered",
      type: "async_canary",
      region: "pull1",
      interval_seconds: 30,
      timeout_seconds: 30,
      failure_threshold: 1,
      config: { workflow: JSON.stringify(workflow) },
    });
    expect(created.status(), await created.text()).toBe(201);
    const monitor = await created.json();

    let msg = "";
    await expect
      .poll(
        async () => {
          const beats = await apiGet(page, `/api/v1/monitors/${monitor.id}/heartbeats?limit=5`);
          const first = (beats as any[])[0];
          msg = first?.msg ?? "";
          return msg !== "";
        },
        { timeout: 90_000, message: "an uncovered region produced no heartbeat at all — the pending state the invariant forbids" },
      )
      .toBe(true);

    // The reason names the fix: nothing announced a runner. Not `capability_mismatch` (which would
    // mean a runner is there speaking another version) and not a stage failure (which would mean the
    // job was dispatched after all).
    expect(msg, `heartbeat message: ${msg}`).toBe("dispatch: no_capable_runner");

    // B2, and it is the half a heartbeat row cannot show. The shortage used to be written through
    // the BARE insert, which touches no status, no confirmation counter, no transition outbox and
    // no service bucket — so this assertion's neighbour above passed while the monitor stayed UP
    // forever with nothing probing it: no alert, no incident, no escalation, and a service SLI
    // still computed over a monitor that had stopped being measured.
    //
    // A row exists in both the broken and the fixed version. The STATUS is what separates them, and
    // this monitor's failure threshold is 1, so one shortage is enough to flip it.
    await expect
      .poll(
        async () => (await apiGet(page, `/api/v1/monitors/${monitor.id}`))?.status,
        {
          timeout: 90_000,
          message:
            "the monitor never left its initial status: a shortage that records a heartbeat and " +
            "moves nothing else is exactly the state B2 is about",
        },
      )
      .toBe("down");
  });

  // B5, on the live stack — `role=all` runs a canary in EVERY non-pull region.
  //
  // The announcement named the DEFAULT region and nothing else, while the in-process dispatcher
  // ignores the region entirely and executes whatever job it is handed. So a canary declared in any
  // other non-pull region was refused with `no_capable_runner` on every tick, forever, while an
  // ordinary monitor of that same region was probed normally by the same process.
  //
  // The region below is named nowhere: not in `pull.regions`, not in any local list. What must NOT
  // appear is the shortage reason the test above asserts — this monitor is dispatched, so its
  // heartbeat carries a STAGE, which is the same shape the first test in this file asserts for
  // `core`.
  test("a canary in an unnamed non-pull region is dispatched, not refused", async ({ page }) => {
    await page.goto("/");
    const { projectID } = await ensureE2EWorkspace(page);
    const target = "https://canary-region.internal.invalid";
    const workflow = {
      kind: "async_transaction_v1",
      submit: {
        kind: "http_json", method: "POST", url: `${target}/files/upload`,
        submit_timeout: 5, accepted_status: [202], body: { tenant: "e2e" },
      },
      correlate: { source: "response_json", path: "task_id" },
      completion: {
        kind: "poll_json", url: `${target}/tasks/{{ correlation_id }}`, timeout: 20,
        poll: { interval: 5, max_attempts: 4, success_path: "status", success_value: "completed" },
      },
      result: { max_latency: 20, required_json_fields: ["s3_path"], lifecycle_path: "s3_path" },
      cleanup: { kind: "none", acknowledged: true },
    };
    const created = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors`, {
      name: "e2e-canary-other-region",
      type: "async_canary",
      region: "geo-frankfurt",
      interval_seconds: 30,
      timeout_seconds: 30,
      failure_threshold: 1,
      config: { workflow: JSON.stringify(workflow) },
    });
    expect(created.status(), await created.text()).toBe(201);
    const monitor = await created.json();

    let msg = "";
    await expect
      .poll(
        async () => {
          const beats = await apiGet(page, `/api/v1/monitors/${monitor.id}/heartbeats?limit=5`);
          msg = ((beats as any[])[0]?.msg ?? "") as string;
          return msg !== "";
        },
        { timeout: 90_000, message: "the canary produced no heartbeat in an unnamed non-pull region" },
      )
      .toBe(true);

    expect(
      msg,
      `heartbeat message: ${msg} — this region is executed by THIS process, exactly as the ` +
        `default one is, so a shortage here means the announcement is enumerating regions instead ` +
        `of deriving them from the dispatcher`,
    ).not.toContain("no_capable_runner");
    expect(msg).toMatch(/^(submit|correlate|await_result|assert_result|cleanup_validation):/);
  });
});
