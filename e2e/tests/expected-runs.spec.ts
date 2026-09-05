import { test, expect } from "@playwright/test";
import { apiGet, apiSend, firstProject, cleanupMonitors } from "./helpers";

// FR-032 — the expected-run ledger, against a LIVE stack (§13a, §14, D-0239/D-0240).
//
// The unit suites pin the arithmetic and the rules. What only a live run can show is that the
// ledger WRITES: that a leader tick materializes a window, that a real result closes it, and that
// the carrier a window records is the one the job actually rode. That last one is not theory —
// running this stack is how a defect was found where the materializer stamped `DueAt` before the
// carrier was chosen, so a window stored `carrier_generation = 4` for a run that rode generation 1
// with `ledger.carrier_enabled` off. Invariant 10c says such a window reads `unknown`; it read
// `covered`, and a stroke would have been drawn across it.

test.describe("the expected-run ledger", () => {
  test.afterEach(async ({ page }) => {
    const { projectID } = await firstProject(page);
    await cleanupMonitors(page, projectID);
  });

  test("records a window for a real run, and answers for it truthfully", async ({ page }) => {
    const { projectID } = await firstProject(page);
    const r = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors`, {
      name: "e2e-ledger", type: "http", target: "https://example.com", method: "GET",
      region: "core", interval_seconds: 10, timeout_seconds: 5, retries: 0,
      failure_threshold: 1, renotify_seconds: 3600,
    });
    expect(r.status()).toBe(201);
    const mon = await r.json();

    // The leader's tick is a second and the interval is ten, so a window and its result take a
    // few seconds. Poll rather than sleep a fixed amount: a fixed wait is either flaky or slow,
    // and this one has a real completion condition.
    const now = () => new Date();
    let answer: any = null;
    for (let i = 0; i < 40; i++) {
      const from = new Date(now().getTime() - 10 * 60_000).toISOString();
      const to = new Date(now().getTime() + 60_000).toISOString();
      answer = await apiGet(
        page,
        `/api/v1/projects/${projectID}/monitors/${mon.id}/expected-runs?from=${from}&to=${to}&limit=200`,
      );
      if ((answer.windows ?? []).length > 0) break;
      await page.waitForTimeout(1000);
    }

    // ── the endpoint answers, and its answer is BOUNDED ──────────────────────────────────
    expect(answer, "the endpoint returned nothing").toBeTruthy();
    expect(answer.windows.length, "no window was materialized within 40s").toBeGreaterThan(0);
    // `ledger_from` travels with every page, because a caller holding rows and not bounds reads an
    // empty range as "nothing was due" when the truth is "the ledger cannot say" (invariant 26b).
    expect(answer).toHaveProperty("ledger_from");
    expect(answer.ledger_from, "a participating monitor answers for time from somewhere").toBeTruthy();
    expect(answer).toHaveProperty("gap_truncated_before");
    expect(answer).toHaveProperty("next_cursor");

    // ── every window is internally consistent ────────────────────────────────────────────
    for (const w of answer.windows) {
      expect(w.due_at).toBeTruthy();
      expect(w.verdict).toBeTruthy();
      expect(w.interval_seconds, "the interval that SPACED the window is on the row").toBe(10);
      // §6.1's biconditional CHECK, visible through the API: a window never dispatched has no
      // carrier, which is a different fact from one dispatched on an old carrier.
      expect(
        Boolean(w.job_id) === Boolean(w.carrier_generation),
        `job_id and carrier_generation disagree: ${JSON.stringify(w)}`,
      ).toBe(true);
      // THE REGRESSION. A window may only record generation 4 if the job really rode it, and this
      // stack runs with `ledger.carrier_enabled` off — so nothing here may claim it. A window that
      // did is the defect this spec file exists to keep out.
      if (w.carrier_generation != null) {
        expect(
          w.carrier_generation,
          `window ${w.due_at} records carrier ${w.carrier_generation} on a stack whose ledger ` +
            `carrier is OFF. Invariant 10c says such a window reads \`unknown\`; recording 4 makes ` +
            `it read \`covered\` and licenses a stroke across a run that never carried its window.`,
        ).toBeLessThan(4);
        // And the verdict follows from that: below the ledger carrier, the ledger cannot answer.
        expect(w.verdict).toBe("unknown");
      }
    }

    // ── the range contract, at the real handler ──────────────────────────────────────────
    const base = `/api/v1/projects/${projectID}/monitors/${mon.id}/expected-runs`;
    const from = new Date(now().getTime() - 60_000).toISOString();
    const to = new Date().toISOString();
    for (const [query, error] of [
      ["", "range_required"],
      [`?from=${to}&to=${from}`, "range_invalid"],
      [`?from=${new Date(now().getTime() - 40 * 86_400_000).toISOString()}&to=${to}`, "range_too_wide"],
      [`?from=${from}&to=${to}&limit=0`, "limit_invalid"],
      [`?from=${from}&to=${to}&limit=201`, "limit_invalid"],
      [`?from=${from}&to=${to}&cursor=not-a-cursor`, "cursor_invalid"],
    ] as [string, string][]) {
      const bad = await page.request.get(base + query);
      expect(bad.status(), `${query || "(no range)"} should be 400`).toBe(400);
      expect((await bad.json()).error, `${query || "(no range)"} names its parameter`).toBe(error);
    }

    // A monitor outside the project is 404 HIDDEN, and the PAIR is validated rather than each
    // half: answering here would let a caller enumerate one project's monitors through another's.
    const orgs = await apiGet(page, "/api/v1/organizations");
    const projects: any[] = [];
    for (const org of orgs ?? []) {
      projects.push(...(await apiGet(page, `/api/v1/organizations/${org.id}/projects`)));
    }
    const other = projects.find((p: any) => p.id !== projectID);
    if (other) {
      const hidden = await page.request.get(
        `/api/v1/projects/${other.id}/monitors/${mon.id}/expected-runs?from=${from}&to=${to}`,
      );
      expect(hidden.status(), "a monitor from another project must be 404, not 403").toBe(404);
    }

    // ── the panel is the one inside the embedded SPA, and it reads the ledger ────────────
    // The panel needs TWO recorded checks before it draws anything at all — a single point has no
    // interval to describe — so wait for the data rather than for the render. Waiting on the
    // element alone would fail here for a reason that says nothing about the ledger.
    for (let i = 0; i < 40; i++) {
      const hbs = await apiGet(page, `/api/v1/monitors/${mon.id}/heartbeats?limit=10`);
      if ((hbs ?? []).length >= 2) break;
      await page.waitForTimeout(1000);
    }
    await page.goto(`/monitors/${mon.id}`);
    // The expectation ruler appears only when the ledger answered, so its presence is proof the
    // SPA called the endpoint — the assertion the unit suite cannot make.
    await expect(page.getByTestId("lat-expect-ruler")).toBeVisible({ timeout: 15_000 });
    // And with every window `unknown` on this stack, NO stroke may be drawn: the line is earned
    // per window, not switched on by the feature existing.
    await expect(page.getByTestId("lat-stroke")).toHaveCount(0);
    // FR-031's observation ruler is untouched: it answers a different question and neither band
    // replaces the other.
    await expect(page.getByTestId("lat-ruler")).toBeVisible();
  });

  test("a push monitor has no expectation at all", async ({ page }) => {
    // §15, the owner's ruling: `checkStalePush` already detects a push that did not arrive, and a
    // window would be a SECOND mechanism for one obligation. The API says so by answering with a
    // null bound rather than an empty list, which a caller must render as not stored.
    const { projectID } = await firstProject(page);
    const r = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors`, {
      name: "e2e-ledger-push", type: "push", region: "core",
      interval_seconds: 3600, grace_seconds: 60, failure_threshold: 1, renotify_seconds: 3600,
    });
    expect(r.status()).toBe(201);
    const mon = await r.json();

    const from = new Date(Date.now() - 60_000).toISOString();
    const to = new Date(Date.now() + 60_000).toISOString();
    const answer = await apiGet(
      page,
      `/api/v1/projects/${projectID}/monitors/${mon.id}/expected-runs?from=${from}&to=${to}`,
    );
    expect(answer.windows ?? []).toHaveLength(0);
    expect(
      answer.ledger_from,
      "a push monitor answers for nothing, and a zero instant would let a caller render its whole " +
        "history as time nothing was due in",
    ).toBeNull();
  });
});
