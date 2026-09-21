# Spec: Status pages and incidents (func-status-pages-incidents)

> Revision 1 — **APPROVED AND IMPLEMENTED 2026-09-21** in `iter-0189`.
> Requirements: `FR-037`, `NFR-031`. Decision: `D-0260`.
> Delivery evidence: [`iter-0189.md`](../iterations/iter-0189.md); semantic/performance hardening:
> [`iter-0190.md`](../iterations/iter-0190.md), `D-0261`.

## 1. Purpose

Cerbix status pages must remain quickly readable when several incidents are open at once. The page
must answer these questions in order:

1. What is the overall state?
2. Which published services/components are affected?
3. What incidents explain that state, and what is the latest public update?
4. Is maintenance scheduled?
5. What happened recently?

The status page is a public communication surface, not an operator console. It must show only
page-configured public facts and must not expose monitor IDs, service IDs, project IDs, external
correlation keys, actors, or inferred root causes.

## 2. Problem

The current public page renders every active incident as a large card before the component list.
With multiple incidents this pushes the service state below the fold, repeats lifecycle status in
the card and latest-update block, gives low-value prominence to the incident source (`auto`), and
requires a small secondary link to reveal the timeline. The result is correct but difficult to scan.

The current public incident projection also redacts the internal monitor/service anchors. That is
correct for privacy, but it means the frontend cannot safely link an affected published component to
its incident without a dedicated page-local relation.

## 3. Scope

### In scope

- Public and authenticated-preview status-page rendering.
- Canonical section order and compact active-incident presentation.
- A privacy-safe page-local mapping from an incident to affected published components.
- Accessible accordion and component-to-incident navigation.
- Deterministic ordering and high-count grouping.
- Responsive behaviour at desktop and narrow mobile widths.

### Out of scope

- Incident lifecycle, impact algebra, auto-open/auto-resolve behaviour, escalation, postmortems,
  subscriptions, webhooks, RSS/Atom/JSON feed schemas, or operator incident-detail screens.
- Root-cause inference, grouping by similar error text, or deduplication of distinct incidents.
- New metrics, persistence schema, or incident ownership semantics.
- Exposing internal monitor/service/project identifiers to public clients.

## 4. Canonical page order

The status page MUST render sections in this order:

1. Overall status and page freshness.
2. **Current status by service**.
3. **Active incidents**.
4. **Scheduled maintenance**.
5. **Past incidents**.
6. Subscription and feed controls.

Empty optional sections may be omitted, but the remaining sections MUST retain this relative order.
The component section is intentionally before active incidents so a visitor sees the affected
surface before reading its incident history.

## 5. Current status by service

1. Page group and component order MUST remain the owner-configured order. Moving the section upward
   MUST NOT silently rewrite the status page's configured information architecture.
2. A component referenced by one or more active incidents MUST show a textual action:
   `Active incident` or `<N> active incidents`.
3. Activating that action MUST scroll to, focus, and expand the first matching active incident. The
   browser URL MUST NOT contain an internal monitor/service/project identifier.
4. Status text, incident linkage, and unavailable/no-data states MUST remain understandable without
   relying on color alone.

## 6. Active incidents

### 6.1 Section summary

- The heading MUST be `Active incidents (<N>)`.
- When at least one active incident exists, the heading row MUST include compact impact counts for
  non-zero `critical`, `major`, `minor`, and `none` values.
- The impact summary MUST be derived from the rendered incidents, not a second API field.

### 6.2 Collapsed incident row

Each active incident MUST render as one compact, full-width accordion row containing:

- title;
- exactly one lifecycle-status badge;
- exactly one impact badge;
- metadata in the form `Opened <relative> · Updated <relative> · <N> updates`;
- the latest public update body, visible by default and clamped to at most two visual lines;
- a disclosure indicator whose accessible state follows the accordion state.

The public row MUST NOT display the incident source (`auto`, `manual`, or `api`). Source remains an
operator fact and may remain available on authenticated operator surfaces.

The whole row header MUST be the disclosure button. A separate small `Show full timeline` link is not
permitted.

### 6.3 Expanded timeline

- Timelines are collapsed by default.
- Expansion MUST reveal the complete public timeline in chronological display order already used by
  the product.
- The latest timeline entry MUST be marked `Latest`; status must not be repeated outside the one
  lifecycle badge in the collapsed header merely to label the preview.
- The unclamped latest body MUST be readable after expansion.
- If an incident has no updates, the row MUST show the existing truthful empty-details message and
  `0 updates`; it MUST NOT invent a synthetic update.

### 6.4 Ordering and high-count grouping

For eight or fewer active incidents, render one flat list ordered by:

1. impact: `critical`, `major`, `minor`, `none`;
2. latest activity (`updated_at`) descending;
3. original API order as a stable final tie-breaker.

For more than eight active incidents, group the same ordered rows under impact headings in the same
impact order. Empty impact groups are omitted. Incident lifecycle status is not a grouping key.

Cerbix MUST NOT group incidents by title similarity, error message, monitor type, region, or an
assumed shared cause.

## 7. Privacy-safe affected-component contract

`IncidentDetail` in a status-page render gains:

```yaml
affected_component_ids:
  type: array
  items: { type: string, format: uuid }
  description: >-
    IDs of components on this rendered status page that are bound to the incident's monitor or
    service anchor. Page-local public identifiers only; never monitor, service, or project IDs.
```

