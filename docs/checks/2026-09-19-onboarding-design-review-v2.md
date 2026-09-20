# Onboarding design review v2 — 2026-09-19

Immutable corrective review of `func-onboarding.md` revision 4 and
`docs/design/mock-onboarding.html` for iter-0187. The original
[`2026-09-19-onboarding-design-review.md`](2026-09-19-onboarding-design-review.md) remains the
revision-2 snapshot. This v2 snapshot incorporates the owner's two corrections: existing projects
must retain their normal Dashboard, and the mock must follow Cerbix's actual component rules rather
than approximate them.

This is a technical/design checklist pass, not owner approval.

## Review result

| Approval check | Result | Evidence |
| --- | --- | --- |
| Product route and navigation terminology match Cerbix | PASS | Sidebar and breadcrumb use `Dashboard`, matching `AppShell.vue` and `DashboardView.vue`; the retired mock-only `Overview` wording is absent. |
| Product shell follows current source | PASS | The product viewport transcribes the 240 px AppShell sidebar, workspace switcher, Project navigation, breadcrumbs, search, `New monitor`, theme control, account avatar, 1180 px content width and semantic tokens. |
| Existing Dashboard content follows current source | PASS | Existing install renders four `Kpi`-shaped cards, the 90-day SVG availability card, and `MonitorCard`-shaped cards with `StatusPill`, `UptimeBar`, `Sparkline` and error-budget geometry. |
| Manual onboarding is additive | PASS | `Get started` opens one compact card above the Dashboard. Collapsing and reopening it leaves four KPI cards, 90 availability segments and three monitor cards unchanged. |
| Automatic existing-install behavior is non-disruptive | PASS | The contract keeps the guide closed automatically when a monitor already has persisted heartbeat evidence; no route guard, modal or replacement state is proposed. |
| All onboarding states remain represented | PASS | Twelve artifact tabs cover fresh admin, org admin, viewer handoff, monitor choice, waiting, worker unavailable, scheduler unknown, first UP, first DOWN, failed read, existing install and push. |
| Accessibility and narrow layout remain reviewable | PASS | Focus styles, ordered progress, `aria-current`, reduced motion, status text, spec-note and narrow-width controls remain present. |
| Exact artifact has owner approval | PENDING | Owner must approve or reject `docs/design/mock-onboarding.html` revision 4. |

## Mechanical and browser verification

- HTML parser: required ids present, no duplicate ids, `Dashboard` present and mock-only `Overview`
  absent.
- Inline script: `node --check` passed.
- Headless Chrome/Playwright: all 12 state tabs opened with their expected heading and active Dashboard
  shell; notes, theme and width toggles passed with no console or page errors.
- Existing-install assertions: 4 KPI cards, 90 project-availability rects, 3 monitor cards and 1 manual
  guide before collapse; 3 monitor cards remained after collapse; `Get started` reopened the guide.
- Desktop and narrow screenshots were visually inspected after rendering. The desktop viewport keeps
  the normal Dashboard hierarchy; the narrow viewport stacks the new guide and existing cards without
  changing their component language.

## Source-of-truth mapping

| Mock surface | Frontend owner used for the transcription |
| --- | --- |
| Tokens, fonts, radii, shadows and themes | `frontend/src/style.css`, `frontend/tailwind.config.js` |
| Sidebar, switcher, navigation, breadcrumbs and top actions | `frontend/src/components/AppShell.vue`, `frontend/src/components/BrandMark.vue`, `frontend/src/components/SearchBox.vue` |
| Project heading, KPI grid, 90-day availability and card grid | `frontend/src/views/DashboardView.vue` |
| KPI typography and spacing | `frontend/src/components/Kpi.vue` |
| Monitor cards, type badges, metrics and error budget | `frontend/src/components/MonitorCard.vue` |
| Status, availability ticks and latency sparkline | `frontend/src/components/StatusPill.vue`, `frontend/src/components/UptimeBar.vue`, `frontend/src/components/Sparkline.vue` |

## Role reviews

- **Agent A — Implementation/design inventory:** compared the artifact directly against the current
  Vue/Tailwind owners above; no production file changed.
- **Agent B — Tests/accessibility:** ran the 12-state browser matrix, exact Dashboard-count assertions,
  collapse/reopen interaction and desktop/narrow visual review.
- **Agent C — Docs + Traceability:** promoted the contract and references to revision 4 and preserved
  the earlier review as an immutable snapshot.
- **Agent D — Observability + Ops:** confirmed the style correction changes no runtime, API, metric,
  readiness or recovery contract.

## Open owner questions

1. Approve or reject the inline Dashboard guide and exact revision-4 artifact.
2. For a long wait with a live worker region, keep generic `No result yet` diagnostics or authorize a
   later bounded scheduler readiness/last-success read owned by scheduler/ops.
