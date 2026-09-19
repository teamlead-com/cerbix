# func-project-gate-policy — inherited reliability-gate policy (FR-034 / NFR-028)

> **Revision 1 — APPROVED for iter-0185 on 2026-09-19.** Backend/schema/API work is authorized by
> the owner instruction opening this iteration. The dedicated SPA editor mock
> [`mock-project-gate-policy.html`](../design/mock-project-gate-policy.html) is also approved by the
> owner on 2026-09-19; frontend implementation is authorized.

## 1. Problem

Reliability-gate policy exists only per service. A project with many services must repeat the same
document and then keep every copy synchronized. A newly created service is `NOT_CONFIGURED` until an
operator notices and writes another policy, so project governance is opt-in by omission.

The feature is inheritance, not policy copying: one project policy is resolved at evaluation time for
services without a live service override. The decision and screen must state which scope supplied the
effective policy.

## 2. Requirements

- **FR-034 — Project-level inherited gate policy.** A project administrator may declare one project
  gate policy. A service with a live service policy uses it; otherwise it inherits the project policy;
  otherwise it remains `NOT_CONFIGURED`.
- **NFR-028 — One deterministic effective policy.** Resolution, override binding, auditing, ledger
  evidence, authorization, and UI all use the same source tuple `(scope, owner_id, revision)` from one
  database snapshot. Inheritance is never implemented by copying rows into services.

## 3. Effective-policy algebra

At `evaluated_at`, under the service row lock and in the gate transaction:

1. A live service policy wins.
2. Else a live policy for the service's project is inherited.
3. Else the state is `NOT_CONFIGURED` with no action.

The result carries:

```json
{
  "policy_source": "service | project",
  "policy_owner_id": "uuid",
  "policy_revision": 7
}
```

These fields are present whenever a policy exists and are stored in every gate-decision ledger row.
They are evidence, not presentation hints. No response may call a project policy a service policy.

## 4. Policy documents and persistence

- `project_gate_policies` uses the same versioned document and validation owner as
  `service_gate_policies`: schema version, clauses, threshold, seal-lag bound, unknown behavior, and —
  until iter-0186 changes it — exactly one window.
- The project row is tombstoned, has a DB-owned monotonic revision never reused, and has a tenant FK to
  `projects(id, org_id)` or the repository's canonical project tenant key.
- Service policy storage remains the explicit override. No migration materializes inherited service
  rows; existing service policies continue to win with unchanged revisions.
- Existing decision rows are historical and remain readable. New ledger columns are nullable only for
  pre-migration rows and constrained for every post-migration insert.

## 5. API contract

New project resource:

- `GET /api/v1/projects/{projectID}/gate/policy`
- `PUT /api/v1/projects/{projectID}/gate/policy`
- `DELETE /api/v1/projects/{projectID}/gate/policy?expected_revision=N`

The write body and CAS semantics match the service policy resource. Project writes require project
administration (`global_admin`, org `org_admin`, or project `project_admin`); an editor may not change a
policy that affects every inheriting service.

Existing service policy resource becomes an effective read and explicit override write:

- `GET …/services/{serviceID}/gate/policy` returns the effective document plus `policy_source`,
  `policy_owner_id`, `policy_revision`, and optional `service_override_revision`.
- `PUT` always creates/replaces the explicit service override. When the effective source is project and
  no override exists, `expected_revision: null` is required; the project revision is not a valid CAS
  token for a service row.
- `DELETE` deletes only the explicit service override. If a project policy exists, the next read and
  evaluation inherit it. If no explicit override exists, deletion returns `404 not_overridden` and
  never touches the project policy.

Strict decoding remains mandatory. OpenAPI, Go DTOs, generated TypeScript, CLI output, and SPA types
move together.

## 6. Overrides and revisions

An active gate override binds to the effective source tuple, not to a naked integer revision:

