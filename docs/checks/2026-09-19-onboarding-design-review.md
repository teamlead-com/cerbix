# Onboarding design review — 2026-09-19

Immutable structured review of `func-onboarding.md` revision 2 and
`docs/design/mock-onboarding.html` for iter-0187. This is a technical/design checklist pass, not owner
approval.

## Review result

| Approval check | Result | Evidence |
| --- | --- | --- |
| A failed read never becomes empty state | PASS | `failed_read` has precedence; the mock's Failed read state says nothing was marked incomplete. |
| Every step names completion and authorization | PASS | §12 inventory maps organization, project, monitor and heartbeat facts to central authz actions. |
| Required audience and runtime states are covered | PASS | Twelve artifact tabs cover fresh global admin, org admin without a project, viewer handoff, partial/waiting, worker unavailable, scheduler-unknown diagnosis, UP, DOWN, failed read, existing installation and push. |
| Skip and re-entry are non-coercive | PASS | Inline region, normal navigation, `Skip for now`, scoped automatic-display dismissal and shell `Get started`. |
| No hidden default or duplicate validation owner | PASS | Existing create dialogs/full monitor form own mutations; no sample resources or reduced monitor form. |
| Accessibility and narrow width are annotated | PASS | Ordered step list, `aria-current`, polite live region, focus restoration rule, reduced motion and vertical narrow layout. |
| API delta is explicit and tenant-isolated | PASS WITH OPEN CHOICE | Canonical list/heartbeat/region reads suffice for progress; scheduler diagnostic remains an explicit future choice. |
| Exact artifact has owner approval | PENDING | Owner must approve or reject `docs/design/mock-onboarding.html`. |

The artifact was also rendered with local headless Chrome at `1440×1000` and `430×900`. The desktop
shell, step rail, actions and annotations were legible; the narrow render hid the sidebar, stacked the
rail above content, preserved the horizontal review-state chooser and required no product-content
horizontal scrolling. Browser stderr contained only sandbox profile/crash-report warnings; both PNGs
were produced successfully.

A temporary Playwright QA script outside the repository then opened the artifact with local Chrome,
selected all 12 review tabs and asserted each expected state heading and active tab, toggled spec notes,
dark theme and interactive narrow width, and captured the failed-read, scheduler-unknown and
narrow/dark states. Result: PASS, with no browser console errors or uncaught page errors.

## Role reviews

- **Agent A — Implementation inventory:** verified current workspace/create routes, central capability
  predicates, typed monitor form ownership, heartbeat completion fact and region liveness. No production
  change is required or authorized.
- **Agent B — Tests/accessibility:** reviewed state precedence, negative/failure branches, UP/DOWN
  semantics, keyboard/live-region/reduced-motion/narrow-width requirements and future test seams.
- **Agent C — Docs + Traceability:** produced revision 2, artifact mock, status/traceability/roadmap/
  decision synchronization and the open iteration report.
- **Agent D — Observability + Ops:** rejected external analytics and resource-id labels, kept dismissal
  local and non-authoritative, preserved secret/token owners, and isolated scheduler diagnosis as a
  missing fact rather than inferring an outage.

Parallel subagent attempts returned provider 429 errors. Per repository workflow, the four reviews were
therefore completed sequentially and recorded independently above.

## Open owner questions

1. Approve the inline dashboard guide and exact revision-2 artifact, or reject it with requested changes.
2. For a long wait with a live worker region, keep generic `No result yet` diagnostics or authorize a
   later bounded scheduler readiness/last-success read owned by scheduler/ops.
