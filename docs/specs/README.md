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
| `ops-` | Operations: runtime, deployment, monitoring, logging. | `ops-logging.md` |
| `sec-` | Security: authn/authz, secrets, audit, attack surface. | `sec-authn-authz.md` |
| `cross-` | Cross-cutting norms that do not fit the other categories. Use sparingly. | (none yet) |

## Current contents

Two axes, deliberately separate. **Spec** is how deep the DOCUMENT is written; **Feature** is what
exists in the product. They diverge in both directions — an area can ship on top of a thin document
(the early features did), and a document can be written to implementable depth before any code
(everything since FR-017 does). One column would have to lie about one of them.

| File | Area | Spec | Feature |
| --- | --- | --- | --- |
| `func-service-incidents.md` | A Service can own an incident (FR-022/NFR-017, opened by D-0169) | CLOSED at iter-0156 (D-0171): service incidents shipped — schema, evaluator, correlation, public projection, SPA deltas | commissioned and built (iter-0156) |
| `func-reliability-gate.md` | A deploy asks whether the error budget allows it (FR-024/NFR-019, opened after iter-0161): a pipeline calls `cerbix gate check` (or one `POST`) immediately before a protected step and gets `ALLOW`/`WARN`/`BLOCK`/`UNKNOWN`/`NOT_CONFIGURED` with every matching reason and the evidence under it — read from facts cerbix has already sealed, never recomputed — governed by a per-service policy that names ONE SLO window and what each clause does, with a time-bounded audited override that changes the action but never the observed state, and a bounded decision ledger that outlives the service | full (21 invariants, §7 matrix); design approved at revision 13 (D-0201) | IMPLEMENTED — DONE at iter-0163 (D-0208): schema, store, fenced ledger maintenance, API + limiter + metrics, CLI, the SPA per the owner-approved mock (D-0207) |
| `func-change-intelligence.md` | A pipeline says what it changed, and the service's facts say what followed (FR-025/NFR-020, opened after iter-0163) | revision 3 (16 decisions, 23 invariants, §7 matrix, 7 owner questions answered) + forward corrections D-0211/D-0212 recorded in place | IMPLEMENTED — DONE at iter-0165 (D-0213; design approved D-0209, UI mock approved D-0210): schema 00094, domain, the ten `change.*` keys, the store (record under the identity lock, timeline, comparison through the series owner, correlation at incident open, retention by whole groups), the token `actions` allow-list in `authz.Can`/`VisibleScope`, the four routes + limiter + metrics + OpenAPI, `cerbix change record`, and the SPA per the approved mock — discharged against §6 as a SET and the §7 matrix, with the browser suite green on a live stack |
| `func-expected-run-ledger.md` | The fact that a run was expected (FR-032, opened by D-0235): cerbix records what happened and not what was supposed to happen, so no surface can distinguish an interval nothing was due in from one whose due run never ran | problem statement only — NOT DESIGNED, no schema, API or retention rule | NOT IMPLEMENTED: needs its own design review and a data-model migration plan |
| `func-truthful-rendering.md` | A rendering claims no more than the facts behind it (FR-031/NFR-025, opened after iter-0173): the reliability timeline's geometry and five encodings, the monitor's Response time panel, and the /sla project-objective card | full (21 invariants, §10) — design approved, D-0235 | NOT IMPLEMENTED: the mock is the next deliverable and gates all frontend code |
| `func-service-reliability.md` | Service as a reliability-domain resource: two axes, sealed facts, impact graph, status-page projection, alerting ownership (FR-021/NFR-016, D-0159/0166/0167/0168) | full (91 invariants) | phases 1–5 shipped; §16.9 items deferred by owner decision |
| `func-secret-inventory.md` | Project-scoped write-only secrets, typed refs, encrypted dispatch (FR-020/NFR-015, D-0155) | full | shipped |
| `func-monitoring-as-code.md` | Hot-reconciled, tenant-scoped Monitoring-as-Code file provider (FR-017, D-0145) | full | shipped |
| `func-result-protocol.md` | Result ingest: typed origins, timestamp hygiene, `execution_revision` (D-0142) | full | shipped |
| `func-oncall-synthetic-pull.md` | On-call/escalations, synthetic checks, HTTP-pull agent | full | shipped |
| `func-project-deletion.md` | Delete a project (FR-018) | full | shipped |
| `func-org-deletion.md` | Delete an organization (FR-019) | full | shipped |
| `func-geo-worker-pools.md` | Geo-distributed probers, region-aware worker pools | written | shipped |
| `func-admin-users.md` | Instance-wide Users administration for the Global Admin | written | shipped |
| `func-settings-members.md` | Members moved into Settings, the Administration group | written | shipped |
| `func-hardening.md` | Hardening package from the 2026-08 deep audit | written | shipped |
| `func-audit-gaps.md` | Audit gap package: saved-but-never-used functionality | written | shipped |
| `func-audit-gaps-2.md` | Audit gap package 2, the second layer | written | shipped |
| `func-e2e-coverage.md` | E2E coverage expansion beyond the D-0124 smoke suite | written | shipped |
| `func-transport-resilience.md` | SSE and transport-level survivability gaps | written | shipped |
| `func-observability-logging.md` | Operational logging expansion beyond failures | written | shipped |
| `func-incident-context.md` | Heuristic RCA context for auto-incidents (iter-0037) | written | shipped |
| `func-burn-rate-windows.md` | Multi-window multi-burn-rate alerts, SRE canon (iter-0038) | written | shipped |
| `func-confirm-phase.md` | Confirm phase: accelerated down confirmation (iter-0039) | written | shipped |
| `func-monitor-dependencies.md` | Dependency graph + cascading alert suppression (iter-0040) | written | shipped |
| `func-multi-region-quorum.md` | Multi-region quorum via composite, variant B (iter-0041) | written | shipped |
| `func-monitoring-checks.md` | Check types, `Prober`, conditions engine, scheduler/worker | skeleton | shipped |
| `func-tenancy-rbac.md` | Organization/Project, membership, roles, isolation | skeleton | shipped |
| `func-sla-sli.md` | SLI/SLO/SLA, windows, error budget, maintenance windows | skeleton | shipped |
| `func-status-pages-incidents.md` | Status pages, components, incidents, postmortems, feeds | skeleton | shipped |
| `func-notifications.md` | Notification channels, binding to monitors, webhooks | skeleton | shipped |
| `sec-authn-authz.md` | OIDC (any issuer), local login, sessions, API tokens, client credentials | written | shipped |
| `ops-keycloak-oidc.md` | Setup and integration guide for Keycloak and any OIDC provider | full | shipped |
| `ops-logging.md` | slog, levels, format, the ban on logging secrets | skeleton | shipped |
| `ops-monitoring.md` | `cerbix_` metrics, health/readiness, alerts | skeleton | shipped |

> A `skeleton` beside a shipped feature is a real debt, not a formality: those five areas were built
> before this project adopted specification-before-code, so the document does not yet describe what
> the code does. Everything from FR-017 onward was written to implementable depth FIRST, which is why
> its `Spec` column says `full`. Each spec names its own `FR`/`NFR`, reflected in `docs/status.md`.
