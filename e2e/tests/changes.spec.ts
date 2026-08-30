import { test, expect, request } from "@playwright/test";
import { randomUUID } from "node:crypto";
import { apiGet, apiSend, ensureE2EWorkspace } from "./helpers";

// Change intelligence for FR-025 (func-change-intelligence.md D2, D3, D5, D6, D7, D8+D-0211,
// D11, D12+D-0212, D13, §7). The Go suite proves the identity lock, the store transaction and
// the compare arithmetic against a real database; this proves the HTTP CONTRACT the CLI verb
// and the SPA are written against, on a running stack, in the order a pipeline would hit it:
//
//   1. phases are append-only and ordered by the domain (D3): `started` then one terminal, an
//      identical replay is 200 with the ORIGINAL row (same id, same recorded_at), a differing
//      replay is 409 `phase_exists` naming the field, a second terminal or a `started` after a
//      terminal is 409 `phase_order`, a differing kind on one identity is 409 `kind_mismatch`
//      and writes nothing; the actor is server-derived and typed (D5); text is normalized to
//      NFC + trim by the transport and the composed spelling replays as identical (D2); a
//      `decision_id` must be a ledger row of this service (D11) and the timeline reads it back
//      live — state and `overridden`, no `action` for NOT_CONFIGURED, no `aged_out`;
//   2. the timeline is a `[from, to)` read of GROUPS by `latest_occurred_at` with all phases
//      nested (one selected by its terminal keeps a `started` older than `from`), filters
//      before the limit, a keyset cursor that never repeats a group, and the closed refusal
//      vocabulary (D6); the comparison rests on the terminal phase, floors T to the canonical
//      bucket, states `sealed_through` (null on a never-sealed service), and each side is
//      EXACTLY one of figure | withheld | pending — never a partial figure (D8, D-0211);
//   3. tenant isolation (a malformed id is 400 before the store, unknown and FOREIGN are the
//      same 404, and a foreign refusal changes nothing) and the token allow-list of D12: a CI
//      token `role: editor, actions: [gate:evaluate, change:record]` records (201, actor
//      `token:<name>`) and asks the gate (200) but is 403 — not 404 — on every read, the list
//      is validated at create (`action_unknown`, `action_not_granted` per D-0212), `actions:
//      null` leaves the role in charge, and a revoked or bogus secret is 401.
//
// API-shaped: the stored session is reused (local logins are rate-limited), everything is
// prefixed `e2e-` and cleaned up in afterEach, per the suite's contract with dev stacks. The
// record limiter allows 30 per principal per minute (§5a): each test stays well under it, and
// the CI token counts against its own key, not the admin's.

const SLUG = "e2e-change";
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const BASE_URL = process.env.CERBIX_URL || "http://localhost:8080";

// Whole-second instants: `occurred_at` is stored to the microsecond and echoed in RFC3339Nano,
// so comparisons are by epoch millis, never by string.
const minutesAgo = (m: number) => new Date(Math.floor(Date.now() / 1000) * 1000 - m * 60_000).toISOString();
const minutesAhead = (m: number) => new Date(Math.floor(Date.now() / 1000) * 1000 + m * 60_000).toISOString();

// Exactly one of figure | withheld | pending, and NEVER a partial figure (D8, invariant 11).
function expectOneSideShape(side: any, label: string) {
  expect(typeof side.from, `${label} states its window`).toBe("string");
  expect(typeof side.to, `${label} states its window`).toBe("string");
  const figure = "availability" in side;
  const withheld = typeof side.withheld === "string" && side.withheld.length > 0;
  const pending = side.pending === true;
  expect(
    [figure, withheld, pending].filter(Boolean),
    `${label} is exactly one of figure|withheld|pending, got ${JSON.stringify(side)}`,
  ).toHaveLength(1);
  const figureFields = ["availability", "good_seconds", "bad_seconds", "unknown_seconds", "excluded_seconds", "buckets"];
  for (const k of figureFields) {
    expect(k in side, `${label}: ${figure ? `a figure carries ${k}` : `no partial figure (${k})`}`).toBe(figure);
  }
}

