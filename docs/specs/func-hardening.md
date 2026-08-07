# Spec: Hardening package (func-hardening)

## Purpose

The 2026-08 deep audit (six parallel auditors + manual verification) found a family of
silent-failure, invariant-violation, tenant-isolation, and resource bugs. This spec
packages the fixes, P0 → P2, one focused iteration each, backend-first. Where a fix has
no new UI surface, no SPA mock is needed (these repair existing behavior).

## Iterations (priority order)

| # | Iteration | Findings |
|---|---|---|
| 1 | iter-0072 | **notify SMTP timeout.** `notify.sendMailFunc` = raw `smtp.SendMail` (no timeout/ctx) stalls the single outbox worker → whole alert pipeline freezes. Point it at the `mailer.sendMailTimeout` treatment. |
| 2 | iter-0073 | **AMQP channel-level resilience.** The supervisor only watches connection loss; a channel death (queue deleted, basic.cancel, 406, ack error) parks consumers/publish forever. Add channel-death detection + bounded backoff retry independent of the connection-reconnect signal; guard `Close()`/nil-logger races. |
| 3 | iter-0074 | **Backfill FK 23503 + status ErrNotFound.** `InsertHeartbeatsBulk` isn't protected against the deleted-monitor race the way `InsertHeartbeat` now is; `RecordCheckStatus` ErrNotFound logs as ERROR. Map both to the quiet path. |
| 4 | iter-0075 | **Agent region-scope bypass + unscoped test-result + channel-secret redaction.** Region-less results/backfill authorized by any token and skip scope; `agentTestResult` has no region check; notification-channel `config` (secrets) returned to viewers. |
| 5 | iter-0076 | **Maintenance never-alerts.** Down during a window is suppressed at enqueue and never delivered even after the window ends. Enqueue the transition and suppress at delivery (outbox), so renotify carries it out of the window. |
| 6 | iter-0077 | **Leadership watchdog + health metrics.** Advisory-lock loss undetected → split-brain. Add a lock-liveness check and `cerbix_scheduler_leader` / `cerbix_broker_up` gauges. |
| 7 | iter-0078 | **Region affinity in shipped wiring.** `--role all` never wires `WithPullRegions` (pull monitors probed from core); distributed compose runs the core worker DB-less (composites always down). |
| 8 | iter-0079 | **Pool & leader-loop robustness.** `pgxpool` MaxConns explicit; `EnqueueDueSLAReports` self-deadlock (tx + nested pool call); per-call timeouts on sub-cadence store calls. |
| 9 | iter-0080 | **Escalation/incident lifecycle.** Escalation-policy monitor with a failed incident-create pages no one; disable/delete of a down monitor escalates forever; auto-incident double-open (partial unique index). |
| 10 | iter-0081 | **Auth hardening.** X-Forwarded-For trust / limiter map growth; OIDC audience check; cross-tenant escalation targets & escalation_policy_id validation; status-page cross-project (VisibleProject); password-change session invalidation; public-router id leakage. |
| 11 | iter-0082 | **Remaining correctness.** Push failure_threshold pin to 1; ICMP echo id/seq match; SSRF 100.64/10 + shared-address; SSE filter-before-buffer + WriteTimeout; OIDC bounded HTTP client + off-boot-path; shutdown exit code / goroutine drain; unique-slug 409; FK existence oracle. |

## Non-goals

Multi-geo cluster awareness; a full tracing/OTEL story; rearchitecting the ingest
three-transaction chain (the audit's INV-1 "one transaction" doc claim is aspirational —
we fix the concrete alert-loss paths, and correct the doc, rather than merge heartbeat
insert into the status tx).

## Acceptance

Per iteration: `-race` suite green (both storage modes where a migration is involved),
targeted unit/integration test for the specific fix, live verification (distributed
compose for transport/leadership items), E2E 34/34 still green, iteration report +
decision record + traceability row. After the package, re-running the six audit sweeps
must return no P0/P1 outside the documented non-goals.
