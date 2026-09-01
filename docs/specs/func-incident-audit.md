# func-incident-audit — an incident write says who wrote it (FR-026 / NFR-021)

> **Lifecycle: DESIGN — revision 4, awaiting approval.** Opened after iter-0165 from the gap D-0171
> named and iter-0156 §2.8 proved: incident writes carry no audit row for EITHER anchor. The owner
> settled the three questions that size it (2026-09-01): only PRINCIPAL writes are audited, the row is
> written in the mutating transaction, and the read surface stays the organization's existing audit
> listing; the owner also kept D8b inside FR-026, because without it "one mutation → one audit row" is
> false on the receiver path a real Alertmanager drives.
>
> Revision 1 was REJECTED with 1 P0, 5 P1, 2 P2; revision 2 with 4 P1 and 2 P2. Every finding is
> addressed and named where it changed the text. **Revision 1.** P0: `nil` as the machine actor let a
> forgotten wiring produce the exact thing FR-026 forbids, and the matrix enshrined it — the actor is
> now carried by the DOOR (D3). P1: the postmortem writer owns no transaction (D2a); a repeated
> acknowledgement rewrites `updated_at` (D8a); the machine list omitted the `🕸 Impact:` note (D1); a
> concurrent firing answers 500 (D8b). P2: the closed vocabulary is a property of the helper, not of
> the column (D4); invariant 1 said "five paths" for seven route variants. **Revision 2.** P1: D8a's
> regression was pinned to a surface that does not read `updated_at` — the status-page lists order by
> `started_at` and `resolved_at` — so the claim now names the consumer that exists (D8a). P1: D1
> promised writers by name and did not deliver them; `⏸ Suppressed:` alone has TWO independent writers
> and neither is in `incidentcontext.go` (D1). P1: "the loser is silent" is not an observable contract
> — the parallel resolve now fixes the HTTP answer and the counters (D8b). P1: a system door where no
> machine writer exists would widen the unaudited surface for nothing, so acknowledgement and
> postmortem are principal-only (D3). P2: two editorial repeats removed (§4, §8).
>
> **Revision 3.** P1: the `internal/api` guard proves only what is REACHED — a system door declared for
> a writer with no machine caller passes it untouched — so a second guard pins what EXISTS, over the
> declarations of `internal/store` (D3, invariant 4, §7). P2: D1's table said "the seven" while listing
> nine writers.
>
> No requirement row exists in `docs/status.md` yet — FR-026 and NFR-021 get theirs when this design is
> approved, per the rule iter-0164 followed for FR-025.

## 1. What this is, in one paragraph

FR-022 shipped a second incident anchor and promised, as its invariant 14, that "every write is audited
with actor and tenant, in the mutating transaction". Building the discharge map found the promise false
of the PRODUCT rather than merely unimplemented: an incident write leaves **no** `audit_logs` row today,
for a monitor incident or a service one, and what FR-022 actually keeps is the absence of asymmetry
between the two anchors (`TestAServiceIncidentUsesTheSameOperatorSurfacesAsAMonitorOne`). An incident
still records what happened — its own timeline of updates, each with an author — but the one place an
org admin reads for *who did this, from which principal, and when* is `audit_logs`, and incidents are
absent from it. A member can resolve someone else's incident, publish a postmortem or acknowledge a
page, and the organization's audit trail says nothing. FR-026 closes that for writes a **principal**
makes, and states in the requirement itself that machine writes are deliberately out.

It is a thin slice on purpose: no migration, no new route, no new read. It adds rows of an existing
shape to an existing table, from write paths that already own a transaction.

## 2. Requirements

- **FR-026 — an incident write is audited.** Every incident mutation performed by a principal — a
  session user or an API token — writes exactly one `audit_logs` row in the SAME transaction as the
  mutation: creating an incident (manually or through the Alertmanager receiver), changing its status,
  adding a note, acknowledging it, and creating or updating its postmortem. The row carries the
  organization resolved from the incident's project, the actor triple the rest of the product already
  uses (`actor_user_id` NULL for a synthetic token identity, `via_token`), an action from a closed
  vocabulary, and a target naming the incident and what changed. An unaudited principal write is a
  defect, not a degradation.
