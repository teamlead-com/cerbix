# Spec: Audit gap package 3 — the post-v0.1.8 review (func-audit-gaps-3)

> **Status: IMPLEMENTED at `iter-0179`, decision `D-0247`. NOT COMMITTED. The independent
> implementation review is COMPLETE — all 52 items reviewed, no open findings (party [381]) — and
> the reviewer states that this closes neither the iteration nor a commit, status or release.** The package carries NO requirement number, and that is the precedent rather
> than an omission: neither `func-audit-gaps.md` (iter-0044) nor `func-audit-gaps-2.md`
> (iter-0055) has one, because an audit-gap package repairs requirements instead of adding one.
> The owner opened the iteration after the independent reviewer raised that delivery documents had
> been changed while none was open.
>
> Every row of the discharge map has been built and has the test its row names, in the order the
> clusters are written. TWO items departed from the decision as written and say so in their own
> rows: **A2**, whose decision names a source that does not exist at two of its three sites, and
> **C4**, whose re-measurement inverted the fact its retry bound was sized from. Nothing else
> deviates.
>
> The findings below come from a two-axis review of `v0.1.8..main` (133 commits,
> ~34,700 added lines of Go/SQL/TS outside `internal/web/dist`) run on 2026-09-06.

## Problem Statement

The work that landed between `v0.1.8` and `main` — the async canary (FR-029), the monitor
description (FR-030), truthful rendering (FR-031/NFR-025), the expected-run ledger (FR-032),
and the credentialed-dispatch fixes on FR-020 — is individually well tested and collectively
unreviewed as a whole. A review of the combined range found defects that no single arc could
see, because each one lives at a seam between two arcs that were built separately.

From an operator's point of view the range currently allows all of the following:

- A canary's project secret can leave the process **in cleartext over HTTP** when a target
  answers a redirect that downgrades its own scheme. The redirect policy drops
  binding-backed headers on a host-or-port change and does not look at the scheme at all;
  `https` is enforced once, at write time, and never re-checked on a hop.
- A canary whose region has **no capable executor never goes DOWN**. The shortage path
  writes a raw heartbeat row instead of recording a result, so the confirmation counter,
  the status flip, the transition outbox and the service SLI never see it. The monitor
  stays green while nothing is probing it, and no alert, incident or escalation fires.
- A monitor with a credential **schema** but no credential **value** — `promql` with
  `auth_mode: none`, `rabbitmq` with `mode: amqp` — is dispatched on a generation-1 carrier
  while still carrying the window field that defines generation 4. The ledger then records
  `carrier_generation = 4` for a run that rode generation 1, which is the exact false record
  the carrier mechanism was built to make impossible, and the reading changes between
  executor versions during a rolling upgrade.
- An `async_canary` declared in a Monitoring-as-Code bundle **silently loses its
  description**, and because the description is folded into the bundle's canonical hash,
  every later edit of that description is a permanent no-op.
- Editing an on-call schedule's name **moves the rotation anchor** by the viewer's UTC
  offset, and the new zone hint beside the control positively asserts a zone the value is
  not in.
- Several new renderings claim more than their facts support: a segment missing its tail
  is described as missing its head, the latency panel denies the strokes it is drawing, and
  a segment whose series is still loading is described as having late-starting records.

The common shape is not carelessness. It is that each arc built its own correct mechanism
and then relied on a **sentence** to connect it to the arc beside it — a docstring, an
interface comment, a test name, an allow-list idiom — and a sentence is not a mechanism.

## Solution

One gap package, taken in clusters rather than in severity order, because the clusters
share a seam and fixing a cluster's seam closes several findings at once. Each cluster ends
with the connecting sentence replaced by something the compiler, the schema or a test can
enforce, so the same gap cannot reopen silently.

Where a fix would change observable product behaviour rather than restore what the design
already promised, the spec says so and asks for the owner's ruling instead of choosing.

## User Stories

