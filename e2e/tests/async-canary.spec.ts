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
});
