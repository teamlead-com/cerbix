# Implementation audit — iter-0183 through iter-0187 — 2026-09-20

Immutable post-close audit of the implementation and delivery evidence for:

- iter-0183 — contract and tenancy seam guards;
- iter-0184 — audit-log retention;
- iter-0185 — project-level inherited gate policy;
- iter-0186 — worst-of-all-windows gate evaluation;
- iter-0187 — onboarding.

The audit reads the canonical specs first, then the current tree, tests, operational rules and
iteration claims. Closed iteration reports remain historical records; this snapshot records where the
current tree does not sustain those claims.

## Result

**NOT APPROVED for an unqualified DONE claim.** The main feature paths are present and the hermetic
suite is green, but the audit found one P0 delivery-evidence failure, five P1 product/operations defects
and two P2 guard-quality gaps. The repeated failure mode is not missing implementation effort; it is
Cerbix accepting evidence from a narrower layer than the requirement names, then transferring the
unproved remainder to a comment, runbook sentence, opt-in environment or future operator.

## Findings

### P0 — FR-033 was marked DONE without its mandatory database proof

The retention spec requires PostgreSQL cases for cutoff equality, mixed global/organization rows,
batch continuation, row-lock contention, statement rollback, two-node fencing, connection poisoning,
migration up/down and a live backlog drain (`docs/specs/ops-audit-log-retention.md:119`). The owner
reports that cutoff, batching, rollback, fencing and live-drain checks were run in a separate session.
The audit cannot disprove that historical execution, but at audit opening the canonical repository did
not retain its command/output: the closed iteration report records the PostgreSQL matrix and live drain
as NOT RUN, and the tree then had configuration/metric tests but no reproducible store/scheduler
retention test. The live traceability row likewise said the DB-gated smoke “remains required before
production rollout”, while `docs/status.md` marked FR-033/NFR-027 DONE.

**Risk at audit opening.** The destructive path had no executable proof that the cutoff boundary,
batch loop, transaction behaviour or advisory-session fencing worked against PostgreSQL. This was a
Definition of Done violation for a data-deletion feature.

**Required closure.** Recover the missing evidence if it still exists, otherwise rerun the full
database matrix in both supported storage modes where applicable, exercise migration up/down and run a
live bounded drain. Preserve the commands/results and add reproducible killing tests before FR-033
returns to DONE.

**Remediation progress, 2026-09-20.** `TestAuditRetentionPostgreSQLMatrix` now passes against an
isolated PostgreSQL 16 probe database in normal and race modes. It proves exact-cutoff survival with
mixed organization/global rows, 251-row continuation across 100-row batches, `SKIP LOCKED` resume,
statement rollback, two-node lock exclusion/failover and a 2500-row live drain within the 30-second
budget. The finding remains open only for the spec's separate migration up/down and poisoned-session
cases, plus the P1 alert defect below.

### P1 — audit-retention alerts disagree with the valid configuration space and fail open on absence

`audit.purge_every` is valid from 5 minutes through 24 hours, but both alerts use a fixed 7200-second
threshold (`docs/alerts.yaml:5`, `docs/alerts.yaml:12`) while their copy promises “two healthy purge
cadences”. A valid cadence above one hour therefore raises a false backlog/staleness alarm. Conversely,
the alert expressions have no `absent()` branch. If the maintenance loop never starts, the store does
not implement the retention runner, or no successful owner ever publishes gauges, both alerts remain
silent because the time series does not exist.

**Required closure.** Export a cadence/configured health fact or generate cadence-aware rules, and add
explicit absence alerts. Test default, minimum and maximum cadences plus a never-emitted metric.

### P1 — the public inherited-policy read is not one database snapshot

`Store.EffectiveGatePolicy` claims one-snapshot resolution at
`internal/store/projectgatepolicy.go:214`, but executes service-scope, service-policy and project-policy
reads independently through the pool at lines 218, 226 and 234. A concurrent service-policy create or
delete can therefore produce a source tuple that never existed in one database snapshot. The gate
decision path uses `effectiveGatePolicyTx` and is safe; the public GET path in
`internal/api/handlers_gate.go:560` uses the non-transactional method.

**Risk.** API/SPA can display project inheritance while an explicit service policy already exists, or
return a service policy after its scope changed. This violates FR-034 invariants 1 and 7 even though the
decision writer itself is correct.

**Required closure.** Resolve scope and precedence in one read-only transaction/snapshot and add a
barrier regression covering service-policy create/delete between reads.

### P1 — CLI evidence promised by iter-0185/0186 was never implemented

FR-034 requires API, ledger, CLI and SPA to state the same source and revision. FR-035's plan requires
API, CLI, ledger and SPA evidence. `internal/cli/gate.go:69` decodes neither `policy_source`,
`policy_owner_id`, `policy_revision`, `window_mode` nor `evaluated_windows`; its human summary at line
341 prints only state, action, override and decision id. Neither feature commit changed the CLI.
`--json` preserves the raw response, but that does not satisfy the documented human-output contract.