1. As an SRE, I want a canary's credentials never to leave the process over plaintext HTTP, so that a compromised or misconfigured target cannot harvest a project secret by answering one redirect.
2. As an SRE, I want every redirect hop of a canary journey re-validated against the rules the declared URL had to pass, so that a write-time guarantee is also a run-time guarantee.
3. As an on-call responder, I want a monitor whose region has no capable executor to go DOWN like any other failing monitor, so that I learn about it from a page rather than from a customer.
4. As an on-call responder, I want a dispatch shortage to count toward the monitor's own failure threshold, so that a transient shortage does not page me and a sustained one does.
5. As a service owner, I want an undispatchable canary's samples to reach the service SLI, so that the number I report is not silently computed over a monitor that stopped being measured.
6. As an operator, I want the reason a run could not be dispatched to stay visible on the heartbeat and in the metrics, so that the alert I receive says *why* without me opening the scheduler log.
7. As a reliability owner, I want a window's recorded carrier generation to be the generation the run actually rode, so that a coverage claim is evidence and not an inference.
8. As a reliability owner, I want a job that cannot carry the ledger's identity field to be published without that field, so that no consumer can mistake it for a generation-4 delivery.
9. As a platform engineer, I want a monitor with a credential schema but no credential value to ride the same carrier rules as every other monitor of its region, so that `promql` with `auth_mode: none` is not a special case nobody wrote down.
10. As a platform engineer running a rolling upgrade, I want a window's verdict to be the same whichever executor version answered it, so that a half-deployed cluster degrades in delivery rather than in truth.
11. As an operator, I want a description declared for an `async_canary` in a bundle to reach the monitor, so that the file provider owns the field for every type and not for all-but-one.
12. As an operator, I want editing a description in a bundle to be a change the reconciler notices, so that I am not editing a file that can never take effect.
13. As an operator, I want a bundle key that a builder is going to discard to be rejected at parse time, so that "applied successfully" means the bundle was applied.
14. As an on-call responder, I want editing a schedule's name to leave its rotation anchor exactly where it was, so that a cosmetic edit does not silently re-order who gets paged.
15. As an on-call responder, I want the zone hint beside a time control to name the zone the control's value is actually in, so that a hint is not a second way to be wrong.
16. As an operator, I want a segment whose records stop early to say its records stop early, so that a storage verdict distinguishes the two ends it claims to distinguish.
17. As an operator, I want a panel that draws expectation strokes to stop telling me it draws none, so that the legend and the subtitle describe the same picture.
18. As an operator, I want an expectation cell to cover only the window it belongs to, so that a covered run cannot paint over the missed runs beside it.
19. As an operator, I want a segment whose series has not loaded yet to say so rather than to quote a storage verdict computed from no data, so that "loading" and "incomplete" are different statements.
20. As an operator, I want a p95 legend to name the population it was computed over, so that a number and its label agree.
21. As an operator, I want a timeout to be either drawn on the scale or declared off it, in every case including the one where nothing was measured.
22. As an operator, I want a marked sub-second outage to state its real duration rather than round it to zero, so that "marked, too small to draw" is not also "too small to exist".
23. As an operator, I want two revision boundaries inside one day to render as two different windows, so that a per-segment label can tell them apart.
24. As an API consumer, I want the endpoint description to state the configured retention bound rather than a hardcoded fourteen days, so that the source of truth agrees with the instance.
25. As a maintainer, I want one canary execution per dispatch, so that the in-flight lease arithmetic describes the work it is leasing.
26. As a maintainer, I want the retry knob for a canary either bounded to what one lease can hold or refused for that type, so that a legal configuration cannot produce two concurrent journeys.
27. As a security-minded operator, I want executor-owned config keys to be unwritable from the API, so that a client cannot pin a canary's idempotency key and make it assert a stale success forever.
28. As an operator running `role=all`, I want a canary to run in any non-pull region my instance serves, so that the region field means the same thing for canaries as for every other type.
29. As a maintainer, I want a schema-invalid job that reaches an executor to fail its stage rather than panic the process, so that one crafted payload cannot take the region's whole prober pool down.
30. As a reliability owner, I want one monitor's invalid expectation not to hold back every other ledgered monitor's dispatch, so that the ledger's truthfulness is not on the critical path of delivery for the whole instance.
31. As a reliability owner, I want the gate that permits a publish to be proof that *this* dispatch's window row was written, not that the schedule moved.
32. As a reliability owner, I want `issued_at` recorded from the publish instant rather than from the tick's start, so that lateness is not systematically under-reported.
33. As a reliability owner, I want the correlation floor and the purge cutoff to agree, so that a row that is still stored and still listed can still be correlated.
34. As a reliability owner, I want rows that landed in the default partition to count toward the retained floor, so that a scheduler outage does not make the ledger declare unanswerable a span it still holds.
35. As an operator reading a canary editor, I want opening and re-saving a workflow to leave every number byte-identical, so that an unrelated edit does not change the semantic hash and re-segment reliability history.
36. As an operator on PostgreSQL 14, I want the refusal message to name every migration that requires 15, so that the list I am given is the list that exists.
37. As a maintainer, I want an incident write that cannot be attributed to fail rather than to commit unaudited, so that the guarantee is carried by the statement and not by a comment.
38. As a maintainer, I want an internal write path with a nil-able actor either to lose the nil or to refuse it, so that a future in-package caller cannot create an unaudited mutation.
39. As a maintainer, I want the API's store interface to declare only the doors the API actually uses, so that no unaudited door is reachable by hand.
40. As a maintainer, I want a comment that cites a guard to cite a guard that exists, so that a name check would catch it drifting.
41. As a maintainer, I want the value of a configuration default to live in the configuration loader, so that one number is not resolved in five places.
42. As a maintainer, I want a guard over a repeated claim to derive the set it guards from the tree, so that the next iteration or the next call site is covered without anyone remembering to add it.
43. As a maintainer, I want a guard that bans an unsafe rendering to ban the hazard rather than one call shape, so that an indirection cannot walk past it.
44. As a maintainer, I want the scheduler's region-capability question answered in one place, so that three resolvers cannot drift into three different answers.
45. As a maintainer, I want a per-field bundle contract to be carried by a shared step rather than by two builders kept in step by hand, so that the next optional field does not fall into the same hole.

## Implementation Decisions

Clusters are ordered by the seam they share, not by severity. Severity is stated per item:
**P0** breaks correctness, data or security; **P1** is wrong behaviour in a real case; **P2**
is latent or cosmetic. Every item carries one of those three and nothing else in the severity
slot. Three SCOPE tags travel beside a severity and never instead of it, because an implementer
has to be able to rank every item on one scale: `structural` marks an item that is not a defect
on its own and exists because the items above it need a seam, `architectural` marks a gap in a
property rather than at a single site, and `security` marks one whose failure discloses a secret
or crosses a trust boundary. A scope tag changes what the item IS, never how it ranks, and a
header carries a severity, at most one scope tag, and a confidence — nothing else. Anything that
is none of those three things, including where a defect came from, belongs in the item's body. Confidence is stated for every item too, and there are three values, not two: `CONFIRMED` means the failure follows from code I read at `main`; `PLAUSIBLE` means the
code was read and the failure still needs a run to settle; `REPORTED` means it came from one
of the six reviewers and was NOT re-read at `main` by hand. `REPORTED` is not a lesser defect
— several are P1 — it is a lesser warrant, and an implementer should confirm it against the
tree before acting, because that step has not been done.

An item may SPLIT its confidence across exactly two named axes, and only these two:
`mechanism` — does the code do the thing described — and `occurrence` — does the state that
reaches it arise. The split exists because collapsing it lies in one direction or the other:
C1's batch abort demonstrably stops every ledgered dispatch, while whether an invalid item
arises is unsettled, and one value would either overstate the reachability or understate the
mechanism. The form is `(P1, mechanism CONFIRMED, occurrence PLAUSIBLE)`, the axis names are
fixed, and a header carrying anything else is malformed — including the two ad-hoc spellings
C1 and C2 used until this revision, which my own header check accepted because it carried a
second branch written for them instead of a rule they had to meet.

### Cluster A — the carrier is a chokepoint, not a reminder

The root decision: **a job must not be able to leave a producer without crossing the carrier
stamp.** Today `dispatch.WithCarrier` is a call each branch of the materializer has to
remember, and one branch does not. Downstream, three consumers infer generation 4 from the
*presence of the window field in the payload* rather than from `DeliveredJob.CarrierGeneration`,
so a single producer slip becomes a coverage claim.

- **A1 (P0, CONFIRMED).** The credentialed materializer in `internal/store/materialize.go`
  has three exits and stamps the carrier on two. A type that is credentialed by schema but
  yields no envelope fields — `promql` with `auth_mode: none`, `rabbitmq` with `mode: amqp`,
  both `CredentialForbidden` variants — leaves with protocol version 1 and the window field
  still populated. The window instant is selected unconditionally, so this reproduces with
  `ledger.carrier_enabled: false` as well. **Decision:** the decided property is that an
  unstamped job is UNCONSTRUCTIBLE, not that a third call is added. A single exit and a
  constructor that refuses an unstamped job are two shapes of that one property, with the same
  discharge either way, so choosing between them is an implementation detail and not an open
  alternative.
