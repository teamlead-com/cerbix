# ops-audit-log-retention — bounded audit history (FR-033 / NFR-027)

> **Revision 1 — APPROVED for iter-0184 by owner instruction on 2026-09-19.** This is a
> product/operations iteration with no SPA surface and no mock requirement.

## 1. Problem

`audit_logs` is append-only and currently unbounded. Organization and instance audit views limit a
single response, but storage grows for the lifetime of the installation. Operators cannot declare how
long audit evidence is kept, observe purge backlog, or predict when an old row stops being readable.

Retention must be an explicit product contract rather than an emergency SQL recipe. It may delete only
audit rows older than the configured horizon; it must not rewrite surviving rows, weaken actor
attribution, cross tenant boundaries, or block the mutation whose audit row is being recorded.

## 2. Requirements

- **FR-033 — Audit-log retention.** An instance operator configures one retention horizon for both
  organization-scoped and instance-scoped `audit_logs`. Cerbix removes expired rows in bounded batches
  under one fenced maintenance owner and documents the readability boundary.
- **NFR-027 — Bounded, observable, non-destructive maintenance.** Retention uses the database clock,
  cannot run concurrently on two nodes, never deletes a row at or newer than the cutoff, exposes stable
  backlog/deletion metrics, and does not turn an audit-write failure into a product-write failure.

## 3. Configuration contract

New strict configuration lives under one owner in `internal/config`:

```yaml
audit:
  retention_days: 365
  purge_every: 1h
  purge_batch_rows: 1000
```

| Key | Default | Valid range | Meaning |
| --- | ---: | --- | --- |
| `audit.retention_days` | `365` | `30..3650` | Rows with `created_at < database_now - retention_days` are eligible. |
| `audit.purge_every` | `1h` | `5m..24h` | Cadence between maintenance passes. |
| `audit.purge_batch_rows` | `1000` | `100..10000` | Maximum rows deleted by one statement. |

Unknown keys, invalid durations, zeroes outside the documented defaults, and out-of-range values fail
configuration validation before business logic starts. Runtime code consumes the validated snapshot;
it does not read environment variables or invent fallback values.

## 4. Data and maintenance contract

1. Migration adds `audit_logs_created_at_id_idx (created_at, id)`. Existing rows and ids are preserved.
2. The maintenance owner is a dedicated `store.LeaderSession` advisory-lock slot, distinct from
   scheduler, migrations, gate maintenance, and every other registered slot. All delete statements use
   the pinned session that owns that lock.
3. At pass start, PostgreSQL supplies one `cutoff = clock_timestamp() - retention`. Every batch in the
   pass uses that same cutoff. Application time is not authoritative.
4. One statement selects at most `purge_batch_rows` ids ordered by `(created_at, id)`, locks them with
   `FOR UPDATE SKIP LOCKED`, and deletes exactly those ids. A row at `created_at = cutoff` survives.
5. A pass has a fixed 30-second wall-clock budget and stops on budget exhaustion, context cancellation,
   lock refusal, or an empty batch. The next pass resumes from the oldest surviving row.
6. Organization-scoped and instance-scoped rows share the same horizon. Retention never accepts an
   organization id and therefore cannot accidentally delete one tenant while retaining another.
7. Retention deletion does not write another `audit_logs` row: doing so would create a self-sustaining
   tail. It emits structured operational logs and metrics instead.
8. The existing `RecordAudit` best-effort contract remains unchanged. Retention cannot make a primary
   product mutation fail merely because its audit append failed.

### Readability and physical lifetime

- A row is guaranteed eligible only after `retention_days`; it remains readable until a successful
  batch deletes it.
- Under a healthy maintenance owner, logical and physical removal complete within
  `retention_days + purge_every + one pass budget`.
- Lock contention, database outage, or a stopped maintenance role may extend lifetime. That is backlog,
  not silent retention extension, and must be observable.

## 5. API, UI, and operator contract

- Existing audit endpoints and response arrays do not change in iter-0184.
- No organization may choose a shorter or longer horizon; this is an instance data-lifecycle policy.
- The runbook and `config.example.yaml` state the horizon, cadence, boundary semantics, recovery
  procedure, and the fact that increasing retention cannot restore rows already deleted.
- No SPA work and no artifact mock are required. A later settings surface is a separate feature.

## 6. Observability

Low-cardinality metrics:

- `cerbix_audit_retention_rows_deleted_total`
- `cerbix_audit_retention_passes_total{result="deleted|empty|lock_busy|error|budget"}`
- `cerbix_audit_retention_oldest_expired_seconds`
- `cerbix_audit_retention_last_success_timestamp_seconds`

No organization, user, action, target, or row id becomes a metric label. Alerts cover a stale last
success and expired-row backlog older than two healthy cadences. Structured logs name counts and
durations but never copy `target` text from deleted rows.

## 7. Security and recovery

- Retention is the only authorized automatic deletion path for `audit_logs`; it is not exposed through
  HTTP and accepts no tenant selector.
- Existing organization deletion continues to cascade its audit rows immediately. Global rows remain
  `org_id IS NULL` and follow the same time horizon.
- A failed migration is fail-fast and deletes nothing. A failed pass rolls back its current statement;
  previously committed batches stay deleted and are not recreated.
- Reducing the horizon may create backlog but does not permit one unbounded DELETE. Increasing it affects
  only surviving rows.

## 8. Acceptance invariants

1. Configuration is strict, centrally defaulted, and rejects every out-of-range key independently.
2. Two processes cannot delete concurrently; loss of the pinned connection removes both authority and
   the ability to issue another delete.
3. Rows before the cutoff are removed in deterministic bounded batches; rows on/after it survive.
4. Org and instance rows are covered without tenant-bearing delete predicates.
5. A lock-busy, timeout, cancelled, and SQL-error pass leaves product writes and surviving audit rows
   usable.
6. Metrics distinguish progress, empty inventory, contention, budget exhaustion, and error without
   high-cardinality labels.
7. Runbook recovery explains backlog, horizon changes, and irreversibility.

## 9. Required tests and gates

- Config table tests for defaults, boundaries, unknown keys, and cross-key independence.
- PostgreSQL migration/index ownership test and DB-gated retention cases for cutoff equality, mixed org
  and global rows, batch continuation, concurrent row lock, rollback, and two-node leader fencing.
- Session-poisoning regression: failed RESET/unlock closes the connection rather than returning it.
- Metrics/log tests proving no tenant/action/target labels or payloads.
- `go test ./...`, full race suite, vet, build, lint, docs-check, migration up/down on supported
  PostgreSQL, and an explicit live backlog-drain smoke.

## 10. Iter-0184 plan and role split

| Priority | Task | Role |
| --- | --- | --- |
| P0 | Approve retention/config/boundary contract | Owner + Agent C |
| P0 | Add migration, config and fenced purger | Agent A |
| P0 | Prove cutoff, batching and authority | Agent B |
| P1 | Add metrics, alerts and recovery runbook | Agent D |
| P1 | Synchronize status and traceability | Agent C |
| P2 | Run repository and live PostgreSQL gates | Agents B/D |

## 11. Non-goals

Per-organization retention, legal holds, exports, archive tiers, immutable/WORM storage, project-level
audit reads, a retention settings UI, and changing which actions are audited.
