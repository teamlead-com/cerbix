# Spec: E2E coverage expansion (func-e2e-coverage)

## Purpose

The base suite (D-0124) smoke-covers every functional area; this package extends it
toward real feature/use-case coverage, in priority order. Test infrastructure only —
no product code changes expected (a dev-compose sidecar where a flow needs one).

## Iterations (by value/cost)

| # | Iteration | Scope |
|---|---|---|
| 1 | iter-0063 | **Cheap wins, no new infra.** Alertmanager ingest (firing → incident with external_key, idempotent re-fire, resolved → closed; rendered chip in the UI); SLA editor round-trip (objective + burn rules via the SlaView flow); dead-letter admin UI (page renders, list endpoint answers). |
| 2 | iter-0064 | **Probers against the stack itself.** http monitor with a conditions expression probing cerbix's own `/healthz` (goes up), a failing-condition variant (goes down), postgres/rabbitmq probers against the compose services, composite quorum over children, test-connection endpoint (result for a live region, 502 for a region without workers). |
| 3 | iter-0065 | **Mail flows via a Mailpit sidecar** (dev compose profile `mail`): password-reset request → letter → confirm link → new password works; status-page subscribe → confirmation letter → confirmed subscriber; SMTP settings pointed at the sidecar via the API. |
| 4 | iter-0066 | **Time-based flows** (short intervals + polling): confirm-phase acceleration (time-to-down), TOTP enroll → login with a generated code (RFC 6238 implemented in the test), session invalidation on password change. Escalation-engine stepping stays unit-tested (tick cadence makes E2E slow/flaky). |
| 5 | iter-0118 | **Repeatable topology facade.** Fixed Make entrypoints build/start/check/test the single, distributed, and geo stacks; the harness idempotently owns an exact `e2e-harness` org/project on a fresh database; `topology-geo.spec.ts` proves geo1 AMQP and geo2 pull execution from their isolated networks. |

## Non-goals

Per-channel notification delivery (Slack/Telegram need external endpoints) and CI wiring
(the suite stays a local/pre-release gate per D-0124). The geo worker/agent transports are now
covered by the explicit `make geo-test` local gate; HA across multiple physical hosts remains an
integration-environment concern.

## Acceptance

Full suite green twice in a row after each iteration; every new test self-cleans
(`e2e-` prefix); runtime stays under a minute without the mail/time specs and under
~3 minutes with them.

The browser harness never selects response element zero from an arbitrary tenant: global setup
creates or reuses the exact `e2e-harness` organization and project. Shared monitor fixtures are
confined to that tenant; destructive tenant specs own unique prefixed fixtures, and tests that
change global settings restore them. Dependencies are installed from the committed E2E lockfile.
Topology Make goals use only loopback application URLs and require the corresponding per-role
readiness gate first. With no active file-provider bundle, its UI assertion is an intentional skip;
the independent `e2e/mac-smoke.sh` gate proves the managed-bundle lifecycle.
