import { test, expect } from "@playwright/test";
import { apiGet, apiSend, ensureE2EWorkspace } from "./helpers";

// FR-024 D8a on a HEALTHY live stack (func-reliability-gate.md, discharge row 15): a policy at
// the `max_seal_lag_seconds` FLOOR (300 s = LateArrivalGrace + CanonicalBucket + 2 buckets of
// headroom) must NOT be `seal_stale` on a system doing exactly what it should — a running
// sealer whose lag sits in [2m, 3m). The Go suite proves the arithmetic; this proves it against
// the real materializer, on the real clock, with a real HTTP monitor the stack can reach:
//
//   1. before anything has sealed, every decision says `never_sealed` for the budget clauses,
//      and with `unknown_behavior: block` that is state UNKNOWN with action BLOCK (D4 rule 3);
//   2. once `sealed_through` appears, `reasons[]` carries NO `seal_stale` and `seal_lag` is
//      below the floor — the healthy lag the floor was derived from.
//
// The seal is pushed by the leader's forward materialization (FloorToBucket(now − 2m)), so the
// first bucket after the declaration seals ~3 minutes in; the poll allows six, at one decision
// per ~20 s (the per-principal decision bucket is 10/min). This lives in its own file with its
// own timeout so the main suite's 60 s budget is untouched. If the stack does not seal within the
// budget the test FAILS with every decision it saw — the assertion is not weakened into a pass.
//
// API-shaped: the stored session is reused (local logins are rate-limited); everything is
// prefixed `e2e-gate-live` and cleaned up in afterEach.

const SLUG = "e2e-gate-live";
const MONITOR = "e2e-gate-live-http";
const POLL_EVERY_MS = 20_000;
const SEAL_BUDGET_MS = 6 * 60_000;
const FLOOR_SECONDS = 300;

const POLICY = {
  schema_version: 1,
  window: "30d",
  clauses: {
    budget_exhausted: "block",
    budget_consumed: "warn",
    page_burn_firing: "block",
    ticket_burn_firing: "warn",
    service_incident_open: "block",
  },
  budget_consumed_percent: 90,
  max_seal_lag_seconds: FLOOR_SECONDS,
  unknown_behavior: "block",
};

type Observed = {
  at: string;
  elapsed_s: number;
  state: string;
  action?: string;
  codes: string[];
  sealed_through?: string;
  seal_lag?: number;
  monitor_status?: string;
};