- **A2 (P2, REPORTED).** `terminalCarrierGeneration`, the ingest claim path and the correlation path
  read generation from payload shape. **Decision:** every one of them takes the carrier from
  `DeliveredJob`, and the payload's own protocol version is treated as body content.

  **DEPARTED FROM, and this is the record of it.** The decision is not implementable at two of the
  three sites it names. `terminalCarrierGeneration` and `correlateExpectedRun` run in the CORE and
  receive a `domain.Heartbeat`; there is no `DeliveredJob` there, and the comment above
  `terminalCarrierGeneration` already argues at length why carrying the executor's carrier
  observation back would mean putting it in the payload — "precisely the source the design forbids
  and the P0 that killed revision 6". Implementing the decision verbatim would have reversed a
  ruling made deliberately, with its reason written down.
  
  What was built instead is the decision's implementable content, in two halves. The PRODUCER stops
  reading its own choice back off the body: `dispatch.CarrierFor`'s answer is held as a value and
  `MaterializedExecution` reports the generation beside the job it stamped, so the ledger records a
  decision rather than a field. The CONSUMER half is A3, at the one place a `DeliveredJob` exists.
  And after A1 the remaining inference — a result that correlates proves generation 4, because only
  generation 4 carries the window — is sound by construction rather than by convention, which is
  what made it unsound before.
- **A3 (P2, CONFIRMED).** `ClaimHeartbeat` in `internal/dispatch/credentials.go` decides
  from payload fields while its comment claims the carrier rule is the whole of the refusal
  on that side. It receives the job body and cannot see the carrier. **Decision:** give it the
  carrier. Deleting the claim from the comment is refused as the alternative: the comment states
  the rule the rest of cluster A is restoring, so weakening the sentence to match a weaker
  mechanism is the wrong side to give way.
- **A4 (P2, CONFIRMED).** The pull agent publishes a batch's claims **before**
  `ValidateAndMaterialize`; the AMQP worker publishes **after**, and states the rule. One
  §8.4 rule, two transports, opposite evidence for a refused job. **Decision:** the AMQP
  ordering is the rule; the pull agent moves its publish after the gate.
- **A5 (P2, CONFIRMED).** The generation-4 queue-name helper in `internal/dispatch/amqp.go`
  is the only one of its six siblings without the empty-region default. Unreachable today;
  a publisher with an empty region would silently create an unconsumed queue. **Decision:** the
  helper takes the same default as its siblings; the six become one helper parameterised by
  generation, so a seventh cannot be written without it.
- **A6 (P2, CONFIRMED).** `LiveJobRegions` in `internal/mqadmin/mqadmin.go` strips the
  generation-2 and -3 prefixes but not generation 4, so a v4 consumer registers a phantom
  region in the union that feeds the region-worker alert. **Decision:** the prefix set the
  stripper knows is derived from the generations the dispatcher can publish, not restated.
- **A7 (P2, CONFIRMED).** Migration `00101_pull_carrier_generation_4.sql` widens the pull
  **test** table's protocol version to admit 4, and no generation-4 test carrier exists on
  any surface. The alternative — a comment saying the value is reserved — is refused rather
  than offered: it is a sentence where a constraint belongs, which is the shape this whole
  package is about, and it leaves the blackhole the migration's own header says the CHECK
  exists to prevent.

  **Decision: a NEW forward migration, not an edit to `00101`.** `00101` is applied; rewriting
  its text changes nothing on any deployed database and would only move the fresh-install
  schema, so "narrow the CHECK back" would have described an install and not an upgrade. The
  new migration:

  1. narrows **`pull_tests`** only, to `protocol_version IN (1, 2, 3)`. `pull_jobs` keeps 4 and
     is not touched: generation 4 is real on the job path and reserved-and-unreachable only on
     the test path, which is the whole asymmetry this item is about.
  2. **fails closed, with a count**, if `pull_tests` already holds a generation-4 row. Narrowing
     would fail on its own — `ADD CONSTRAINT` validates the rows already present — but the bare
     message names no table, no count and no procedure. The guarded `DO` block that `00101`'s
     own Down already uses is the pattern: refuse, say how many and where, and delete nothing.
     A row at generation 4 on the test path can only be a payload no build emits, so the
     refusal is a diagnosis, not an operational chore; it still must not discard the row.
  3. carries a **Down that re-widens `pull_tests` to `(1, 2, 3, 4)`** unconditionally. Widening
     validates nothing and can never fail, so the rollback contract here is the easy direction —
     the asymmetry is worth stating precisely because `00101`'s Down is the hard one.

  This makes the item's discharge an upgrade-path statement rather than a fresh-install one.

  Measured on PostgreSQL 16.14 against a throwaway instance, not reasoned — the same discipline
  C3 needed after its first revision was wrong. `pull_tests` is a PLAIN table, not partitioned,
  so none of C3's partitioned-index restrictions apply here. The combined
  `ALTER TABLE … DROP CONSTRAINT IF EXISTS …, ADD CONSTRAINT … CHECK (…)` narrows cleanly, is
  re-runnable unchanged, and widens back the same way. With a generation-4 row present it fails
  with exactly the message the guard exists to replace — `check constraint … of relation
  "pull_tests" is violated by some row`, naming no count and no procedure. One further fact the
  first draft did not state and a reader would need: because the drop and the add are ONE
  statement, a failed narrowing rolls back atomically and the WIDE constraint survives, so a
  refused upgrade never leaves the table unconstrained.

### Cluster B — a canary is an ordinary monitor everywhere it can be

- **B1 (P0, security, CONFIRMED).** The canary origin comparison in
  `internal/prober/canary.go` normalises host and port and ignores the scheme, defaulting an
  absent port to 443 for both schemes. An `https` → `http` redirect to the same host is
  therefore "the same origin" and binding-backed headers survive it. `https` is enforced only
  at write time, in `internal/domain/canary.go`. **Decision:** one executable hop predicate,
  stated here rather than as "the same rules", because the write-time validator is not
  applicable to a hop verbatim — it also enforces template rules that have no meaning on the
  wire, where substitution has already happened. The hop predicate is the write validator's
  **address-independent, non-template subset**, and nothing else. It is enforced in the
  redirect policy, where the client has already parsed the hop URL and will not call the
  policy at all if it could not — so the write validator's parseability rule has no runtime
  half, and stating it as a fourth condition would be a condition that cannot fail. Three
  conditions, all of which can:

  1. the hop's scheme is `https`;
  2. it names a host;
  3. it carries no userinfo.

  The placeholder rules — at most one correlation placeholder, occupying one whole path
  segment, legal only in the completion URL — stay write-time only and are NOT hop rules.
  The address-level policy (loopback, link-local, private, metadata, rebinding) is unchanged
  and stays where it already is, in the executor's dialer after resolution.

  **Refusal contract.** A hop that fails the predicate is not followed: the request for that
  hop is never made, so no header leaves the process. The stage fails with a bounded reason
  drawn from the existing non-oracular vocabulary and carrying no URL, consistent with the
  rule that a stage failure never carries one. It is a stage failure and not a
  headers-stripped continuation, because a canary that quietly succeeds without its
  credential is a second false claim.

  Independently of the predicate, the origin comparison gains the **scheme**, so a
  cross-scheme hop is an origin change and binding-backed headers drop on it even in a
  future where some downgrade becomes legal.
