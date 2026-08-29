import { test, expect, type Page } from "@playwright/test";
import { apiGet, apiSend, ensureE2EWorkspace } from "./helpers";

// The browser gate for FR-024's SPA (AC-0163-8, D-0207): the `Release gate` card on the service page,
// the two ledger views and the override history, driven the way an operator drives them. The Go
// suite proves the transaction and the CAS; gate.spec.ts proves the HTTP contract; this proves the
// SURFACE, and in particular the five things the reviewer reads through the tests:
//
//   1. the card never asks the gate — every `POST …/gate` below is the PIPELINE's, made via API, and
//      the card shows what the LEDGER holds after a reload;
//   2. the editor offers the service's target inventory and saves the whole document under the
//      revision it saw;
//   3. the override is created and revoked from the card, by its own id, one at a time, and the
//      decision it changed says so (`unoverridden_action`);
//   4. the ledger outlives the policy: after Delete, the decision history still lists the rows;
//   5. a stale screen's Save is refused with a 409 that PRESERVES the draft and BLOCKS every mutation
//      until the explicit Reload (P1 [86]) — proven with two pages on one session.
//
// The session is shared and already signed in (local logins are rate-limited): nothing here logs
// in or out. Everything is `e2e-gate-ui-` prefixed and cleaned up in afterEach.

