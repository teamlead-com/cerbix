<!-- markdownlint-disable MD033 MD041 -->
<p align="center">
  <img src="docs/logo.png" width="120" alt="cerbix logo" />
</p>
<!-- markdownlint-enable MD033 MD041 -->

# cerbix

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.25-00ADD8.svg)](https://go.dev)
[![Single binary](https://img.shields.io/badge/deploy-single%20binary-success.svg)](#quickstart)
[![OpenAPI 3.0](https://img.shields.io/badge/OpenAPI-3.0.3-green.svg)](openapi.yaml)
[![Security Policy](https://img.shields.io/badge/security-policy-informational.svg)](SECURITY.md)

**Define what reliable means for a service — then measure it and run the response.**

Self-hosted, multi-tenant service reliability platform. A **Service** declares what its
reliability *is* — which checks are its SLI, how regions aggregate, what counts as
pageable — in **versioned** definitions, so every number is traceable to the rule that
produced it. cerbix measures that from its own checks, reports SLO / error budget / burn
rate **and withholds any number it cannot defend**, then drives the response: incidents,
dependency impact, status pages, on-call escalation.

## What is cerbix

Most tools tell you a service is down. cerbix makes you say what *reliable* means for that
service, and then holds you to it.

An observation (HTTP, TCP, DNS, TLS, gRPC, SQL, PromQL, a scripted flow, a dead-man's
switch — taken from inside your perimeter, including private and geo segments) is
normalized to **GOOD / BAD / UNKNOWN with a reason** and reduced into one duration-weighted
timeline per Service. The SLO window ends at the **seal watermark, never at `now`**;
storage continuity and decidable coverage independently govern every aggregate; a number
that cannot be honestly stated is **absent with a reason instead of rounded to 100%**. The
same Service then drives suppression, incidents, the status page and the escalation ladder.

The whole app ships as **one static Go binary**: the Vue SPA, the REST API, and the
database migrations are all embedded, so there is **no separate web server, no frontend
bundle, and no migration scripts** to deploy. Point it at a Postgres, give it a config,
run it.

cerbix is a control plane for **reliability definitions and operational response** — not
for your traffic, deploys or infrastructure. It sits out of band and acts on nothing but
its own alerts, incidents and pages.

### Highlights

- **16 check types** — HTTP, TCP, ICMP, DNS, TLS-cert, gRPC, PostgreSQL, MySQL, Redis,
  PromQL, RabbitMQ, WebSocket, SSH, composite (groups), push (dead-man's-switch), and
  **synthetic** (scripted multi-step HTTP flows). All behind an SSRF guard.
- **Service as the reliability object** — explicit SLI membership (and separately
  declared *context* members that inform without counting), aggregation policies
  (`all`/`any`/quorum, region-aware), **immutable definition revisions** and evaluation
  epochs: you can answer "under which definition was this month measured".
- **Reliability that refuses to guess** — GOOD/BAD/UNKNOWN with reasons, two independent
  coverage axes, sealed facts, and withheld-with-a-reason instead of a confident 100%.
- **Service SLO, error budget & burn rate** — per window, quoted against the objective in
  force, with burn-rate alerting that says which watermark it was computed from. Monitor
  SLA and weekly SLA reports too; maintenance windows are excluded.
- **Reliability gate** — a deploy pipeline asks `cerbix gate check` (or one `POST`) whether the
  error budget allows the release and gets `ALLOW` / `WARN` / `BLOCK` / `UNKNOWN` with every reason
  attached, from the same sealed facts the service page shows, in one database snapshot:
  **cerbix decides, the pipeline acts.**
- **Paging that does not double** — a Service can own paging for its members: suppression
  is per signal, per polarity, and only while coverage is demonstrably armed. Anything
  ambiguous **fails open** — a page that was not needed is noise, a page that was owed and
  never sent is the failure the design exists to prevent.
- **Incidents & status pages** — auto/manual/API incidents anchored to a **monitor or a
  Service**, timelines, postmortems, member snapshots, public status pages, inbound
  Alertmanager webhook receiver.
- **Dependency impact** — a same-project service graph correlates an incident with its
  upstreams and downstreams. It records **candidates** and annotates; it never elects a
  culprit, never suppresses and never hides.
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

## What cerbix is not

Stated plainly, because the boundary is a feature. cerbix does **not** serve arbitrary
time-series queries, store generic telemetry, support user-defined downsampling, expose a
query language, act as a metrics backend, or provide a service catalog — those are quoted
from its own specification's non-goals, not softened for a README. There is no trace or log
ingestion anywhere in it, and no automatic root-cause analysis: what it offers is
correlation candidates and a heuristic context note over a dependency graph **you**
declared.

## Where it fits

| Alongside | The honest relationship |
| --- | --- |
| Prometheus, Grafana, Loki, Tempo | **Not a replacement.** cerbix stores none of your telemetry and gives you no query language. It reads **PromQL as a check source** and exports its own `cerbix_*` metrics into your Prometheus. Keep them for *why*; cerbix answers *is this service reliable by our definition, and what are we doing about it*. |
| Checkly, Pingdom, Uptime Kuma | Synthetic checks overlap, but there a monitor is the final object. Here a monitor is an **observation** and the object is a Service with a versioned definition, a budget and a response — plus probing from **inside** the perimeter, multi-tenancy and self-hosting. |
| PagerDuty, Opsgenie | cerbix carries on-call rotations and escalation ladders for **its own** incidents. It does not claim to be the incident-response hub for signals from a dozen other systems. |
| Nobl9, Pyrra, Sloth | Those compute SLOs over **your** metrics. cerbix produces the observations itself, keeps one derived timeline per Service, and versions the *definition* rather than only the threshold. |

## Architecture

One binary, several **roles** (chosen by `--role`), so it scales from a laptop to a
distributed deployment:

| Role | What it does | Needs |
| --- | --- | --- |
| `all` | Everything in one process (in-process dispatcher) — the default. | Postgres |
| `api` | REST + SSE + SPA + the check-result consumer + outbox delivery. | Postgres + RabbitMQ |
| `scheduler` | Leader-elected job scheduler; rollups, retention, alerts, escalations. | Postgres + RabbitMQ |
| `worker` | Stateless prober pool (AMQP). | RabbitMQ |
| `agent` | HTTP-pull prober for a geo with no broker access (outbound HTTPS only). | — (DB-less/broker-less) |

Stack: Go 1.25, Vue 3 + Vite + TypeScript (embedded via `go:embed`), PostgreSQL (pgx +
embedded goose migrations), RabbitMQ (distributed roles only). No nginx — the binary
serves the SPA; put Traefik / your ingress in front for TLS.

## Quickstart

Everything app-side is in the binary. The only external dependency is **PostgreSQL 15 or newer**
(16 is what every image, compose file and CI job here uses). The schema itself needs 15: five
migrations use the column-list `ON DELETE SET NULL (col)` form introduced in that release, and on 14
they are a syntax error. `cerbix migrate` checks the server version before applying anything and
refuses with that explanation rather than dying halfway through.

Everything app-side is in the binary. The only external dependency is **PostgreSQL**;
migrations apply themselves on startup and a bootstrap admin is created from config.
Full step-by-step production guides (Docker Compose and bare binary + systemd) live in
[`INSTALL.md`](INSTALL.md).

### With Docker Compose (single node)

```bash
# For a fresh broker volume only; retained volumes require their matching image pin.
make dev-init
make dev-up
make dev-test             # browser suite; idle-provider MaC UI assertion may skip
make dev-down             # named volumes survive
# UI + API on http://localhost:8080 — log in with the bootstrap admin from docker/config.dev.yaml.
```

The same facade covers the non-production role and geo topologies:

```bash
make dev-up-distributed   # api :8082 + scheduler + worker
make dev-test-distributed # distributed-role browser/prober smoke
make dev-down             # stop distributed before switching to geo
make geo-init             # once, fresh geo broker volume only
make geo-up-all           # central + AMQP geo1 worker + HTTP-pull geo2 agent
make geo-test             # generic + geo1 AMQP / geo2 pull transport smoke
make geo-down             # geo stack; named volumes survive
```

Base and geo cannot run together because their fixed dev ports overlap. Their separate
`.env.dev`/`.env.geo` pins are intentional: never reuse one broker image selector for the other
retained volume.

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

See [`docker/config.example.yaml`](docker/config.example.yaml) for
every option (RabbitMQ, OIDC, mail, secrets-at-rest, geo/pull transport), and
[`docs/overview.md`](docs/overview.md) for architecture, the deployment map, and design
notes. The API contract lives in [`openapi.yaml`](openapi.yaml).

## Building

```bash
# Native (no Docker): frontend → embedded assets, then the Go binary.
cd frontend && npm install && npm run build
rm -rf ../internal/web/dist && cp -r dist ../internal/web/dist
cd .. && go build -o cerbix ./cmd/cerbix

# Or build the non-production images (multi-stage: SPA + binary → distroless).
# Each build requires its initialized env file with the image pin matching that
# topology's retained RabbitMQ volume:
make dev-build
make geo-build
```

CLI:

```text
cerbix serve --config <path> [--role all|api|scheduler|worker|agent] [--region <name>]
cerbix migrate --config <path>     # apply DB migrations and exit
cerbix reencrypt --config <path>   # rotate the secrets-at-rest key
cerbix gate check --project <id> --service <id> [--json] [--timeout 10s]
                                   # reliability gate; CERBIX_URL + CERBIX_TOKEN from the environment,
                                   # exit 0 ALLOW/WARN, 2 BLOCK, 4 NOT_CONFIGURED, 1 error (no retry on 429)
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