- **NFR-021 — the trail says what happened, never what was written, and widens nothing.** No note body,
  postmortem body or alert annotation reaches `audit_logs`: a target carries ids, a closed enum and a
  status transition, because the audit read is organization-wide while an incident body may carry
  customer detail. Machine writes are ABSENT by design, named here so their absence is a decision and
  not an oversight. The read surface, its RBAC and the instance/organization split are untouched. And
  no incident behaviour changes: lifecycle, timeline, escalation, notification, status-page projection
  and postmortems are identical before and after, with exactly TWO intentional exceptions, both named
  in D8a and D8b and both making an existing claim true rather than adding behaviour: a repeated
  acknowledgement stops rewriting `updated_at`, and a concurrent duplicate Alertmanager firing is
  ignored instead of answering 500. Anything else that moves is a regression.

## 3. The decisions

**D1 — only principal writes are audited (owner, 2026-09-01).** Audited: the manual create, the
Alertmanager receiver's create and its resolve (both authenticated as a project write with a token —
`internal/api/handlers_alertmanager.go`), the status change and the note (`POST …/incidents/{id}/updates`),
the acknowledgement (`internal/api/handlers_escalation.go`), and the postmortem upsert. Two reasons,
both already precedent in this tree: a machine's work IS recorded — in the incident's own timeline,
which names `system` as the author and is the document an operator reads for it — and a flapping
service would bury an organization's audit log under its own heartbeat, which is exactly why D10 of
`func-reliability-gate.md` keeps gate DECISIONS out of `audit_logs` while auditing gate policy and
override mutations.

The machine set is defined by ENUMERATING ITS WRITERS, not by a family of markers. Revision 1 wrote
"the notes appended through the marker guard", which is not a definition: the `🕸 Impact:` note is a
direct insert, and `⏸ Suppressed:` has two independent writers in two different files. The nine, in
four families:

| # | Writer | File | What it writes |
|---|---|---|---|
| 1 | `CreateIncident` via the reconciler | `internal/ingest/reconciler.go` | the monitor auto-incident and its opening update |
| 2 | `AddIncidentUpdate` via the reconciler | `internal/ingest/reconciler.go` | the monitor auto-resolve |
| 3 | `OpenServiceIncidentTx` | `internal/store/serviceincidents.go` | the service auto-incident |
| 4 | `ResolveServiceIncidentTx` | `internal/store/serviceincidents.go` | the service auto-resolve |
| 5 | `AppendIncidentContext` | `internal/store/incidentcontext.go` | the `⚡ Context:` note |
| 6a | `AppendSuppressionNote` | `internal/store/dependencies.go` | the `⏸ Suppressed:` note, dependency flavour, on the pool |
| 6b | `appendSuppressionNoteTx` | `internal/store/alertdelegation.go` | the `⏸ Suppressed:` note, delegation flavour, in the caller's transaction |
| 7a | the `🚀 Changes:` note | `internal/store/change.go` | FR-025's correlation note |
| 7b | `CorrelateIncident` | `internal/store/serviceimpact.go` | the impact links and their `🕸 Impact:` note, a DIRECT insert |

None of them writes an audit row, and each has its own case in §7 — a writer that grows a sibling
without a case is a gap this table exists to make visible.

**D2 — the row is written inside the mutating transaction.** Not the best-effort `h.audit(...)` helper
of `internal/api/handlers_audit.go`, which runs after the mutation on the pool and only logs its own
failure. The pattern is `insertGateAudit` (`internal/store/gate.go`): a store-level helper taking the
caller's `pgx.Tx`, resolving the organization from the project in the same statement. Each incident
writer owns a transaction — `CreateIncident`, `AddIncidentUpdate` and `AcknowledgeIncident` in
`internal/store/incidents.go` begin one — so the audit insert joins work that is already atomic, and
"audited" becomes a property of the recorded fact rather than an intention. This is the decision that
makes invariant 14's original wording true instead of aspirational.

