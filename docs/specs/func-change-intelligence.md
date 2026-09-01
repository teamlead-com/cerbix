# func-change-intelligence — a pipeline says what it changed, and the service's facts say what followed (FR-025 / NFR-020)

> **Lifecycle: IMPLEMENTED — iter-0165, closed 2026-09-01 (D-0213); FR-025/NFR-020 are `DONE` in `docs/status.md`.**
> Design approved at revision 3, 2026-08-30 (party [15]); the owner answered the seven questions of §11
> the same day (D-0209). No SPA code before an owner-approved UI mock of the timeline and the comparison; backend
> work may proceed under the approved design meanwhile, as FR-024's did.** Forward correction at implementation,
> owner decision D-0211 (2026-08-30): `pending` applies to EITHER comparison side whose end exceeds
> `sealed_through`, not to `after` alone (D8, invariant 11, §7 amended in place). Revisions 1–3 were reviewed as one design phase
> (iter-0164 task 3). Revision 1 was reviewed at party [9]: 2 P0 (tenant integrity of the link
> table; a false persistence claim on the comparison), 4 P1 (identical replay undefined; the timeline's
> group key and cursor; the link's anchor; retention by rows splitting a group), 3 P2 (text normalization;
> identity case; partial correlation delivery) — each addressed in revision 2 and named where it changed the
> text. Round 2 (party [13]) closed both P0 and returned 2 P1 (the DB CHECK claimed more than a CHECK can
> enforce; the normalization owner per layer was unstated) and a P2 (invariant 18 restated D13) — addressed
> in revision 3. Bounded by D17 of `func-reliability-gate.md` ("Deployment/rollback/flag
> events, the service timeline, incident correlation and before/after SLI comparison … append-only phases
> keyed `UNIQUE(service_id, source, external_id, phase)`, a domain-owned phase order, serialization per
> external identity, and closed enums with no free payload") and carrying the token-scoped CI capability
> that D12 of the same spec named as a follow-up requirement. The owner's answers to the seven questions of §11
> are recorded there and in D-0209; FR-025 and NFR-020 are `IN_PROGRESS` in `docs/status.md`;
> the implementation is iter-0165 and its evidence map is the FR-025 acceptance discharge in `docs/traceability.md`.

## 1. What this is, in one paragraph

The reliability gate (FR-024) answers "may this release go?" from facts cerbix has already sealed. What
cerbix still cannot say is **what happened to those facts after a release went**: a pipeline that got
`ALLOW` at 14:03 deploys at 14:05, the error budget falls off a cliff at 14:20, an auto-incident opens at
14:31 — and the on-call engineer reconstructs the sequence from three tools. FR-025 lets the pipeline
**record the change as a fact** — a deploy, a rollback, a flag flip, each with its own external identity
and phases — and lets the service's existing facts answer three questions that need no new measurement:
*what changed on this service and when* (the timeline), *which changes preceded this incident* (the
correlation), and *how did the SLI read before and after* (the comparison, from sealed buckets only).
Cerbix computes no new reliability fact, keeps no deployment catalog, takes no action on any external
system, and never says "caused by" — only "preceded by", with the lag stated. A CI token that records
changes and asks the gate gets, for the first time, an authority narrower than a role.

## 2. Requirements

- **FR-025 — Change Intelligence.** A principal with `change:record` reports a **change event** for a
  service — kind ∈ {deploy, rollback, flag}, phase ∈ {started, succeeded, failed, cancelled}, the
  instant it occurred, an external identity `(source, external_id)` under which phases of one change
  are grouped and ordered, an optional bounded `ref` (a version or commit label), an optional `url`, and
  optionally the gate `decision_id` the release rested on. Phases are append-only and idempotent under
  their key; the phase order is owned by the domain and a violation is refused by name; writes for one
  external identity are serialized. The service page shows the timeline — change marks on the same
  strip that shows the sealed facts, and a list — and an API lists it over a bounded range with a cursor.
  When an auto-incident opens on a service, the changes recorded on that service and on its probable-root
  upstream services within a bounded window before the open are linked to the incident (both directions
  readable) and named in one system note with the marker `🚀 Changes:`, best-effort and never blocking
  the incident. For a change with a terminal phase, the API and the service page state the SLI **before**
  and **after** its instant over a chosen horizon, from sealed canonical buckets only, with every
  withholding the reliability page would apply; a side whose window is not yet sealed is `pending`
  with `sealed_through` stated, never partial (D-0211). A CLI verb records a change the way `cerbix gate check`
  asks the gate: credentials from the environment, one stdout line, stable exit codes, `--json` verbatim.
- **NFR-020 — a change never rewrites a fact, and the comparison is the page's own number.** Recording a
  change mutates no reliability fact, no incident, no policy and no decision; correlation and comparison
  are reads over facts other owners sealed, and their figures are the values the reliability page would
  show for the same range and the same snapshot instant, because they are produced by the same store
  functions (`serviceReliabilitySeriesTx` extended, never re-implemented). A withheld number is withheld
  everywhere: a `before`/`after` figure is absent with a reason whenever the page would withhold it. The
  correlation is fail-open: a failed or slow correlation is logged and counted and the incident opens and
  resolves exactly as it does today. A CI token's authority is bounded by a per-token action list that
  is checked by the one central predicate every route already calls (`authz.Can`), with no role
  comparison in handlers.

## 3. The decisions

**D1 — a change is a fact about time, not a catalog entry. DECIDED (review [15]).**
`func-service-reliability.md` §1 excludes "stack, deployment metadata, generic scorecards" as belonging
to an external catalog. FR-025 does not reopen that: it stores **events** — that at instant T a change
of kind K in phase P happened to service S under identity (source, external_id) — and nothing about the
artefact (no repository, no owner team, no environment matrix, no manifest). `ref` is a bounded opaque
label so the timeline can say `v4.2.1`; `url` is a bounded link so the operator can open the run. Neither
is interpreted, joined or searched; neither is a payload. Anything richer is the catalog's, and a future
integration may link to it by `url`. **Invariant 1.**

**D2 — closed enums, bounded scalars, no free payload. DECIDED (owner, 2026-08-30 — three kinds, case-sensitive identity; D17).**
`kind ∈ {deploy, rollback, flag}`, `phase ∈ {started, succeeded, failed, cancelled}`, `source` a slug
`^[a-z0-9][a-z0-9-]{0,63}$`, `external_id` 1..128 characters (any printable, trimmed), `ref` 0..128
printable, `url` 0..512 and `https://` only — the exact grammar (review [36]): empty, or an absolute URL that
parses with scheme `https` and a non-empty host; `https://` alone, `http://` and scheme-relative forms are
refused as `url_invalid` (the UI renders it as a link and must not become a phishing surface) —
`occurred_at` RFC3339 with at most `change.max_past` behind and
`change.max_future` ahead of the server clock (§5a), stored in UTC to the microsecond. **Text is normative
(review P2-1; layers per round 2 P1-1/P1-2):** the canonical form of `external_id`, `ref` and `url` is
Unicode NFC, trimmed of leading and trailing whitespace, containing no code point of category Cc or Cf
and none of U+000A, U+000D, U+2028, U+2029, with length counted in code points. Three layers own it:
the **transport** (the HTTP handler) NORMALIZES — NFC and trim — every text field of the body before
anything else sees it, so the CLI and any other client may send what they were given; the **domain**
VALIDATES canonical values (`domain.ValidateChangeText`: NFC-invariant, trimmed, no Cc/Cf, no line
separators, length) and is the ONLY authority for the Unicode rules; the **store** accepts a change only
through `RecordChangePhase`, which calls the domain validator on the values it is about to write and
refuses non-canonical input with an error, so there is no writer that reaches SQL with un-validated
text. The **database** CHECK enforces what a CHECK can — length in characters and the absence of ASCII
control characters `[\x00-\x1F\x7F]` — as a last line against direct SQL; it does NOT enforce Cf, NFC or
the Unicode line separators, and the schema claims no more than that. Direct SQL is outside the contract
(a row it writes with a Cf character is a corruption, not a state the API can produce), and the negative
test for the DB CHECK is exactly the ASCII class it enforces. Identity is byte-exact after normalization: `source` is
lower-case by its class; `external_id` is case-sensitive (`Run-42` and `run-42` are two changes) —
owner question 7. No JSON field is accepted beyond these plus
`decision_id`; an unknown field is a 400 naming it, exactly as `POST …/gate` refuses one. Extending an
enum is a schema decision with its own migration and a docs-check guard, not a data change.
**Invariants 2, 3.**

**D3 — phases are append-only, idempotent under their key, and ordered by the domain. DECIDED (review [9]/[15]; D17).**
The key is `UNIQUE (service_id, source, external_id, phase)`. A phase row is never updated or deleted
by any route; retention removes whole changes by age (D9). Replaying a phase with an IDENTICAL body is
200 with the existing row (a pipeline retry is not an error); replaying it with a different body is 409
`phase_exists` naming the differing field. **IDENTICAL (review P1-1)** means: the client-owned canonical
fields — `kind`, `occurred_at` (UTC, microseconds), `ref`, `url`, `decision_id` — are equal after the
normalization of D2. The server-derived fields (`actor_label`, `actor_user_id`, `via_token`,
`recorded_at`) take no part: a retry under a rotated token, or by a person re-running the pipeline, is
200 with the ORIGINAL row — original actor, original `recorded_at` — writes nothing and emits nothing
(recording is not an audit event, D5). The order is: `started` may be followed by exactly one of
`succeeded | failed | cancelled` (the terminal phases); a second terminal is 409 `phase_order`
(`"succeeded already recorded"`); `started` after a terminal is 409 `phase_order`; a terminal WITHOUT a
prior `started` is ACCEPTED — many pipelines can only report the end — and the change simply has no
`started` row. `occurred_at` of a terminal must not precede the `started` of the same identity
(400 `occurred_at_before_start`). **Invariants 3, 4.**

**D4 — one external identity, one writer at a time. DECIDED (D17).**
Order checks and inserts for one `(service_id, source, external_id)` run under a transaction-scoped
advisory lock keyed by a stable hash of that triple (`pg_advisory_xact_lock`), taken before the phase
rows are read, so two pipelines reporting `succeeded` and `failed` for the same run at the same instant
cannot both pass the order check. **The key (review [43], implementation):** the two-int form
`pg_advisory_xact_lock(hashtext(service_id::uuid::text), hashtext(source || '/' || external_id))`, hashed
over the CANONICAL uuid text so an upper-case or brace-wrapped spelling of the same service takes the same
lock (the adversarial pass found two terminals landing when it did not); one SQL fragment owns the key and
retention takes the same lock (D9). Two distinct identities may collide on the 64-bit key and serialize
briefly — a bounded performance effect, never mixed data, because every query uses the exact identity;
the lock lives only as long as the transaction. **Invariant 4.**

**D5 — the actor is server-derived and stored twice. DECIDED (FR-024 D9 pattern).**
`actor_label` (immutable text; `token:<name>` for a token) plus the typed pair `actor_user_id`
(NULL for a token) and `via_token`. The body carries no actor field; one is a 400 unknown field. A change
recorded by a CI token therefore names its pipeline for as long as the row exists. Recording is NOT an
audit event — the row is the record, and a pipeline's heartbeat would bury the audit log (FR-024 D10's
reasoning); token creation with an `actions` list IS audited (D12). **Invariant 5.**

