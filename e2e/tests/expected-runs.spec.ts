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

// Whether the stack under test has `ledger.carrier_enabled: true`. The dev stack does not, and
// that is the shape the regression below is about — but the assertion used to be written as if OFF
// were the only possible state, so turning FR-032 ON made `make dev-test` fail with a message
// saying the carrier was off. A test that cannot be run against the feature it tests is a test
// that never saw the feature work.
const ledgerCarrierOn = process.env.CERBIX_LEDGER_CARRIER === "true";

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
    // The THIRD bound, and the one a caller needs before it can ask a valid question at all: the
    // widest range this instance answers is CONFIGURATION (2..90), not a constant. It was asserted
    // nowhere on a live stack, and the range case below assumed it — which is the defect revision
    // 32 fixed in the SPA, in a file that cannot see this one.
    expect(
      typeof answer.retention_days,
      "every page carries the bound a caller needs to form a valid query",
    ).toBe("number");
    expect(answer.retention_days).toBeGreaterThanOrEqual(2);
    expect(answer.retention_days).toBeLessThanOrEqual(90);

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
      // Invariant 10c, and it holds whatever the carrier setting is: below the ledger carrier the
      // ledger cannot answer, so the verdict is `unknown` and never `issued_never_claimed`.
      if (w.carrier_generation != null && w.carrier_generation < 4) {
        expect(
          w.verdict,
          `window ${w.due_at} rode carrier ${w.carrier_generation}, which carries no job identity, ` +
            `so no result could ever correlate to it`,
        ).toBe("unknown");
      }
      // THE REGRESSION, and it is about the carrier being OFF: a window may only record generation
      // 4 if the job really rode it. The defect this file exists to keep out stamped `DueAt` from
      // the schedule BEFORE the carrier was chosen, so a `role=all` stack with the flag off stored
      // `carrier_generation = 4` for a run that rode generation 1 — reading `covered`, licensing a
      // stroke across a run that never carried its window.
      //
      // Gated on the stack's actual configuration rather than assumed, so the same spec runs on an
      // instance with the carrier ON and asserts the invariant above instead.
      if (!ledgerCarrierOn && w.carrier_generation != null) {
        expect(
          w.carrier_generation,
          `window ${w.due_at} records carrier ${w.carrier_generation} on a stack whose ledger ` +
            `carrier is OFF (set CERBIX_LEDGER_CARRIER=true if it is on).`,
        ).toBeLessThan(4);
      }
    }

    // ── invariant 27g: a NEGATIVE claim is cross-checked against the facts beside it ──────
    //
    // Every other assertion here is about one window's internal consistency, and the suite passed
    // 68 tests and skipped 1 on an instance holding two windows that claimed a run had never happened while the
    // heartbeat of that very run sat in the table. This is the one assertion that would have caught
    // it, and it is deliberately SUPPLEMENTARY — §17.14's other eight rows are deterministic tests;
    // this one watches a live stack for the class none of them can see.
    //
    // It counts rather than pairs, and the first version of it did pair — "no `expected_never_issued`
    // window may have a heartbeat inside its own interval" — which is TIME PROXIMITY, the exact
    // heuristic FR-031 and FR-032 exist to abolish. It failed on a freshly created monitor whose
    // first run answered the standing window LATE: the heartbeat sat inside the NEXT window's
    // interval, and that window's `expected_never_issued` was true. Correlation is by identity, and
    // a live surface cannot see identity — so the check compares populations instead, which needs
    // neither. If the ledger holds fewer dispatched windows than there are runs, some run answered
    // no window at all, and that is the silent half of the defect: the loud half is the fabricated
    // absence that follows it.
    const ledgerFrom = answer.ledger_from ? Date.parse(answer.ledger_from) : null;
    if (ledgerFrom != null) {
      const beats = await apiGet(page, `/api/v1/monitors/${mon.id}/heartbeats?limit=200`);
      const inSpan = (ms: number) => ms >= ledgerFrom && ms <= now().getTime();
      const runs = (beats ?? []).filter((hb: any) => inSpan(Date.parse(hb.ts))).length;
      const dispatched = answer.windows.filter(
        (w: any) => Boolean(w.job_id) && inSpan(Date.parse(w.due_at)),
      ).length;
      expect(
        runs,
        `${runs} runs were recorded for this monitor at or after ledger_from and only ${dispatched} ` +
          `windows say a job was ever dispatched. A run that reaches no window is the silent half ` +
          `of the defect phase F exists for — the loud half is the window later materialized as an ` +
          `absence at the instant that run answered.`,
      ).toBeLessThanOrEqual(dispatched);
    }

    // ── the range contract, at the real handler ──────────────────────────────────────────
    const base = `/api/v1/projects/${projectID}/monitors/${mon.id}/expected-runs`;
    const from = new Date(now().getTime() - 60_000).toISOString();
    const to = new Date().toISOString();
    for (const [query, error] of [
      ["", "range_required"],
      [`?from=${to}&to=${from}`, "range_invalid"],
      // Derived from the instance's own bound, never assumed: a fixed forty days is "too wide" only
      // on an instance keeping less than forty, so on a ninety-day one this asserted a 400 the
      // handler is right not to give. One day past whatever it publishes is too wide everywhere.
      [
        `?from=${new Date(now().getTime() - (answer.retention_days + 1) * 86_400_000).toISOString()}&to=${to}`,
        "range_too_wide",
      ],
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
    if (ledgerCarrierOn) {
      // With the carrier ON, the windows between two recorded checks are `covered` and the panel
      // must draw. This is the half no live run has ever asserted, and it is the half that says
      // the feature WORKS rather than that it stays quiet.
      await expect(page.getByTestId("lat-stroke")).not.toHaveCount(0);
      await expect(page.getByTestId("lat-expect-cell").first()).toHaveAttribute("data-kind", /covered|late|empty/);
    } else {
      // With every window `unknown` on this stack, NO stroke may be drawn: the line is earned per
      // window, not switched on by the feature existing.
      await expect(page.getByTestId("lat-stroke")).toHaveCount(0);
    }
    // FR-031's observation ruler is untouched: it answers a different question and neither band
    // replaces the other.
    await expect(page.getByTestId("lat-ruler")).toBeVisible();
  });

  // A1, on the live stack — a monitor credentialed BY SCHEMA whose variant forbids a credential
  // rides the same carrier as every other monitor of its region.
  //
  // The materializer had three exits and stamped the carrier on two. `promql` with
  // `auth_mode: none` takes neither the no-envelope exit (it IS a credentialed type) nor the sealed
  // one (it yields no envelope field), and left with the protocol version it was born with while
  // the window field stayed populated. On a carrier-ON stack like this one that means its windows
  // record generation 1 for a job the region publishes at generation 4 — so the run happens, its
  // result correlates, and invariant 10c then reads the window `unknown`: coverage silently lost
  // for one class of monitor and no surface saying why.
  //
  // The class is the point, so the fixture is that class and not a plain HTTP monitor: the existing
  // case above cannot see this, because a plain monitor takes the exit that always stamped.
  test("a credentialed schema with no credential rides its region's carrier", async ({ page }) => {
    test.skip(!ledgerCarrierOn, "the ledger carrier is off on this stack, so no window records 4");
    const { projectID } = await firstProject(page);
    const r = await apiSend(page, "post", `/api/v1/projects/${projectID}/monitors`, {
      name: "e2e-ledger-promql", type: "promql", target: "http://prometheus.invalid:9090",
      region: "core", interval_seconds: 10, timeout_seconds: 5, retries: 0,
      failure_threshold: 1, renotify_seconds: 3600,
      // The API takes `config`; `settings` is the FILE provider's spelling, and a create body
      // carrying it silently drops the query the type requires.
      config: { auth_mode: "none", query: "up" },
    });
    expect(r.status(), await r.text()).toBe(201);
    const mon = await r.json();

    const now = () => new Date();
    let dispatched: any[] = [];
    for (let i = 0; i < 40; i++) {
      const from = new Date(now().getTime() - 10 * 60_000).toISOString();
      const to = new Date(now().getTime() + 60_000).toISOString();
      const answer = await apiGet(
        page,
        `/api/v1/projects/${projectID}/monitors/${mon.id}/expected-runs?from=${from}&to=${to}&limit=200`,
      );
      dispatched = (answer.windows ?? []).filter((w: any) => w.job_id);
      if (dispatched.length > 0) break;
      await page.waitForTimeout(1000);
    }
    expect(dispatched.length, "no window was dispatched within 40s").toBeGreaterThan(0);
    for (const w of dispatched) {
      expect(
        w.carrier_generation,
        `window ${w.due_at} of a credentialed-schema monitor records carrier ` +
          `${w.carrier_generation} on a stack whose ledger carrier is ON: this class left the ` +
          `materializer without crossing the stamp, so its windows can never read covered`,
      ).toBe(4);
    }
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