test.describe("change intelligence", () => {
  test.afterEach(async ({ page }) => {
    await page.goto("/services");
    const { orgID, projectID } = await ensureE2EWorkspace(page);
    for (const s of await apiGet(page, `/api/v1/projects/${projectID}/services`)) {
      if ((s.service.slug as string).startsWith(SLUG)) {
        await apiSend(page, "delete", `/api/v1/projects/${projectID}/services/${s.service.id}`);
      }
    }
    for (const t of await apiGet(page, `/api/v1/organizations/${orgID}/tokens`)) {
      if ((t.name as string).startsWith(SLUG)) {
        await apiSend(page, "delete", `/api/v1/tokens/${t.id}`);
      }
    }
    for (const p of await apiGet(page, `/api/v1/organizations/${orgID}/projects`)) {
      if ((p.slug as string).startsWith(`${SLUG}-other`)) {
        await apiSend(page, "delete", `/api/v1/projects/${p.id}`);
      }
    }
  });

  // A service is enough — a change needs no SLO target, and the gate on an unconfigured
  // service still ledgers a NOT_CONFIGURED decision (FR-024 invariant 2), which D11 accepts.
  async function createService(page: import("@playwright/test").Page, projectID: string, slug: string) {
    const created = await apiSend(page, "post", `/api/v1/projects/${projectID}/services`, {
      slug, name: `E2E Change ${slug}`,
    });
    expect(created.status(), `create service ${slug}`).toBe(201);
    return (await created.json()).id as string;
  }

  test("phases: the domain order, the identical replay, the refusals, the typed actor, and the decision link", async ({ page }) => {
    // The helpers fetch RELATIVE urls from inside the page; land on the app's origin first.
    await page.goto("/services");
    const { projectID } = await ensureE2EWorkspace(page);
    const svcID = await createService(page, projectID, `${SLUG}-phases`);
    const changes = `/api/v1/projects/${projectID}/services/${svcID}/changes`;

    const me = await apiGet(page, "/api/v1/me");
    const adminEmail = me.user.email as string;
    const adminID = me.user.id as string;

    // ── Identity 1 (github-actions/run-1, deploy): started → succeeded, then every replay shape.
    const startedAt = minutesAgo(30);
    const succeededAt = minutesAgo(20);
    const succeededBody = {
      kind: "deploy", phase: "succeeded", source: "github-actions", external_id: "run-1",
      ref: "v1.0.0", url: "https://ci.example.com/run/1", occurred_at: succeededAt,
    };

    const started = await apiSend(page, "post", changes, {
      kind: "deploy", phase: "started", source: "github-actions", external_id: "run-1", occurred_at: startedAt,
    });
    expect(started.status(), await started.text()).toBe(201);
    const startedRec = await started.json();
    expect(startedRec.replayed).toBe(false);
    expect(startedRec.change.id).toMatch(UUID_RE);
    expect(startedRec.change.service_id).toBe(svcID);
    expect(startedRec.change.source).toBe("github-actions");
    expect(startedRec.change.external_id).toBe("run-1");
    expect(startedRec.change.kind).toBe("deploy");
    expect(startedRec.change.phase).toBe("started");
    expect(new Date(startedRec.change.occurred_at).getTime(), "occurred_at is stored as sent").toBe(new Date(startedAt).getTime());
    // The actor is the principal, server-derived and typed (D5): the session admin.
    expect(startedRec.change.actor_label, "the actor is the session admin's label").toBe(adminEmail);
    expect(startedRec.change.actor_user_id, "a person's typed actor id").toBe(adminID);
    expect(startedRec.change.via_token).toBe(false);
    expect("decision_id" in startedRec.change, "decision_id is absent when none was given").toBe(false);
    expect(startedRec.change.recorded_at, "the row names when it was recorded").toBeTruthy();

    const succeeded = await apiSend(page, "post", changes, succeededBody);
    expect(succeeded.status(), await succeeded.text()).toBe(201);
    const succeededRec = await succeeded.json();
    expect(succeededRec.replayed).toBe(false);
    expect(succeededRec.change.ref).toBe("v1.0.0");
    expect(succeededRec.change.url).toBe("https://ci.example.com/run/1");

    // An IDENTICAL replay is 200 with the ORIGINAL row — same id, same recorded_at (D3,
    // invariant 3): a pipeline retry is not an error and writes nothing.
    const replay = await apiSend(page, "post", changes, succeededBody);
    expect(replay.status(), await replay.text()).toBe(200);
    const replayRec = await replay.json();
    expect(replayRec.replayed).toBe(true);
    expect(replayRec.change.id, "the replay answers with the SAME row").toBe(succeededRec.change.id);
    expect(new Date(replayRec.change.recorded_at).getTime(), "original recorded_at, not a new write")
      .toBe(new Date(succeededRec.change.recorded_at).getTime());

    // A second terminal is 409 `phase_order` naming the terminal already recorded (D3).
    const secondTerminal = await apiSend(page, "post", changes, { ...succeededBody, phase: "failed" });
    expect(secondTerminal.status()).toBe(409);
    expect((await secondTerminal.json()).error).toBe("phase_order (phase): succeeded already recorded");

    // A replay with a different body is 409 `phase_exists` NAMING the differing field (D3).
    const refDiffers = await apiSend(page, "post", changes, { ...succeededBody, ref: "v1.0.1" });
    expect(refDiffers.status()).toBe(409);
    expect((await refDiffers.json()).error).toBe("phase_exists (ref): succeeded is already recorded with a different ref");
    const atDiffers = await apiSend(page, "post", changes, { ...succeededBody, occurred_at: minutesAgo(19) });
    expect(atDiffers.status()).toBe(409);
    expect((await atDiffers.json()).error).toBe("phase_exists (occurred_at): succeeded is already recorded with a different occurred_at");

    // ── Identity 2 (spinnaker/roll-2): a group has ONE kind — a phase of another kind is 409
    // `kind_mismatch`, and the timeline below proves the refusal wrote nothing.
    const rollbackStarted = await apiSend(page, "post", changes, {
      kind: "rollback", phase: "started", source: "spinnaker", external_id: "roll-2", occurred_at: minutesAgo(15),
    });
    expect(rollbackStarted.status(), await rollbackStarted.text()).toBe(201);
    const kindMismatch = await apiSend(page, "post", changes, {
      kind: "deploy", phase: "succeeded", source: "spinnaker", external_id: "roll-2", occurred_at: minutesAgo(14),
    });
    expect(kindMismatch.status()).toBe(409);
    expect((await kindMismatch.json()).error).toBe("kind_mismatch (kind): this change is a rollback; a deploy phase cannot join it");

    // ── The 400 vocabulary of D2: strict body, closed enums, bounded scalars.
    const withActor = await apiSend(page, "post", changes, {
      kind: "deploy", phase: "started", source: "e2e-ci", external_id: "bad-1", actor: "me",
    });
    expect(withActor.status(), "the body carries no actor field (D5)").toBe(400);
    expect((await withActor.json()).error).toBe("actor: unknown field");

    const httpURL = await apiSend(page, "post", changes, {
      kind: "deploy", phase: "succeeded", source: "e2e-ci", external_id: "bad-2", url: "http://ci.example.com/run/2",
    });
    expect(httpURL.status(), "http:// is refused — the UI renders url as a link").toBe(400);
    expect((await httpURL.json()).error).toContain("url_invalid");

    const badSource = await apiSend(page, "post", changes, {
      kind: "deploy", phase: "succeeded", source: "Deploy_Bot", external_id: "bad-3",
    });
    expect(badSource.status(), "source is a lower-case slug").toBe(400);
    expect((await badSource.json()).error).toContain("source_invalid");

    const tooOld = await apiSend(page, "post", changes, {
      kind: "deploy", phase: "succeeded", source: "e2e-ci", external_id: "bad-4",
      occurred_at: new Date(Date.now() - 25 * 3600e3).toISOString(),
    });
    expect(tooOld.status(), "occurred_at may lag the clock by change.max_past (24h) at most").toBe(400);
    expect((await tooOld.json()).error).toContain("occurred_at_out_of_bounds");

    const missingField = await apiSend(page, "post", changes, {
      kind: "deploy", phase: "succeeded", source: "e2e-ci",
    });
    expect(missingField.status(), "a missing required field is 400 naming it").toBe(400);
    expect((await missingField.json()).error).toBe("external_id: is required");

    // ── Identity 3 (e2e-ci/nfc-3): the transport normalizes — NFC + trim (D2, invariant 23).
    // `ref` arrives DECOMPOSED with a trailing space; the stored canonical form is composed.
    const nfcAt = minutesAgo(10);
    const nfc = await apiSend(page, "post", changes, {
      kind: "deploy", phase: "succeeded", source: "e2e-ci", external_id: "nfc-3",
      ref: "café ", occurred_at: nfcAt,
    });
    expect(nfc.status(), await nfc.text()).toBe(201);
    const nfcRec = await nfc.json();
    expect(nfcRec.change.ref, "the record echoes the composed, trimmed canonical form").toBe("café");

    // The COMPOSED spelling at the stored instant is the same statement: an identical replay.
    const nfcReplay = await apiSend(page, "post", changes, {
      kind: "deploy", phase: "succeeded", source: "e2e-ci", external_id: "nfc-3",
      ref: "café", occurred_at: nfcAt,
    });
    expect(nfcReplay.status(), "the composed spelling replays as identical").toBe(200);
    expect((await nfcReplay.json()).change.id).toBe(nfcRec.change.id);

    // A terminal alone is a legal group (D3) — but `started` may not follow it.
    const lateStart = await apiSend(page, "post", changes, {
      kind: "deploy", phase: "started", source: "e2e-ci", external_id: "nfc-3", occurred_at: minutesAgo(12),
    });
    expect(lateStart.status()).toBe(409);
    expect((await lateStart.json()).error).toBe("phase_order (phase): started cannot follow succeeded");

    // ── D11: a decision_id must be a ledger row of THIS service in THIS project.
    const bogusDecision = await apiSend(page, "post", changes, {
      kind: "deploy", phase: "succeeded", source: "e2e-ci", external_id: "dec-bad",
      decision_id: randomUUID(), occurred_at: minutesAgo(6),
    });
    expect(bogusDecision.status()).toBe(400);
    expect((await bogusDecision.json()).error).toContain("decision_unknown");

    // A REAL ledger row: NOT_CONFIGURED needs no policy — one POST …/gate on the unconfigured
    // service is a decision like any other (FR-024 invariant 2), and D11 accepts its id.
    const gate = await apiSend(page, "post", `/api/v1/projects/${projectID}/services/${svcID}/gate`);
    expect(gate.status(), await gate.text()).toBe(200);
    const decision = await gate.json();
    expect(decision.state).toBe("NOT_CONFIGURED");
    expect(decision.decision_id).toMatch(UUID_RE);

    const withDecision = await apiSend(page, "post", changes, {
      kind: "deploy", phase: "succeeded", source: "e2e-ci", external_id: "dec-4",
      ref: "v2.0.0", decision_id: decision.decision_id, occurred_at: minutesAgo(5),
    });
    expect(withDecision.status(), await withDecision.text()).toBe(201);
    expect((await withDecision.json()).change.decision_id).toBe(decision.decision_id);

    // ── The timeline over everything above (D6, D11): groups newest by their latest phase,
    // phases nested in the domain order, `incidents` always an array, `decision` absent where
    // none — and the two refusals (kind_mismatch, late started) wrote NOTHING.
    const timeline = await apiGet(page, `${changes}?from=${minutesAgo(120)}&to=${minutesAhead(5)}`);
    expect(timeline.items.map((g: any) => `${g.source}/${g.external_id}`), "groups newest by latest_occurred_at").toEqual([
      "e2e-ci/dec-4",      // −5m
      "e2e-ci/nfc-3",      // −10m
      "spinnaker/roll-2",  // −15m
      "github-actions/run-1", // −20m
    ]);
    expect(timeline.next_cursor, "one page: the cursor is null, not absent").toBeNull();

    const [dec4, nfc3, roll2, run1] = timeline.items;
    expect(run1.kind).toBe("deploy");
    expect(run1.phases.map((p: any) => p.phase), "phases in the domain order").toEqual(["started", "succeeded"]);
    expect(run1.ref, "the group's ref is the LATEST phase's").toBe("v1.0.0");
    expect(run1.url).toBe("https://ci.example.com/run/1");
    expect(new Date(run1.latest_occurred_at).getTime()).toBe(new Date(succeededAt).getTime());

    expect(roll2.kind).toBe("rollback");
    expect(roll2.phases.map((p: any) => p.phase), "the kind_mismatch refusal wrote nothing").toEqual(["started"]);

    expect(nfc3.ref, "the canonical composed ref, read back").toBe("café");
    expect(nfc3.phases.map((p: any) => p.phase), "the late started wrote nothing").toEqual(["succeeded"]);

    // The decision link, read back live (D11): state and `overridden`, NO `action` for a
    // NOT_CONFIGURED decision (the gate's own response shape), and not aged out.
    expect(dec4.decision, "the group that rested on a decision carries the link").toBeTruthy();
    expect(dec4.decision.decision_id).toBe(decision.decision_id);
    expect(dec4.decision.state).toBe("NOT_CONFIGURED");
    expect(dec4.decision.overridden).toBe(false);
    expect("action" in dec4.decision, "NOT_CONFIGURED carries no action").toBe(false);
    expect("aged_out" in dec4.decision, "a live ledger row is not aged out").toBe(false);

    for (const g of timeline.items) {
      expect(Array.isArray(g.incidents), `incidents is ALWAYS an array on ${g.external_id}`).toBe(true);
      expect(g.incidents, "no incident preceded — the array is empty, not absent").toEqual([]);
      if (g !== dec4) {
        expect("decision" in g, `decision absent where none was given (${g.external_id})`).toBe(false);
      }
      for (const p of g.phases) {
        expect(p.actor_label, "every phase names its actor").toBeTruthy();
        expect("actor_user_id" in p, "actor_user_id is present-and-typed on every phase").toBe(true);
      }
    }
  });

  test("the timeline: grouping, order, filters before the limit, the cursor, the bounds; the comparison and its withholding", async ({ page }) => {
    await page.goto("/services");
    const { projectID } = await ensureE2EWorkspace(page);
    const svcID = await createService(page, projectID, `${SLUG}-timeline`);
    const changes = `/api/v1/projects/${projectID}/services/${svcID}/changes`;
    const record = async (body: Record<string, unknown>) => {
      const r = await apiSend(page, "post", changes, body);
      expect(r.status(), await r.text()).toBe(201);
      return (await r.json()).change;
    };

    // Four groups, distinct sources, three kinds. Newest by latest phase: D, C, B, A.
    await record({ kind: "deploy", phase: "started", source: "github-actions", external_id: "run-a", occurred_at: minutesAgo(200) });
    await record({ kind: "deploy", phase: "succeeded", source: "github-actions", external_id: "run-a", ref: "va", occurred_at: minutesAgo(180) });
    await record({ kind: "deploy", phase: "succeeded", source: "jenkins", external_id: "build-b", occurred_at: minutesAgo(120) });
    await record({ kind: "flag", phase: "succeeded", source: "argo", external_id: "flip-c", occurred_at: minutesAgo(60) });
    const dOccurredAt = minutesAgo(30);
    const dTerminal = await record({ kind: "rollback", phase: "succeeded", source: "spinnaker", external_id: "roll-d", ref: "rd", occurred_at: dOccurredAt });

    const from = minutesAgo(240);
    const to = minutesAhead(5);
    const range = `from=${from}&to=${to}`;
    const identities = (body: any) => body.items.map((g: any) => g.external_id);

    // ── Order and the group key (D6): newest first by the group's LATEST phase.
    const all = await apiGet(page, `${changes}?${range}`);
    expect(identities(all)).toEqual(["roll-d", "flip-c", "build-b", "run-a"]);
    expect(all.items[0].ref, "the group ref is the latest phase's").toBe("rd");
    expect(all.next_cursor).toBeNull();

    // ── Filters are applied BEFORE the limit: `kind` is a repeatable OR-set, `source` one slug.
    const rollbacks = await apiGet(page, `${changes}?${range}&kind=rollback`);
    expect(identities(rollbacks)).toEqual(["roll-d"]);
    const deployOrFlag = await apiGet(page, `${changes}?${range}&kind=deploy&kind=flag`);
    expect(identities(deployOrFlag)).toEqual(["flip-c", "build-b", "run-a"]);
    const argo = await apiGet(page, `${changes}?${range}&source=argo`);
    expect(identities(argo)).toEqual(["flip-c"]);
    const noSuchSource = await apiGet(page, `${changes}?${range}&source=no-such-source`);
    expect(noSuchSource.items, "an unknown source is an empty page, never an error").toEqual([]);

    // Filter + limit: two of the three matches, a cursor, then the third — the filter ran first.
    const filteredPage1 = await apiGet(page, `${changes}?${range}&kind=deploy&kind=flag&limit=2`);
    expect(identities(filteredPage1)).toEqual(["flip-c", "build-b"]);
    expect(filteredPage1.next_cursor).toBeTruthy();
    const filteredPage2 = await apiGet(page, `${changes}?${range}&kind=deploy&kind=flag&limit=2&cursor=${encodeURIComponent(filteredPage1.next_cursor)}`);
    expect(identities(filteredPage2)).toEqual(["run-a"]);
    expect(filteredPage2.next_cursor).toBeNull();

    // ── A group selected by its terminal keeps a `started` that precedes `from` (D6): run-a's
    // started (−200m) is outside [−190m, to) but its latest (−180m) is inside — BOTH phases come.
    const partialFrom = await apiGet(page, `${changes}?from=${minutesAgo(190)}&to=${to}`);
    const runA = partialFrom.items.find((g: any) => g.external_id === "run-a");
    expect(runA, "the group is selected by its latest phase").toBeTruthy();
    expect(runA.phases.map((p: any) => p.phase), "ALL phases nested, the early started included").toEqual(["started", "succeeded"]);

    // ── The bounds vocabulary (D6): each refusal is its one closed code.
    const toMs = new Date(to).getTime();
    const ok92 = await page.request.get(`${changes}?from=${new Date(toMs - 92 * 24 * 3600e3).toISOString()}&to=${to}`);
    expect(ok92.status(), "92 days — a quarter — is accepted").toBe(200);
    const wide93 = await page.request.get(`${changes}?from=${new Date(toMs - 93 * 24 * 3600e3).toISOString()}&to=${to}`);
    expect(wide93.status()).toBe(400);
    expect((await wide93.json()).error).toBe("range_too_wide");
    const noRange = await page.request.get(changes);
    expect(noRange.status(), "the range is explicit, never defaulted").toBe(400);
    expect((await noRange.json()).error).toBe("range_required");
    const halfRange = await page.request.get(`${changes}?from=${from}`);
    expect(halfRange.status()).toBe(400);
    expect((await halfRange.json()).error).toBe("range_required");
    const inverted = await page.request.get(`${changes}?from=${to}&to=${from}`);
    expect(inverted.status()).toBe(400);
    expect((await inverted.json()).error).toBe("range_invalid");
    const limitZero = await page.request.get(`${changes}?${range}&limit=0`);
    expect(limitZero.status()).toBe(400);
    expect((await limitZero.json()).error).toBe("limit_invalid");
    const hotfix = await page.request.get(`${changes}?${range}&kind=hotfix`);
    expect(hotfix.status(), "the kind enum is closed").toBe(400);
    expect((await hotfix.json()).error).toContain("kind_invalid");
    const badCursor = await page.request.get(`${changes}?${range}&cursor=garbage`);
    expect(badCursor.status()).toBe(400);
    expect((await badCursor.json()).error).toBe("cursor_invalid");

    // ── The cursor pages in GROUPS without a duplicate; the final cursor is null (D6).
    const page1 = await apiGet(page, `${changes}?${range}&limit=2`);
    expect(identities(page1)).toEqual(["roll-d", "flip-c"]);
    expect(page1.next_cursor).toBeTruthy();
    const page2 = await apiGet(page, `${changes}?${range}&limit=2&cursor=${encodeURIComponent(page1.next_cursor)}`);
    expect(identities(page2)).toEqual(["build-b", "run-a"]);
    expect(page2.next_cursor, "the last page's cursor is null").toBeNull();
    const seen = [...identities(page1), ...identities(page2)];
    expect(new Set(seen).size, "no page holds a duplicate group").toBe(4);

    // ── The comparison (D8, D-0211): the terminal phase fixes T, floored to the canonical
    // bucket; sealed_through is PRESENT and null on a never-sealed service; both sides are
    // withheld `no_facts` — exactly one shape each, no partial figure, no delta.
    const compare = `${changes}/compare`;
    const cmpRes = await page.request.get(`${compare}?source=spinnaker&external_id=roll-d`);
    expect(cmpRes.status(), await cmpRes.text()).toBe(200);
    const cmp = await cmpRes.json();
    expect(cmp.source).toBe("spinnaker");
    expect(cmp.external_id).toBe("roll-d");
    expect(cmp.kind).toBe("rollback");
    expect(cmp.ref).toBe("rd");
    expect(cmp.change_id, "the comparison rests on the terminal row").toBe(dTerminal.id);
    expect(cmp.terminal_phase).toBe("succeeded");
    const tMs = new Date(cmp.t).getTime();
    expect(tMs, "T is the terminal occurred_at floored to the minute").toBe(Math.floor(new Date(dOccurredAt).getTime() / 60_000) * 60_000);
    expect(cmp.horizon).toBe("1h");
    expect("sealed_through" in cmp, "sealed_through is present, never absent").toBe(true);
    expect(cmp.sealed_through, "a never-sealed service states null").toBeNull();
    expect(cmp.as_of, "the snapshot instant is stated").toBeTruthy();
    expect(new Date(cmp.before.from).getTime(), "before is [T − h, T)").toBe(tMs - 3600e3);
    expect(new Date(cmp.before.to).getTime()).toBe(tMs);
    expect(new Date(cmp.after.from).getTime(), "after is [T, T + h)").toBe(tMs);
    expect(new Date(cmp.after.to).getTime()).toBe(tMs + 3600e3);
    expectOneSideShape(cmp.before, "before");
    expectOneSideShape(cmp.after, "after");
    expect(cmp.before.withheld, "no sealed bucket anywhere: no_facts, not pending (the seal is null)").toBe("no_facts");
    expect(cmp.after.withheld).toBe("no_facts");
    expect("delta" in cmp, "delta only when both sides are figures").toBe(false);

    // Two reads are equal apart from the snapshot instant (D8: nothing stored, nothing cached).
    const cmpAgainRes = await page.request.get(`${compare}?source=spinnaker&external_id=roll-d`);
    expect(cmpAgainRes.status()).toBe(200);
    const cmpAgain = await cmpAgainRes.json();
    const { as_of: _a, ...cmpRest } = cmp;
    const { as_of: _b, ...cmpAgainRest } = cmpAgain;
    expect(cmpAgainRest, "the comparison is a pure read").toEqual(cmpRest);

    // A narrower horizon narrows the windows in the same shape.
    const cmp15 = await apiGet(page, `${compare}?source=spinnaker&external_id=roll-d&horizon=15m`);
    expect(cmp15.horizon).toBe("15m");
    expect(new Date(cmp15.after.to).getTime() - new Date(cmp15.after.from).getTime()).toBe(15 * 60_000);
    expect(new Date(cmp15.before.to).getTime() - new Date(cmp15.before.from).getTime()).toBe(15 * 60_000);

    // The horizon vocabulary is closed (D8: 15m | 1h | 6h | 24h).
    const badHorizon = await page.request.get(`${compare}?source=spinnaker&external_id=roll-d&horizon=2h`);
    expect(badHorizon.status()).toBe(400);
    expect((await badHorizon.json()).error).toContain("horizon_invalid");

    // A change whose only phase is `started` has no comparison yet (404 no_terminal_phase).
    await record({ kind: "deploy", phase: "started", source: "e2e-ci", external_id: "run-e", occurred_at: minutesAgo(10) });
    const startedOnly = await page.request.get(`${compare}?source=e2e-ci&external_id=run-e`);
    expect(startedOnly.status()).toBe(404);
    expect((await startedOnly.json()).error).toContain("no_terminal_phase");

    // An unknown identity is 404; a half identity is 400 naming the missing field's rule.
    const unknownIdentity = await page.request.get(`${compare}?source=spinnaker&external_id=nope`);
    expect(unknownIdentity.status()).toBe(404);
    expect((await unknownIdentity.json()).error).toBe("not found");
    const noSource = await page.request.get(`${compare}?external_id=roll-d`);
    expect(noSource.status()).toBe(400);
    expect((await noSource.json()).error).toContain("source_invalid");
    const noExternalID = await page.request.get(`${compare}?source=spinnaker`);
    expect(noExternalID.status()).toBe(400);
    expect((await noExternalID.json()).error).toContain("external_id_invalid");
  });

  test("tenant isolation on record/timeline/compare/incident-changes, and the token actions allow-list", async ({ page }) => {
    await page.goto("/services");
    const { orgID, projectID } = await ensureE2EWorkspace(page);
    const svcID = await createService(page, projectID, `${SLUG}-tenant`);
    const base = `/api/v1/projects/${projectID}/services`;
    const changes = `${base}/${svcID}/changes`;
    const unknown = randomUUID();
    const range = `from=${minutesAgo(60)}&to=${minutesAhead(5)}`;
    const validBody = (id: string) => ({
      kind: "deploy", phase: "succeeded", source: "e2e-ci", external_id: id, occurred_at: minutesAgo(5),
    });

    // ── Malformed service id: 400 naming the format, BEFORE the store is asked — on all three.
    const malformedRecord = await apiSend(page, "post", `${base}/not-a-uuid/changes`, validBody("m-1"));
    expect(malformedRecord.status(), "MALFORMED record").toBe(400);
    expect((await malformedRecord.json()).error).toContain("UUID");
    const malformedTimeline = await page.request.get(`${base}/not-a-uuid/changes?${range}`);
    expect(malformedTimeline.status(), "MALFORMED timeline").toBe(400);
    expect((await malformedTimeline.json()).error).toContain("UUID");
    const malformedCompare = await page.request.get(`${base}/not-a-uuid/changes/compare?source=e2e-ci&external_id=m-1`);
    expect(malformedCompare.status(), "MALFORMED compare").toBe(400);
    expect((await malformedCompare.json()).error).toContain("UUID");

    // ── UNKNOWN: a well-formed id no service anywhere has — the tenant 404, existence hidden.
    const unknownRecord = await apiSend(page, "post", `${base}/${unknown}/changes`, validBody("u-1"));
    expect(unknownRecord.status(), "UNKNOWN record").toBe(404);
    expect((await unknownRecord.json()).error).toBe("not found");
    const unknownTimeline = await page.request.get(`${base}/${unknown}/changes?${range}`);
    expect(unknownTimeline.status(), "UNKNOWN timeline").toBe(404);
    expect((await unknownTimeline.json()).error).toBe("not found");
    const unknownCompare = await page.request.get(`${base}/${unknown}/changes/compare?source=e2e-ci&external_id=u-1`);
    expect(unknownCompare.status(), "UNKNOWN compare").toBe(404);
    expect((await unknownCompare.json()).error).toBe("not found");

    // ── FOREIGN: a second `e2e-` project holding a REAL service with a real timeline. Reached
    // through the FIRST project's path every route is the same 404 as UNKNOWN — and the foreign
    // timeline is BYTE-equal before and after the refusals.
    const otherSlug = `${SLUG}-other-${Date.now()}`;
    const otherRes = await apiSend(page, "post", `/api/v1/organizations/${orgID}/projects`, { slug: otherSlug, name: "E2E Change Other" });
    expect(otherRes.status(), await otherRes.text()).toBe(201);
    const otherID = (await otherRes.json()).id as string;
    try {
      const foreignSvc = await createService(page, otherID, `${SLUG}-foreign`);
      const foreignChanges = `/api/v1/projects/${otherID}/services/${foreignSvc}/changes`;
      const foreignStarted = await apiSend(page, "post", foreignChanges, {
        kind: "deploy", phase: "started", source: "e2e-ci", external_id: "f-1", occurred_at: minutesAgo(20),
      });
      expect(foreignStarted.status(), await foreignStarted.text()).toBe(201);
      const foreignSucceeded = await apiSend(page, "post", foreignChanges, {
        kind: "deploy", phase: "succeeded", source: "e2e-ci", external_id: "f-1", ref: "vf", occurred_at: minutesAgo(10),
      });
      expect(foreignSucceeded.status(), await foreignSucceeded.text()).toBe(201);

      const foreignRange = `from=${minutesAgo(60)}&to=${minutesAhead(5)}`; // frozen for both reads
      const beforeText = await (await page.request.get(`${foreignChanges}?${foreignRange}`)).text();
      expect(beforeText, "the foreign timeline holds its group").toContain("vf");

      const foreignRecord = await apiSend(page, "post", `${base}/${foreignSvc}/changes`, validBody("f-x"));
      expect(foreignRecord.status(), "FOREIGN record through the first project's path").toBe(404);
      expect((await foreignRecord.json()).error).toBe("not found");
      const foreignTimeline = await page.request.get(`${base}/${foreignSvc}/changes?${range}`);
      expect(foreignTimeline.status(), "FOREIGN timeline").toBe(404);
      expect((await foreignTimeline.json()).error).toBe("not found");
      const foreignCompare = await page.request.get(`${base}/${foreignSvc}/changes/compare?source=e2e-ci&external_id=f-1`);
      expect(foreignCompare.status(), "FOREIGN compare").toBe(404);
      expect((await foreignCompare.json()).error).toBe("not found");

      const afterText = await (await page.request.get(`${foreignChanges}?${foreignRange}`)).text();
      expect(afterText, "FOREIGN refusals changed nothing — byte-equal timeline").toBe(beforeText);

      // …and none of the refusals recorded anything on the FIRST project's own service either.
      const ownTimeline = await apiGet(page, `${changes}?${range}`);
      expect(ownTimeline.items, "the first project's timeline is still empty").toEqual([]);
    } finally {
      const del = await apiSend(page, "delete", `/api/v1/projects/${otherID}`);
      expect([200, 204], `delete second project -> ${del.status()}`).toContain(del.status());
    }

    // The incident side of the link table has the same transport discipline (D7, invariant 9).
    const malformedIncident = await page.request.get(`/api/v1/projects/${projectID}/incidents/not-a-uuid/changes`);
    expect(malformedIncident.status(), "MALFORMED incident id").toBe(400);
    expect((await malformedIncident.json()).error).toContain("UUID");
    const unknownIncident = await page.request.get(`/api/v1/projects/${projectID}/incidents/${unknown}/changes`);
    expect(unknownIncident.status(), "UNKNOWN incident id").toBe(404);
    expect((await unknownIncident.json()).error).toBe("not found");

    // ── The token allow-list (D12, D-0212). Creation validates the list against the central
    // catalogue AND the token's own role, so the mistake surfaces at the form, not at the
    // pipeline's first 403.
    const tokens = `/api/v1/organizations/${orgID}/tokens`;
    const ciName = "e2e-change-ci-bot";
    const ciRes = await apiSend(page, "post", tokens, {
      name: ciName, role: "editor", project_id: projectID, actions: ["gate:evaluate", "change:record"],
    });
    expect(ciRes.status(), await ciRes.text()).toBe(201);
    const ci = await ciRes.json();
    const ciSecret = ci.token as string;
    expect(ciSecret, "the plaintext secret is returned once").toBeTruthy();
    expect(ci.api_token.actions, "the create response echoes the allow-list").toEqual(["gate:evaluate", "change:record"]);

    const unknownAction = await apiSend(page, "post", tokens, {
      name: "e2e-change-bad-1", role: "editor", project_id: projectID, actions: ["not:an:action"],
    });
    expect(unknownAction.status(), "an entry outside the catalogue is refused").toBe(400);
    expect((await unknownAction.json()).error).toContain("action_unknown");

    const notGranted = await apiSend(page, "post", tokens, {
      name: "e2e-change-bad-2", role: "editor", project_id: projectID, actions: ["gate:override"],
    });
    expect(notGranted.status(), "an entry the role does not grant is refused at create (D-0212)").toBe(400);
    expect((await notGranted.json()).error).toContain("action_not_granted");

    const viewerNotGranted = await apiSend(page, "post", tokens, {
      name: "e2e-change-bad-3", role: "viewer", project_id: projectID, actions: ["change:record"],
    });
    expect(viewerNotGranted.status(), "change:record is editor+ — a viewer list may not carry it").toBe(400);
    expect((await viewerNotGranted.json()).error).toContain("action_not_granted");

    // No list: `actions` is an explicit null — the role decides, exactly as before FR-025.
    const viewerRes = await apiSend(page, "post", tokens, {
      name: "e2e-change-viewer", role: "viewer", project_id: projectID,
    });
    expect(viewerRes.status(), await viewerRes.text()).toBe(201);
    const viewer = await viewerRes.json();
    expect(viewer.api_token.actions, "no list means null on create").toBeNull();

    const listed = await apiGet(page, tokens);
    const ciListed = listed.find((t: any) => t.name === ciName);
    const viewerListed = listed.find((t: any) => t.name === "e2e-change-viewer");
    expect(ciListed.actions, "the read model carries the immutable list").toEqual(["gate:evaluate", "change:record"]);
    expect(viewerListed.actions, "…and an explicit null for an unrestricted token").toBeNull();

    // ── The CI token as a bearer, in a context with an EXPLICITLY EMPTY storage state — the
    // shared admin session cookie would otherwise ride along and beat the Authorization header.
    const ciCtx = await request.newContext({
      baseURL: BASE_URL,
      storageState: { cookies: [], origins: [] },
      extraHTTPHeaders: { Authorization: `Bearer ${ciSecret}` },
    });
    let ciChangeID = "";
    const ciBody = {
      kind: "deploy", phase: "succeeded", source: "e2e-ci", external_id: "ci-run-1",
      ref: "v9", occurred_at: minutesAgo(5),
    };
    try {
      // The token records: 201, and the row names the pipeline for as long as it exists (D5).
      const ciRecord = await ciCtx.post(changes, { data: ciBody });
      expect(ciRecord.status(), await ciRecord.text()).toBe(201);
      const ciRec = await ciRecord.json();
      expect(ciRec.change.actor_label, "a token's actor is token:<name>").toBe(`token:${ciName}`);
      expect(ciRec.change.via_token).toBe(true);
      expect(ciRec.change.actor_user_id, "a machine identity has no user id — null, not absent").toBeNull();
      ciChangeID = ciRec.change.id;

      // gate:evaluate is on the list: the gate answers the pipeline (200, NOT 403).
      const ciGate = await ciCtx.post(`${base}/${svcID}/gate`);
      expect(ciGate.status(), "the CI token may ask the gate").toBe(200);

      // Everything OUTSIDE the list is 403 — not 404, because visibility is membership, not
      // action (D-0212): the project is visible, the action is not granted.
      const forbiddenReads: [string, string][] = [
        [`/api/v1/projects/${projectID}/services`, "GET services"],
        [`${changes}?${range}`, "the timeline"],
        [`${changes}/compare?source=e2e-ci&external_id=ci-run-1`, "the comparison"],
        [`/api/v1/projects/${projectID}/incidents/${unknown}/changes`, "incident changes"],
      ];
      for (const [path, what] of forbiddenReads) {
        const r = await ciCtx.get(path);
        expect(r.status(), `CI token on ${what} -> 403, not 404`).toBe(403);
        expect((await r.json()).error).toBe("forbidden");
      }
      const ciPolicy = await ciCtx.put(`${base}/${svcID}/gate/policy`, { data: { expected_revision: null } });
      expect(ciPolicy.status(), "gate:policy:write is not on the list").toBe(403);
      expect((await ciPolicy.json()).error).toBe("forbidden");
    } finally {
      await ciCtx.dispose();
    }

    // The admin's IDENTICAL replay of the token's phase: 200 with the ORIGINAL row — the
    // original `token:` actor, not the admin (D3: server-derived fields take no part).
    const adminReplay = await apiSend(page, "post", changes, ciBody);
    expect(adminReplay.status(), await adminReplay.text()).toBe(200);
    const adminReplayRec = await adminReplay.json();
    expect(adminReplayRec.replayed).toBe(true);
    expect(adminReplayRec.change.id).toBe(ciChangeID);
    expect(adminReplayRec.change.actor_label, "the replay keeps the ORIGINAL token actor").toBe(`token:${ciName}`);
    expect(adminReplayRec.change.via_token).toBe(true);
    expect(adminReplayRec.change.actor_user_id).toBeNull();

    // A viewer token WITHOUT a list: the role decides — it reads, it may not record.
    const viewerCtx = await request.newContext({
      baseURL: BASE_URL,
      storageState: { cookies: [], origins: [] },
      extraHTTPHeaders: { Authorization: `Bearer ${viewer.token}` },
    });
    try {
      const viewerServices = await viewerCtx.get(`/api/v1/projects/${projectID}/services`);
      expect(viewerServices.status(), "an unrestricted viewer token reads").toBe(200);
      const viewerTimeline = await viewerCtx.get(`${changes}?${range}`);
      expect(viewerTimeline.status(), "the timeline is project:read (viewer+)").toBe(200);
      const viewerRecord = await viewerCtx.post(changes, { data: validBody("v-1") });
      expect(viewerRecord.status(), "change:record is editor+ — the viewer may not record").toBe(403);
      expect((await viewerRecord.json()).error).toBe("forbidden");
    } finally {
      await viewerCtx.dispose();
    }

    // A bogus secret never authenticates.
    const bogusCtx = await request.newContext({
      baseURL: BASE_URL,
      storageState: { cookies: [], origins: [] },
      extraHTTPHeaders: { Authorization: `Bearer cbx_${"0".repeat(48)}` },
    });
    try {
      expect((await bogusCtx.get(`/api/v1/projects/${projectID}/services`)).status(), "bogus secret").toBe(401);
    } finally {
      await bogusCtx.dispose();
    }

    // Revocation kills the REAL secret: the same bearer that just recorded is 401 now.
    const revoke = await apiSend(page, "delete", `/api/v1/tokens/${ci.api_token.id}`);
    expect([200, 204], `revoke CI token -> ${revoke.status()}`).toContain(revoke.status());
    const revokedCtx = await request.newContext({
      baseURL: BASE_URL,
      storageState: { cookies: [], origins: [] },
      extraHTTPHeaders: { Authorization: `Bearer ${ciSecret}` },
    });
    try {
      const afterRevoke = await revokedCtx.post(changes, { data: ciBody });
      expect(afterRevoke.status(), "the revoked secret no longer authenticates").toBe(401);
    } finally {
      await revokedCtx.dispose();
    }
  });
});