**D6 — the timeline is a read over `[from, to)` with a cursor, bounded to 92 days. DECIDED (review [9]/[15]).**
`GET …/services/{s}/changes?from&to&kind&source&cursor&limit` returns change GROUPS (one per external
identity) with their phases nested, over an explicit half-open range of at most 92 days (a quarter; changes are sparse — a busy service records tens a day,
not thousands), `limit` 1..200 default 50, an opaque keyset cursor, `next_cursor` null on the last page.
A `kind` filter is a set (repeatable, OR); `source` is one slug. 400 `range_required | range_invalid |
range_too_wide | limit_invalid | cursor_invalid | kind_invalid | source_invalid`. A deleted service has
no timeline (D10). **The group key and the cursor (review P1-2):** a group's `latest_occurred_at` is the
maximum `occurred_at` over its phases; `[from, to)` selects groups by `latest_occurred_at`, and a selected
group nests ALL its phases, including a `started` that precedes `from`. Order is
`latest_occurred_at DESC, source, external_id` — the identity is the collision-safe tiebreaker, never a
phase id; the cursor is the opaque encoding of that triple for the LAST RETURNED group; a page is
`LIMIT limit + 1` GROUPS from a grouped subquery bound strictly below the cursor. The traversal is live,
as the ledger's: a group returned once is never returned again; a phase recorded during the traversal
moves its group's `latest_occurred_at` forward and, if that carries it above the cursor, the group is not
returned on THIS traversal — a client that needs a fixed set re-reads the range. No page ever contains a
duplicate group. **Invariant 6.**