test.describe("reliability gate — the seal-lag floor on a healthy stack", () => {
  // Setup ≈ 5 s, polling ≤ 6 min, cleanup ≈ 2 s: eight minutes is generous without hiding a hang.
  test.setTimeout(8 * 60_000);

  test.afterEach(async ({ page }) => {
    const { projectID } = await ensureE2EWorkspace(page);
    for (const s of await apiGet(page, `/api/v1/projects/${projectID}/services`)) {
      if ((s.service.slug as string).startsWith(SLUG)) {
        await apiSend(page, "delete", `/api/v1/projects/${projectID}/services/${s.service.id}`);
      }
    }
    for (const m of await apiGet(page, `/api/v1/projects/${projectID}/monitors`)) {
      if ((m.name as string).startsWith(MONITOR)) await apiSend(page, "delete", `/api/v1/monitors/${m.id}`);
    }
  });

  test("max_seal_lag_seconds = 300 is not seal_stale once the live sealer has sealed; before that, never_sealed is UNKNOWN/BLOCK", async ({ page }) => {
    await page.goto("/services");
    const { projectID } = await ensureE2EWorkspace(page);

    // A REAL, enabled HTTP monitor against the app's own health endpoint on the compose network —
    // the same target services.spec.ts declares against; it goes UP within one interval.
    const mon = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors`, {
      name: MONITOR, type: "http", target: "http://cerbix:8080/healthz", region: "core", enabled: true,
      interval_seconds: 30, timeout_seconds: 5, retries: 1, failure_threshold: 1, renotify_seconds: 3600,
    });
    expect(mon.status(), await mon.text()).toBe(201);
    const monitorID = (await mon.json()).id as string;

    const created = await apiSend(page, "post", `/api/v1/projects/${projectID}/services`, { slug: SLUG, name: "E2E Gate Live" });
    expect(created.status(), await created.text()).toBe(201);
    const svcID = (await created.json()).id as string;
    const svc = `/api/v1/projects/${projectID}/services/${svcID}`;
    const declaredAt = new Date();
    const decl = await apiSend(page, "put", `${svc}/declaration`, { expected_revision: 0, monitors: [monitorID], sli: [monitorID] });
    expect(decl.status(), await decl.text()).toBe(200);
    const target = await apiSend(page, "put", `${svc}/sla-target`, { window: "30d", objective: 99.9 });
    expect(target.status(), await target.text()).toBe(200);
    const put = await apiSend(page, "put", `${svc}/gate/policy`, { expected_revision: null, ...POLICY });
    expect(put.status(), await put.text()).toBe(200);
    expect((await apiGet(page, `${svc}/gate/policy`)).max_seal_lag_seconds, "the floor is accepted as written").toBe(FLOOR_SECONDS);

    // Poll one decision every ~20 s until `sealed_through` appears or the budget is spent.
    const seen: Observed[] = [];
    const t0 = Date.now();
    let sealed: any = null;
    for (;;) {
      const r = await apiSend(page, "post", `${svc}/gate`);
      expect(r.status(), await r.text()).toBe(200);
      const d = await r.json();
      const monitor = await apiGet(page, `/api/v1/monitors/${monitorID}`);
      const obs: Observed = {
        at: d.evaluated_at, elapsed_s: Math.round((Date.now() - t0) / 1000), state: d.state, action: d.action,
        codes: (d.reasons as any[]).map((x) => x.code), sealed_through: d.sealed_through, seal_lag: d.seal_lag,
        monitor_status: monitor.status,
      };
      seen.push(obs);
      console.log(`[gate-live] +${obs.elapsed_s}s state=${obs.state} action=${obs.action} codes=${obs.codes.join(",")} sealed_through=${obs.sealed_through ?? "-"} seal_lag=${obs.seal_lag ?? "-"} monitor=${obs.monitor_status}`);
      if (d.sealed_through) { sealed = d; break; }
      if (Date.now() - t0 >= SEAL_BUDGET_MS) break;
      await page.waitForTimeout(POLL_EVERY_MS);
    }
    const log = JSON.stringify(seen, null, 1);

    // ── (1) Before the first seal: `never_sealed` on the budget clauses; UNKNOWN and, because
    // the policy says so, BLOCK (D4 rule 3 with unknown_behavior block). Every such decision
    // carries no `sealed_through`, no `seal_lag` and — trivially, but it is the claim — no
    // `seal_stale`, which is a verdict about sealed data.
    const unsealed = seen.filter((o) => !o.sealed_through);
    expect(unsealed.length, `at least one decision before the first seal; saw ${log}`).toBeGreaterThan(0);
    for (const o of unsealed) {
      expect(o.codes, `+${o.elapsed_s}s: the budget clauses are never_sealed`).toContain("never_sealed");
      expect(o.codes, `+${o.elapsed_s}s: never_sealed is not seal_stale`).not.toContain("seal_stale");
      expect(o.state, `+${o.elapsed_s}s: nothing sealed is UNKNOWN`).toBe("UNKNOWN");
      expect(o.action, `+${o.elapsed_s}s: unknown_behavior block`).toBe("BLOCK");
      expect(o.seal_lag, `+${o.elapsed_s}s: no lag without a seal`).toBeUndefined();
    }

    // ── (2) The live sealer sealed within the budget: NOT seal_stale at the floor, with the
    // observed lag below it. A stack that could not seal in six minutes is a FAILURE that
    // reports everything it saw — never a pass by omission.
    expect(sealed, `the stack sealed nothing in ${SEAL_BUDGET_MS / 1000}s after the declaration at ${declaredAt.toISOString()}; decisions: ${log}`).not.toBeNull();
    const codes = (sealed.reasons as any[]).map((x) => x.code);
    expect(codes, `sealed decision carries no seal_stale (seal_lag=${sealed.seal_lag}); decisions: ${log}`).not.toContain("seal_stale");
    expect(codes, "sealed decision no longer says never_sealed").not.toContain("never_sealed");
    expect(typeof sealed.seal_lag, "seal_lag is present with sealed_through").toBe("number");
    expect(sealed.seal_lag, `seal_lag ${sealed.seal_lag}s is below the ${FLOOR_SECONDS}s floor`).toBeLessThan(FLOOR_SECONDS);
    expect(sealed.seal_lag, "seal_lag is non-negative").toBeGreaterThanOrEqual(0);
    const lagFromTimestamps = (new Date(sealed.evaluated_at).getTime() - new Date(sealed.sealed_through).getTime()) / 1000;
    expect(Math.abs(lagFromTimestamps - sealed.seal_lag), `seal_lag ${sealed.seal_lag}s is evaluated_at − sealed_through (${lagFromTimestamps}s)`).toBeLessThanOrEqual(2);
    expect(sealed.max_seal_lag_seconds).toBe(FLOOR_SECONDS);
    expect(sealed.policy_revision).toBe(1);
    expect(sealed.state, "a sealed decision is a verdict, not the not-configured shape").not.toBe("NOT_CONFIGURED");
    expect(["ALLOW", "WARN", "BLOCK"]).toContain(sealed.action);

    // The SLI member did go UP against the reachable target — the seal rests on real facts.
    const monitor = await apiGet(page, `/api/v1/monitors/${monitorID}`);
    expect(monitor.status, `monitor status after ${seen[seen.length - 1].elapsed_s}s`).toBe("up");
    console.log(`[gate-live] sealed_through=${sealed.sealed_through} evaluated_at=${sealed.evaluated_at} seal_lag=${sealed.seal_lag}s state=${sealed.state} action=${sealed.action} codes=${codes.join(",")}`);
  });
});