- **B2 (P0, CONFIRMED).** The dispatch-shortage path in `internal/scheduler/scheduler.go`
  writes through the bare insert in `internal/store/heartbeats.go`, which touches no status,
  no confirmation counter, no transition outbox and no service bucket. Its own docstring says
  the opposite. **Seam ruled by the owner on 2026-09-06**, in answer to a direct question
  about this item's seam and about nothing else. It settles HOW B2 is built and tested; it is
  not approval of the item, of its severity, of its place in any order, or of this spec — all
  of which the banner above still governs. The ruling: route the shortage through the
  **existing result pipeline** — `RecordScheduledResult` in `internal/store/monitors.go` — so the
  monitor's own failure threshold decides the flip, the outbox carries the transition and the
  service SLI sees the sample. No new store method and no new seam. The bounded reason stays
  on the heartbeat message and in the refusal metric. Applies to every shortage reason, not
  only the missing-runner one.
- **B3 (P1, CONFIRMED).** The runner applies `retries + 1` attempts, each bounded by the
  monitor's own timeout, while the in-flight lease is one timeout plus a fixed slack and the
  pull claim lease is the same. Retries are bounded only by the generic maximum, and the
  create form offers the control for canaries. **Decision:** the type REFUSES a non-zero
  retry count, at every write surface and in the domain validator. The alternative — computing
  the lease from `attempts × timeout` — is refused, not offered: it makes a canary journey
  re-runnable, and a journey is a transaction whose retry is a second external side effect at
  someone else's expense.
- **B4 (P1, CONFIRMED).** `async_canary` is not a credentialed type, so the unknown-key
  rejection never runs for it and the create/update handlers pass the config map through
  verbatim. A client can set the run key that the scheduler otherwise mints per window, and
  the plain dispatch path yields to a value already present. A target honouring the
  idempotency key then returns the first task forever. **Decision:** a key whitelist for the
  canary's config on every write surface, with the executor-owned keys refused by name.
- **B5 (P1, REPORTED).** `role=all` announces canary capability for the default region only, while the
  in-process dispatcher ignores the region entirely. A canary in any other non-pull region is
  refused forever. **Decision:** the announcement is derived from the same predicate the
  dispatcher uses.
- **B6 (P2, CONFIRMED).** The await paths dereference the completion sub-documents without a
  nil check, two lines below a comment stating a schema-invalid document can reach there on a
  crafted carrier. No recover exists in the worker or the agent. **Decision:** the structural
  gate rejects the document; the dereference is not reached.
- **B7 (P2, CONFIRMED).** The canary semantic hash is documented as what the file provider
  compares to decide create/update/no-op and has no non-test caller; the file provider hashes
  the flat config instead. **Decision:** call it or delete it, and fix the doc line either way.
- **B8 (P2, PLAUSIBLE).** Path escaping of the target-supplied correlation id leaves dot
  segments live, so a normalising proxy can change the path the completion rule is about.
  **Decision:** the correlation id is rejected rather than escaped when it is not a single safe
  path segment, which is the same rule the write-time placeholder check already applies to the
  segment the id will occupy.
- **B9 (P1, CONFIRMED).** The canary editor's read path in
  `frontend/src/lib/canaryWorkflow.ts` parses with the plain JSON parser and stringifies
  values, while the write path preserves exact number tokens and the server was fixed to do
  the same. Opening a saved canary and changing its name rewrites its numbers, its canonical
  document, its semantic hash and the bytes sent to the target. **Decision:** the read path
  uses the same token-preserving reader as the write path, and the round trip is pinned by the
  existing cross-surface seam fixture.

### Cluster C — the ledger records evidence without gating delivery on a batch

- **C1 (P1, mechanism CONFIRMED, occurrence PLAUSIBLE).** `ReserveExpectations` in
  `internal/store/expectedruns.go` returns on the first item that fails validation, and the
  scheduler treats a batch error as "hold every ledgered dispatch this tick". A single bad
  item therefore stops all ledgered probing, and recurs on the next tick. The function's own
  comment promises per-element isolation for exactly this reason. **Decision:** validate per
  element, skip the bad one with a recorded reason, and let the rest of the batch proceed.
- **C2 (P1, mechanism CONFIRMED, occurrence PLAUSIBLE).** The publish gate reads the rows returned by
  the schedule update, not by the window insert, and the window insert does nothing on
  conflict. A repeated due instant therefore publishes a job whose window row belongs to a
  different job id, and the run is recorded nowhere. **Decision:** the gate reads a return
  value produced by the statement that wrote the window row.
