# Spec: Service monitoring (ops-monitoring)

> Skeleton.

## Purpose

Self-observability of cerbix: metrics, health/readiness, alerts.

## Scope

- Prometheus metrics with the `cerbix_` prefix, low-cardinality labels.
- HTTP endpoints `/healthz`, `/readyz`, `/metrics`.
- Base series (iter-0001): `cerbix_build_info`, `cerbix_up`, `cerbix_ready`,
  `cerbix_uptime_seconds`. Next: check durations, queue sizes, worker lag,
  leader state, notification errors.
- Alerts in `docs/alerts.yaml` (added as metrics appear).

## Requirements (draft)

- NFR: `/readyz` is gated on live **DB connectivity** — a background ping (`pingDatabase`) flips readiness and `cerbix_database_up`; a DSN-less process stays ready in scaffold mode. (Broker reachability is not wired into readiness.)
- NFR: key paths have metrics with the `cerbix_` prefix.