**D2a — the postmortem writer has no transaction, and FR-026 gives it one (P1, revision 1).** Revision 1
claimed every writer already owned one. `UpsertPostmortem` does not: it is a single `s.pool.QueryRow`
with an `ON CONFLICT DO UPDATE`, and it cannot say whether it created or replaced — which D5's target
needs. FR-026 therefore rewrites it as a transaction that (1) locks the incident row `FOR UPDATE`, the
same serialization point `AddIncidentUpdate` and `AcknowledgeIncident` take, so a postmortem write and a
timeline write cannot interleave; (2) reads whether a postmortem exists UNDER that lock, which is what
makes `created` vs `updated` a fact rather than a guess; (3) upserts; (4) writes the audit row; (5)
commits. A postmortem for an incident that does not exist stays `ErrNotFound`, decided by the lock read
rather than by a foreign-key error.

**D3 — the door carries the actor; there is no value that can be forgotten (P0, revision 1).** Revision
1 passed an actor into one shared writer and read `nil` as "machine". That is a silent bypass: an API
handler that forgets the argument produces exactly what FR-026 forbids, an unaudited principal write,
and the empty-label check cannot tell an omission from a machine. It also cannot be caught by review
reliably, because the two callers of `CreateIncident` and `AddIncidentUpdate` — `internal/api` and
`internal/ingest/reconciler.go` — look identical at the call site.

Each writer is therefore split into two entry points, and the actor lives in the signature of one of
them:

- the PRINCIPAL door takes `store.AuditActor` as a required parameter and always writes the audit row;
- the SYSTEM door takes no actor at all and writes none.

A forgotten actor is then a compile error, not a runtime condition. `store.AuditActor` is built in the
handler exactly as the gate builds its own —
`{ActorUserID: p.AuditUserID(), ViaToken: p.ViaToken, Label: p.AuditActorLabel()}` (`internal/authz/authz.go`)
— and the principal door refuses a zero-valued actor (empty label) before any statement, so a
half-wired construction fails loudly rather than writing an anonymous row. The remaining mistakes the compiler cannot catch need TWO guards, because they are two different
mistakes and one test cannot see both:

- **What is REACHED** — an API handler calling a system door. A test parses the syntax tree of
  `internal/api` and fails if any file there references a system-door writer.
- **What EXISTS** — a system door appearing for a writer that has no machine caller. The test above is
  blind to it: `AcknowledgeIncidentBySystem` declared in `internal/store` and called by nobody passes
  an `internal/api` scan cleanly, and the next handler to want it finds an unaudited door already
  built. A second test therefore parses the DECLARATIONS of `internal/store` and pins the door surface
  exactly: `CreateIncident` and `AddIncidentUpdate` have both doors (four methods); `AcknowledgeIncident`
  and `UpsertPostmortem` have `…ByPrincipal` and no `…BySystem`; and no other incident writer declares
  a `…BySystem` method at all. The set is written out in the test, so widening it is an edit a reviewer
  sees rather than a method that quietly appears (P1, revision 3). Naming
follows the doors, not the actor: `…ByPrincipal` / `…BySystem`.

A system door is created ONLY where a machine writer exists today (P1, revision 2). `CreateIncident`
and `AddIncidentUpdate` are genuinely shared — the reconciler drives both — so both split. Nothing
machine-driven acknowledges an incident or writes a postmortem: `AcknowledgeIncident` and
`UpsertPostmortem` have exactly one caller each, in `internal/api`. They stay PRINCIPAL-ONLY, with no
second door at all, because a system door there would widen the unaudited surface for a caller that
does not exist. A future machine acknowledgement would add its door in the change that adds the
caller, under this requirement's own invariant 4. `store.GateActor` and
`store.SecretActor` are the same shape as `AuditActor` and could later collapse into it; this
requirement does not refactor them.

