import { test, expect } from "@playwright/test";
import { randomUUID } from "node:crypto";
import { apiGet, apiSend, ensureE2EWorkspace } from "./helpers";

// The live gate for FR-024 (func-reliability-gate.md D7, D9, D10, D13a, D15; §7 groups Ledger,
// Tenant, Presence, Policy and override routes). The Go suite proves the transaction, the CAS and
// the status function against a real database; this proves the HTTP CONTRACT a CI/CD client and
// the SPA are written against, on a running stack, in the order an operator would hit it:
//
//   1. a service with no policy is NOT_CONFIGURED — a 200 with no `action`, never an error
//      (invariant 2), and it is a ledger row like any other decision;
//   2. the policy is a full D11 document with a generation that a stale writer cannot overwrite
//      (409 before the no-op comparison);
//   3. every decision is readable by id through the PROJECT-scoped ledger route and shows up in
//      the listing with the same id (D10, §5);
//   4. an override is created through its endpoint only, one active per service, revoked by its
//      immutable id, and its record keeps the revoker's label after revocation (invariant 17);
//   5. a malformed id is 400; a well-formed id is 404 whether it is UNKNOWN (no such service
//      anywhere) or FOREIGN (another project's real service, reached through this project's
//      path) — on decide, policy, override and ledger alike, and a FOREIGN refusal changes
//      nothing in the other project (D15);
//   6. the ledger OUTLIVES the service: after DELETE the by-id read still answers 200 with
//      `service_id: null` — invariant 12, "proven by an HTTP read after the service is deleted".
//
// API-shaped: the stored session is reused (local logins are rate-limited), everything is
// prefixed `e2e-` and cleaned up in afterEach, per the suite's contract with dev stacks.

const SLUG = "e2e-gate";
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

// The full D11 document — every clause of schema version 1, both thresholds, the unknown
// behaviour. `expected_revision: null` is "I believe nothing is configured" (D13a).
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
  max_seal_lag_seconds: 900,
  unknown_behavior: "warn",
};