- creating/changing/deleting a service policy revokes that service's active override;
- creating a service override while the project policy was effective revokes the inherited override;
- changing/deleting the project policy revokes active overrides for services currently inheriting it;
- services with live service policies are unaffected by a project-policy mutation.

Revocation occurs in the same transaction as the policy mutation with reason
`policy_changed|policy_deleted|policy_source_changed`. The update is set-based and indexed by project,
source scope, and revoked state. It does not walk services in application code.

## 7. Audit, UI, and CLI

- Project mutations write typed actor audit rows in the same transaction:
  `gate.project_policy.create|update|delete`.
- Service audit actions remain explicit-override actions; falling back to inheritance is visible in the
  delete target text without copying the project document.
- The service gate panel always shows `Service policy`, `Inherited from project`, or `Not configured`.
  Editors may create a service override; only project administrators see project-policy mutation
  controls. The inherited document is readable to every existing gate viewer.
- A dedicated project policy editor is a new SPA surface and requires an owner-approved artifact mock
  before frontend implementation. Backend/schema work may proceed only after the spec itself is
  approved; SPA work waits for the mock.
- CLI gate-check output includes the policy source. Project policy CRUD in CLI is optional and not part
  of iter-0185.

## 8. Security and tenant isolation

- Project policy reads/writes are constrained by caller-visible `org_id/project_id`.
- Effective resolution joins the service and project policy inside the same tenant scope. A foreign
  project policy id or manually corrupted row is never used.
- Missing and foreign ids remain non-oracular (`404 not found`).
- No project/service/user id becomes a metric label.

## 9. Observability

Existing gate evaluation metrics gain one bounded `policy_source="service|project|none"` label where
they already count decisions. Policy mutation metrics use operation/result labels only. The runbook
adds inheritance diagnosis: source tuple, revision conflicts, mass override revocation, and how to
return one service to inheritance safely.

## 10. Acceptance invariants

1. Exactly one source wins in the order service → project → none, in the same transaction as evidence.
2. Inheritance creates no service policy rows and no background synchronization job.
3. Service CAS never accepts a project revision, and project CAS never accepts a service revision.
4. Deleting a service override falls back to project policy without deleting or mutating that policy.
5. Every override is bound to a full source tuple and becomes inert when that tuple changes.
6. A project mutation revokes only inherited-service overrides; explicit-service overrides survive.
7. API, ledger, CLI, and SPA state the same source and revision.
8. Tenant and role refusals are non-oracular and covered at store, API, and direct-SQL boundaries.

## 11. Required tests and gates

- Domain table tests for source precedence and revision tuples.
- PostgreSQL migration/constraint tests, project-policy CRUD/CAS/no-op/tombstone tests, inherited
  resolution in one snapshot, set-based revocation, and cross-project corruption refusals.
- API authorization and strict-body tests; OpenAPI/Go/generated-client parity.
- Existing service-policy compatibility tests proving old explicit policies evaluate identically.
- SPA component tests and live Playwright for inherited display, create override, delete-to-inherit,
  forbidden editor controls, stale CAS, and no-policy state — only after mock approval.
- Full Go/race/vet/build/lint/docs/migration gates and frontend type/build/vitest/live E2E.

## 12. Iter-0185 plan and role split

| Priority | Task | Role |
| --- | --- | --- |
| P0 | Approve inheritance/source/revision contract | Owner + Agent C |
| P0 | Add project policy schema and resolution | Agent A |
| P0 | Prove CAS, tenancy and override revocation | Agent B |
| P1 | Produce and approve the SPA mock | Owner + Agent C |
| P1 | Add API, effective reads and UI | Agent A |
| P1 | Extend gate metrics and runbook | Agent D |
| P2 | Synchronize traceability and run all gates | Agents B/C/D |

## 13. Non-goals

Organization-wide policies, policy templates, per-service opt-out/disable, bulk materialization,
different project policies by environment/tag, CLI policy authoring, and all-window evaluation (owned by
iter-0186).