**D7 — correlation is a link table written at incident open, best-effort, symmetric on read. DECIDED (owner, 2026-08-30 — own service + `probable_root` upstream only; review [9]/[15]; §14.3 of `func-service-reliability.md`).**
When a service auto-incident's `opened` event is delivered by the outbox worker — the same place
`attachIncidentContext` runs today — the worker computes the incident's **preceding changes**: every
change whose latest phase's `occurred_at` lies in `[opened_at − change.correlation_window, opened_at]`
on the incident's own service (`role = own_service`) and on every service the incident's
`incident_service_impacts` rows mark `probable_root` (`role = upstream`). Each link is one row of
`incident_changes (incident_id, change_id, project_id, role, occurred_at, lag_seconds)`. **The anchor
(review P1-3):** `change_id` is the group's latest phase KNOWN at the `opened` delivery — that row's id,
its `occurred_at` and the lag are copied into the link and are never updated; a terminal phase recorded
after the open does not rewrite the link or the note (the group's current phases are read live beside
the anchor and shown as such). **Tenant integrity is the database's (review P0-1):** the link carries
`project_id` and both references are composite — `(incident_id, project_id)` → `incidents (id,
project_id)` and `(change_id, project_id)` → `service_changes (id, project_id)` — so a cross-project link
cannot be inserted by any path, direct SQL included, and both read directions filter by the caller's
project. **One transaction (review P2-3):** the links and the note are written in the SAME transaction,
guarded by the marker: a retried delivery that finds the note present writes nothing; a partial state
(links without note, note without links) cannot exist. The note `🚀 Changes: <n> preceded this
incident — <kind ref by source, −<lag>>; …` (at most `change.correlation_note_max` entries, the rest
counted) is appended ONCE through the existing `NOT EXISTS … LIKE marker` guard. Reads are symmetric
because both come from the link table: `GET /incidents/{id}/changes` and each change group carries
`incidents[]` (id, opened_at, lag). Nothing here is causal: the field is `preceded_by`, the note says
"preceded", and the UI says "preceded" — cerbix does not know that the deploy caused anything. Fail-open runs in BOTH directions (review [64]): the correlation never fails the incident's delivery, and
the delivery's own outcome never skips the correlation — the attempt is made for the `opened` event whether
the notification succeeded, failed or is heading for the dead letter, on its own bounded context, and its
idempotence (the incident row is taken and the marker checked inside the transaction) is what makes a
retried delivery write nothing the second time. An error is counted
(`cerbix_change_correlation_errors_total`) and the incident's delivery is decided elsewhere; a
change recorded AFTER the incident opened is not back-linked (the window is fixed at open; a later
`resolved` does not recompute). **Invariants 7, 8, 9.**

**D8 — before/after is the reliability page's own arithmetic over sealed buckets, or it is withheld. DECIDED (owner, 2026-08-30 — the four horizons; review [9]/[15]; NFR-020, FR-024 D1 precedent).**
For a change group with a terminal phase at instant T and a horizon `h ∈ {15m, 1h, 6h, 24h}` (the
request's `horizon`; default `1h`), the comparison reads the canonical buckets of `[T − h, T)` and
`[T, T + h)` through ONE store function, `serviceReliabilityCompareTx`, which is `serviceReliabilitySeriesTx`
extended with a bucket-aligned range sum — the same decidability, exclusion and provisional rules, the
same `sealed_through` clamp, the same epoch/revision segmentation. Each side is either a figure
(`availability`, `good_seconds`, `bad_seconds`, `unknown_seconds`, `excluded_seconds`, `buckets`) or
withheld with ONE reason from the page's own vocabulary: `definition_changed` (a revision or epoch
boundary inside the side), `undecidable` (the page would withhold availability for that range),
`no_facts` (no sealed bucket in the range), or `pending` when the side's end exceeds `sealed_through` —
`after` when `T + h > sealed_through`, `before` when `T > sealed_through` (a change reported minutes ago has
exactly this shape; owner decision D-0211, forward correction of revision 3's "after only") — with
`sealed_through` stated and NO partial figure. The response also states `delta`
(after − before, availability points) only when both sides are figures. T is the terminal phase's
`occurred_at` floored to the canonical bucket; a change whose only phase is `started` has no comparison
(`404 no_terminal_phase`). The comparison is a READ: nothing is stored, nothing is cached. **Its stability is the page's (review
P0-2):** two reads in one database snapshot are equal; across time the figures follow the reliability
page's existing historical-correction semantics — repair, reconciliation, retention, definition history
— exactly as the series they are summed from; sealing `T + h` makes `after` ELIGIBLE, it does not freeze
it. The contract is parity with the series for the same range and snapshot, never byte stability across
later maintenance. **Invariants 10, 11, 12.**

