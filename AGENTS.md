# Repository Guidelines — cerbix

## Canonical Sources of Requirements

Use `docs/` as the source of truth. If code conflicts with the specification, the
specification wins.

Priority order:

1. `docs/project-description.md` — main PRD.
2. `docs/specs/*.md` — area-scoped technical specifications (functional, ops, security).
   Naming convention in `docs/specs/README.md`.
3. `docs/status.md` — live checklist of requirement statuses.
4. `docs/traceability.md` — mapping from requirement to code, tests, metrics, runbook.
5. `docs/decisions.md` — recorded architectural/contract decisions.

## Documentation Layout

`docs/` is organised by document lifetime:

| Folder / file | Lifetime | Convention |
| --- | --- | --- |
| `docs/project-description.md`, `docs/status.md`, `docs/decisions.md`, `docs/traceability.md`, `docs/runbook.md`, `docs/alerts.yaml` | Long-lived, edited in-place | One sticky name; history via `git log`. |
| `docs/specs/<kind>-<scope>.md` | Long-lived, edited in-place | `<kind>` is `func-`/`ops-`/`sec-`/`cross-`; `<scope>` is kebab-case; no date, no version suffix. See `docs/specs/README.md`. |
| `docs/checks/YYYY-MM-DD-<kind>[-vN].md` | Immutable snapshots | Dated reviews/audits; never edited after publication. |
| `docs/iterations/iter-XXXX.md` | Immutable per-iteration reports | Sequential numbered; never edited after the iteration closes. |

Before creating a new doc, decide which folder it belongs to based on lifetime.

## Project Structure & Module Organization

cerbix is a monorepo: a Go backend and a Vue 3 frontend.

- `cmd/cerbix/`: CLI entrypoint (`serve --role`, `version`).
- `internal/<pkg>/`: service packages (`config`, `logging`, `metrics`,
  `buildinfo`, `httpsrv`, `cli`, and later `auth`, `authz`, `store`, `domain`,
  `dispatch`, `scheduler`, `worker`, `prober`, `mq`, `sla`, `notify`, `statuspage`,
  `incidents`, `apitoken`, `api`). **No `pkg/`.**
- `deploy/`: compose stacks, role configs, and `config.example.yaml`.
- `frontend/`: Vue 3 + TS SPA (Vite), embedded into the binary via `embed.FS`.
- `deploy/`: `docker-compose.yml` local dev stack.
- `docs/`: PRD, TZ, status, traceability, decisions, runbook, iteration reports.

Keep transport (HTTP), domain, application, and infra concerns isolated. Interfaces
such as `Prober`, `Dispatcher`, `Notifier`, and the `authz` decision function stay
small and testable.

## Mandatory Iteration Workflow

Work only in iterations named `iter-XXXX`.

Each iteration must:

1. Read the canonical spec, `docs/status.md`, `docs/decisions.md`, and the last 2–3
   files in `docs/iterations/`.
2. Update `docs/status.md` with atomic `FR`, `NFR`, `AC`, and `DoD` items using only
   `TODO`, `IN_PROGRESS`, or `DONE`. Every `DONE` item links to code, tests, and
   metrics where applicable.
3. Write an iteration plan with 3–8 tasks ordered `P0`, `P1`, then `P2`.
4. Execute with role separation for Agent A/B/C/D. If real multi-agent execution is
   unavailable, simulate the roles sequentially and report each role output.
5. Run required tests and checks.
6. Write `docs/iterations/iter-XXXX.md`.
7. If root analysis artifacts were used, confirm transfer into canonical docs and
   removal of those artifacts.

Do not stop at planning. After the plan, proceed with implementation until the
iteration requirements are complete or a blocker is documented.

## Mandatory Agent Roles

Every iteration must show outputs for these roles:

