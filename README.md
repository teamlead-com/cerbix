# cerbix

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.25-00ADD8.svg)](https://go.dev)
[![Single binary](https://img.shields.io/badge/deploy-single%20binary-success.svg)](#quickstart)
[![OpenAPI 3.0](https://img.shields.io/badge/OpenAPI-3.0.3-green.svg)](openapi.yaml)
[![Security Policy](https://img.shields.io/badge/security-policy-informational.svg)](SECURITY.md)

Self-hosted uptime & SLA monitoring for internal infrastructure — heavier-duty and
multi-tenant. It probes **internal** services from inside the perimeter, with
SLO/error-budget alerting, incidents & status pages, on-call escalation, and
geo-distributed probers.

## What is cerbix

cerbix watches your services and tells you — reliably — when they break and how they've
been doing over time. The whole app ships as **one static Go binary**: the Vue SPA, the
REST API, and the database migrations are all embedded, so there is **no separate web
server, no frontend bundle, and no migration scripts** to deploy. Point it at a Postgres,
give it a config, run it.

### Highlights

- **16 check types** — HTTP, TCP, ICMP, DNS, TLS-cert, gRPC, PostgreSQL, MySQL, Redis,
  PromQL, RabbitMQ, WebSocket, SSH, composite (groups), push (dead-man's-switch), and
  **synthetic** (scripted multi-step HTTP flows). All behind an SSRF guard.
- **SLO & SLA** — error budgets, burn-rate alerts, scheduled weekly SLA reports;
  maintenance windows excluded from the SLA.
- **Incidents & status pages** — auto/manual/API incidents, timelines, postmortems,
  public status pages, inbound Alertmanager webhook receiver.
- **On-call / escalation** — escalation ladders, on-call rotations with vacation
  overrides, and acknowledge-to-stop.
- **Reliable delivery** — a transactional outbox (retry/backoff/dead-letter),
  confirmations, re-notify, instance-wide silence.
- **Auth** — OIDC (any provider) + local argon2id login, 2FA/TOTP, self-service password
  reset; org→project multi-tenancy with RBAC.
- **Geo-distributed probers** — probe a network segment from inside it: RabbitMQ
  region-pools (AMQP workers) or an HTTP-pull agent (outbound HTTPS only, no broker in the
  geo). Live-region picker + "region has no worker/agent" alerting.
- **Security** — AES-256-GCM secrets at rest with key rotation; distroless, non-root
  image.

## Architecture

One binary, several **roles** (chosen by `--role`), so it scales from a laptop to a
distributed deployment:

| Role | What it does | Needs |
| --- | --- | --- |
| `all` | Everything in one process (in-process dispatcher) — the default. | Postgres |
| `api` | REST + SSE + SPA + ingest + outbox delivery. | Postgres + RabbitMQ |
| `scheduler` | Leader-elected job scheduler; rollups, retention, alerts, escalations. | Postgres + RabbitMQ |
| `worker` | Stateless prober pool (AMQP). | RabbitMQ |
| `agent` | HTTP-pull prober for a geo with no broker access (outbound HTTPS only). | — (DB-less/broker-less) |

Stack: Go 1.25, Vue 3 + Vite + TypeScript (embedded via `go:embed`), PostgreSQL (pgx +
embedded goose migrations), RabbitMQ (distributed roles only). No nginx — the binary
serves the SPA; put Traefik / your ingress in front for TLS.

## Quickstart

Everything app-side is in the binary. The only external dependency is **PostgreSQL**;
migrations apply themselves on startup and a bootstrap admin is created from config.

### With Docker Compose (single node)

```bash
# From the repo root — builds the SPA + Go binary into one image and runs the dev stack.
docker compose -f deploy/docker-compose.yml --profile single up -d --build
# UI + API on http://localhost:8080 — log in with the bootstrap admin from deploy/config.dev.yaml.
```

### Just the binary + Postgres

Minimal `config.yaml`:

```yaml
database:
  dsn: "postgres://cerbix:pass@localhost:5432/cerbix?sslmode=disable"
local:
  enabled: true
security:
  admin_email: "admin@example.com"
  admin_password: "change-me-please"   # creates the global admin on an empty system
session:
  secure: false   # http dev only; behind TLS set true
```

```bash
cerbix serve --config config.yaml --role all
# Applies migrations, creates the admin, serves UI + API on :8080.
# Ops endpoints: /healthz  /readyz  /metrics
```

See [`deploy/config.example.yaml`](deploy/config.example.yaml) for
every option (RabbitMQ, OIDC, mail, secrets-at-rest, geo/pull transport), and
[`docs/overview.md`](docs/overview.md) for architecture, the deployment map, and design
notes. The API contract lives in [`openapi.yaml`](openapi.yaml).

## Building

```bash
# Native (no Docker): frontend → embedded assets, then the Go binary.
cd frontend && npm install && npm run build
rm -rf ../backend/internal/web/dist && cp -r dist ../backend/internal/web/dist
cd ../backend && go build -o cerbix ./cmd/cerbix

# Or build the image (multi-stage: SPA + binary → distroless):
docker compose -f deploy/docker-compose.yml build
```

CLI:

```text
cerbix serve --config <path> [--role all|api|scheduler|worker|agent] [--region <name>]
cerbix migrate --config <path>     # apply DB migrations and exit
cerbix reencrypt --config <path>   # rotate the secrets-at-rest key
cerbix version
```

## Documentation

Detailed architecture diagrams and workflows live in [`docs/architecture.md`](docs/architecture.md).

Design and status live under [`docs/`](docs/): [`architecture.md`](docs/architecture.md)
(system topology, data flow, geo-distribution, auth, escalation & database ERD),
[`ops-keycloak-oidc.md`](docs/specs/ops-keycloak-oidc.md) (Keycloak & OIDC setup guide),
[`overview.md`](docs/overview.md) (deployment map), `decisions.md` (the numbered decision log), `traceability.md`, and `status.md` (the live FR/NFR checklist).

## Community & Contributing

We welcome contributions! Please see our [`CONTRIBUTING.md`](CONTRIBUTING.md) for development setup and guidelines, and our [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md) for community standards.

## License

[MIT](LICENSE) © 2026 TeamLead.
