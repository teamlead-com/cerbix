# Spec: Incident-aware overall status on public pages

**File:** `func-status-pages-overall-status.md`
**Requirements:** FR-038 / NFR-032
**Iteration:** iter-0191; post-close hardening iter-0192
**Status:** IMPLEMENTED — owner-approved revision 1 delivered in iter-0191 and hardened under D-0263
in iter-0192 on 2026-09-21
**Related contracts:** [`func-status-pages-incidents.md`](func-status-pages-incidents.md),
[`func-service-reliability.md`](func-service-reliability.md) §15.0/§17

## 1. Purpose

The public status-page hero must communicate two independent truths without turning either into the
other:

1. what the currently measured components report; and
2. whether Cerbix is still communicating an active incident.

Today an all-operational component summary produces `All systems operational` even when the same
response contains active Major or Critical incidents. Both source facts are individually correct, but
their composition is an all-clear that contradicts the page directly below it. This contract removes
that false all-clear without deriving component health from incident lifecycle.

## 2. Problem

The render response already contains:

- `summary`, `summary_state`, and `unmeasured_count`, owned by component/reliability evaluation; and
- `active_incidents[]`, each with a public lifecycle status and explicit impact.

The SPA currently renders the hero entirely from the component summary. Consequently:

- a recovered monitor can make every component operational while its incident is still
  `investigating`, `identified`, or `monitoring`;
- a manually opened project-level Major incident can coexist with operational components by design;
- stale but unresolved incidents remain active, yet the page says `All systems operational`;
- the green band and check icon reinforce the false all-clear even though `Active incidents` is
  visible lower on the same page.

The defect is in presentation composition, not in either underlying domain fact.

## 3. Canonical facts and rule owners

### 3.1 Component health remains authoritative

`summary`, `summary_state`, and `unmeasured_count` keep their existing meanings and owners. An
incident MUST NOT mutate, downgrade, or synthesize any component status. Uptime strips, component
badges, feeds, service reliability, maintenance semantics, and SLA calculations remain unchanged.

### 3.2 Incident attention remains authoritative

`active_incidents[]` is the only incident-attention source for this page render. The presentation
layer derives:

```text
active_count  = len(active_incidents)
worst_impact  = max(active_incidents[].impact)
impact_order  = critical > major > minor > none
```

Resolved incidents in `recent_incidents[]` do not participate. Incident age does not participate: an
old unresolved incident is still active until the incident lifecycle owner resolves it. The status
page MUST NOT silently hide, auto-resolve, or downgrade it.

### 3.3 No duplicate transport fact

No new backend field, database column, migration, configuration key, metric, or persisted summary is
introduced. `active_count` and `worst_impact` are deterministic projections of the already delivered
public `active_incidents[]` array. Adding transport fields for the same facts would create a second
owner that could drift from the list rendered below the hero.

## 4. Scope

### In scope

- Public and authenticated-preview status-page hero headline, supporting copy, icon, and attention
  band.
- A pure presentation helper that composes component summary and active-incident attention.
- Unit, component, and live desktop/430 px regressions for the complete state matrix.
- Preservation of public redaction and public/preview parity.

### Out of scope

- Incident opening, acknowledgement, status progression, automatic resolution, or stale-incident
  policy.
- Component summary algebra or severity ordering.
- Changing component rows from `Operational` merely because an incident is open.
- Feeds, webhooks, subscriptions, postmortems, incident detail, maintenance, or SLA/SLO calculations.
- Redefining the hero's existing `updated` timestamp; timestamp freshness requires a separate source
  contract because the current field is page metadata rather than a complete status-fact watermark.

## 5. Overall presentation model

The hero has one primary headline but may state both facts in its supporting sentence. Primary
ownership follows this order:

1. **Measured impairment owns the headline.** When `summary_state == impaired`, retain the existing
   component headline (`Degraded performance`, `Partial system outage`, `Major system outage`, or
   maintenance copy). Active-incident count and worst impact are appended to the supporting copy.
2. **Otherwise an active incident owns the headline.** When `active_count > 0` and measured
   components are not impaired, render `1 active incident` or `<N> active incidents`. The supporting
   copy states the worst incident impact and then states the component measurement truth.
3. **Otherwise preserve the existing component-only hero.** With no active incidents, every current
   headline, subline, icon, band, `no_data`, `empty`, and unmeasured rule remains byte-for-byte in
   meaning.

Compatibility fallback: if an older response lacks `summary_state`, any measured `summary` other
than `operational` owns the headline. `no_data` and an absent summary remain non-all-clear states.

## 6. Required copy and visual matrix

| Component fact | Active incident fact | Primary headline | Supporting copy | Visual owner |
| --- | --- | --- | --- | --- |
| all measured operational | none | `All systems operational` | Existing operational subline, including any unmeasured disclosure | existing component operational treatment |
| all measured operational | one or more | `1 active incident` / `<N> active incidents` | `<Impact>. All measured services are currently operational.` plus the existing unmeasured disclosure when applicable | incident attention |
| `no_data` | none | Existing `No measurements available` | Existing no-data subline | existing neutral component treatment |
| `no_data` | one or more | `1 active incident` / `<N> active incidents` | `<Impact>. Component measurements are not available yet.` | incident attention |
| `empty` | none | Existing `No components configured` | Existing empty-page subline | existing neutral component treatment |
| `empty` | one or more | `1 active incident` / `<N> active incidents` | `<Impact>. No components are configured on this page.` | incident attention |
| measured impairment or maintenance | none | Existing component headline | Existing component subline | existing component severity |
| measured impairment or maintenance | one or more | Existing component headline | Existing component subline followed by `<N> active incident(s) · <Impact>.` | existing component severity |