Contract rules:

1. The field MUST always be present as an array on active and recent incident details.
2. Values are status-page component IDs already present in `components[].id` in the same response.
3. The server derives the values while rendering the page from the incident's canonical monitor or
   service anchor and the current page's component bindings.
4. Values MUST be deduplicated and ordered by the rendered component order.
5. Multiple page components bound to the same canonical anchor are all included.
6. A project-level/manual incident with no anchor returns `[]`.
7. A component belonging to another page is never included.
8. Public redaction continues to clear `project_id`, `monitor_id`, `service_id`, `external_key`,
   acknowledgement actors, update IDs/authors, and postmortem IDs/authors.
9. Authenticated preview and public render MUST produce the same `affected_component_ids` for the same
   page snapshot; authenticated preview may retain its existing operator-only incident fields.
10. Subscriber, webhook, feed, and incident-detail API contracts are unchanged by this field.

The page-local mapping is presentation data owned by the status-page render application layer. It is
not persisted on the incident and does not become a second source of incident ownership truth.

## 8. Accessibility and responsive behaviour

1. Every accordion header MUST be a native button with `aria-expanded` and `aria-controls` targeting
   a stable panel ID.
2. Enter and Space MUST toggle the row; focus MUST remain visible in both themes.
3. Component-to-incident actions MUST be keyboard reachable. After activation, focus moves to the
   matching incident header so assistive technology receives the new context.
4. At a 430 px viewport there MUST be no horizontal page overflow. Badges and metadata may wrap, but
   the title, latest-update preview, and disclosure control must remain readable.
5. The two-line preview uses visual clamping only; the full text remains in the expanded DOM and is
   not destructively truncated in data.
6. Headings retain a logical hierarchy and impact/status information has textual labels.
7. Existing reduced-motion behaviour and theme contrast rules remain authoritative.

## 9. Acceptance invariants

| ID | Invariant |
| --- | --- |
| AC-0189-1 | `Current status by service` is the first content section after overall status, before active incidents and maintenance. |
| AC-0189-2 | An active incident has one compact collapsed row, one lifecycle badge, one impact badge, useful opened/updated/update-count metadata, and no public source label. |
| AC-0189-3 | The latest public update is visible in at most two lines while collapsed and fully available after expansion. |
| AC-0189-4 | The entire incident header toggles an accessible, collapsed-by-default timeline; no separate `Show full timeline` link remains. |
| AC-0189-5 | Eight or fewer incidents form one deterministic list; more than eight group only by explicit impact. No root-cause-like inference is introduced. |
| AC-0189-6 | `affected_component_ids` contains only IDs of components rendered on the same page, in page order, and never leaks internal anchor IDs. |
| AC-0189-7 | An affected component action focuses and opens a matching incident without placing internal IDs in the URL. |
| AC-0189-8 | Desktop and 430 px layouts remain readable, keyboard operable, and free of horizontal overflow. |
| AC-0189-9 | Public feeds, webhooks, subscriptions, incident lifecycle, summary algebra, maintenance behaviour, and past-incident semantics do not change. |

## 10. Required implementation slices

### Agent A — Implementation

- Extend the status-page incident view and OpenAPI/generated TypeScript contract with
  `affected_component_ids`.
- Derive the page-local relation in the status-page render path without changing incident storage.
- Refactor `PublicStatusView.vue` to the canonical section order, compact active-incident accordion,
  deterministic sorting/grouping, and component navigation.

### Agent B — Tests

- Add API/store-backed render tests for monitor binding, service binding, duplicate bindings,
  project-level incidents, foreign-page exclusion, deterministic order, and public redaction.
- Add component tests for section order, one-status presentation, metadata, source absence, clamping,
  accordion accessibility, high-count grouping, and component-to-incident focus/expansion.
- Add live Playwright coverage for the approved desktop scenario and a 430 px viewport.

### Agent C — Docs + Traceability

- Keep `status.md`, `traceability.md`, `roadmap.md`, this spec, the OpenAPI description, and
  `iter-0189.md` synchronized with the final implementation and test evidence.
- Do not add links to local assistant/session history.

### Agent D — Observability + Ops

- Confirm no new metric is needed: this is presentation and response-projection work, not a new
  runtime health mechanism.
- Verify public cache/render parity, response-size impact, privacy redaction, and live readiness.

## 11. Required verification

At minimum, iteration closure requires:

- focused Go API/store tests for the new projection and redaction;
- `go test ./...`;
- `make race`;
- `make build` and `go vet ./...`;
- configured `golangci-lint` gate;
- generated TypeScript parity / SPA snapshot gate;
- full frontend unit suite, `vue-tsc`, and Vite production build;
- `make docs-check` and `git diff --check`;
- local live status-page Playwright at desktop and 430 px, with multiple active incidents and at
  least one component-to-incident navigation assertion.

## 12. Definition of done

`FR-037` and `NFR-031` are `DONE`: every acceptance invariant has executable evidence, the public
redaction regression proves internal anchors remain absent, the live page passes at both target
widths, and all canonical documents name the shipped contract. The original delivery matrix is
recorded in [`iter-0189.md`](../iterations/iter-0189.md); post-close heading semantics, linear relation
indexing and their repeated/full gates are recorded forward in
[`iter-0190.md`](../iterations/iter-0190.md) without modifying the closed delivery report.
