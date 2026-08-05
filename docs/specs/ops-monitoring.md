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

- NFR: `/readyz` reflects the readiness of dependencies (DB/broker) in future iterations.
- NFR: key paths have metrics with the `cerbix_` prefix.