**Required closure.** Extend the decoded response and stable summary grammar, keep exit codes unchanged,
and add golden tests for inherited service/project/none and one/all modes.

### P1 — migration 00109 cannot downgrade a database containing all-window policies

The down migration sets `window_name` NOT NULL before removing `window_mode`
(`internal/store/migrations/00109_gate_policy_all_windows.sql:71`). Valid all-window project and service
rows have `window_name IS NULL`, so downgrade fails immediately. Existing tests prove only 108→109
upgrade compatibility and do not execute 109→108 with an all-window row, despite the spec requiring
migration up/down.

**Required closure.** Declare downgrade unsupported and fail with an explicit preflight, or define a
loss policy approved in `docs/decisions.md`; then test it with service/project all-window rows and
ledger data. Do not silently invent a singular window.

### P1 — onboarding can render stale project evidence after a workspace switch

`DashboardView.load` has no request epoch or abort controller. The watcher at
`frontend/src/views/DashboardView.vue:395` starts a new load on organization/project change, while an
older load may still complete and assign monitors, cards, evidence, region state and onboarding state
at lines 233–243. A slower project-A request can overwrite project B after the selector and header have
already changed. Adjacent product code already treats this class as a P0 and tests it in `SlaView`, but
`DashboardOnboarding.spec.ts` has only the existing-install happy path.

**Risk.** A user can see stale authorized project-A monitor evidence under the project-B shell and the
guide can poll/open the wrong monitor. The API remains tenant scoped; the defect is cross-project UI
state contamination.

**Required closure.** Add a monotonically increasing load token or cancellation for the whole load,
including `loadMonitorExtras` and `readRegion`, then add deferred A→B and unmount regressions.

### P2 — iter-0183's Go/OpenAPI “contract” guard checks names only

`internal/api/requestcontract_test.go` parses only `properties` and compares field names. A drift in
requiredness, nullable semantics, array/scalar shape, UUID format or property type remains green. The
generated TypeScript check proves OpenAPI→TypeScript, not Go↔OpenAPI type parity. The guard is useful,
but the iteration and traceability overstate it as a closed contract chain.

**Required closure.** Compare required sets and a bounded Go/OpenAPI type/format model for the registered
DTO family, with fixture mutations proving each dimension fails.

### P2 — the tenant-reference owner inventory proves text presence, not ownership

`internal/store/tenantreferenceowners_test.go:43` succeeds when a token string occurs in a named file.
It does not prove that the token is on the write path, that the constraint covers the relevant columns,
or that the cited regression executes the owner. The exact six-key list also cannot discover a newly
introduced tenant-bearing reference unless a human first updates the same inventory.

**Required closure.** Keep the inventory as an index, but bind each entry to executable behavioural
tests/direct-SQL corruption probes and add a review guard for newly introduced cross-project id fields.

## Cross-iteration tracing

| Iteration | Implemented well | Where Cerbix transferred responsibility |
| --- | --- | --- |
| 0183 | Shared component behaviour inventory; exact property-name drift; explicit owner table | Type/requiredness parity was called a contract without being tested; source-token presence was called ownership |
| 0184 | Strict config, database clock, bounded ordered deletion, dedicated advisory slot, bounded labels | Destructive PostgreSQL proof was left “required before rollout” after status became DONE; alert correctness was left to operator interpretation |
| 0185 | Tombstoned project stream, transactional decision source tuple, set-based override revocation | Public effective GET escaped the transaction; CLI parity was satisfied only by raw JSON |
| 0186 | Set-based inventory, complete ledger evidence, deterministic worst-case algebra | Downgrade was left unexercised; CLI mode/source evidence was omitted while delivery claimed complete |
| 0187 | Pure state resolver, no parallel progress database, truthful first DOWN, scoped dismissal | Async scope consistency was delegated to request timing; the component test suite never exercised a project switch |

## Verification performed for this audit

- PASS: `go test ./...` on the current tree.
- PASS: `make build`.
- PASS: `make docs-check` — 184 documentation guard tests plus the living-reference check.
- PASS: `git diff --check` before documentation changes.
- PASS: Dockerized Node 22 `npm test` — 61 files, 706 tests.
- PASS: Dockerized Node 22 `npm run type-check`.
- NOT RUN: PostgreSQL retention matrix, migration downgrade with all-window rows, live stack and
  Playwright. Their absence is evidence for the findings, not a substitute for a pass.

## Disposition

Open `iter-0188` as a remediation iteration. FR-033/NFR-027, FR-034/NFR-028, FR-035/NFR-029 and
FR-036/NFR-030 return to `IN_PROGRESS` in the living status until their named gates close. Historical
iteration lifecycle statements remain unchanged.