**D9 — retention removes whole changes by age; the link table follows. DECIDED (review [9]/[15]).**
`change.retention_days` (default 400, bounds [30, 1460]) — a year and a month, so a quarterly review
still has its history. A daily pass deletes change groups whose `latest_occurred_at` is older than the
bound, `incident_changes` rows cascading; the pass runs on the scheduler leader's existing retention
cadence. **Whole groups, never split (review P1-4):** each statement selects at most
`change.retention_groups_per_batch` group KEYS `(service_id, source, external_id)` whose
`latest_occurred_at < cutoff`, ordered by `(latest_occurred_at, service_id, source, external_id)`, and
deletes EVERY phase row of those keys in the same transaction (at most four rows per group, so the row
bound is `4 × groups`); it repeats until a batch selects fewer than the bound. A group whose `started` is
old but whose terminal is young is not selected — the group's age is its latest phase. **Under the
identity lock (review [42]):** the purge takes the SAME per-identity lock as `RecordChangePhase` for each
selected key inside its transaction, re-evaluates `latest_occurred_at < cutoff` under the lock, and deletes
only the keys still old — a terminal committed between selection and delete keeps its whole group; the
purge WAITS on a held lock, bounded by the caller's context (a cancelled batch rolls back and deletes
nothing), and reports the groups and rows actually deleted. Up to `retention_groups_per_batch` locks are held
per transaction, which is why the bound stays at 2 500. Change volume does not justify
partitioning: the design capacity (§5a) is ~10⁵ rows a year for a hundred busy services. **Invariant 13.**

