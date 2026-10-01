# iter-0195 — Wave 2 architecture and operator documentation refresh

**Opened:** 2026-10-01
**Lifecycle:** CLOSED BY THE OWNER — independent docs-only review APPROVED with no findings; one local commit authorized, push not authorized.
**Requirement:** none; this iteration reconciles documentation and documentation-check tooling with already-shipped behavior.
**Decision:** none. No new product, architecture, storage, transport, compatibility, or toolchain policy is chosen.
**Change class:** documentation and documentation-check tooling only; no runtime, API, OpenAPI, schema, migration, Makefile, frontend, generated asset, deployment, or production change.

## Goal

Close the documentation gaps deliberately left by Wave 1 without rewriting unrelated documents: document
current-domain persistence boundaries, expose the two shipped operator recovery paths, describe the shipped
cross-brand shell behavior, make architecture claims part of `make docs-check`, and record observed
build/toolchain contexts without inventing policy intent.

## Exact scope

| Area | Wave 2 result | Source of truth |
| --- | --- | --- |
| Modern current-domain ERD | Add partial ERD views for service reliability, the reliability gate, change intelligence, the expected-run ledger, and audit logs. Mark nullable references, composite keys, partitioning, and deliberate non-FK historical references. | `internal/store/migrations/00064`–`00094`, `00102`–`00109`; `internal/store/audit.go`; the corresponding `docs/specs/func-*.md` files. |
| Operator recovery CLI | Catalog `cerbix adopt-fact-month` and `cerbix enqueue-service-repair` as operator/admin maintenance tools, including required flags, scope, no dry-run/confirmation behavior, idempotency/coalescing, maintenance-window caveats, and affected reliability facts. | `internal/cli/cli.go`; `internal/cli/cli_adopt_test.go`; `internal/cli/cli_repair_test.go`; `docs/runbook.md`. |
| Cross-brand and shell coverage | Add a short factual overview of Sealed C, custom-logo priority, contrast-safe accents, responsive drawer, workspace fencing, accessible overlays, SearchBox fencing, truthful Dashboard states, breadcrumbs, theme, and announcements. Preserve the no-deploy/infrastructure and no-API/data-model boundary. | `docs/brand-guidelines.md`; `docs/specs/cross-brand-identity.md`; shipped Vue components/stores/views. |
| Architecture docs-check | Add `docs/architecture.md` to living-document reference checks and add focused semantic guards for bare agent routes, adaptive heartbeat-storage vocabulary/universal partition claims, retired heartbeat ERD fields, and falsely exact queue diagrams. | `scripts/check-docs-references.py`; `scripts/check_docs_references_test.py`; current architecture/source files. |
| Toolchain policy wording | Record declared Go floor, CI/source context, Node 22 snapshot/CI context, Docker Node 26 and Go 1.27 release stages, canonical `make spa-snapshot`, and the absence of a host-Node requirement. Treat alignment as follow-up because intent is not proven. | `go.mod`; `Makefile`; `frontend/Makefile`; `docker/Dockerfile`; `.github/workflows/{build,tests,security,image}.yml`. |
| Runbook links | Add only links to the current partial ERD and the operator recovery catalog around the existing service-reliability procedures. | `docs/runbook.md`; `docs/architecture.md`; `docs/overview.md`. |

## Documentation changed

- `README.md`
- `CHANGELOG.md`
- `docs/architecture.md`
- `docs/overview.md`
- `docs/runbook.md`
- `docs/specs/README.md`
- `docs/status.md`
- `docs/traceability.md`
- this report
- `scripts/check-docs-references.py`
- `scripts/check_docs_references_test.py`

`INSTALL.md` was reviewed against the current Wave 1 wording and required no additional Wave 2 edit.
No decision record is added: this iteration documents existing contracts and observed pins rather than
choosing a future architecture or toolchain policy.

## Acceptance criteria