test.describe("reliability gate", () => {
  test.afterEach(async ({ page }) => {
    const { projectID } = await ensureE2EWorkspace(page);
    for (const s of await apiGet(page, `/api/v1/projects/${projectID}/services`)) {
      if ((s.service.slug as string).startsWith("e2e-gate")) {
        await apiSend(page, "delete", `/api/v1/projects/${projectID}/services/${s.service.id}`);
      }
    }
  });

  // One service whose objective for the policy's window is set, because a policy may only
  // govern a window the service has a target for (D2; store: "the service has no SLO target").
  async function createGovernedService(page: import("@playwright/test").Page, projectID: string, slug: string) {
    const created = await apiSend(page, "post", `/api/v1/projects/${projectID}/services`, {
      slug, name: `E2E Gate ${slug}`,
    });
    expect(created.status(), `create service ${slug}`).toBe(201);
    const svcID = (await created.json()).id as string;
    const target = await apiSend(page, "put", `/api/v1/projects/${projectID}/services/${svcID}/sla-target`, {
      window: "30d", objective: 99.9,
    });
    expect(target.status(), await target.text()).toBe(200);
    return svcID;
  }

  test("NOT_CONFIGURED, the policy generation, the ledger, the override lifecycle, and a ledger that outlives its service", async ({ page }) => {
    // The helpers fetch RELATIVE urls from inside the page; land on the app's origin first.
    await page.goto("/services");
    const { projectID } = await ensureE2EWorkspace(page);
    const svcID = await createGovernedService(page, projectID, SLUG);
    const gate = `/api/v1/projects/${projectID}/services/${svcID}/gate`;
    const ledger = `/api/v1/projects/${projectID}/gate/decisions`;

    // ── (1) NOT_CONFIGURED is a 200 with no `action` and exactly one reason (D4, D7, invariant 2).
    const first = await apiSend(page, "post", gate);
    expect(first.status(), await first.text()).toBe(200);
    const notConfigured = await first.json();
    expect(notConfigured.state).toBe("NOT_CONFIGURED");
    expect("action" in notConfigured, "NOT_CONFIGURED carries no action").toBe(false);
    expect("policy_revision" in notConfigured, "NOT_CONFIGURED carries no policy fields").toBe(false);
    expect(notConfigured.reasons).toHaveLength(1);
    expect(notConfigured.reasons[0].code).toBe("not_configured");
    expect(notConfigured.reasons[0].docs, "the one reason points at the docs").toMatch(/^https?:\/\//);
    expect(notConfigured.decision_id, "NOT_CONFIGURED is ledgered too").toMatch(UUID_RE);
    expect(notConfigured.service_id).toBe(svcID);
    expect(notConfigured.schema_version).toBe(1);

    // GET policy on a never-configured service is the 404 of D13a, not an empty document.
    const noPolicy = await page.request.get(`${gate}/policy`);
    expect(noPolicy.status()).toBe(404);
    expect((await noPolicy.json()).error).toBe("not_configured");

    // A decision body that names anything at all is refused (D9: the override path is the only
    // way an override comes to exist; §7 "the same field in a decision body is refused").
    const smuggled = await apiSend(page, "post", gate, { override: { reason: "x" } });
    expect(smuggled.status(), "a decision body with a field is refused").toBe(400);
    // …and `{}` is accepted exactly like no body (D6).
    const emptyObject = await apiSend(page, "post", gate, {});
    expect(emptyObject.status(), await emptyObject.text()).toBe(200);

    // ── (2) PUT policy: the full D11 document, first generation.
    const put = await apiSend(page, "put", `${gate}/policy`, { expected_revision: null, ...POLICY });
    expect(put.status(), await put.text()).toBe(200);
    expect(await put.json()).toEqual({ revision: 1 });

    const stored = await apiGet(page, `${gate}/policy`);
    expect(stored.revision).toBe(1);
    expect(stored.schema_version).toBe(POLICY.schema_version);
    expect(stored.window).toBe(POLICY.window);
    expect(stored.clauses).toEqual(POLICY.clauses);
    expect(stored.budget_consumed_percent).toBe(POLICY.budget_consumed_percent);
    expect(stored.max_seal_lag_seconds).toBe(POLICY.max_seal_lag_seconds);
    expect(stored.unknown_behavior).toBe(POLICY.unknown_behavior);
    expect(stored.updated_at, "the policy names when it was written").toBeTruthy();
    expect(stored.updated_by, "the policy names who wrote it").toBeTruthy();

    // A stale writer — even with an IDENTICAL body — is 409, because the CAS runs before the
    // no-op comparison (D13a, review round 10's added mutation). The policy does not move.
    const stale = await apiSend(page, "put", `${gate}/policy`, { expected_revision: 0, ...POLICY });
    expect(stale.status(), "stale expected_revision").toBe(409);
    expect((await stale.json()).error).toBe("revision_conflict");
    const staleNull = await apiSend(page, "put", `${gate}/policy`, { expected_revision: null, ...POLICY });
    expect(staleNull.status(), "null against a configured policy is also stale").toBe(409);
    expect((await apiGet(page, `${gate}/policy`)).revision, "a refused write changes nothing").toBe(1);

    // A body missing a field is refused NAMING it; the server fills nothing in (D11).
    const { unknown_behavior: _omitted, ...missing } = POLICY;
    const incomplete = await apiSend(page, "put", `${gate}/policy`, { expected_revision: 1, ...missing });
    expect(incomplete.status()).toBe(400);
    expect((await incomplete.json()).error).toContain("unknown_behavior");
    expect((await apiGet(page, `${gate}/policy`)).revision, "a refused write bumps no generation").toBe(1);

    // ── (3) A decision under the policy: the presence contract of D7, and the ledger of D10.
    const second = await apiSend(page, "post", gate);
    expect(second.status(), await second.text()).toBe(200);
    const decision = await second.json();
    // A fresh service has sealed nothing, so the honest state is UNKNOWN — but the contract
    // asserted here is presence, not the verdict: a policy exists, so an action exists.
    expect(["ALLOW", "WARN", "BLOCK", "UNKNOWN"]).toContain(decision.state);
    expect(["ALLOW", "WARN", "BLOCK"], "a configured service always has an action").toContain(decision.action);
    expect(decision.policy_revision).toBe(1);
    expect(decision.window).toBe("30d");
    expect(decision.reasons.length, "reasons are never empty under a policy").toBeGreaterThan(0);
    expect(decision.decision_id).toMatch(UUID_RE);
    expect(decision.decision_id).not.toBe(notConfigured.decision_id);
    expect(decision.service_id).toBe(svcID);
    expect(decision.service_slug).toBe(SLUG);
    expect(decision.evaluated_at).toBeTruthy();
    if (decision.state === "UNKNOWN") {
      // unknown_behavior "warn" is the policy's own word for this case (D7).
      expect(decision.action).toBe("WARN");
    }

    // Invariant 12's HTTP read, while the service still exists: the same decision, by id.
    const byID = await apiGet(page, `${ledger}/${decision.decision_id}`);
    expect(byID.decision_id).toBe(decision.decision_id);
    expect(byID.state).toBe(decision.state);
    expect(byID.action).toBe(decision.action);
    expect(byID.policy_revision).toBe(1);
    expect(byID.service_id).toBe(svcID);
    expect(byID.reasons).toEqual(decision.reasons);

    // The §5 listing, filtered to this service: both decisions are there, each with its id and
    // its service. A malformed or absent range is refused, never defaulted.
    const from = new Date(Date.now() - 60 * 60 * 1000).toISOString();
    const to = new Date(Date.now() + 60 * 60 * 1000).toISOString();
    const listing = await apiGet(page, `${ledger}?from=${from}&to=${to}&service_id=${svcID}`);
    const ids = listing.items.map((i: any) => i.decision_id);
    expect(ids).toContain(decision.decision_id);
    expect(ids).toContain(notConfigured.decision_id);
    const listed = listing.items.find((i: any) => i.decision_id === decision.decision_id);
    expect(listed.service_id).toBe(svcID);
    expect(listed.state).toBe(decision.state);
    expect(listed.policy_revision).toBe(1);
    expect(listing.next_cursor, "one page: the cursor is null, not absent").toBeNull();
    const noRange = await page.request.get(ledger);
    expect(noRange.status()).toBe(400);
    expect((await noRange.json()).error).toBe("range_required");

    // ── (4) The override lifecycle (D9, D13a, invariant 17).
    const noneActive = await page.request.get(`${gate}/override`);
    expect(noneActive.status()).toBe(404);
    expect((await noneActive.json()).error).toBe("none_active");

    const expiresAt = new Date(Date.now() + 60 * 60 * 1000).toISOString();
    const createOverride = await apiSend(page, "post", `${gate}/override`, {
      policy_revision: 1, reason: "e2e-gate: planned release window", expires_at: expiresAt,
    });
    expect(createOverride.status(), await createOverride.text()).toBe(201);
    const overrideID = (await createOverride.json()).id as string;
    expect(overrideID).toMatch(UUID_RE);

    const active = await apiGet(page, `${gate}/override`);
    expect(active.id).toBe(overrideID);
    expect(active.reason).toBe("e2e-gate: planned release window");
    expect(active.policy_revision).toBe(1);
    expect(active.actor_label, "the actor is the principal, server-derived").toBeTruthy();
    expect(new Date(active.expires_at).getTime()).toBe(new Date(expiresAt).getTime());
    expect("action" in active, "the read shapes carry no action (D9)").toBe(false);

    // One active per service.
    const secondOverride = await apiSend(page, "post", `${gate}/override`, {
      policy_revision: 1, reason: "e2e-gate: a second one", expires_at: expiresAt,
    });
    expect(secondOverride.status()).toBe(409);
    expect((await secondOverride.json()).error).toBe("override_active");

    // The active record, by id: every closure field PRESENT and null (D13a: "no field ever absent").
    const openRecord = await apiGet(page, `${gate}/overrides/${overrideID}`);
    expect(openRecord.status).toBe("active");
    for (const k of ["revoked_at", "revoked_reason", "revoked_by_label", "revoked_by_user_id", "revoked_via_token"]) {
      expect(k in openRecord, `${k} present on an active row`).toBe(true);
      expect(openRecord[k], `${k} null on an active row`).toBeNull();
    }
    expect(openRecord.actor_label).toBe(active.actor_label);
    expect(openRecord.via_token, "a session principal is not a token").toBe(false);

    // A decision with the override in force: D9 fixes what it does — a BLOCK becomes ALLOW and
    // says so; WARN and ALLOW are left alone. Either way the override is not an error.
    const overridden = await apiSend(page, "post", gate);
    expect(overridden.status(), await overridden.text()).toBe(200);
    const withOverride = await overridden.json();
    expect(withOverride.state).toBe(decision.state);
    if (withOverride.state === "BLOCK") {
      expect(withOverride.action).toBe("ALLOW");
      expect(withOverride.unoverridden_action).toBe("BLOCK");
      expect(withOverride.override_id).toBe(overrideID);
    } else {
      expect(withOverride.action).toBe(decision.action);
    }

    // Revocation is by the immutable id: 204 once, 409 after — never a silent second 204.
    const revoke = await apiSend(page, "delete", `${gate}/overrides/${overrideID}`);
    expect(revoke.status(), await revoke.text()).toBe(204);
    const revokeAgain = await apiSend(page, "delete", `${gate}/overrides/${overrideID}`);
    expect(revokeAgain.status()).toBe(409);
    expect((await revokeAgain.json()).error).toBe("override_not_active");
    expect((await page.request.get(`${gate}/override`)).status(), "the slot is released").toBe(404);

    // Invariant 17's later read: `revoked` with the revoker's label, a manual closure carrying
    // all five closure fields — via a session, so `revoked_via_token` is false, not null.
    const revoked = await apiGet(page, `${gate}/overrides/${overrideID}`);
    expect(revoked.status).toBe("revoked");
    expect(revoked.revoked_reason).toBe("manual");
    expect(revoked.revoked_at).toBeTruthy();
    expect(revoked.revoked_by_label, "a manual closure names the revoker").toBeTruthy();
    expect(revoked.revoked_via_token).toBe(false);
    expect(revoked.actor_label, "the creator is still named after revocation").toBe(active.actor_label);
    const history = await apiGet(page, `${gate}/overrides`);
    expect(history.items.map((i: any) => i.id)).toContain(overrideID);
    expect(history.items.find((i: any) => i.id === overrideID).status).toBe("revoked");

    // ── (6) Invariant 12: the ledger outlives the service. Delete it, then read the decision
    // through the project-scoped route — 200, and `service_id` is null, not absent.
    const del = await apiSend(page, "delete", `/api/v1/projects/${projectID}/services/${svcID}`);
    expect([200, 204], `delete service -> ${del.status()}`).toContain(del.status());
    expect((await page.request.get(`/api/v1/projects/${projectID}/services/${svcID}`)).status(),
      "the service is gone").toBe(404);

    const afterDelete = await page.request.get(`${ledger}/${decision.decision_id}`);
    expect(afterDelete.status(), "the decision must still be readable after the service is deleted").toBe(200);
    const survivor = await afterDelete.json();
    expect(survivor.decision_id).toBe(decision.decision_id);
    expect("service_id" in survivor, "service_id is present-and-null, never absent").toBe(true);
    expect(survivor.service_id).toBeNull();
    expect(survivor.service_slug, "the row keeps the slug the service had").toBe(SLUG);
    expect(survivor.state).toBe(decision.state);
    expect(survivor.action).toBe(decision.action);
    expect(survivor.policy_revision).toBe(1);
    expect(survivor.reasons).toEqual(decision.reasons);

    // The NOT_CONFIGURED row outlives it too.
    const survivorNC = await apiGet(page, `${ledger}/${notConfigured.decision_id}`);
    expect(survivorNC.state).toBe("NOT_CONFIGURED");
    expect(survivorNC.service_id).toBeNull();
    expect("action" in survivorNC).toBe(false);

    // A `service_id` filter for a service that no longer exists is an EMPTY page, never a 404,
    // because the ledger outlives services (§5).
    const orphanFilter = await apiGet(page, `${ledger}?from=${from}&to=${to}&service_id=${svcID}`);
    expect(orphanFilter.items).toEqual([]);
  });

  test("tenant (D15): malformed -> 400; UNKNOWN (random UUID) -> 404; FOREIGN (another project's real service, and its real override id under our own service) -> 404 and nothing there changes", async ({ page }) => {
    await page.goto("/services");
    const { orgID, projectID } = await ensureE2EWorkspace(page);
    const svcID = await createGovernedService(page, projectID, `${SLUG}-tenant`);
    const base = `/api/v1/projects/${projectID}/services`;
    const ledger = `/api/v1/projects/${projectID}/gate/decisions`;
    const unknown = randomUUID();
    const overrideBody = () => ({ policy_revision: 1, reason: "x", expires_at: new Date(Date.now() + 3600e3).toISOString() });

    // Every service-scoped gate route, as (method, sub-path, body). The two override-by-id
    // routes take the id to use, because the FOREIGN case must name a REAL foreign override.
    const serviceRoutes = (overrideID: string) => [
      ["post", "/gate", undefined],
      ["get", "/gate/policy", undefined],
      ["put", "/gate/policy", { expected_revision: 1, ...POLICY }],
      ["delete", "/gate/policy?expected_revision=1", undefined], // the CAS rides the query string (D13a)
      ["get", "/gate/override", undefined],
      ["post", "/gate/override", overrideBody()],
      ["get", "/gate/overrides", undefined],
      ["get", `/gate/overrides/${overrideID}`, undefined],
      ["delete", `/gate/overrides/${overrideID}`, undefined],
    ] as const;
    const hit = (method: "get" | "post" | "put" | "delete", path: string, data: unknown) =>
      page.request[method](path, data === undefined ? undefined : { data });

    // ── Malformed service id: 400 BEFORE the store is asked (D15), on every service-scoped route.
    for (const [method, sub, data] of serviceRoutes(unknown)) {
      const path = `${base}/not-a-uuid${sub}`;
      const r = await hit(method, path, data);
      expect(r.status(), `MALFORMED ${method.toUpperCase()} ${path}`).toBe(400);
      expect((await r.json()).error, `MALFORMED ${method.toUpperCase()} ${path} names the rule`).toContain("UUID");
    }

    // ── UNKNOWN: a well-formed id that no service anywhere has — the tenant 404 of D15 on every
    // service-scoped route, with the spec's `not found` body (existence hidden).
    for (const [method, sub, data] of serviceRoutes(unknown)) {
      const path = `${base}/${unknown}${sub}`;
      const r = await hit(method, path, data);
      expect(r.status(), `UNKNOWN ${method.toUpperCase()} ${path}`).toBe(404);
      expect((await r.json()).error, `UNKNOWN ${method.toUpperCase()} ${path} body`).toBe("not found");
    }

    // A malformed or UNKNOWN OVERRIDE id under a real service, and the same for a decision id.
    const malformedOverride = await page.request.get(`${base}/${svcID}/gate/overrides/not-a-uuid`);
    expect(malformedOverride.status(), "MALFORMED override id").toBe(400);
    const malformedRevoke = await apiSend(page, "delete", `${base}/${svcID}/gate/overrides/not-a-uuid`);
    expect(malformedRevoke.status(), "MALFORMED override id on revoke").toBe(400);
    const unknownOverride = await page.request.get(`${base}/${svcID}/gate/overrides/${unknown}`);
    expect(unknownOverride.status(), "UNKNOWN override id").toBe(404);
    const unknownRevoke = await apiSend(page, "delete", `${base}/${svcID}/gate/overrides/${unknown}`);
    expect(unknownRevoke.status(), "UNKNOWN override id on revoke").toBe(404);
    const malformedDecision = await page.request.get(`${ledger}/not-a-uuid`);
    expect(malformedDecision.status(), "MALFORMED decision id").toBe(400);
    const unknownDecision = await page.request.get(`${ledger}/${unknown}`);
    expect(unknownDecision.status(), "UNKNOWN decision id").toBe(404);

    // ── FOREIGN: a SECOND `e2e-` project the caller is authorized to, holding a REAL governed
    // service with a policy, a decision and an active override. Reached through the FIRST
    // project's path, every route is the same 404 as UNKNOWN — and nothing in the second
    // project moves: its policy, its override and its decision are read BEFORE the block of
    // foreign requests and AFTER it, through the CORRECT project, and compared field for field.
    const otherSlug = `${SLUG}-other-${Date.now()}`;
    const otherRes = await apiSend(page, "post", `/api/v1/organizations/${orgID}/projects`, { slug: otherSlug, name: "E2E Gate Other" });
    expect(otherRes.status(), await otherRes.text()).toBe(201);
    const otherID = (await otherRes.json()).id as string;
    try {
      const foreignSvc = await createGovernedService(page, otherID, `${SLUG}-foreign`);
      const own = `/api/v1/projects/${otherID}/services/${foreignSvc}/gate`;
      const ownLedger = `/api/v1/projects/${otherID}/gate/decisions`;
      const putOwn = await apiSend(page, "put", `${own}/policy`, { expected_revision: null, ...POLICY });
      expect(putOwn.status(), await putOwn.text()).toBe(200);
      const decided = await apiSend(page, "post", own);
      expect(decided.status(), await decided.text()).toBe(200);
      const foreignDecision = await decided.json();
      expect(foreignDecision.policy_revision).toBe(1);
      const createdOverride = await apiSend(page, "post", `${own}/override`, {
        policy_revision: 1, reason: "e2e-gate: foreign project's own override", expires_at: new Date(Date.now() + 3600e3).toISOString(),
      });
      expect(createdOverride.status(), await createdOverride.text()).toBe(201);
      const foreignOverride = (await createdOverride.json()).id as string;

      const from = new Date(Date.now() - 60 * 60 * 1000).toISOString();
      const to = new Date(Date.now() + 60 * 60 * 1000).toISOString();
      const snapshot = async () => ({
        policy: await apiGet(page, `${own}/policy`),
        active: await apiGet(page, `${own}/override`),
        record: await apiGet(page, `${own}/overrides/${foreignOverride}`),
        history: (await apiGet(page, `${own}/overrides`)).items,
        decision: await apiGet(page, `${ownLedger}/${foreignDecision.decision_id}`),
        listing: (await apiGet(page, `${ownLedger}?from=${from}&to=${to}&service_id=${foreignSvc}`)).items,
      });
      const before = await snapshot();
      expect(before.policy.revision).toBe(1);
      expect(before.active.id).toBe(foreignOverride);
      expect(before.record.status).toBe("active");
      expect(before.decision.decision_id).toBe(foreignDecision.decision_id);
      expect(before.listing.map((i: any) => i.decision_id), "one decision so far in the foreign project").toEqual([foreignDecision.decision_id]);

      // The foreign service's real id, the foreign override's real id — through the FIRST
      // project's path. 404 `not found` everywhere: not 400, not 403, not 200.
      for (const [method, sub, data] of serviceRoutes(foreignOverride)) {
        const path = `${base}/${foreignSvc}${sub}`;
        const r = await hit(method, path, data);
        expect(r.status(), `FOREIGN ${method.toUpperCase()} ${path}`).toBe(404);
        expect((await r.json()).error, `FOREIGN ${method.toUpperCase()} ${path} body`).toBe("not found");
      }
      // The foreign override's REAL id under the FIRST project's OWN real service: the service check
      // passes, so this pair is the only HTTP route that reaches the override-id scoping itself —
      // a missing service/project predicate in the by-id read or the revoke would answer 200/204
      // here and show up in the after-snapshot below. 404 `not found`, both.
      const crossGet = await page.request.get(`${base}/${svcID}/gate/overrides/${foreignOverride}`);
      expect(crossGet.status(), "FOREIGN override id under the first project's own service").toBe(404);
      expect((await crossGet.json()).error).toBe("not found");
      const crossRevoke = await apiSend(page, "delete", `${base}/${svcID}/gate/overrides/${foreignOverride}`);
      expect(crossRevoke.status(), "FOREIGN override id revoked through the first project's own service").toBe(404);
      expect((await crossRevoke.json()).error).toBe("not found");

      // The project-scoped ledger: the foreign decision by id is 404, and a `service_id` filter
      // naming the foreign service is an EMPTY page (§5: never a 404, because the ledger
      // outlives services — but never another project's rows either).
      const foreignByID = await page.request.get(`${ledger}/${foreignDecision.decision_id}`);
      expect(foreignByID.status(), "FOREIGN decision id through the first project").toBe(404);
      expect((await foreignByID.json()).error).toBe("not found");
      const foreignFilter = await apiGet(page, `${ledger}?from=${from}&to=${to}&service_id=${foreignSvc}`);
      expect(foreignFilter.items, "FOREIGN service_id filter is an empty page").toEqual([]);
      expect(foreignFilter.next_cursor).toBeNull();

      // AFTER: field for field, nothing in the second project changed — the policy (revision
      // and document), the active override (id, status, every closure field), the override
      // record and history, the decision by id, and the ledger still holds exactly one row.
      const after = await snapshot();
      expect(after.policy, "FOREIGN refusals left the policy document untouched").toEqual(before.policy);
      expect(after.active, "FOREIGN refusals left the active override untouched").toEqual(before.active);
      expect(after.record, "FOREIGN refusals left the override record untouched").toEqual(before.record);
      expect(after.history, "FOREIGN refusals wrote no override history").toEqual(before.history);
      expect(after.decision, "FOREIGN refusals left the decision untouched").toEqual(before.decision);
      expect(after.listing, "FOREIGN POST …/gate ledgered nothing in the second project").toEqual(before.listing);
      expect(after.policy.revision).toBe(1);
      expect(after.active.status ?? after.record.status).toBe("active");
    } finally {
      // The second project and everything in it — the `e2e-` service, its policy, override and
      // ledger rows — go with the project (the ledger keeps rows only while the project does).
      const del = await apiSend(page, "delete", `/api/v1/projects/${otherID}`);
      expect([200, 204], `delete second project -> ${del.status()}`).toContain(del.status());
    }

    // None of the refusals above touched the FIRST project's real service either: still unconfigured.
    const untouched = await apiSend(page, "post", `${base}/${svcID}/gate`);
    expect(untouched.status()).toBe(200);
    expect((await untouched.json()).state).toBe("NOT_CONFIGURED");
    expect((await page.request.get(`${base}/${svcID}/gate/policy`)).status(), "no policy was written on the first project's service").toBe(404);
  });
});