**D10 — changes belong to their service; deleting the service deletes its timeline. DECIDED (owner, 2026-08-30 — cascade, not the ledger's outlive rule).**
`(service_id, project_id)` is a composite FK `ON DELETE CASCADE`. The gate's ledger outlives a service
because a decision is the gate's own record of what IT said; a change is a fact about the service, and
the incident note (text) remains as the historical trace after a cascade, exactly as `⚡ Context:` does.
A deleted service's incidents keep their `🚀 Changes:` note; their `incident_changes` rows are gone with
the changes. **Invariant 14.**

**D11 — the decision link is validated on write and read back by id. DECIDED.**
`decision_id`, when given, must be a ledger row of THIS service in THIS project (`GET
…/gate/decisions/{id}` semantics, `service_id` matching) — 400 `decision_unknown` otherwise; it is stored
as a uuid without a FK (the ledger's partitions are dropped by age, D10 of FR-024). The timeline shows
`decision: BLOCK → ALLOW (overridden)` from the ledger when the row still exists and `decision: <id>
(aged out)` when it does not. **Invariant 15.**

**D12 — authorisation, and the token-scoped capability FR-024 D12 deferred. DECIDED (owner, 2026-08-30 — the allow-list, not a `ci` role; immutable after create).**
Two new central actions: `change:record` (editor+) and — no new read action: the timeline, the
comparison and the incident links are `project:read` (viewer+). The capability: `api_tokens` gains
`actions text[] NULL`. `NULL` means what it means today — the token's role decides. A non-null list is an
ALLOW-LIST intersected with the role: `Can(action)` for a token principal is `roleGrants[role] ∋ action
AND (actions IS NULL OR action ∈ actions)`. A CI token is therefore `role: editor, actions:
[gate:evaluate, change:record]` — it can ask the gate and record changes and can do NOTHING else, not
even `project:read` (its `GET …/services` is 403; its project is still visible for 404-vs-403 purposes
because visibility is membership, not action — `VisibleProject`, the 404-versus-403 predicate, therefore
reads membership alone, while `Can` and its query-scope mirror `VisibleScope` intersect the list). The list
is validated against the central `Action` catalogue on create (400 `action_unknown`) AND against the token's
own role — an entry the role does not grant is 400 `action_not_granted` naming it, so an operator's mistake
surfaces at creation and not at the pipeline's first 403 (owner decision D-0212) — is immutable after create (a different list is a new token —
tokens are cheap, audit is not), and appears in the token's read model and in the audit row
`token.create`. The list is consulted in ONE central authorization predicate — `authz.Can` and its
query-scope mirror `VisibleScope`, which must intersect the same list or a narrowed token could still
enumerate what it may not read (review [36]) — and nowhere else; handlers keep calling
`projectAccess(w, r, projectID, action)`. **Invariants 16, 17.**

**D13 — the CLI verb `cerbix change record`. DECIDED (FR-024 D16 contract).**
`cerbix change record --project <id> --service <id> --kind deploy|rollback|flag --phase started|succeeded|failed|cancelled --source <slug> --external-id <id> [--ref <label>] [--url <https url>] [--decision <id>] [--at <RFC3339>] [--json] [--timeout 10s]`.
Credentials from `CERBIX_URL` and `CERBIX_TOKEN` only, never flags. stdout ONE line:
`recorded change=<id> kind=<k> phase=<p>` (or `replayed …` for an identical replay); stderr carries
refusals verbatim. Exit codes: **0** recorded or replayed; **2** refused by the contract (400/404/409 —
the pipeline's own mistake, printed) and usage errors (a missing or invalid flag); **1** transport (auth,
timeout, 429 with `Retry-After` printed, 5xx) and a missing environment variable, named, with no request
made — the gate verb's precedent (iter-0165 §1.5). No retry on 429, as the gate verb. `--json` prints the API response verbatim. `--at` defaults to
the invocation instant. **Invariant 18.**

**D14 — the SPA surfaces, and what waits for the mock. DECIDED.**
On the service page: change marks on `ReliabilityStrip` (one mark per terminal phase, placed by
`occurred_at`, kind-shaped, never a colour of a state) and a `Changes` card between `Release gate` and
`Dependencies` listing the last 30 days (kind, ref, phases with instants, source, actor, the decision
link, `preceded` incidents, a before/after row per terminal change at the default horizon with the
horizon selectable); an incident page section `Preceded by` from `GET /incidents/{id}/changes`. Nothing
is rendered before the owner approves a UI mock, exactly as FR-024. **Invariant 19.**

**D15 — observability. DECIDED.**
`cerbix_changes_recorded_total{kind,phase,outcome="recorded|replayed"}`,
`cerbix_change_record_rejected_total{reason}` (a closed set: the 400/409 codes, `body_invalid` for a shape
refusal without a code, and the four §5a limiter codes — an uncounted 429 would hide the load; D-0212),
`cerbix_change_correlations_total{role}`,
`cerbix_change_correlation_errors_total`, `cerbix_change_compare_total{outcome="figure|withheld|pending"}`,
`cerbix_changes_retained` (gauge: the ROWS of `service_changes`, sampled once per retention pass by the
leader and cleared on leadership loss — D-0212). No `service_id`, `source` or `external_id`
label anywhere (cardinality; the same rule that keeps `job_id` out of labels). Runbook rows for
`correlation_errors_total > 0 for 15m` (warn) and `record_rejected_total{reason="phase_order"}` rising
(inform: a pipeline reports out of order). **Invariant 20.**

**D16 — what FR-025 is not (§9), stated as decisions.** No automatic rollback or any action on an external
system; no deployment catalog (D1); no vendor integration (one generic route and one CLI verb; a GitHub
Action or a GitLab template is a wrapper someone else writes around the verb); no causal attribution
(D7); no flag STATE (only the flip event); no change approval or freeze windows (the gate is the control,
this is the record); no DORA dashboards (the timeline API is enough to build them outside).

## 4. What must move WITH the implementation, not after it

- `docs/overview.md` — the change routes, the CLI verb, the correlation note, the comparison and its
  withholding vocabulary, the token `actions` allow-list.
- `docs/runbook.md` — "a pipeline reports out of order" (409 `phase_order` is the pipeline's bug, not
  cerbix's), the correlation-error alert, the retention knob and capacity, the CI-token recipe
  (`role: editor, actions: [gate:evaluate, change:record]`).
- `docs/specs/func-service-reliability.md` §14 — a §14.9 cross-reference: incident↔change links live in
  `incident_changes`, computed at open, the `🚀 Changes:` marker's single home; §16.8 invariants for the
  comparison's parity with the series.
- `docs/specs/func-incident-context.md` — the second system note and the shared marker guard.
- `docs/specs/sec-authn-authz.md` — the token `actions` allow-list and the intersection rule.
- `internal/domain` — `ChangeKind`, `ChangePhase`, the phase-order function, the two markers.
- `openapi.yaml` — the record, timeline, comparison, incident-changes routes; `ApiToken.actions`;
  regenerate `frontend/src/api/schema.d.ts`.
- `docs/specs/README.md` — the row; `README.md` — one clause.
- `docs/traceability.md` — the FR-025 acceptance map (§6 as a set, §7 scenarios), checked by
  `make docs-check` like FR-024's.

## 5. Schema (decided shape; column detail belongs to the implementation)

```
service_changes
  id              uuid PRIMARY KEY           -- v7 from the DB instant, as the gate's ids
  project_id      uuid NOT NULL
  service_id      uuid NOT NULL
  source          text NOT NULL              -- slug, CHECK ~ '^[a-z0-9][a-z0-9-]{0,63}$'
  external_id     text NOT NULL              -- CHECK char_length 1..128 AND external_id !~ '[\x00-\x1F\x7F]'
  kind            text NOT NULL              -- CHECK IN ('deploy','rollback','flag')
  phase           text NOT NULL              -- CHECK IN ('started','succeeded','failed','cancelled')
  ref             text NOT NULL DEFAULT ''   -- CHECK char_length <= 128 AND ref !~ '[\x00-\x1F\x7F]'  (ASCII control only; Unicode rules are the domain's)
  url             text NOT NULL DEFAULT ''   -- CHECK length <= 512 AND (url = '' OR url LIKE 'https://%')
  occurred_at     timestamptz NOT NULL
  decision_id     uuid NULL                  -- validated on write, no FK (ledger partitions age out)
  actor_label     text NOT NULL
  actor_user_id   uuid NULL
  via_token       boolean NOT NULL
  recorded_at     timestamptz NOT NULL DEFAULT now()
  UNIQUE (service_id, source, external_id, phase)
  UNIQUE (id, project_id)                            -- the target of the composite FK below
  FOREIGN KEY (service_id, project_id) REFERENCES services (id, project_id) ON DELETE CASCADE
  INDEX (service_id, occurred_at DESC, id DESC)      -- the timeline's grouped subquery
  INDEX (project_id, occurred_at)                    -- retention

incidents
  + UNIQUE (id, project_id)                          -- if absent today; the target of the composite FK below

incident_changes
  incident_id     uuid NOT NULL
  change_id       uuid NOT NULL
  project_id      uuid NOT NULL
  role            text NOT NULL              -- CHECK IN ('own_service','upstream')
  occurred_at     timestamptz NOT NULL       -- the anchored phase's instant, copied, immutable
  lag_seconds     integer NOT NULL           -- opened_at − occurred_at, CHECK >= 0
  computed_at     timestamptz NOT NULL DEFAULT now()
  PRIMARY KEY (incident_id, change_id)
  FOREIGN KEY (incident_id, project_id) REFERENCES incidents (id, project_id) ON DELETE CASCADE
  FOREIGN KEY (change_id, project_id) REFERENCES service_changes (id, project_id) ON DELETE CASCADE

api_tokens
  + actions       text[] NULL                -- D12; NULL = the role decides; validated against authz.Actions
```

A change GROUP in the API is the set of `service_changes` rows sharing `(service_id, source,
external_id)`; it is not a table. `kind` is stored per phase row so a replay with a different kind is a
detectable 409, but the domain refuses a phase whose kind differs from the group's existing rows
(`409 kind_mismatch`), so a group has one kind in practice. Migrations are goose, `NO TRANSACTION` with
guarded DO-blocks, per the repository pattern.

## 5a. Resource bounds — the numbers, the algorithm, the capacity they imply

Ten keys under `change.*`, each with bounds, each refused outside them at boot with the key named:

| key | default | bounds | meaning |
| --- | --- | --- | --- |
| `change.record_rate_process_per_minute` | 300 | [10, 3000] | token bucket, process-wide, for `POST …/changes` |
| `change.record_rate_principal_per_minute` | 30 | [1, 600] | per principal (user id or token id) |
| `change.record_inflight_process` | 32 | [1, 256] | in-flight permits for record |
| `change.read_inflight_process` | 64 | [1, 512] | in-flight permits for timeline/compare/incident-changes reads |
| `change.max_past` | 24h | [1h, 168h] | `occurred_at` may lag the server clock by at most this |
| `change.max_future` | 5m | [0s, 1h] | `occurred_at` may lead the server clock by at most this |
| `change.correlation_window` | 60m | [5m, 24h] | preceding-change window at incident open |
| `change.correlation_note_max` | 5 | [1, 20] | entries named in the `🚀 Changes:` note |
| `change.retention_days` | 400 | [30, 1460] | age bound on change groups |
| `change.retention_groups_per_batch` | 250 | [10, 2500] | group keys selected per retention statement (≤ 4 rows each) |

Limiter order is the gate's: permits first, then the atomic bucket debit; a refusal returns 429 with
`Retry-After = ceil(seconds to one token) ≥ 1` and the error code `process_inflight | principal_inflight |
process_rate | principal_rate`. Capacity at defaults: 300 records/min process-wide is 18 000/h; a
hundred services each deploying ten times a day with two phases each is ~2 000 rows/day, ~7·10⁵
rows/year — a plain table with two indexes. The comparison reads at most `2 × 24h / 1m = 2 880` buckets
per call, bounded by the series function's own statement timeout.

## 6. Acceptance invariants (FR-025) — design contract, discharged on implementation

Twenty-three, compared as a SET against the traceability map by `make docs-check` once built.

1. a change is a fact about time, never a catalog entry: no field of `service_changes` names a
   repository, owner, environment or artefact beyond the bounded opaque `ref` and `url`; nothing joins
   or searches them;
2. the enums are closed and every scalar is bounded on the wire and in the schema (D2); an unknown field
   in the body is 400 naming it; an `http://` url is refused;
3. a phase row is never updated or deleted by any route; a replay whose client-owned canonical fields
   (`kind`, `occurred_at`, `ref`, `url`, `decision_id`, after D2 normalization) are equal is 200 with the
   ORIGINAL row regardless of actor, writes nothing and emits nothing; a differing replay is 409
   `phase_exists` naming the field;
4. the phase order is the domain's: `started` then exactly one terminal, a terminal alone accepted, a
   second terminal or a `started` after a terminal 409 `phase_order`, a terminal that predates `started`
   400; two concurrent writers for one external identity are serialized (D4) and only one terminal
   survives;
5. the actor is server-derived and stored twice; a body with an actor field is a 400 unknown field;
6. the timeline is a `[from, to)` read of at most 92 days with an explicit range selecting GROUPS by
   `latest_occurred_at`, all phases nested, ordered `latest_occurred_at DESC, source, external_id`, a
   keyset cursor over that triple never returning a group twice, `LIMIT` counted in groups, and
   `kind`/`source` filters applied before the limit;
7. at a service auto-incident's `opened` delivery, preceding changes on the own service and on its
   `probable_root` upstream services within `change.correlation_window` are linked — each link anchoring
   the latest phase known at that instant with its `occurred_at` and lag copied and never updated — and
   the `🚀 Changes:` note is appended in the SAME transaction, exactly once through the marker guard; a
   later terminal phase rewrites neither;
8. correlation is fail-open in both directions: a failing correlation is counted and the incident's
   delivery, open and resolve are unchanged, AND a failing notification does not skip the correlation —
   the attempt is made for the `opened` event whatever the delivery did, idempotently, so a retry neither
   duplicates nor double-counts; a change recorded after open is not back-linked;
9. the links read symmetrically — `GET /incidents/{id}/changes` and the group's `incidents[]` come from
   the same rows — and no field, note or screen says "caused";
10. the comparison's figures come from `serviceReliabilityCompareTx`, an extension of the series owner,
    never a second implementation; a range the page would withhold is withheld here with the same
    reason;
11. a side whose end exceeds `sealed_through` is `pending` with `sealed_through` stated — `after` when
    `T + h > sealed_through`, `before` when `T > sealed_through` (D-0211) — never a partial figure; `delta`
    is present only when both sides are figures;
12. the comparison stores and caches nothing; two reads in one snapshot are equal; across time it
    follows the reliability page's historical-correction semantics, and its contract is parity with the
    series for the same range and snapshot;
13. retention selects group KEYS by `latest_occurred_at` in a deterministic order, at most
    `change.retention_groups_per_batch` per statement, deletes every phase row of those keys in one
    transaction, never splits a group, and `incident_changes` follow by cascade;
14. deleting a service cascades its changes and links; the incident note remains as text;
15. a `decision_id` on write is a ledger row of this service in this project or 400 `decision_unknown`;
    the timeline reads it back by id and says `aged out` when the row is gone;
16. `change:record` is a central action (editor+); reads are `project:read`; no role string appears in a
    handler;
17. a token's `actions` allow-list intersects its role in the one central predicate — `authz.Can` and its
    query-scope mirror `VisibleScope` — and nowhere else, while project visibility (`VisibleProject`, the
    404-versus-403 predicate) is membership alone; an entry the role does not grant is refused at create
    (`action_not_granted`, D-0212); `NULL` leaves
    every existing token's authority unchanged; the list is validated against the action catalogue on
    create, immutable after, visible in the read model and the `token.create` audit row; a CI token with
    `[gate:evaluate, change:record]` is 403 on `GET …/services`;
18. the CLI verb follows D13 exactly;
19. no SPA file exists before the owner approves the mock; then the marks are placed by `occurred_at` on
    the same strip as the facts and carry no state colour;
20. metrics carry no per-service, per-source or per-identity label; the runbook names the two alert rows;
21. bounds of §5a are enforced at boot with the key named, the limiter order is permits-then-bucket, and
    `Retry-After` is `ceil ≥ 1`;
22. tenant integrity of the link table is the database's: `incident_changes.project_id` with composite
    foreign keys to `incidents (id, project_id)` and `service_changes (id, project_id)`; a cross-project
    link inserted by direct SQL fails, and both read directions are scoped by the caller's project;
23. text fields have one canonical form (NFC, trimmed, no Cc/Cf, no line separators, lengths in code
    points): the transport normalizes, the domain validator is the single Unicode authority, the store
    writes only through the one function that calls it, and the DB CHECK enforces exactly length and the
    ASCII control class `[\x00-\x1F\x7F]` — no more is claimed of it; `external_id` identity is
    case-sensitive after normalization.

## 7. Required test matrix (written before the code)

*Phases:* `started` then `succeeded` → two rows, one group · `succeeded` alone → one row, group without
start · `succeeded` then `failed` → 409 `phase_order` naming `succeeded` · `started` after `failed` → 409
· terminal `occurred_at` before `started` → 400 `occurred_at_before_start` · identical replay → 200 same
id · replay with a different `ref` → 409 `phase_exists` naming `ref` · a phase whose `kind` differs from
the group → 409 `kind_mismatch` · two goroutines racing `succeeded`/`failed` for one identity → exactly one
row, the other 409 (the advisory lock, asserted with a planted lock holder) · an identical replay under
ANOTHER token → 200, the ORIGINAL `actor_label` and `recorded_at`, no audit row · `external_id` `Run-42`
then `run-42` → two groups · `ref` with a U+200B or a newline → 400 `ref_invalid` from the domain validator; NFC: a decomposed
`é` replays as identical to the composed one · the store's `RecordChangePhase` given a non-canonical
value (a leading space, a U+200B) → refused before SQL · direct SQL inserting `ref` with `\x01` → the DB
CHECK refuses; direct SQL inserting U+200B → accepted by the database (the CHECK claims no more), which
is why the domain, not the schema, is the Unicode authority.

*Bounds and shape:* `source` `Deploy_Bot` → 400 `source_invalid` · `external_id` 129 chars → 400 · `url`
`http://…` → 400 · `occurred_at` 25 h ago → 400 `occurred_at_out_of_bounds`; 4 min ahead accepted; 6 min
ahead refused · an `actor` field → 400 unknown field · `decision_id` of another service → 400
`decision_unknown` · a limiter refusal → 429 with `Retry-After ≥ 1` and the four codes.

*Timeline:* 93 days → 400 `range_too_wide`; 92 accepted · groups newest first by latest phase · cursor
continues without duplicates across three pages of 2 · `kind=deploy&kind=flag` OR · `source` filter ·
a group whose `started` precedes `from` but whose terminal is inside → returned with BOTH phases · a
phase recorded mid-traversal moves its group above the cursor → the group is absent from this traversal
and no page holds a duplicate · two groups with the same `latest_occurred_at` → the identity tiebreaker
orders them and the cursor continues past both · a foreign service id → 404; a deleted service → 404 and
its rows gone · `decision_id` read back: a live
ledger row renders `state/action`; a dropped partition renders `aged out`.

*Correlation:* an incident opens 12 min after a deploy on its service → one `own_service` link, lag 720,
one note naming it · an upstream `probable_root` service's deploy 40 min before → `upstream` link · a
deploy 61 min before at a 60 min window → no link · a deploy AFTER open → no link at open and none on
resolve · the note appended once under two deliveries, links and note in one transaction (a delivery killed
between them leaves neither) · `started` 10 min before open, `succeeded` recorded AFTER open → the link
anchors the `started` row with its lag; the group's live phases show both; the note is unchanged ·
direct SQL inserting a link across two projects → FK violation · a planted correlation error → counted, the
incident's `opened` delivery completes and later `resolved` is unaffected · `GET /incidents/{id}/changes`
and the group's `incidents[]` agree row for row · the note and the API say "preceded", never "caused".

*Comparison:* a deploy at T with sealed facts around it → `before` and `after` figures equal the sums of
`ServiceReliabilitySeries` over the same bucket ranges (parity, to the microsecond) · `T + h` past
`sealed_through` → `after: pending` with `sealed_through`, no figure, no `delta` · `T` itself past
`sealed_through` → BOTH sides `pending` (D-0211) · a revision boundary
inside `before` → `withheld: definition_changed` · a range the report path would withhold → `withheld:
undecidable`, same reason string · no buckets → `no_facts` · `horizon=2h` → 400 `horizon_invalid` · a
group with only `started` → 404 `no_terminal_phase` · two calls in one snapshot → identical bytes; a repaired bucket between two calls → the second equals
the series' new sum (parity, not persistence).

*Authorisation and tokens:* viewer records → 403; editor → 201; a token `role: editor, actions:
[gate:evaluate, change:record]` → 201 on record, 200 on `POST …/gate`, 403 on `GET …/services`, 403 on
`PUT …/gate/policy` · `actions: [not:an:action]` on create → 400 `action_unknown` · `actions: NULL` token
behaves exactly as before (the whole existing token suite unchanged) · the `token.create` audit row
carries the list · `PATCH` of `actions` → 405/400, immutable.

*Retention:* groups older than the bound deleted by KEY in batches of `retention_groups_per_batch`,
every phase of a selected group gone in one transaction and none of an unselected one (a batch boundary
planted between two phases of one group), their links gone, a younger group's rows untouched, an old
`started` with a young terminal kept (group age = latest phase).

*CLI:* recorded → exit 0, stdout `recorded change=… kind=deploy phase=succeeded` · identical replay →
exit 0 `replayed …` · 409 `phase_order` → exit 2, stderr verbatim · 401 → exit 1 · 429 → exit 1 with
`Retry-After` printed, no retry · `--json` bytes equal the API body · `--at` missing → now.

*Docs:* `make docs-check` compares §6 as a set against the traceability map; the stale-spelling guard
refuses `deployment_events`, `change_events`, `caused_by` (retired spellings of this design).

## 8. Threat model

| Threat | Mitigation |
| --- | --- |
| A stolen CI token records fake changes | `change:record` only; rate-bounded; every row names `token:<name>`; the timeline is a record, not a control — a fake change blocks nothing and pages nobody |
| A stolen CI token reads the service page or policies | the `actions` allow-list: `[gate:evaluate, change:record]` is 403 on every other action (D12) |
| A hostile `url` on the timeline | `https://` only, rendered with `rel="noopener noreferrer"`, never auto-followed |
| Free-text payload smuggled into `ref`/`external_id` | bounded printable text, never interpreted, rendered escaped |
| A pipeline floods phases to hide a change | per-identity uniqueness caps a group at four rows; rate limits cap the principal |
| Correlation as a denial vector at incident open | fail-open, bounded window, bounded note, counted |

## 9. Non-goals of FR-025

Automatic rollback or any action by cerbix on an external system; a deployment catalog (repository,
owner, environment, artefact — D1); vendor-specific integrations (one route, one verb); causal
attribution ("preceded", never "caused" — D7); feature-flag STATE (only the flip event); change approval,
freeze windows or a change calendar (the gate is the control); DORA metrics or any dashboard beyond the
timeline API; back-linking changes recorded after an incident opened; per-monitor changes (services
only); a change on a project or an organization (services only).

## 10. Stale-spelling guard

`make docs-check` refuses, in this file and the living documents: `deployment_events`, `change_events`
(the table is `service_changes`), `caused_by`, `root_cause_change` (the field is `preceded_by`, the note
says "preceded"), `change:read` (reads are `project:read`), and `token_scopes` for the token list (it is
`actions`; the bare word `scopes` stays legal — it is the OIDC vocabulary elsewhere in the tree).

## 11. The owner's questions, and the answers (D-0209, 2026-08-30)

Every recommendation below was accepted by the owner on 2026-08-30; the decisions above carry the answer.

1. **Kinds.** `deploy | rollback | flag` per D17. Should `config` (a configuration change without a
   deploy) be a fourth kind, or is that a `deploy` with a `ref`?
2. **Cascade vs outlive.** D10 cascades a service's changes on delete. The alternative is the ledger's
   rule (rows outlive the service with `service_id NULL`). Recommendation: cascade — a change is the
   service's fact; the incident note survives as text.
3. **Correlation reach.** D7 links the own service and `probable_root` upstream services. Should
   `affected` downstream services' changes also be considered (they cannot have caused the incident, but
   an operator may want to see them)? Recommendation: no.
4. **Comparison horizons.** `{15m, 1h, 6h, 24h}`. Enough, or should `7d` exist for slow-burn effects
   (it would read 20 160 buckets per side)? Recommendation: the four; a slow burn is the burn rules' job.
5. **Token actions immutability.** D12 makes the list immutable after create. Acceptable, or should
   `PATCH` be allowed with an audit row?
6. **The CI token's role.** D12 requires `role: editor` under the allow-list because `change:record` is
   editor+. An alternative is a dedicated `ci` role in `roleGrants` holding exactly `gate:evaluate` and
   `change:record`, without any allow-list mechanism. Recommendation: the allow-list — it generalizes to
   every future action; a `ci` role would be a second mechanism the moment one pipeline needs one more
   action.
7. **Identity case.** `external_id` is case-sensitive after NFC (D2, review P2-2): `Run-42` and `run-42`
   are two changes. Should cerbix fold case instead? Recommendation: no — CI run ids are opaque, and
   folding would merge identities a system distinguishes.