**D4 — a closed action vocabulary, five words.** `incident.create`, `incident.status`, `incident.note`,
`incident.acknowledge`, `incident.postmortem`. A resolve is not its own action: it is a status change
whose target names the transition, so the vocabulary does not grow a word per state. The closure is a
property of the INCIDENT AUDIT HELPER, not of the column: `audit_logs.action` is free text with no DB
CHECK and already carries `member.add`, `token.create`, `gate.policy.*` and more. The helper takes a
typed constant rather than a string, so no incident path can spell an action the list does not have,
and nothing about other writers changes (P2, revision 1: revision 1 said "no other spelling can be
written", which was a claim about the table and was false).

**D5 — the target is metadata (NFR-021).** Shapes, fixed here so a reader can parse them by eye and a
test can assert them:
- create — `incident <id> · <anchor> · impact=<impact> · source=<source>`, where `<anchor>` is
  `monitor <id>`, `service <id>` or `project-level`;
- status — `incident <id> · <from> → <to>`, both ends read from the row that was locked, never from the
  request body;
- note — `incident <id> · note`, with no excerpt of the note;
- acknowledge — `incident <id> · acknowledged`;
- postmortem — `incident <id> · postmortem created` or `… updated`.
A token principal appends ` · actor=<label>`, because `actor_user_id` is NULL for a synthetic token
identity and the typed half would otherwise read "some token" (the reason `gatePolicyAuditText` carries
the immutable label too).

**D6 — tenancy is resolved, not passed.** `INSERT INTO audit_logs (org_id, …) SELECT p.org_id, … FROM
projects p WHERE p.id = $1`, as `insertGateAudit` does. No `project_id` column and no `incident_id`
column: the organization is the tenant of this table (00018, with `org_id` made nullable in 00047 for
instance actions), and an incident always keeps its project even after its anchor is cleared by a
service deletion — so a project-level incident and an orphaned one both resolve.

**D7 — failure semantics follow from D2, and are stated rather than discovered.** An audit insert error
aborts the mutation: the caller sees 500 and the incident is unchanged. That is the intended trade —
an incident write that cannot be attributed does not happen. It is also why the insert must be cheap and
unconditional: no lookup, no second round trip, one statement against a table with one index.

**D8 — an idempotent no-op audits nothing, and to earn that it must really be a no-op.** An audit row
means a change happened, so a retry must not manufacture history. Revision 1 asserted the property for
two paths that do not have it today, so FR-026 carries the two corrections that make the assertion
true. A requirement may not claim a property its code lacks.

**D8a — a repeated acknowledgement becomes a genuine no-op (P1, revision 1).** `AcknowledgeIncident`
already locks the row and keeps the FIRST acknowledgement through `COALESCE`, but it writes
`updated_at = $3` unconditionally, so every repeat modifies the incident. Under the lock it already
holds, the writer now returns the row unchanged when `acknowledged_at IS NOT NULL`: no `UPDATE`, no
`updated_at` move, no audit row, same 200 and same body as before. This is the first of NFR-021's two
intentional behaviour changes. Revision 2 pinned its regression to the status-page lists, which do not
read `updated_at` at all — the active list orders by `started_at`, the recent one filters and orders by
`resolved_at` (`internal/store/statuspagecontext.go`). The consumer that exists is the incident's own
JSON: `updated_at` is a field of the API's incident representation (`internal/domain/incident.go`), so
the regression is stated there, and the property that must SURVIVE is the one D-0176's lock ordering
bought — an acknowledgement that does change something still stamps forward, never behind a timeline
update it queued behind. The mandatory assertion is the timestamp equality across a repeated
acknowledgement; there is no third surface to guard, and revision 2's claim that there was one is
withdrawn.

**D8b — a concurrent duplicate firing is ignored, not a 500 (P1, revision 1).** The receiver's
"already open — idempotent" branch is a read-then-write: `FindOpenIncidentByExternalKey`, then
`CreateIncident`. Two simultaneous firings of one fingerprint both miss the read, and the loser
violates `incidents_external_key_open_idx` (00029). `CreateIncident` maps only
`incidents_one_open_auto` to `ErrAlreadyOpen`, so the loser today reaches the generic error path and
the caller answers 500 — where the sequential retry answers "ignored". The external-key conflict is
mapped to the same benign `ErrAlreadyOpen`, the receiver counts it as ignored, and neither branch
writes an audit row.

The parallel RESOLVE needs the same treatment and an OBSERVABLE contract, because "the loser is silent"
says nothing a test can read (P1, revision 2). Both requests find the open incident; the winner's
`AddIncidentUpdate` resolves it, the loser's returns `ErrIncidentTerminal` (`internal/store/incidents.go`),
and `resolveExternalIncident` hands every error to `h.serverError`, so the loser answers 500 today. The
contract, stated in the units the receiver already reports: for that alert the winner counts
`resolved=1`, the loser counts `ignored=1` and `resolved=0`, BOTH requests answer HTTP 200 with the
`{opened, resolved, ignored}` body, the loser writes no audit row, and nothing is logged as a server
error — a duplicate delivery is not an incident of its own. `ErrIncidentTerminal` from this path is
therefore mapped to "ignored" in the receiver, and only there: the human route
(`internal/api/handlers_incidents.go`) keeps answering the operator that the incident is already
resolved.

FR-026 carries both because its own invariant depends on them; they are the second of NFR-021's two
intentional behaviour changes.

**D9 — no read is added (owner, 2026-09-01).** Rows appear in the organization listing that exists
(`GET /api/v1/organizations/{id}/audit`, org-manage, `internal/api/handlers_audit.go`) and nowhere else.
An incident-scoped panel and a project-scoped audit read were both considered and declined for this
requirement: the first needs a target index or an incident column, the second needs a `project_id`
column plus its own RBAC — either is a bigger change than the gap being closed. Both stay open as
follow-ups, named in §9 so nobody re-derives the decision.

## 4. What must move WITH the implementation, not after it

- `docs/specs/func-service-incidents.md` — invariant 14's correction says "the audit gap is named as its
  own future requirement"; the sentence gains the number and the state.
- `docs/traceability.md` — the FR-026 discharge map: every invariant of §6 with a test that exists.
- `docs/status.md` — the FR-026 and NFR-021 rows, and the iteration's acceptance criteria.
- `docs/overview.md` — the audit vocabulary gains its incident words, and the two user-visible
  behaviour changes are stated beside the incident description: a repeated acknowledgement no longer
  moves the incident's modification time (D8a), and a duplicate concurrent alert is ignored rather than
  answered 500 (D8b).
- `docs/runbook.md` — how an org admin answers "who resolved this incident", and the note that a machine
  resolve is in the incident's timeline rather than the audit log.

## 5. Schema

**No migration.** `audit_logs` as built by `internal/store/migrations/00018_audit_logs.sql` (and relaxed
by `internal/store/migrations/00047_admin_users.sql`) carries every column this needs; `audit_logs_org_idx`
serves the only read. The requirement adds rows of an existing shape and no read the current index cannot
answer. A spec that needed a column here would be D9's declined follow-up, not this one.

## 6. Acceptance invariants (FR-026)

1. Every principal write produces exactly ONE audit row: five action classes (D4) over seven route
   variants — the manual create, the receiver create, the status change, the note, the acknowledgement,
   the postmortem create and the postmortem update (P2, revision 1: "five paths" named seven things).
2. The row is committed by the same transaction as the mutation: no committed incident change exists
   without its row, and no row exists for a rolled-back change.
3. An audit insert failure aborts the mutation — the caller sees 500 and reads back the unchanged
   incident.
4. A principal write CANNOT reach the store without an actor: the principal door requires it in its
   signature, and a zero-valued actor is refused before any statement (P0, revision 1). Two guards hold
   the door surface, one per mistake: no file in `internal/api` REFERENCES a system door, and the
   declarations of `internal/store` DECLARE exactly the doors D3 names — both for the incident create
   and the timeline update, `…ByPrincipal` only for the acknowledgement and the postmortem, and no
   other `…BySystem` incident writer. A door that exists for a caller that does not is a failure of the
   second guard even though the first one passes (P1, revision 3).
5. `org_id` is the organization of the incident's project, resolved in the insert; an incident with no
   anchor, and one whose service was deleted, both audit to that organization.
6. `actor_user_id` is the principal's user uuid, and NULL exactly when the principal is a synthetic
   token identity; `via_token` is true exactly for a token principal.
7. A machine write produces NO row, for each of the NINE writers named in D1's table — including both
   `⏸ Suppressed:` writers, which live in different files and have different idempotency keys, and
   the `🕸 Impact:` note, which is a direct insert rather than a marker-guarded one.
8. The action is one of the five constants of D4, and the incident audit helper accepts nothing else;
   no claim is made about `audit_logs.action` in general, which stays free text for every other writer.
9. A status target names both ends of the transition, taken from the locked row.
10. No note body, postmortem body or alert annotation appears in any audit row, for any path.
11. The postmortem write is transactional under the incident's row lock, and its target's `created` vs
    `updated` is decided by a read taken under that lock (D2a).
12. A repeated acknowledgement changes nothing: no audit row, no `UPDATE`, and `updated_at` is the
    instant of the FIRST acknowledgement, to the microsecond, in the incident row and in the JSON the
    API returns (D8a). An acknowledgement that DOES change something still stamps forward, never behind
    a timeline update it waited for.
13. A duplicate Alertmanager delivery is ignored whether it is sequential or CONCURRENT, and the
    contract is observable: one incident and one audit row for the pair; both requests answer HTTP 200
    with the `{opened, resolved, ignored}` body; the winner counts `opened=1` (firing) or `resolved=1`
    (resolve) and the loser counts `ignored=1` with `opened=0`/`resolved=0`; the loser writes no audit
    row and logs no server error (D8b). The human resolve route is unaffected: an operator resolving an
    already-resolved incident still gets the answer it gets today.
14. The read surface is unchanged: incident rows appear in the organization listing under the same
    org-manage rule, and never in the instance listing (they carry an organization).
15. Incident behaviour is otherwise unchanged (NFR-021): the pre-FR-026 suite passes untouched —
    lifecycle, timeline ordering, escalation progress, notification payloads, status-page projection
    and postmortems — with D8a and D8b as the only two intentional differences, each covered by its own
    invariant above.
16. Audit rows outlive the incident and its project: deleting a project leaves the organization's trail
    intact, and only deleting the organization removes it (00018's cascade). Deleting the actor user
    leaves the row with a NULL actor and its target text intact.

## 7. Required test matrix (written before the code)

*The doors (P0, and the surface guard of revision 3):* a principal door called with a zero-valued actor
→ refused before any statement, the incident unchanged · a principal door with a label but no user id
and `via_token` false → refused (an attribution that names nobody is not an attribution) · a system door
→ the mutation succeeds and no row exists · **reached:** an AST test over `internal/api` fails if any
file there references a system-door writer · **exists:** an AST test over the declarations of
`internal/store` pins the door surface as a SET — `CreateIncident` and `AddIncidentUpdate` with both
doors, `AcknowledgeIncident` and `UpsertPostmortem` with the principal door only, no other `…BySystem`
incident writer — so a door built ahead of its caller fails here even though the first test passes ·
both guards driven by fixtures that DO contain the violation, so each is known to fail when it should,
the way the FR-024 stale-spelling guard is fixture-tested.

*Store, per path:* create → one row, action `incident.create`, target naming the anchor · the same
create rolled back by a planted failure after the incident insert → no incident and no row · an
injected audit failure → the mutation returns an error and the incident is absent · status change
investigating → resolved → one row naming both ends · note → one row with no fragment of the body,
asserted by planting a marker string in the body and searching every audit row for it · acknowledge →
one row · postmortem create then update → two rows, `created` then `updated`.

*Postmortem transaction (D2a):* the writer holds the incident's row lock — asserted with a competing
timeline write that must serialize behind it, not interleave · `created` vs `updated` decided under the
lock: two concurrent first-writes produce one `created` and one `updated`, never two `created` ·
a postmortem for an unknown incident → `ErrNotFound` from the lock read · a planted failure after the
upsert → no postmortem and no audit row.

*Idempotence (D8a, D8b):* acknowledging twice → one row, and `updated_at` equal to the value read after
the FIRST acknowledgement, to the microsecond, in the row and in the API's JSON · an acknowledgement
that waits behind a timeline update still stamps forward (the property D8a must not break) · the
receiver posting one fingerprint twice sequentially → one incident, one row, `ignored=1` · TWO firings
of one fingerprint in parallel, driven through the handler → one incident, one audit row, one response
with `opened=1` and one with `opened=0, ignored=1`, both HTTP 200, and no server-error log line · TWO
resolves of one fingerprint in parallel → one resolve, one audit row, one response with `resolved=1`
and one with `resolved=0, ignored=1`, both 200, the loser writing nothing · the HUMAN resolve of an
already-resolved incident → unchanged answer, proving the `ErrIncidentTerminal` mapping is scoped to
the receiver · a note that fails validation → no incident change and no row.

*Tenancy and attribution:* an incident in project A audits to A's organization, and a reader of
organization B never sees it · a service-anchored incident whose service is deleted still audits to the
organization · a session user → `actor_user_id` set, `via_token` false · a Cerbix token →
`actor_user_id` NULL, `via_token` true, label in the target · an OIDC client-credentials principal →
a real user uuid, `via_token` true.

*Machine paths, one case per writer of D1's table (nine):* the reconciler's auto-open and its
auto-resolve → the incident exists, its timeline names the system author, `audit_logs` is empty ·
`OpenServiceIncidentTx` and `ResolveServiceIncidentTx` → same · `AppendIncidentContext` writing a
`⚡ Context:` note → same · `AppendSuppressionNote` (dependency flavour, on the pool) → same ·
`appendSuppressionNoteTx` (delegation flavour, in a transaction) → same, as its own case, because the
two are different code with different idempotency keys and a single "⏸ is not audited" test would
cover only one of them · the `🚀 Changes:` note → same · `CorrelateIncident` writing impact links and
their `🕸 Impact:` note → same.

*API level:* every audited route asserted through the handler with a real principal, including that a
viewer's refused write leaves no row (403 before the store), and a foreign project's incident (404)
leaves none either.

*Read:* the organization listing shows the new rows, ordered with the rest by `created_at DESC` and
bounded by the same limit clamp; the instance listing shows none of them.

*Regression:* the FR-022 operator-surface test
(`TestAServiceIncidentUsesTheSameOperatorSurfacesAsAMonitorOne`) and the escalation ladder tests pass
unchanged.

## 8. Threat model

A viewer cannot write, so cannot forge a row; the row is written by the store, never by a client-supplied
field. A token cannot impersonate a user — the attribution comes from the auth layer under D3 and
invariant 6, never from the request body. The audit target is machine-composed from ids and
enums, so a hostile incident title cannot inject a fake transition into an operator's reading of the
trail — titles never reach it. The trail widens no visibility: it is org-manage, exactly as before, and
an incident body stays where it already is. The DoS shape of an audited hot path is answered by D1: the
paths a machine can drive at high frequency are the ones that write nothing.

## 9. Non-goals of FR-026

An incident-scoped audit panel and its endpoint (D9 — a follow-up needing a target index or an incident
column); a project-scoped audit read and the `project_id` column it requires (D9); auditing machine
writes (D1 — a position, with its reasons); a diff of every field a write touched (the target names the
transition, not a patch); retention or archival of `audit_logs` (unchanged by this requirement, and
unbounded today for every action); auditing READS of an incident; a second audit sink or an export
format; and refactoring `GateActor` / `SecretActor` into the shared actor (D3 names the possibility and
declines it here). D8a and D8b are NOT non-goals: they are corrections FR-026 carries, because an
invariant may not assert a property the code does not have.