const PREFIX = "e2e-gate-ui";
const UUID_RE = /[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/i;

// A `datetime-local` value: tomorrow at this minute, in the browser's zone (the input's own format).
function tomorrowLocal(): string {
  const d = new Date(Date.now() + 24 * 3600_000);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

// One service whose 30d window has a target (a policy may only govern a window with one — D2).
async function createGovernedService(page: Page, projectID: string, slug: string) {
  const created = await apiSend(page, "post", `/api/v1/projects/${projectID}/services`, { slug, name: `E2E Gate UI ${slug}` });
  expect(created.status(), `create service ${slug}`).toBe(201);
  const svcID = (await created.json()).id as string;
  const target = await apiSend(page, "put", `/api/v1/projects/${projectID}/services/${svcID}/sla-target`, { window: "30d", objective: 99.9 });
  expect(target.status(), await target.text()).toBe(200);
  return svcID;
}

// The PIPELINE's call — the only POST to that route in this file. The SPA never makes it.
//
// Paced like a well-behaved pipeline: `POST …/gate` draws from a per-PRINCIPAL token bucket
// (`gate.evaluate_rate_principal_per_minute`, 10 → one token every 6 s) and the whole browser suite
// is ONE principal. gate-live.spec.ts and gate.spec.ts share that bucket and run beside this file,
// so a decision here is never made faster than the bucket refills — the file's net footprint stays
// at one token — and a 429 is absorbed once by honouring `Retry-After`, exactly as the CLI does.
const TOKEN_INTERVAL_MS = 6_500;
let lastDecideAt = 0;
async function pipelineDecides(page: Page, projectID: string, svcID: string) {
  const wait = lastDecideAt + TOKEN_INTERVAL_MS - Date.now();
  if (wait > 0) await page.waitForTimeout(wait);
  lastDecideAt = Date.now();
  let r = await apiSend(page, "post", `/api/v1/projects/${projectID}/services/${svcID}/gate`);
  if (r.status() === 429) {
    const retryAfter = Number(r.headers()["retry-after"] ?? "1");
    await page.waitForTimeout(Math.min(Math.max(retryAfter, 1), 7) * 1000);
    lastDecideAt = Date.now();
    r = await apiSend(page, "post", `/api/v1/projects/${projectID}/services/${svcID}/gate`);
  }
  expect(r.status(), await r.text()).toBe(200);
  return r.json();
}

test.describe("reliability gate UI", () => {
  test.afterEach(async ({ page }) => {
    const { projectID } = await ensureE2EWorkspace(page);
    for (const s of await apiGet(page, `/api/v1/projects/${projectID}/services`)) {
      if ((s.service.slug as string).startsWith(PREFIX)) {
        await apiSend(page, "delete", `/api/v1/projects/${projectID}/services/${s.service.id}`);
      }
    }
    for (const m of await apiGet(page, `/api/v1/projects/${projectID}/monitors`)) {
      if ((m.name as string).startsWith(PREFIX)) await apiSend(page, "delete", `/api/v1/monitors/${m.id}`);
    }
  });

  test("the card: configure through the UI, the pipeline's decision, the override and its effect, revoke, delete — and the ledger outlives the policy", async ({ page }) => {
    await page.goto("/services");
    const { projectID } = await ensureE2EWorkspace(page);
    const svcID = await createGovernedService(page, projectID, `${PREFIX}-card`);

    // ── Screen 1: the empty state. The inventory chip is the service's own target.
    await page.goto(`/services/${svcID}`);
    await expect(page.getByTestId("service-gate")).toBeVisible();
    await expect(page.getByTestId("gate-state")).toHaveText("not configured");
    await expect(page.getByTestId("gate-policy-chip")).toHaveText("no policy");
    await expect(page.getByTestId("gate-windows").locator('[data-testid="gate-window-chip"][data-window="30d"]')).toBeVisible();
    await expect(page.getByTestId("gate-latest-empty")).toBeVisible();
    await expect(page.getByTestId("gate-cli-command")).toContainText(`CERBIX_TOKEN=… cerbix gate check --project ${projectID} --service ${svcID}`);

    // ── Screen 2: the editor, prefilled with the template; the window is from the inventory.
    await page.getByTestId("gate-configure").click();
    await expect(page.getByTestId("gate-policy-form")).toBeVisible();
    await expect(page.getByTestId("gate-window")).toHaveValue("30d");
    await expect(page.getByTestId("gate-threshold")).toHaveValue("90");
    await expect(page.getByTestId("gate-seal-lag-minutes")).toHaveValue("15");
    await expect(page.getByTestId("gate-clause-service_incident_open-warn")).toHaveAttribute("aria-pressed", "true");
    await page.getByTestId("gate-clause-service_incident_open-block").click();
    await expect(page.getByTestId("gate-clause-service_incident_open-block")).toHaveAttribute("aria-pressed", "true");
    // `block` for an unavailable fact, so the fresh service's UNKNOWN is BLOCK-actioned and an
    // override has something to change (D9 acts on the action).
    await page.getByTestId("gate-unknown-behavior").selectOption("block");
    await page.getByTestId("gate-seal-lag-minutes").fill("15");
    await page.getByTestId("gate-save").click();
    await expect(page.getByTestId("gate-policy-chip")).toHaveText("revision 1");
    await expect(page.getByTestId("gate-policy-form")).toHaveCount(0);
    await expect(page.getByTestId("gate-readonly-clause-service_incident_open")).toHaveAttribute("data-assignment", "block");
    await expect(page.getByTestId("gate-latest-empty"), "a policy alone is no decision").toBeVisible();
    const stored = await apiGet(page, `/api/v1/projects/${projectID}/services/${svcID}/gate/policy`);
    expect(stored).toMatchObject({ revision: 1, window: "30d", max_seal_lag_seconds: 900, unknown_behavior: "block", budget_consumed_percent: 90 });
    expect(stored.clauses.service_incident_open).toBe("block");

    // ── The pipeline asks (the ONLY caller of POST …/gate). The card shows the ledger after reload.
    const first = await pipelineDecides(page, projectID, svcID);
    expect(first.state, "a fresh service has sealed nothing").toBe("UNKNOWN");
    expect(first.action, "unknown_behavior block").toBe("BLOCK");
    await page.reload();
    await expect(page.getByTestId("gate-latest-state")).toHaveText("UNKNOWN");
    await expect(page.getByTestId("gate-state"), "the header pill is the latest decision's").toHaveText("UNKNOWN");
    await expect(page.getByTestId("gate-latest-id")).toBeVisible();
    await expect(page.getByTestId("gate-latest-id")).toHaveAttribute("title", first.decision_id);
    await expect(page.getByTestId("gate-latest-action")).toHaveText("action BLOCK · exit 2");
    await expect(page.getByTestId("gate-latest-revision")).toHaveText("policy rev 1");
    expect(await page.locator('[data-testid="gate-reason"][data-code="never_sealed"]').count()).toBeGreaterThan(0);
    await expect(page.getByTestId("gate-latest-never-sealed")).toBeVisible();
    await expect(page.getByTestId("gate-latest-override")).toHaveCount(0);

    // ── Screen 3: an override from the card. One at a time.
    await expect(page.getByTestId("gate-override-active")).toHaveCount(0);
    await page.getByTestId("gate-override-input-reason").fill("e2e-gate-ui: hotfix");
    await page.getByTestId("gate-override-input-until").fill(tomorrowLocal());
    await page.getByTestId("gate-override-create").click();
    await expect(page.getByTestId("gate-override-active")).toBeVisible();
    await expect(page.getByTestId("gate-override-reason")).toHaveText("e2e-gate-ui: hotfix");
    await expect(page.getByTestId("gate-override-blocked")).toBeVisible();
    await expect(page.getByTestId("gate-override-create")).toBeDisabled();
    const active = await apiGet(page, `/api/v1/projects/${projectID}/services/${svcID}/gate/override`);
    expect(active.reason).toBe("e2e-gate-ui: hotfix");
    expect(active.policy_revision).toBe(1);

    // The pipeline asks again: the override changes the ACTION only, and the card says so.
    const second = await pipelineDecides(page, projectID, svcID);
    expect(second.state).toBe("UNKNOWN");
    expect(second.action).toBe("ALLOW");
    expect(second.unoverridden_action).toBe("BLOCK");
    expect(second.override_id).toBe(active.id);
    await page.reload();
    await expect(page.getByTestId("gate-latest-id")).toHaveAttribute("title", second.decision_id);
    await expect(page.getByTestId("gate-latest-action")).toHaveText("override applied → action ALLOW");
    await expect(page.getByTestId("gate-latest-override")).toBeVisible();
    await expect(page.getByTestId("gate-latest-unoverridden")).toHaveText("BLOCK");
    await expect(page.getByTestId("gate-latest-override")).toContainText("e2e-gate-ui: hotfix");

    // ── Revoke from the card, by the override's own id: asked first, then gone.
    await page.getByTestId("gate-override-revoke").click();
    await expect(page.getByTestId("gate-override-active"), "asking is not doing").toBeVisible();
    await page.getByTestId("gate-override-revoke-confirm").click();
    await expect(page.getByTestId("gate-override-active")).toHaveCount(0);
    await expect(page.getByTestId("gate-override-blocked")).toHaveCount(0);
    expect((await page.request.get(`/api/v1/projects/${projectID}/services/${svcID}/gate/override`)).status(), "the slot is released").toBe(404);
    const revoked = await apiGet(page, `/api/v1/projects/${projectID}/services/${svcID}/gate/overrides/${active.id}`);
    expect(revoked.status).toBe("revoked");
    expect(revoked.revoked_reason).toBe("manual");

    // ── Delete: the dialog, then the card is empty again — and the decisions are still there.
    await page.getByTestId("gate-configure").click();
    await page.getByTestId("gate-delete").click();
    await expect(page.getByTestId("gate-delete-dialog")).toBeVisible();
    await expect(page.getByTestId("gate-delete-dialog")).toContainText("revision 2");
    await expect(page.getByTestId("gate-delete-dialog")).toContainText("revision 3");
    await page.getByTestId("gate-delete-confirm").click();
    await expect(page.getByTestId("gate-delete-dialog")).toHaveCount(0);
    await expect(page.getByTestId("gate-state")).toHaveText("not configured");
    await expect(page.getByTestId("gate-policy-chip")).toHaveText("no policy");
    expect((await page.request.get(`/api/v1/projects/${projectID}/services/${svcID}/gate/policy`)).status()).toBe(404);

    // The ledger outlives the policy: the card's link pre-filters the history to this service.
    await page.getByTestId("gate-all-decisions").click();
    await page.waitForURL((u) => u.pathname === "/gate/decisions" && u.searchParams.get("service") === svcID);
    await expect(page.getByTestId("gate-decisions-service")).toHaveValue(svcID);
    await expect(page.getByTestId("gate-decision-row")).toHaveCount(2);
    const states = await page.getByTestId("gate-decision-row").evaluateAll((rows) => rows.map((r) => r.getAttribute("data-state")));
    expect(states).toEqual(["UNKNOWN", "UNKNOWN"]);
    await expect(page.locator(`[data-testid="gate-decision-row"][data-id="${second.decision_id}"]`)).toBeVisible();
    await expect(page.locator(`[data-testid="gate-decision-row"][data-id="${first.decision_id}"]`)).toBeVisible();
  });

  test("the ledger views: all decisions (cold deep link too) → one decision by id → the override history", async ({ page }) => {
    await page.goto("/services");
    const { projectID } = await ensureE2EWorkspace(page);
    const svcID = await createGovernedService(page, projectID, `${PREFIX}-ledger`);
    const gate = `/api/v1/projects/${projectID}/services/${svcID}/gate`;

    // Arranged over the API: a policy, two decisions, one override revoked and one active.
    const put = await apiSend(page, "put", `${gate}/policy`, {
      expected_revision: null,
      schema_version: 1,
      window: "30d",
      clauses: { budget_exhausted: "block", budget_consumed: "warn", page_burn_firing: "block", ticket_burn_firing: "warn", service_incident_open: "warn" },
      budget_consumed_percent: 90,
      max_seal_lag_seconds: 900,
      unknown_behavior: "block",
    });
    expect(put.status(), await put.text()).toBe(200);
    const d1 = await pipelineDecides(page, projectID, svcID);
    const expiresAt = new Date(Date.now() + 3600_000).toISOString();
    const o1 = await apiSend(page, "post", `${gate}/override`, { policy_revision: 1, reason: "e2e-gate-ui: first", expires_at: expiresAt });
    expect(o1.status(), await o1.text()).toBe(201);
    const o1ID = (await o1.json()).id as string;
    const d2 = await pipelineDecides(page, projectID, svcID);
    expect(d2.override_id).toBe(o1ID);
    expect((await apiSend(page, "delete", `${gate}/overrides/${o1ID}`)).status()).toBe(204);
    const o2 = await apiSend(page, "post", `${gate}/override`, { policy_revision: 1, reason: "e2e-gate-ui: second", expires_at: expiresAt });
    expect(o2.status(), await o2.text()).toBe(201);
    const o2ID = (await o2.json()).id as string;

    // ── A COLD deep link (P1 [88]): the workspace initialises after the view mounts, and the
    // route's pre-filter must survive it — the rows are this service's only.
    await page.goto(`/gate/decisions?service=${svcID}`);
    await expect(page.getByTestId("gate-decisions")).toBeVisible();
    await expect(page.getByTestId("gate-decisions-service")).toHaveValue(svcID);
    await expect(page.getByTestId("gate-decision-row")).toHaveCount(2);
    await expect(page.getByTestId("gate-decisions-error")).toHaveCount(0);

    // ── From the card: the same view, pre-filtered.
    await page.goto(`/services/${svcID}`);
    await expect(page.getByTestId("gate-latest-id")).toHaveAttribute("title", d2.decision_id);
    await page.getByTestId("gate-all-decisions").click();
    await page.waitForURL((u) => u.pathname === "/gate/decisions" && u.searchParams.get("service") === svcID);
    await expect(page.getByTestId("gate-decisions-service")).toHaveValue(svcID);
    await expect(page.getByTestId("gate-decision-row")).toHaveCount(2);
    const newest = page.locator(`[data-testid="gate-decision-row"][data-id="${d2.decision_id}"]`);
    await expect(newest).toHaveAttribute("data-state", "UNKNOWN");
    expect(await newest.locator('[data-testid="gate-reason-chip"][data-code="never_sealed"]').count()).toBeGreaterThan(0);
    await expect(page.getByTestId("gate-decisions-live-note")).toBeVisible();

    // ── One decision, by id: the record, and the raw JSON carrying the id.
    await newest.getByTestId("gate-decision-link").click();
    await page.waitForURL(new RegExp(`/gate/decisions/${d2.decision_id}$`));
    await expect(page.getByTestId("gate-decision-state")).toHaveText("UNKNOWN");
    await expect(page.getByTestId("gate-decision-action")).toHaveText("ALLOW");
    await expect(page.getByTestId("gate-decision-override-applied")).toBeVisible();
    await expect(page.getByTestId("gate-decision-override")).toContainText("e2e-gate-ui: first");
    await expect(page.getByTestId("gate-decision-json")).toContainText(`"decision_id": "${d2.decision_id}"`);
    await expect(page.getByTestId("gate-decision-json")).toContainText('"unoverridden_action": "BLOCK"');
    await page.getByTestId("gate-decision-service-link").click();
    await page.waitForURL(new RegExp(`/services/${svcID}$`));

    // ── The override history: one active, one revoked "by …" — no Revoke button here.
    await page.getByTestId("gate-override-history").click();
    await page.waitForURL(new RegExp(`/services/${svcID}/gate/overrides$`));
    await expect(page.getByTestId("gate-overrides")).toBeVisible();
    await expect(page.getByTestId("gate-override-row")).toHaveCount(2);
    const activeRow = page.locator(`[data-testid="gate-override-row"][data-id="${o2ID}"]`);
    await expect(activeRow).toHaveAttribute("data-status", "active");
    await expect(activeRow).toContainText("e2e-gate-ui: second");
    const revokedRow = page.locator(`[data-testid="gate-override-row"][data-id="${o1ID}"]`);
    await expect(revokedRow).toHaveAttribute("data-status", "revoked");
    await expect(revokedRow).toContainText("by ");
    await expect(page.getByTestId("gate-overrides").getByRole("button", { name: /revoke/i })).toHaveCount(0);
    await page.getByTestId("gate-overrides-back").click();
    await page.waitForURL(new RegExp(`/services/${svcID}$`));
    await expect(page.getByTestId("gate-override-active")).toBeVisible();
    await expect(page.getByTestId("gate-override-reason")).toHaveText("e2e-gate-ui: second");
  });

  test("P1 [86] live: a stale screen's Save is a 409 that preserves the draft and blocks every mutation until Reload", async ({ page, context }) => {
    await page.goto("/services");
    const { projectID } = await ensureE2EWorkspace(page);
    const svcID = await createGovernedService(page, projectID, `${PREFIX}-stale`);

    // Screen 1 creates revision 1.
    await page.goto(`/services/${svcID}`);
    await page.getByTestId("gate-configure").click();
    await page.getByTestId("gate-save").click();
    await expect(page.getByTestId("gate-policy-chip")).toHaveText("revision 1");

    // Screen 2 — same session, a second tab — opens the editor on revision 1 and edits.
    const stale = await context.newPage();
    try {
      await stale.goto(`/services/${svcID}`);
      await expect(stale.getByTestId("gate-policy-chip")).toHaveText("revision 1");
      await stale.getByTestId("gate-configure").click();
      await stale.getByTestId("gate-threshold").fill("80");
      await expect(stale.getByTestId("gate-save")).toBeEnabled();

      // Screen 1 saves revision 2 underneath it.
      await page.getByTestId("gate-configure").click();
      await page.getByTestId("gate-threshold").fill("70");
      await page.getByTestId("gate-save").click();
      await expect(page.getByTestId("gate-policy-chip")).toHaveText("revision 2");

      // Screen 2 saves against revision 1: refused, draft kept, everything blocked.
      await stale.getByTestId("gate-save").click();
      await expect(stale.getByTestId("gate-policy-error")).toContainText("changed while you were editing");
      await expect(stale.getByTestId("gate-save")).toBeDisabled();
      await expect(stale.getByTestId("gate-delete")).toBeDisabled();
      await expect(stale.getByTestId("gate-override-create")).toBeDisabled();
      await expect(stale.getByTestId("gate-threshold"), "what was typed stays").toHaveValue("80");
      await expect(stale.getByTestId("gate-reload")).toBeVisible();
      expect((await apiGet(page, `/api/v1/projects/${projectID}/services/${svcID}/gate/policy`)).budget_consumed_percent, "the refused write changed nothing").toBe(70);

      // Discard does not unblock (P1 [86]); Reload does, and re-prefills with the server's policy.
      await stale.getByTestId("gate-discard").click();
      await expect(stale.getByTestId("gate-policy-form")).toHaveCount(0);
      await expect(stale.getByTestId("gate-reload")).toBeVisible();
      await expect(stale.getByTestId("gate-configure")).toBeDisabled();
      await stale.getByTestId("gate-reload").click();
      await expect(stale.getByTestId("gate-reload")).toHaveCount(0);
      await expect(stale.getByTestId("gate-policy-chip")).toHaveText("revision 2");
      await expect(stale.getByTestId("gate-configure")).toBeEnabled();
      await stale.getByTestId("gate-configure").click();
      await expect(stale.getByTestId("gate-threshold")).toHaveValue("70");
      await expect(stale.getByTestId("gate-save")).toBeEnabled();
      await stale.getByTestId("gate-threshold").fill("85");
      await stale.getByTestId("gate-save").click();
      await expect(stale.getByTestId("gate-policy-chip")).toHaveText("revision 3");
      expect((await apiGet(page, `/api/v1/projects/${projectID}/services/${svcID}/gate/policy`)).budget_consumed_percent).toBe(85);
    } finally {
      await stale.close();
    }
  });
});