- [x] The architecture contains partial, source-linked ERD views for all five requested domains.
- [x] ERD prose distinguishes PK, UNIQUE, FK, nullable references, partitions, and deliberate non-FK history.
- [x] README/overview identify both shipped operator recovery commands and their maintenance boundaries.
- [x] README and overview describe the shipped cross-brand/shell behavior without marketing claims or API/data-model drift.
- [x] `docs/architecture.md` is included in living-document path/test citation checking.
- [x] Architecture semantic guards reject stale agent routes, universal heartbeat partition claims, retired heartbeat ERD fields, and falsely exact incomplete queue topology.
- [x] Unit fixtures fail on each stale formulation and pass on the current architecture.
- [x] Toolchain text separates declared floor, local/CI builders, release-image builders, and unproven policy alignment.
- [x] Runbook changes remain link-level and do not rewrite recovery procedures.
- [x] One compact Unreleased changelog item records the Wave 2 documentation and guard coverage.
- [x] Only authorized documentation/tooling files change; no production/runtime/API/schema/frontend/generated asset changes occur.
- [x] `make docs-check`, targeted checker tests, and `git diff --check` pass.
- [x] Independent docs-only review is `APPROVED` with no Critical, Important, or Minor findings.
- [x] Owner approval was received on 2026-10-01 before closure and before the one local commit.

## Verification

- PASS — `python3 -m unittest -v scripts.check_docs_references_test.ArchitectureSemanticGuards`: 8
  architecture guard cases pass, including every bare agent route, adaptive storage vocabulary, all
  retired heartbeat ERD claims, and incomplete exact-queue topology.
- PASS — `python3 -m unittest -q scripts/check_docs_references_test.py`: 192 tests pass.
- PASS — `make docs-check`: 192 checker tests pass and the living-document reference/acceptance gate is
  green, including `docs/architecture.md`.
- PASS — `git diff --check`.
- PASS — the changed-file set contains only the authorized documentation/tooling files plus this report:
  `README.md`, `CHANGELOG.md`, `docs/architecture.md`, `docs/overview.md`, `docs/runbook.md`,
  `docs/specs/README.md`, `docs/status.md`, `docs/traceability.md`,
  `scripts/check-docs-references.py`, `scripts/check_docs_references_test.py`, and this report.
  No `frontend/`, Go, API/OpenAPI, migration, Makefile, Docker-image or generated-SPA path changed.
- PASS — source-claim spot checks cover the five migration-backed ERDs, both CLI recovery entrypoints and
  end-to-end CLI tests, the cross-brand specification and shipped shell components, and the declared
  toolchain files.

## Review and closure

- **APPROVED** — independent read-only docs-only review of the final current tree on `gpt-5.6-sol`;
  no Critical, Important, or Minor findings remain after the correction round.
- **Owner approval:** the owner approved closure and the one local commit on 2026-10-01.
- Iter-0195 is **CLOSED BY THE OWNER**. The authorized commit is documentation/tooling only;
  push, PR, merge, deploy, restart, and production mutations remain unauthorized.

## Known non-goals / Wave 3 candidates

- No full database ERD beyond the five current-domain partial views.
- No new recovery command, recovery API, dry-run mode, confirmation mechanism, or runtime safety feature.
- No architecture semantic parser for all Mermaid syntax; guards remain focused on known stale-claim shapes.
- No formal decision about aligning Go 1.25.13/1.27 or Node 22/26.
- No runbook rewrite, deployment guidance expansion, cross-brand redesign, status/reliability formula change,
  API/OpenAPI change, schema/migration change, production code, frontend code, Makefile, Docker image, or
  generated SPA asset change.
- Any broader UX documentation, route/queue/ERD checker expansion, or additional domain history belongs
  to a later owner-approved iteration.

## Lifecycle

The independent docs-only review and owner approval gates both completed on 2026-10-01. Iter-0195 is
**CLOSED BY THE OWNER**. The one local commit carrying this report is authorized; push, PR, merge, deploy,
restart, and production mutations remain unauthorized.