- **C3 (P2, CONFIRMED).** The partial index on the job identity in
  `00102_expected_run_ledger.sql` serves no query in the tree; every event statement binds the
  due instant and hits the primary key. It costs a non-HOT update on the hottest table, against
  the storage argument the spec itself makes. **Decision:** drop it. The alternative — produce
  the query the index was built for — is not an alternative today: no such query exists, and
  inventing one to justify an index is backwards. If a future read needs it, the index returns
  with that read and is justified by it.

  **Upgrade path.** `00102` is applied, so deleting the `CREATE INDEX` from its text moves the
  fresh-install schema and leaves the index in place on every deployed database. A NEW forward
  migration drops it.

  It is a PLAIN `DROP INDEX IF EXISTS`, and it carries NO `-- +goose NO TRANSACTION`. An earlier
  revision of this item said the opposite — `DROP INDEX CONCURRENTLY` on the hot table, hence the
  no-transaction pattern — and that was wrong three times over, because `expected_runs` is
  `PARTITION BY RANGE (due_at)` and the index is therefore a PARTITIONED index. Measured on
  PostgreSQL 16.14 against a throwaway instance with this exact shape, rather than reasoned:

  - `DROP INDEX CONCURRENTLY IF EXISTS` → `ERROR: cannot drop partitioned index … concurrently`
  - `CREATE INDEX CONCURRENTLY` on the parent → `ERROR: cannot create index on partitioned table … concurrently`
  - plain `DROP INDEX IF EXISTS`, then plain `CREATE INDEX IF NOT EXISTS`, then a re-run of the
    same create → succeed, the re-run reporting `NOTICE: relation … already exists, skipping`

  So `CONCURRENTLY` is not available in either direction here, the no-transaction pattern has
  nothing to enable, and the invalid-index hazard that would make `IF NOT EXISTS` a silent no-op
  over a broken index does not arise: it belongs to interrupted CONCURRENTLY builds, and a plain
  `CREATE INDEX` that fails rolls back leaving nothing behind. The Down is the same statement
  `00102` used.

  `IF EXISTS` stays on the drop, but NOT for the reason first given here. A fresh install does
  have the index at that point — `00102` runs first — so "a fresh install never had it" was
  false. It is there for re-runnability and for a database where an operator already dropped it
  by hand.

  The cost to state plainly: a plain `DROP INDEX` on a partitioned index takes `ACCESS EXCLUSIVE`
  on the parent and each partition. It is brief because dropping an index rewrites no data, but
  it is a lock, and this is the ledger's busiest table.

  One dimension this item does NOT have, stated because a reader would reasonably look for it:
  the adaptive-storage rule does not apply. `expected_runs` is declaratively partitioned in both
  storage modes — `internal/store/expectedrunretention.go` says so where it explains why its
  retention has no TimescaleDB branch — so running this in both modes would prove nothing a
  single run does not.
- **C4 (P2, CONFIRMED).** The claim-retry rationale, and the retry limit sized from it,
  describe the ordering that phase F inverted. **Decision:** re-measure, then restate.

  **The measurement, with its numbers.** Taken on the `role=all` dev instance carrying a full day of
  generation-4 traffic: **20052** issued windows at `carrier_generation = 4`, of which **20050**
  hold a `claimed_at` — 99.99%, against the 1.6% ("one claim across sixty-two windows") the old note
  recorded. The two that do not were never reserved at all (`reserved_at IS NULL`): they are §8.3
  ADOPTIONS, windows a terminal event created for a run the leader had not recorded, and no retry
  could have matched them because there was no row to match at any point.

  **What the restatement had to concede.** Under phase F's ordering a re-offer cannot convert an
  unmatched claim into a matched one: the window is committed before the job leaves the process, so
  every reachable unmatched shape — an adoption, a job id or revision the window does not carry, a
  window past the correlation floor — is unmatched for a reason two seconds do not change. The
  mechanism is KEPT and both comments now say that, because removing it is a behaviour change and
  not a re-measurement. Whether to remove it is a decision worth taking on its own terms.
- **C5 (P2, CONFIRMED).** A credential backoff advances the next due instant by the delay
  while writing the snapshot's interval as the interval in force. For intervals below the
  delay floor the grid instants between are neither materialized nor fenced, and the next
  window's lateness threshold is the longer interval — the over-claiming direction.
  **Decision:** the column records the interval that actually produced the instant, so a backoff
  writes the delay it used.
- **C6 (P2, CONFIRMED).** The retained floor ignores the default partition, so after a
  multi-day scheduler outage the floor jumps forward and declares unanswerable a span whose
  rows are still stored and still listed. **Decision:** the floor counts the default partition,
  because a row that is listed must be answerable.
- **C7 (P2, CONFIRMED).** The confirm statement writes the issue instant from the tick's start
  clock rather than the publish instant, systematically shrinking measured lateness — the same
  direction the design says it refuses. **Decision:** the instant is taken at the publish, and
  travels with the confirm rather than being re-read from the tick.
- **C8 (P2, CONFIRMED).** The correlation floor is a rolling window from now; the purge cutoff
  is midnight-aligned. Up to a day of rows exist, are listed, and refuse correlation.
  **Decision:** one owner for the cutoff — the correlation floor reads the same aligned instant
  the purge uses.

### Cluster D — a guarantee is carried by a statement, not by a sentence

- **D1 (P1, CONFIRMED).** The bundle parser routes `async_canary` to a dedicated builder
  before the generic one, and the dedicated builder in `internal/fileprovider/canary.go`
  never copies the description. The raw monitor accepts the key, so strict field checking
  passes it, and the canonical hash in `internal/fileprovider/canonical.go` folds the
  description — which is always empty for this type — so a later edit is a permanent no-op.
  **Decision:** the two builders share one struct-to-struct step for every non-type-specific
  field, so the next optional field cannot fall into the same hole (see G5).
- **D2 (P2, CONFIRMED).** The project audit insert in `internal/store/incidentaudit.go` is an
  `INSERT … SELECT` whose zero-row case is neither an error nor an audit row, while the file
  promises that an unattributable incident write does not happen. Unreachable today through
  the foreign keys; the guarantee is carried by nothing. **Decision:** the decided property is that a
  write which produced no audit row FAILS. Checking rows affected and resolving the tenant
  before the insert are two shapes of it, with the same observable outcome and the same
  discharge, so neither is an open alternative.
- **D3 (P2, CONFIRMED).** An internal incident write in `internal/store/incidents.go` takes a
  pointer actor and has a live nil branch; the single caller passes the address of a value
  parameter, so the branch is unreachable today, and the guard that enumerates system doors
  counts declarations only. **Decision:** make nil **unrepresentable** — the internal function
  takes the actor by value — rather than deleting the branch and leaving the pointer. Deleting
  the branch alone would earn nothing: the parameter would still accept `nil`, a future
  in-package caller would still compile, and the failure would move from an unaudited write to
  a nil dereference. With the value parameter, `nil` is a compile error, which is what makes
  this item's `compile` kind honest rather than assumed.
- **D4 (P2, CONFIRMED).** Two system doors are declared on the API's store interface in
  `internal/api/api.go` with no caller in the package, so every fake must implement them and a
  hand-reachable unaudited door stays in the API's own contract. **Decision:** delete both
  declarations. Note what does *not* discharge this: the compiler proves nothing here, because
  there is no caller to refuse and a fake carrying extra methods still satisfies a narrower
  interface. `TestTheStoreDeclaresExactlyTheSystemDoorsThatHaveMachineCallers` in
  `internal/api/incidentdoors_test.go` asserts the declared set and is what must be extended to
  cover the API interface too.