Impact copy is total:

| Worst impact | Text | Attention treatment when incident owns hero |
| --- | --- | --- |
| `critical` | `Critical impact` | danger/red, alert icon |
| `major` | `Major impact` | warning/amber, alert icon |
| `minor` | `Minor impact` | warning/amber, alert icon |
| `none` | `Impact not specified` | neutral non-green, information/alert icon |

The exact phrase `All systems operational` MUST NOT appear anywhere in the hero while
`active_count > 0`. Green banding and the all-clear check icon are likewise forbidden in that state.
Text remains authoritative; colour is never the only carrier of incident attention.

## 7. Approved owner decisions

The owner approved all five decisions on 2026-09-21. They are normative implementation authority:

1. **Primary precedence:** active incidents own the headline only when measured components are not
   already impaired.
2. **Operational-with-incident copy:** render `N active incidents` with `Major impact. All measured
   services are currently operational.` rather than weakening the incident into an operational
   headline.
3. **No transport change:** derive count and worst impact from the existing redacted
   `active_incidents[]`.
4. **No stale-age heuristic:** old unresolved incidents remain visible and suppress all-clear until
   resolved by their lifecycle owner.
5. **Visual mapping:** Critical uses danger; Major and Minor use warning; unspecified impact uses a
   neutral non-green treatment.

## 8. Accessibility and responsive behaviour

1. The hero retains exactly one `h1` whose accessible name equals the selected primary headline.
2. Incident count, worst impact, and component measurement truth are present as text.
3. The hero does not use an ARIA live region; this view is loaded as a page and must not announce
   duplicate state during hydration.
4. Copy wraps without horizontal overflow at 430 px. The update timestamp may move to its existing
   wrapped position but must not overlap the headline or incident copy.
5. Public and authenticated-preview renders use the same composition helper and differ only by their
   existing authorization/redaction boundaries.

## 9. Acceptance invariants

| ID | Invariant |
| --- | --- |
| AC-0191-1 | `All systems operational`, the green all-clear band, and the all-clear check icon appear only when there are zero active incidents and the existing component rules permit an all-clear. |
| AC-0191-2 | Operational components plus active incidents render an incident-count headline and state both worst impact and current measured-service health. |
| AC-0191-3 | A measured component impairment keeps its existing headline; active incident count/impact is additional context and never overwrites the measured severity. |
| AC-0191-4 | `no_data` and `empty` remain explicit while active incidents still own the primary headline. |
| AC-0191-5 | Worst impact is deterministic and independent of API incident order: `critical > major > minor > none`. |
| AC-0191-6 | Resolved/recent incidents and incident age do not affect the hero; only `active_incidents[]` participates. |
| AC-0191-7 | No new API field, persistence, metric, config, or internal identifier exposure is introduced. |
| AC-0191-8 | Public and authenticated-preview hero composition is identical for identical public facts. |
| AC-0191-9 | Desktop and 430 px layouts remain readable, keyboard-neutral, text-complete, and free of horizontal overflow. |

## 10. Required implementation slices

### Agent A — Implementation

- Add a pure incident-attention/overall-presentation helper under `frontend/src/lib`.
- Refactor the hero in `PublicStatusView.vue` to consume that one helper for headline, subline, icon,
  band, and text class.
- Reuse existing component and incident vocabularies; do not add backend or OpenAPI state.

### Agent B — Tests

- Add a table-driven helper matrix for operational/impaired/no-data/empty × zero/one/many incidents
  × every impact.
- Add component regressions proving the all-clear phrase/icon/band are absent with an active Major
  incident, component impairment retains precedence, and order does not change worst impact.
- Add live public and preview coverage at desktop and 430 px.

### Agent C — Docs + Traceability

- On approval, record the decision in `docs/decisions.md`, move FR-038/NFR-032 and AC-0191 rows through
  `IN_PROGRESS` to `DONE`, and synchronize PRD, roadmap, traceability, changelog, and the final
  iteration report.
- Do not modify closed iter-0189 or iter-0190 reports and do not add local session links.

### Agent D — Observability + Ops

- Confirm no metric or alert is warranted because no runtime health fact changes.
- Verify response size and cache behaviour are unchanged, and the live stack remains ready while the
  public and preview pages render the same composition.

## 11. Required verification

- focused frontend helper and `PublicStatusView` tests;
- full frontend unit suite, `vue-tsc`, and Vite production build;
- generated API and committed SPA snapshot parity even though the API schema is intentionally
  unchanged;
- `go test ./...`, `make race`, build, vet, and configured lint as repository-wide regression gates;
- `make docs-check` and `git diff --check`;
- live public and authenticated-preview Playwright at desktop and 430 px with operational components
  plus at least one active Major incident.

## 12. Definition of done

The approved decisions in §7 remain unchanged; the implementation satisfies AC-0191-1 through
AC-0191-9; the phrase, colour, and icon mutations each have a killing regression; all required gates
pass; living documents and changelog describe the shipped behaviour; and iter-0191 closes without
modifying either prior immutable iteration report.

Post-close review corrections do not rewrite that closure snapshot. D-0263 and iter-0192 own the
authoritative-`summary_state` correction, shared incident vocabulary reuse, complete cross-product and
conflict matrix, full live unmeasured-disclosure assertion, and restoration of immutable iter-0191.
