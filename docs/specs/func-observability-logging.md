# Spec: Operational logging expansion (func-observability-logging)

## Purpose

Today the log is mostly failures plus scheduler housekeeping: no HTTP access log, no
record of who changed what, no monitor status transitions, no delivery confirmations.
An operator reading the log cannot reconstruct what happened. This spec adds a coherent
logging taxonomy on top of the existing slog/JSON setup (`log.level` already exists and
stays the single knob).

## Taxonomy

- **ERROR** — a failure a human may need to act on (unchanged).
- **WARN** — degradation the system survives (unchanged).
- **INFO** — the operational narrative: every API/auth request, every mutation with its
  actor, every monitor status transition, every successful alert delivery.
- **DEBUG** — chatter: static-asset requests, requests aborted by the client
  (`context.Canceled` — today a false ERROR).

## Iterations

| # | Iteration | Scope |
|---|---|---|
| 1 | iter-0068 | **HTTP access log + error hygiene.** Middleware around the app mux: `http_request` with method, path, status, bytes, `dur_ms`, remote IP. INFO for `/api/*` and `/auth/*`, DEBUG for the SPA/static catch-all; the SSE stream (`/api/v1/events`) is skipped (hours-long requests). `serverError` downgrades `context.Canceled` to DEBUG `request_canceled` — the last recurring false ERROR. |
| 2 | iter-0069 | **Lifecycle events with actors.** A `logEvent(r, msg, kv...)` helper stamps the principal automatically. INFO on: monitor created/updated/deleted/paused, incident opened/acknowledged/status-changed, settings saved (per group), API/agent token issued/revoked, webhook & channel created/deleted/toggled, status page and component CRUD, subscriber removed, member add/role/remove and admin user actions. Plus the two engine-side gaps: `monitor_status_changed` (prev→cur, suppressed flag) in ingest's transition reconciler and `outbox_delivered` (topic, attempts) at delivery success. |

## Non-goals

Request IDs / tracing (a future OTEL story), changing the log format, log shipping,
audit-table changes (the audit trail stays the tenant-facing record; logs are the
operator's).

## Acceptance

`-race` suite green; live checks: an API call produces exactly one `http_request` line
with a correct status/duration; asset requests appear only at `log.level: debug`;
a monitor flip produces `monitor_status_changed`; a delivered notification produces
`outbox_delivered`; page-close no longer produces `api_error … context canceled`.