- **D6 (P2, CONFIRMED).** The comment above that interface describes an Alertmanager exemption
  that no longer exists — the receiver uses the principal doors, and
  `TestTheAPINeverCallsASystemDoor` in the same file states there is no exemption now — and it
  cites its guard under a name the tree does not have, so a name check would not catch the
  drift. Split from D4 because it is a different defect with a different discharge: D4 removes
  a door, D6 removes a false sentence about doors. **Decision:** rewrite the comment to the
  current rule, and extend the citation guard so that a `Test`-prefixed name cited in a Go
  comment must resolve, the way one cited in a living document already must. That is the
  mechanism the finding is about; correcting the name alone would leave the next drift
  uncaught.
- **D5 (P2, CONFIRMED).** In `internal/store/migrate.go` the comment above the version check
  was corrected to six migrations and the operator-facing refusal below it still lists five,
  omitting the migration that most recently created the requirement. Half the patch landed.
  **Decision:** derive both from one list, and extend the guard (F3) so the third site is
  covered.

### Cluster E — a rendering claims no more than its facts

- **E1 (P1, CONFIRMED).** In
  `frontend/src/views/EscalationView.vue` the schedule editor pre-fills a `datetime-local`
  control by slicing the zone marker off an RFC-3339 instant; the control reads its value as
  local and the save path converts it back as local, so editing anything about a schedule
  moves its rotation anchor by the viewer's offset. This is the ONE item in the package whose
  defect predates `v0.1.8`: the slice arrived in August. What this range added is the NFR-025c
  zone hint beside the control, which now positively asserts a zone the value is not in — so
  the range did not create the bug, it made the UI claim the bug away. The provenance is stated
  here and not in the header, because the header carries severity, an optional scope tag and
  confidence, and nothing else. **Decision:** the pre-fill converts to the viewer's
  zone; the hint keeps its claim; and the round trip is pinned by a surface assertion at a
  non-UTC test zone.
- **E2 (P1, CONFIRMED).** The storage verdict in `frontend/src/lib/reliabilitygeometry.ts`
  computes a tail measure and has no shape for it: everything that is not interior falls to
  the prefix shape, so a segment whose records stop early renders "records begin later in
  this segment". **Decision:** a fourth shape, with its own sentence.
- **E3 (P1, CONFIRMED).** The latency panel subtitle in
  `frontend/src/views/MonitorDetailView.vue` states that the panel draws no stroke and makes
  no claim about whether a check was due, while the expectation strokes are drawn below it and
  the legend explains them. **Decision:** the subtitle is derived from whether strokes were
  drawn, and the assertion that pins it runs in both mounts.
- **E4 (P1, REPORTED).** In the same view the expectation cell width is derived from the number of
  points while cells are positioned at window instants. When windows outnumber points a cell
  overpaints its neighbours, and because the windows arrive newest-first the oldest paints
  last, so a covered cell can cover the missed windows beside it. The hit rectangles overlap
  identically, so the readout can name a window the pointer is not over. **Decision:** the
  width comes from the window grid.
- **E5 (P1, REPORTED).** A segment whose series request is pending or failed reaches the storage verdict
  with an empty series and renders the prefix sentence from no data at all, beside the loading
  line. **Decision:** an absent series is a distinct state and renders as one.
- **E6 (P2, REPORTED).** The per-segment range label truncates to whole days, so two revision boundaries
  inside one day render identically. **Decision:** the label renders the extent it actually
  has, to the precision that distinguishes two boundaries.
- **E7 (P2, REPORTED).** The legend names the drawn population while the statistic is computed over the
  measured one. **Decision:** the legend names the measured population, since that is what the
  statistic is over.
- **E8 (P2, REPORTED).** The out-of-scale note is gated on a measured average, so when nothing recorded a
  latency the timeout is neither drawn nor declared off the scale. **Decision:** the note is
  gated on whether the timeout is on the scale, not on whether an average exists.
- **E9 (P2, PLAUSIBLE).** A marked sub-second outage rounds to whole seconds and reads as zero
  under a label promising its exact duration. **Decision:** the readout carries the precision
  its own label promises.
- **E10 (P2, CONFIRMED).** `openapi.yaml` hardcodes a fourteen-day retention window in an
  endpoint description while documenting the bound as configurable in the same file.
  **Decision:** the description is CHECKED AGAINST the domain default by a guard, not
  "derived". A static schema document cannot read a runtime value, so deriving is not on
  offer, and saying it was would be a discharge nobody could perform; a guard that fails when
  the two disagree is.
- **E11 (P1, architectural, REPORTED).** The NFR-025c guard bans a call shape — the ISO serializer and the
  date field getters — rather than the hazard, which is any value that becomes a wall clock.
  E1 walks past it with a slice of an API string. Also unbanned and reachable: the epoch
  getter, the offset getter, the UTC and date string forms, and a direct locale formatter. The
  control-surface inventory matches a literal attribute, so a bound attribute or a wrapper
  component disappears from it. **Decision:** the guard bans the hazard — no product module
  outside the two owner modules may derive a displayable value from an instant by any route —
  and the control inventory is derived rather than pattern-matched.

### Cluster F — configuration and the guards themselves

- **F1 (P2, CONFIRMED).** The central loader populates defaults for its sibling sub-configs and not for the
  ledger's, while the validator's message promises a default it does not supply; the value is
  resolved instead in five runtime call sites across the store and the scheduler. No
  behavioural defect today — every reader goes through a setter that defaults — but it
  breaches the rule that defaults live in the loader, and it is one number in five places.
  **Decision:** add the block to the loader and delete the coercions.
- **F2 (P2, CONFIRMED).** A constant naming the phase that made the carrier flag admissible is referenced only
  from its own comment; unused constants do not fail the build. **Decision:** delete it, and
  keep the sentence.
- **F3 (P2, CONFIRMED).** The guard that keeps the PostgreSQL-15 migration list honest matches a backticked
  phrase, which covers the project README and the code comment and not the operator-facing
  refusal string, which is worded differently. That is why D5 drifted. **Decision:** the guard
  derives the list from the tree and checks every site that states it, selected by content
  rather than by phrasing.
- **F4 (P2, CONFIRMED).** The iteration-count guards are enumerated by hand, one call per iteration. The next
  iteration is unguarded until someone remembers to add a line, while the set of iterations is
  derivable from the directory. **Decision:** derive the set.
- **F5 (P2, CONFIRMED).** A comment in `.github/workflows/tests.yml` states a unit-test count
  that has since drifted. **Decision:** the count is DROPPED from the comment. "Derive it" is
  refused as the alternative and the reason is worth writing down, because it is the same
  reason F3 and F4 exist: deriving would mean teaching a guard to read a CI workflow and count
  vitest files, which buys a number nobody needs at the cost of a guard nobody would maintain.
  A comment that says what the job does, with no figure in it, has nothing to drift.

  This collapses the branch the map carried while this item was undecided: with the count
  dropped the kind is `green` — there is no site to revert, so no guard can fail by name, and
  claiming one would be the over-claim this package is about.

