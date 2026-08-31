import { test, expect } from "@playwright/test";
import { apiGet, apiSend, ensureE2EWorkspace } from "./helpers";

// The browser gate for FR-025's SPA (AC-0165-7, D-0210 items 1, 3 and 5): the `Changes` card on the
// service page, the comparison view it links to, and the per-service timeline — driven the way an
// operator drives them, against a live stack. changes.spec.ts proves the HTTP contract; the vitest
// specs prove the rendering rules in isolation; this proves that the three surfaces are actually
// WIRED to those routes and to each other:
//
//   * a terminal group and a started-only one are both listed, and only the terminal one carries
//     `data-terminal` — the started-only row's before/after cell is `no-terminal` and says so,
//     never a partial number;
//   * the terminal row's cell settles on a real answer from `…/changes/compare` — a figure, or the
//     page's own `withheld` word, or `pending` — and the header's horizon applies to it;
//   * the row's link reaches the comparison view, addressed by `(source, external_id, horizon)`,
//     and both sides render in one of the three shapes;
//   * `full timeline →` reaches `/services/:id/changes`, which lists both groups.
//
// The record is the PIPELINE's (D14): every change below is written over the API, never through the
// UI — there is no record form, and this test would fail to find one. The session is shared and
// already signed in (local logins are rate-limited): nothing here logs in or out. Everything is
// `e2e-change-ui-` prefixed and cleaned up in afterEach.

const PREFIX = "e2e-change-ui";

// Whole-second instants a few minutes from now: the server bounds `occurred_at` by
// `change.max_past` (24 h) and `change.max_future` (5 m), and the container clock is UTC.
const minutesAgo = (m: number) => new Date(Math.floor(Date.now() / 1000) * 1000 - m * 60_000).toISOString();

const SIDE_KINDS = ["figure", "withheld", "pending"];