- **Agent A, Implementation**: code, configs, infrastructure changes.
- **Agent B, Tests**: unit, integration, e2e, and harness fixes.
- **Agent C, Docs + Traceability**: `status.md`, `traceability.md`, runbook, README, specs.
- **Agent D, Observability + Ops**: metrics, alerts, health/readiness, SLO/SLI, recovery.

## Engineering Invariants

Do not simplify requirements without recording a decision in `docs/decisions.md`.
Do not delete heartbeat, incident, or audit data during fixes. Changes to contracts or
reliability must update `docs/decisions.md` and `docs/traceability.md`.

Configuration is strict-only: fail fast on invalid or incomplete values, no runtime
self-healing, no silent downgrade, and no warn-and-continue behavior for config
contracts. Defaults are allowed only in the central config loader
(`internal/config`) and only for optional fields. Runtime code uses a validated
config snapshot, not inline environment reads.

Each validation rule has one owner:

- `transport`: request shape, required fields, format, type.
- `domain`: invariants and business rules (e.g. tenant isolation, role permissions).
- `application`: orchestration and use-case flow.
- `infra`: repository and technical guarantees, without duplicating business rules.

Before deduplicating rules, document drift analysis in `docs/decisions.md`.

**Tenant isolation is a domain invariant.** Every data access for monitors,
heartbeats, incidents, and status pages must be constrained to the caller's
authorized `org_id`/`project_id` set. A missing filter is a security defect.

## Build, Test, and Development Commands

From the repo root:

- `make test` / `go test ./...` — unit and fake-backed integration tests.
- `make race` / `go test -race ./...` — concurrency checks (scheduler, worker, SSE).
- `make build` — builds `bin/cerbix` with version/commit ldflags.
- `./bin/cerbix serve --config deploy/config.example.yaml --role all` — run locally.
- `make lint` / `golangci-lint run ./...` — required when configured.
- `gofmt -w <files>` — format changed Go files before review.

## Coding Style & Naming Conventions

Idiomatic Go: tabs via `gofmt`, short package names, exported names only for
cross-package contracts, context-aware external calls. Metrics keep the `cerbix_`
prefix and low-cardinality labels. Config keys, metric names, and CLI flags match
the specification.

Prefer small, PR-like changes. Do not introduce fallback or legacy paths unless the
spec requires them. Do not leave duplicate business-rule checks across layers.

## Testing Guidelines

Tests are fixture-driven and deterministic. Live probes (real HTTP/DNS/TCP/DB
targets), a live Postgres, RabbitMQ, or Keycloak must be opt-in via explicit
environment variables and must never run in default CI. Cover config validation,
tenant-isolation enforcement, role permissions, prober condition evaluation, SLA
window math, scheduler leader election, and HTTP health/readiness/metrics.

Before completing an iteration, run at minimum: `go test ./...`,
`golangci-lint run ./...` (if configured), and the smoke/e2e commands for the
changed scope. Record results in the iteration report.

## Definition of Done

An iteration is complete only when:

- Target items in `docs/status.md` are `DONE`, linked to code/tests/metrics.
- Required tests are green.
- Delivery docs are synchronized: `project-description.md`, `runbook.md`,
  `status.md`, `traceability.md`, `decisions.md`, and `iterations/*.md`.
- Config is strictly validated before business logic starts.
- Runtime paths contain no self-healing or fallback for invalid configuration.
- Single sources of truth are defined for key rules and invariants.
- Tests cover invariants, negative scenarios, and tenant-isolation regressions.

## Commit & Pull Request Guidelines

Use imperative, scoped subjects such as `Add http prober` or
`Fix sla rolling window`. Pull requests include a problem statement, implementation
summary, test results, operational impact, and links to related issues.

## Security & Operational Safety

Never commit secrets (DB passwords, Keycloak client secrets, API tokens, service
credentials). Store only hashes of cerbix API tokens. Never log bearer tokens,
`client_secret`, or session cookies. Public status pages must expose only the
information a page is explicitly configured to show — never internal topology by
default (default page visibility is `internal`).