### Cluster G — structural work the clusters above depend on

- **G1 (P2, structural, REPORTED).** The scheduler's leader loop holds three independent resolvers for the same question —
  what a region's executors can consume — each merging the same three sources with the same
  pull-region exclusion: credential readiness inline, the ledger carrier as a closure, and the
  canary announcement as a method. A5, A6 and B5 are all that surface leaking. **Decision:**
  one region-capability step returning a per-region record; the loop only dispatches.
  **Implementation note.** The first pass moved two resolvers of the three and left
  `canaryAnnouncements` holding its own copy of the exclusion, while the comment beside the record
  claimed all three shared it — the defect this package exists to find, inside this package's own
  fix for it. Completed rather than recorded as a deviation: unlike A2 and C4 nothing made the
  decision's letter unimplementable, the obstacle was the order of two statements. The lazy
  announcement stays lazy — the closure decides WHEN to resolve, the record decides what the
  resolver READS — and the shape is now guarded, because no behavioural test can see a second
  owner: the record is built FROM that map, so both readers return the same bool for every input.
- **G2 (P2, structural, REPORTED).** The plain and credentialed dispatch branches repeat the same canary admission
  sequence — availability, slot claim, skip reason. **Decision:** one admission function
  returning the skip reason.
- **G3 (P2, structural, CONFIRMED).** The monitor detail view re-types the ledger's minimum retention bound as a literal,
  while the canary and monitor bounds ship shared fixtures for exactly this. **Decision:** a
  fixture.
- **G4 (P2, structural, REPORTED).** `internal/store/expectedrunretention.go` carries retention, the read API's cursor
  codec and pagination, and a metrics sampler under a name that describes one of the three.
  **Decision:** split the list/cursor half out.
- **G5 (P2, structural, CONFIRMED).** The file provider has per-type builders behind a per-field contract, kept in step by
  hand, with no shared struct-to-struct step and no test that iterates types. D1 is that gap
  realised. **Decision:** the shared step, plus a test that asserts every declared type carries
  every non-type-specific field.
- **G6 (P2, structural, REPORTED).** Two delegations exist only to anchor a comment — an internal spelling of an exported
  accessor, and a function returning a bare constant. **Decision:** inline them, keep the
  comments.

## Testing Decisions

A good test here asserts **external behaviour at the highest existing seam** and would fail
if the mechanism were removed. The dominant defect class in this range is a test that names a
mechanism it never reaches: several of the findings above are pinned by assertions that run
only in the mount or the configuration where the defect cannot appear. So a test for a finding
must be written in the state the defect needs, not in the state the surrounding suite already
mounts.

**What each item owes, and what it does not.** An earlier draft of this section promised a
test and a killing mutation for *every* item. That was the same over-claim this package is
about: a third of the items change no observable behaviour, so a mutation for them could only
be theatre. The rule is therefore stated by kind, and the discharge map below assigns every
item exactly one.

Exactly one, with one explicit exception and no implicit ones. An item whose decision offers
**alternative fixes** carries a `branch:` kind naming the kind of each alternative, and the
branch collapses to one kind the moment the alternative is chosen. A composite kind is not
allowed: an item that seems to need two is two findings, and D6 was split out of D4 for
exactly that reason. The branch row is B7 alone — F5 carried one while it was undecided and
lost it when the alternative was resolved — and that set is stated here so the map can be read
against it.

| Kind | What discharges it | Mutation required |
| --- | --- | --- |
| `behaviour` | A test at the seam named below, written in the state the defect needs | **Yes**, recorded with the fix |
| `guard` | A guard that fails on the reverted site, plus its fixture case in the guard's own suite | **Yes** — reverting one site must fail it by name |
| `compile` | The deletion IS the experiment: remove the symbol, and a surviving reference fails the build with an undefined name. Valid only where a reference would be a compile error — so for a symbol, not for an interface method a fake may keep, and not for a comment | No — the build is the result |
| `green` | A restructuring with no observable change: the existing suites stay green and the seam it creates is used by the items that depend on it | No, and none is claimed |
| `record` | A claim corrected against a re-measurement, recorded in the iteration report with its numbers | No — the measurement is the evidence |

A `green` item is not evidence-free by choice: it is evidence-free by construction, and saying
so is the point. Three of them (G1, G4, G6) exist only so that the `behaviour` items above
them have one place to be tested, and their real discharge is that those items pass.

Seams, all of them pre-existing — no new seam is proposed:

- **Store and ledger (A1, C1–C8, D1–D3, D5).** The package-internal store suites against a live
  `cerbix_test`, alongside `internal/store/expectedruns_internal_test.go`,
  `internal/store/expectedrunretention_internal_test.go` and
  `internal/store/incidentaudit_internal_test.go`. A1 in particular must be exercised with a
  credentialed-schema-but-no-credential monitor, which no existing case builds.
- **Dispatch and carriers (A2–A7).** `internal/dispatch/carrierenvelope_test.go` and
  `internal/dispatch/ledgercarrier_test.go` for the stamp and the admissibility predicate;
  `internal/worker/storeboundary_test.go` and `internal/agent/storeboundary_test.go` for the
  claim ordering, which is where the two transports currently disagree.
- **Canary execution (B1, B3, B6–B8).** `internal/prober/canary_test.go` with an httptest
  server that answers a scheme-downgrading redirect — the header assertion is on what the
  second hop received, not on what the policy decided.
- **Scheduler (B2, B5, C1, C7, G1, G2).** `internal/scheduler/canary_test.go` and
  `internal/scheduler/expectedrun_test.go`. B2 asserts through the result outcome the pipeline
  returns, not through the presence of a heartbeat row: a row exists in both the broken and the
  fixed version, which is why this was invisible.
- **API surface (B4, D4).** `internal/api/api_canary_test.go` and
  `internal/api/incidentdoors_test.go`; B4 posts the executor-owned key and expects a refusal.
- **File provider (D1, G5).** `internal/fileprovider/canary_test.go`, with the type-iterating
  assertion in G5 rather than one more per-type case.
- **Frontend (B9, E1–E9, E11).** The vitest surface specs beside each view —
  `frontend/src/views/SlaView.spec.ts` and `frontend/src/views/ResponseTimePanel.spec.ts` are
  the prior art for asserting what an operator reads rather than what a helper returns. The
  whole suite must stay green at a non-UTC test zone as well as at UTC, which is what makes E1
  and E6 evidence rather than a claim.