test.describe("change intelligence UI", () => {
  test.afterEach(async ({ page }) => {
    await page.goto("/services");
    const { projectID } = await ensureE2EWorkspace(page);
    for (const s of await apiGet(page, `/api/v1/projects/${projectID}/services`)) {
      if ((s.service.slug as string).startsWith(PREFIX)) {
        await apiSend(page, "delete", `/api/v1/projects/${projectID}/services/${s.service.id}`);
      }
    }
  });

  test("the card lists both groups, compares only the terminal one, and links on to the comparison and the timeline", async ({ page }) => {
    // The helpers fetch RELATIVE urls from inside the page; land on the app's origin first.
    await page.goto("/services");
    const { projectID } = await ensureE2EWorkspace(page);
    const slug = `${PREFIX}-card`;
    const created = await apiSend(page, "post", `/api/v1/projects/${projectID}/services`, { slug, name: `E2E Change UI ${slug}` });
    expect(created.status(), await created.text()).toBe(201);
    const svcID = (await created.json()).id as string;

    // ── The pipeline records: one identity that reached a terminal phase, one still running.
    const changes = `/api/v1/projects/${projectID}/services/${svcID}/changes`;
    const record = async (body: Record<string, unknown>) => {
      const r = await apiSend(page, "post", changes, body);
      expect(r.status(), await r.text()).toBe(201);
    };
    await record({ kind: "deploy", phase: "started", source: "e2e-ci", external_id: "ui-done", ref: "v9.9.9", occurred_at: minutesAgo(6) });
    await record({
      kind: "deploy",
      phase: "succeeded",
      source: "e2e-ci",
      external_id: "ui-done",
      ref: "v9.9.9",
      url: "https://ci.example.com/run/ui-done",
      occurred_at: minutesAgo(3),
    });
    await record({ kind: "rollback", phase: "started", source: "e2e-ci", external_id: "ui-running", ref: "v9.9.8", occurred_at: minutesAgo(2) });

    // ── The card. Both groups are listed; only the terminal one carries `data-terminal`.
    await page.goto(`/services/${svcID}`);
    await expect(page.getByTestId("service-changes")).toBeVisible();
    const doneRow = page.locator('[data-testid="changes-group"][data-external-id="ui-done"]');
    const runningRow = page.locator('[data-testid="changes-group"][data-external-id="ui-running"]');
    await expect(doneRow).toHaveCount(1);
    await expect(runningRow).toHaveCount(1);
    await expect(doneRow).toHaveAttribute("data-terminal", "succeeded");
    await expect(doneRow).toHaveAttribute("data-kind", "deploy");
    await expect(runningRow, "a started-only group has no terminal phase").not.toHaveAttribute("data-terminal", /.+/);
    await expect(runningRow).toHaveAttribute("data-kind", "rollback");
    await expect(doneRow.getByTestId("changes-run-link")).toHaveAttribute("href", "https://ci.example.com/run/ui-done");

    // The started-only row is never asked for a comparison and never shows a number.
    const runningCell = runningRow.getByTestId("changes-compare");
    await expect(runningCell).toHaveAttribute("data-state", "no-terminal");
    await expect(runningCell).toHaveText("before/after unavailable until a terminal phase");
    await expect(runningCell.getByTestId("changes-compare-link"), "no link to a comparison that would 404").toHaveCount(0);

    // The terminal row's cell settles on a real answer: exactly one of the three shapes.
    const doneCell = doneRow.getByTestId("changes-compare");
    await expect(doneCell).toHaveAttribute("data-state", "ok", { timeout: 15_000 });
    await expect(doneCell).toHaveAttribute("data-horizon", "1h");
    for (const key of ["before", "after"]) {
      const sideCell = doneCell.getByTestId(`changes-compare-${key}`);
      const kind = await sideCell.getAttribute("data-kind");
      expect(SIDE_KINDS, `${key} is exactly one of figure|withheld|pending`).toContain(kind);
      const text = (await sideCell.textContent())!.trim();
      if (kind === "figure") expect(text).toMatch(/^\d+(\.\d+)? %$/);
      else expect(text, `a ${kind} side never quotes a partial number`).not.toMatch(/\d/);
    }

    // ── The header's horizon applies to every row on the card.
    await page.getByTestId("changes-horizon-6h").click();
    await expect(doneCell).toHaveAttribute("data-horizon", "6h");
    await expect(doneCell).toHaveAttribute("data-state", "ok", { timeout: 15_000 });

    // ── The row's link: the comparison view, addressed by (source, external_id, horizon).
    await doneCell.getByTestId("changes-compare-link").click();
    await page.waitForURL((u) => u.pathname === `/services/${svcID}/changes/compare` && u.searchParams.get("external_id") === "ui-done");
    await expect(page.getByTestId("change-compare")).toBeVisible();
    await expect(page.getByTestId("change-compare")).toHaveAttribute("data-horizon", "6h");
    await expect(page.getByTestId("change-compare")).toHaveAttribute("data-source", "e2e-ci");
    await expect(page.getByTestId("compare-error")).toHaveCount(0);
    await expect(page.getByTestId("compare-ref")).toHaveText("v9.9.9");
    await expect(page.getByTestId("compare-terminal")).toHaveAttribute("data-phase", "succeeded");
    for (const key of ["before", "after"]) {
      const sideCell = page.getByTestId(`compare-${key}`);
      await expect(sideCell).toBeVisible();
      expect(SIDE_KINDS, `${key} is exactly one of figure|withheld|pending`).toContain(await sideCell.getAttribute("data-kind"));
    }

    // ── Back to the card, and on to the per-service timeline: both groups are there.
    await page.goto(`/services/${svcID}`);
    await expect(page.getByTestId("service-changes")).toBeVisible();
    await page.getByTestId("changes-timeline-link").click();
    await page.waitForURL((u) => u.pathname === `/services/${svcID}/changes`);
    await expect(page.getByTestId("service-changes-view")).toBeVisible();
    await expect(page.getByTestId("changes-view-error")).toHaveCount(0);
    await expect(page.getByTestId("changes-row")).toHaveCount(2);
    await expect(page.locator('[data-testid="changes-row"][data-external-id="ui-done"]')).toHaveAttribute("data-terminal", "succeeded");
    await expect(page.locator('[data-testid="changes-row"][data-external-id="ui-running"]').getByTestId("changes-compare")).toHaveAttribute("data-state", "no-terminal");
  });
});
