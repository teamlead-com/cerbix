# Spec: Monitoring and check types (func-monitoring-checks)

## Purpose

How cerbix performs availability checks: check types, the `Prober` interface, the conditions
engine, scheduling and execution (scheduler → Dispatcher → worker → ingestion).

## Model (implemented in iter-0004)

- `domain.Monitor{type, target, interval_seconds, timeout_seconds, retries, conditions[],
  enabled, status}`; types `http`, `tcp`, `push` (push is passive, later iter);
  `domain.Heartbeat{monitor_id, ts, up, latency_ms, code, msg}`.
- Migration `00003_monitors.sql`: `monitors` + `heartbeats` (a regular table for now; hypertable
  in the SLA iteration). Store: `internal/store/{monitors,heartbeats}.go`.

## Prober and conditions (`internal/prober`)

- `Prober` interface + `Runner` (registry by type). **HTTP** and **TCP** are implemented;
  ICMP/Push — later (ICMP requires privileges).
- `Runner.Run(ctx, monitor)`: a prober with `context.WithTimeout(timeout)` and `retries`;
  condition evaluation → `Heartbeat`.
- **Conditions engine (declarative)**: `[STATUS]`, `[RESPONSE_TIME]` (ms), `[CONNECTED]` (1/0),
  `[CERT_EXPIRY]` (days, TLS), `[BODY]`; operators: numeric `== != < <= > >=`; string
  (`[BODY]`) `== != contains matches` (regex). Example: `[STATUS] == 200`, `[RESPONSE_TIME] < 500`,
  `[BODY] contains "UP"`, `[CERT_EXPIRY] > 14`.
- Default without conditions: HTTP — 2xx; TCP/ICMP/DNS — successful connect/resolve; TLS — handshake + a valid
  certificate.
- **Implemented types**: HTTP, TCP, ICMP, **DNS** (A/AAAA resolve, addresses in `[BODY]`), **TLS**
  (handshake through the SSRF guard, `[CERT_EXPIRY]` in days; `InsecureSkipVerify` — we monitor
  self-signed too) — D-0058, **gRPC** (grpc.health.v1 Check through the SSRF guard, plaintext; up=SERVING) —
  D-0059, **composite** (D-0061), **PostgreSQL** (D-0062), **MySQL/Redis/PromQL** (D-0063),
  **RabbitMQ** (AMQP handshake / management HTTP API), **WebSocket** (RFC 6455 Upgrade + verification of
  `Sec-WebSocket-Accept`), **SSH** (identification banner in `[BODY]`) — D-0065; Push
  (dead-man's-switch). All — through the SSRF guard; secrets (`password`) are encrypted in `monitors.config`.

## Scheduling and execution

- `internal/dispatch`: `Dispatcher` (PublishJob/Jobs/PublishResult/Results) — the transport
  between scheduler and worker. Implementation `inproc` (channels); `rabbitmq` — later (prod).
  `CheckJob` carries a snapshot of the monitor (the worker has no DB access).
- `internal/scheduler`: **the only active one** (Postgres advisory-lock leader),
  tick scan of enabled monitors, publishes a job when `next_run` arrives. Passive
  (push) are skipped.
- `internal/worker`: a worker pool, `Jobs()` → `Runner.Run` → `PublishResult`.
- `internal/ingest`: `Results()` → `InsertHeartbeat` + `SetMonitorStatus` + metric
  `cerbix_checks_total{result}`.

## Requirements

- FR-007 (monitors + probers + conditions) — DONE (full prober set: http/tcp/icmp/dns/tls/grpc/
  websocket/ssh/push + postgres/mysql/redis/promql/rabbitmq/composite/synthetic).
- FR-008 (scheduler leader + dispatcher + worker + ingestion) — DONE (inproc dev; distributed
  over AMQP with per-region queues + a broker-less HTTP-pull agent transport).
- NFR: a single probe does not block the pool (context timeout); scheduler leadership is exclusive
  (advisory lock); jobs carry a snapshot of the monitor.

## API (implemented)

`GET/POST /api/v1/projects/{id}/monitors`, `GET/DELETE /api/v1/monitors/{id}`,
`GET /api/v1/monitors/{id}/heartbeats` — with authz (`ProjectRead`/`ProjectWrite`) and isolation.

## Open questions / next

- RabbitMQ implementation of `Dispatcher` for cross-process roles (scheduler/worker separate).
- ICMP prober (needs privileges/CAP_NET_RAW), Push (dead-man's-switch + push endpoint),
  DNS/TLS (D-0058), gRPC (D-0059), **composite/group** (D-0061), **PostgreSQL** (D-0062),
  **MySQL/Redis/PromQL** (D-0063: RESP client / go-sql-driver with guarded dial / Prometheus HTTP +
  `[RESULT]`), **RabbitMQ/WebSocket/SSH** (D-0065) — **implemented**. Check catalog:
  HTTP/TCP/ICMP/DNS/TLS/gRPC/composite/PostgreSQL/MySQL/Redis/PromQL/RabbitMQ/WebSocket/SSH + Push.
  RabbitMQ implementation of `Dispatcher` (job transport) is a separate topic, not a prober.
- Monitor update (PATCH) and pause; separate default intervals/timeouts.