- **Guards (F3–F5).** `scripts/check_docs_references_test.py`, which already invokes each guard
  against a fixture rather than modelling it.
- **Live stack (A1, B1, B2, B5).** `e2e/tests/expected-runs.spec.ts` and
  `e2e/tests/async-canary.spec.ts` on `make dev-test`, plus `make geo-test` for the two
  findings that only a real region boundary reproduces. The dev stack already runs with the
  ledger carrier on and envelopes enforced, so A1's monitor class needs only to be added to
  the fixtures.

### Discharge map

Every item in *Implementation Decisions* appears exactly once. The map is the contract: an
item is not done until its row is discharged, and an item with no row is a defect in this
spec rather than an item without a test.

| Item | Kind | Seam |
| --- | --- | --- |
| A1 | behaviour | store, plus the live stack |
| A2 | behaviour | dispatch + store |
| A3 | behaviour | dispatch |
| A4 | behaviour | worker and agent store-boundary suites |
| A5 | behaviour | dispatch |
| A6 | behaviour | the broker-admin unit suite |
| A7 | behaviour | store, on a MIGRATED database: `pull_tests` rejects generation 4, `pull_jobs` still accepts it, and the narrowing refuses with a count when such a row exists |
| B1 | behaviour | prober, plus the live stack |
| B2 | behaviour | scheduler + store, plus the live stack |
| B3 | behaviour | prober + scheduler |
| B4 | behaviour | API |
| B5 | behaviour | scheduler, plus the geo stack |
| B6 | behaviour | prober |
| B7 | branch: `compile` if deleted, `behaviour` if given a caller | deletion, or the prober suite |
| B8 | behaviour | prober |
| B9 | behaviour | the SPA suite, through the cross-surface seam fixture |
| C1 | behaviour | store + scheduler |
| C2 | behaviour | store |
| C3 | green | a plain forward migration drops the partitioned index; the store suite stays green |
| C4 | record | re-measured, recorded with its numbers |
| C5 | behaviour | store |
| C6 | behaviour | store |
| C7 | behaviour | scheduler |
| C8 | behaviour | store |
| D1 | behaviour | file provider |
| D2 | behaviour | store |
| D3 | compile | the internal actor parameter becomes a value, so `nil` is a compile error |
| D4 | guard | the declared-door assertion in the API suite, extended to the API interface and to everything it EMBEDS — named and inline elements through ONE recursive walk at any depth, an unresolvable element reported rather than read as absence, and a cycle guard |
| D5 | guard | the extended migration-list guard (F3) |
| D6 | guard | the citation guard, extended to `Test` names cited in Go comments — BOTH spellings, backticked and bare, with three stated rules for what is not a citation and a fixture per rule |
| E1 | behaviour | the SPA suite at a non-UTC zone, plus the live stack |
| E2 | behaviour | the SPA suite (pure function) |
| E3 | behaviour | the SPA suite, asserted in the ledger-on mount |
| E4 | behaviour | the SPA suite |
| E5 | behaviour | the SPA suite |
| E6 | behaviour | the SPA suite |
| E7 | behaviour | the SPA suite |
| E8 | behaviour | the SPA suite |
| E9 | behaviour | the SPA suite |
| E10 | guard | a guard comparing the description's bound to the domain default, with BOTH sources injectable so the failing cases — a disagreeing number, a deleted sentence, a lost constant — are each pinned by a fixture |
| E11 | guard | the widened rendering guard, matching the locale formatter with or without `new`; reverting one site fails it by name |
| F1 | behaviour | the config package's own suite: the default comes from the loader |
| F2 | compile | deletion |
| F3 | guard | the guard's own fixture suite, over a DERIVED site set — every present-tense file that states something about the column-list form, records of past events excluded by rule — with the derivation itself driven end-to-end over a synthetic tree |
| F4 | guard | the guard's own fixture suite |
| F5 | green | the count is dropped; there is no site left to revert |
| G1 | guard | the seam A5, A6 and B5 are tested through, plus a source guard that the pull-region exclusion has ONE owner; reverting any resolver to the raw map fails it BY NAME |
| G2 | green | the scheduler suite stays green |
| G3 | behaviour | the SPA suite, through the shared bounds fixture |
| G4 | green | the store suite stays green |
| G5 | behaviour | the type-iterating file-provider assertion |
| G6 | green | inlining; the suites stay green |

Counts are deliberately absent from this section. The map is checked as a SET against the
item identifiers in *Implementation Decisions*, in both directions, and a number stated beside
it would be a second thing to keep true.

Full gate per cluster: `go build`, `go vet`, `-race` with the forty-minute timeout, `make
docs-check`, the frontend build and unit suite, and the browser suite on a live stack.
Storage-adaptive changes, if any land here, run in both hypertable and declarative-partition
mode.

## Out of Scope

- Assigning requirement numbers, opening iterations, and deciding the order of the clusters.
  Those are the owner's.
- Any change to what a canary journey *means* — the shape of the workflow document, the
  completion protocol, the semantic hash's inputs. B9 restores the round trip; it does not
  redefine the hash.
- The pre-existing schedule-anchor round trip beyond E1's fix: the on-call rotation model
  itself, override handling, and the escalation engine are untouched.
- Performance work on the ledger's ingest path. The review notes that generation 4 doubles
  results-queue volume and routes claims through the single ingest goroutine with two
  synchronous round trips each; that is a measurement task, not a defect, and belongs in its
  own iteration with numbers rather than in this package.
- Dependency upgrades. The range carries two patch bumps and nothing else.
- The two verbatim non-English quotations in `docs/specs/func-async-canary.md` and
  `docs/decisions.md`. The English-only rule has no quotation exemption, but altering a quoted
  ruling to satisfy a formatting rule trades a real property for a formal one. Raised here;
  the owner decides.

## Further Notes

- **Provenance.** These findings come from six parallel reviewers over disjoint slices of
  `v0.1.8..main`, each given its slice's own spec. Every P0 and most P1 items were then
  re-read at `main` by hand before being written down; the items marked PLAUSIBLE were not
  executed. `go build ./...` and `go vet ./...` are clean at `main`. The Go test suite was
  **not** run: the test database is shared and a concurrent run would have invalidated
  whatever gate was in flight.
- **Not reviewed.** `internal/outbox`, `internal/settings`, and the bodies of the new
  end-to-end specs (checked only for silent skips, of which there are none).
- **Why the clusters and not a severity list.** Six of the findings are one architectural
  shape each: a mechanism connected to its neighbour by a sentence. Fixing them one by one in
  severity order would restore the sentences. The cluster decisions above each replace one
  sentence with a mechanism, and the individual findings fall out of that.
