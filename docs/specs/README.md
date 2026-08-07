# docs/specs

Directory of cerbix specs. Each file is a long-lived specification for a
part of the system. Unlike `docs/checks/` (immutable snapshots) and `docs/iterations/`
(immutable iteration reports), specs are edited in-place; history — via `git log`.

## Naming convention

```
<kind>-<scope>.md
```

- **`<kind>`** — category (see table).
- **`<scope>`** — kebab-case area within the category.
- The extension is always `.md`. No date, no version in the name.

### Categories (`<kind>`)

| Prefix | Purpose | Examples |
| --- | --- | --- |
| `func-` | Functional behavior. What the service does. | `func-monitoring-checks.md`, `func-tenancy-rbac.md` |
| `ops-` | Operations: pipeline, runtime, deployment, monitoring, logging. | `ops-cicd.md`, `ops-logging.md` |
| `sec-` | Security: authn/authz, secrets, audit, attack surface. | `sec-authn-authz.md` |
| `cross-` | Cross-cutting norms that do not fit the other categories. Use sparingly. | (none yet) |

## Current contents

| File | Area | Status |
| --- | --- | --- |
| `func-monitoring-checks.md` | Check types, `Prober`, conditions engine, scheduler/worker | skeleton |
| `func-tenancy-rbac.md` | Organization/Project, membership, roles, isolation | skeleton |
| `func-sla-sli.md` | SLI/SLO/SLA, windows, error budget, maintenance windows | skeleton |
| `func-status-pages-incidents.md` | Status pages, components, incidents, postmortems, feeds | skeleton |
| `func-notifications.md` | Notification channels, binding to monitors, webhooks | skeleton |
| `func-incident-context.md` | Heuristic RCA context for auto-incidents (iter-0037) | implemented |
| `func-burn-rate-windows.md` | Multi-window multi-burn-rate alerts, SRE canon (iter-0038) | implemented |
| `func-confirm-phase.md` | Confirm phase: accelerated down confirmation (iter-0039) | implemented |
| `func-monitor-dependencies.md` | Dependency graph + cascading alert suppression (iter-0040) | implemented |
| `func-multi-region-quorum.md` | Multi-region quorum via composite, variant B (iter-0041) | implemented |
| `func-result-protocol.md` | Result ingest: typed origins, timestamp hygiene, `execution_revision` (P0a/P0b, D-0142) | spec |
| `sec-authn-authz.md` | OIDC (any issuer), local login, sessions, API tokens, client-credentials | implemented |
| `ops-cicd.md` | GitLab CI (backend/frontend), build, coverage gate | skeleton |
| `ops-keycloak-oidc.md` | Setup and integration guide for Keycloak and any OIDC providers | implemented |
| `ops-logging.md` | slog, levels, format, ban on logging secrets | skeleton |
| `ops-monitoring.md` | `cerbix_` metrics, health/readiness, alerts | skeleton |

> Skeletons are filled in during the corresponding iterations before implementing a feature
> (specification before code). Each spec lists its own `FR`/`NFR`, which are reflected in `docs/status.md`.
