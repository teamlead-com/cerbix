# Spec: Status pages and incidents (func-status-pages-incidents)

> Revision 1 — **APPROVED AND IMPLEMENTED 2026-09-21** in `iter-0189`.
> Requirements: `FR-037`, `NFR-031`. Decision: `D-0260`.
> Delivery evidence: [`iter-0189.md`](../iterations/iter-0189.md); semantic/performance hardening:
> [`iter-0190.md`](../iterations/iter-0190.md), `D-0261`.
>
> Revision 2 — **APPROVED AND IMPLEMENTED 2026-10-08** in `iter-0203`: a bounded
> "Past incidents" list on the page and a paginated incident history (§13). Requirements: `FR-039`,
> `NFR-033`. Decision: `D-0270` (UTC months and the 90-day depth confirmed explicitly). §13 amends
> §1–§12 where they differ (the bounded *Past incidents* list).

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
| AC-0189-9 | Public feeds, webhooks, subscriptions, incident lifecycle, summary algebra, maintenance behaviour, and past-incident semantics do not change. *(Revision 2, §13, changes one past-incident property on purpose: the page lists at most ten of them and the rest move to a paginated history. Which incidents count as past — resolved within the last 90 days — and every other item in this row stay unchanged.)* |

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

## 13. Revision 2 — a bounded page and a paginated incident history (`FR-039`, `NFR-033`)

> Approved by the owner 2026-10-08 (`D-0270`). Implemented in `iter-0203`, closed 2026-10-08 after four
> review rounds; their corrections are part of this section. UI mock:
> [`mock-status-incident-history.html`](../design/mock-status-incident-history.html), approved by the
> owner 2026-10-08.

### 13.1 Problem

The page renders **every** incident resolved in the last 90 days under *Past incidents*, each with
its full public timeline and postmortem (`store.IncidentsForPage`, no `LIMIT`). Two consequences grow
with the number of incidents, and incidents only accumulate:

1. **Length.** The section pushes the subscription and feed controls far below the fold, and a
   visitor looking for one past event scrolls through all of them.
2. **Cost.** The render is the one endpoint an anonymous visitor can request for free. Its size and
   its enrichment work (timelines, postmortems, affected components) are unbounded in the number of
   past incidents. The component list already has a fail-closed public ceiling
   (`publicComponentHardCeiling`); the incident history has none.

### 13.2 Definitions

- **History window `H`** — `(now − 90 days, now]` on `resolved_at`. This is today's *Past
  incidents* window, unchanged: an incident is past when its status is `resolved` and its
  `resolved_at` is inside `H`.
- **History order** — `resolved_at` descending, then incident `id` descending. Today's query orders
  by `resolved_at` alone; the `id` tie-break is added so that the order is total and a cursor
  (§13.4) is well defined.
- **History month** — a calendar month in **UTC**, written `YYYY-MM`. An incident belongs to the
  month that contains its `resolved_at` in UTC. The page shows times in the visitor's local zone as
  it does today; month membership is UTC so that every visitor sees the same month for the same
  incident and the server needs no visitor time zone. The UI says so (§13.5).
- **Offered months** — the history months that intersect `H`, newest first. Because `H` is 90 days
  long, there are up to **five** (on 1 May the window starts on 31 January; review round 1 corrected
  an earlier "at most four"). The oldest offered month is usually partial: it starts at
  `now − 90 days`, not on its first day.

### 13.3 The page render

`statusPageRender` changes in exactly two ways:

1. `recent_incidents` holds **at most 10** past incidents (`PAST_INCIDENTS_ON_PAGE = 10`), the
   newest in history order. Each element keeps the shape it has today: timeline, postmortem,
   `affected_component_ids`, and the same public redaction (§7).
2. A new field, always present:

   ```yaml
   recent_incidents_more:
     type: boolean
     description: >-
       True when more past incidents exist in the 90-day history window than recent_incidents
       holds. The rest are reached through the incident history (§13.4).
   ```

   The store reads `PAST_INCIDENTS_ON_PAGE + 1` rows; the extra row only sets this flag and is
   never rendered or enriched.

`active_incidents`, `maintenance`, components, summary and the 90-day window are unchanged. The
public render keeps its cache, coalescing and component ceiling as they are.

### 13.4 The incident history endpoint

Two routes with the access rules of the existing render routes:

| Route | Access |
| --- | --- |
| `GET /api/v1/public/status-pages/{slug}/history` | As `GET /api/v1/public/status-pages/{slug}`: `public` served to anyone; `unlisted` only with the matching `token`; `internal` → `404`. |
| `GET /api/v1/status-pages/{pageID}/history` | As `GET /api/v1/status-pages/{pageID}/render`: an authenticated member with access to the page, any visibility. |

**Query parameters**

| Parameter | Meaning |
| --- | --- |
| `month` | Optional, `YYYY-MM`. Default: the UTC month of `now`. Malformed → `400 invalid_month`. A well-formed month that is not an offered month → `400 month_outside_history`. |
| `cursor` | Optional, opaque, taken verbatim from a previous response's `next_cursor`. It encodes the `(resolved_at, id)` of the last incident returned, in exactly one spelling. Malformed, not in that spelling, or naming a position outside the requested month or after `now` → `400 invalid_cursor`. |
| `token` | Public route only: the unlisted page's token, as on the render route. |

**Response**

```yaml
IncidentHistory:
  title:          string   # the page's title, for the history page's header
  month:          string   # "2026-09"
  from:           string   # date-time: the month's start, or now − 90 days if later
  to:             string   # date-time: the month's end, or now if earlier
  history_from:   string   # date-time: now − 90 days, so the client can say where history begins
  months:         array    # every offered month, newest first: { month: "2026-10", count: 3 }
  incidents:      array    # IncidentDetail, at most 50 (HISTORY_PAGE_SIZE), history order
  next_cursor:    string   # null when this is the last page of the month
```

Rules:

1. `incidents` are the month's past incidents after `cursor` (if any) in history order, at most
   `HISTORY_PAGE_SIZE = 50`. The store reads 51 rows; the extra row only decides `next_cursor`.
2. Each incident uses **the same projection as `recent_incidents`**: timeline, postmortem,
   `affected_component_ids` derived from the page's current components (§7), and the same public
   redaction. One function produces both; the history must not grow a second projection.
3. `months[].count` is the number of past incidents in that month inside `H`. The counts and the
   incident list are read in **one read-only `REPEATABLE READ` snapshot**, so the count shown for a
   month agrees with the list read with it. Both are bounded above by the handler's `now`, the same
   instant the response publishes as `to`: an incident resolved after it is in neither (review R1-4).
4. **Pagination is a live keyset.** Within one traversal of a month (first page, then each
   `next_cursor`), an incident is never returned twice and the order never goes backwards.
   Incidents resolved after the traversal started land at the newest end and are not part of it. An
   incident whose resolution changes mid-traversal (reopened, or resolved again with a newer
   `resolved_at`) may be missed by that traversal. Incidents that age out of `H` disappear from the
   oldest month's tail. These are stated limits, not defects.
5. The public history route goes through the same render cache as the page — short TTL,
   coalescing, bounded number of entries. The key is a structured value (endpoint, page, access
   shape, token, month, cursor) with no field able to spell another key; a PUBLIC page's token,
   which nothing checks, is not part of it (review R1-1). Parameters are validated after the page
   lookup that the access gate needs and before any history read or cache entry.
5a. A cursor is any well-formed position, so a caller can mint new cache keys freely; the cache
   bounds memory, not work. Uncached public history renders are therefore limited per process to
   `HISTORY_RENDER_SLOTS = 8` in flight, independently of the key; a distinct request beyond them is
   refused with `429 history_busy` and `Retry-After: 1`, and the refusal is never cached (review
   R1-6). Cursors are not signed: a signature would need a key shared by every API replica, which
   this deployment has no owner for; a forged position yields a page of the same public history,
   at the same bounded cost.
6. The public route refuses a page above `publicComponentHardCeiling` exactly as the render does
   (`503`), because `affected_component_ids` needs the page's components.
7. Nothing older than `H` is served. Keeping a longer public history is a separate decision.

**Storage and cost.** A new partial index on `incidents (project_id, resolved_at DESC, id DESC)
WHERE status = 'resolved'` serves the page query, the month query and the counts. Incidents are an
ordinary table in both storage modes, so no hypertable or partition branch is involved.

What is bounded, stated precisely (review round 1 R1-5, re-reviews 1 and 2). Three different claims:

1. **Guaranteed — the result.** Each project yields at most the limit (11 on the page, 51 per
   history page) through its own `LATERAL … LIMIT`, and the merge returns at most the limit. What a
   response materializes, enriches and serves is bounded by that, whatever the plan.
2. **Designed for, and tested — early stop.** When the planner scans
   `incidents_resolved_history_idx` in order, each project's scan stops after its limit; a cursor
   page's keyset is a plain tuple comparison, so it is an index condition in custom AND generic
   plans (re-review 1: one statement with `$cursor IS NULL OR …` became a Filter under pgx's cached,
   generically planned statement and walked the month down to the cursor). Regression tests hold
   rows read — returned + filtered + rechecked, per loop — to `limit × projects` on realistic data
   for the first page and for a deep cursor under a generic plan.
3. **Not guaranteed — rows examined.** The planner may prefer a sequential scan where the table is
   small: on plain PostgreSQL 16 with two projects of 100 incidents, a cursor page chose a Seq Scan
   inside the `LATERAL` and examined 400 rows to produce 102 intermediate rows before the global `LIMIT`
   returned 51 (re-review 2). There is no universal bound on
   rows examined or physical I/O, and the product does not force a plan to pretend otherwise.

The month COUNTS cannot stop early: an exact count reads every past incident of the page's
projects in the 90-day window. That is bounded by the window, not by the response; it is the price
of exact counts, and one reason the public route also has render slots (rule 5a).

*Amended during implementation (iter-0203): `title` was added to the response so the history page's
header needs no second request.*

### 13.5 The pages

**Status page** (`/status/:slug`):

- *Past incidents* renders `recent_incidents` as today: at most ten rows, same accordion.
- When `recent_incidents_more` is true, the section ends with a link **View incident history** to
  the history page. The link carries the page's `token` (unlisted) or `preview` (internal preview)
  query parameter, so it opens the same page the visitor is looking at.
- The section label states what is shown: the latest ten when there are more, otherwise the last
  90 days, as today. Exact copy is settled in the mock.

**History page** (`/status/:slug/history?month=YYYY-MM`, plus `token` / `preview` as above):

- A header with the page title and a link back to the status page.
- A month navigator over the offered months, newest first, each with its count; the current month
  is marked `aria-current="page"`; previous / next controls are disabled at the ends. Months are
  labelled as UTC.
- The month's incidents in the same accordion rows as *Past incidents*.
- **Show more** when `next_cursor` is present: it appends the next page in place; the cursor never
  goes into the browser URL. Only `month` does, so a month link can be shared.
- An empty month says so: *No incidents were resolved in <month>.* — never a blank area.
- The oldest offered month, when partial, says where history begins: *History covers the last 90
  days; this month is shown from <date>.*
- Changing month moves focus to the list's heading. At a 430 px viewport there is no horizontal
  overflow. Section 8's accessibility rules apply to every accordion row here as on the page.

### 13.6 Unchanged

Feeds (RSS, Atom, JSON), webhooks, subscriptions, incident lifecycle and impact, active incidents
and their ordering, maintenance, component status and strips, page summary, section order (§4) and
the 90-day definition of a past incident.

### 13.7 Acceptance invariants

| ID | Invariant |
| --- | --- |
| AC-0203-1 | The render's `recent_incidents` holds at most ten past incidents, the newest in history order, each with the revision-1 projection and redaction; `recent_incidents_more` is true exactly when the 90-day window holds more. |
| AC-0203-2 | The extra row read to decide `recent_incidents_more` (and `next_cursor`) is never rendered or enriched. |
| AC-0203-3 | Both history routes enforce the same visibility and access as their render routes: internal → `404` publicly, unlisted only with its token, the authenticated route by page access. |
| AC-0203-4 | `month` and `cursor` are validated: a malformed month, a month outside the offered months, a malformed cursor and a cursor outside the month are each refused with their own `400` code, before any cache entry is made. |
| AC-0203-5 | A month's traversal by `next_cursor` returns each incident at most once, in history order, at most 50 per response, and only incidents resolved inside both the month and `H`. |
| AC-0203-6 | `months` lists exactly the offered months, newest first, and each `count` equals the number of incidents that month's traversal returns when nothing changes. |
| AC-0203-7 | History incidents and page incidents are produced by one projection: same fields, same `affected_component_ids` rule, same public redaction — no internal anchor or actor leaks through the history route. |
| AC-0203-8 | The public history route uses the bounded, coalescing render cache with `month` and `cursor` in its key, and refuses a page above the component ceiling with `503`. |
| AC-0203-9 | The status page shows *View incident history* only when `recent_incidents_more` is true, and the link keeps the page's `token` or `preview`. |
| AC-0203-10 | The history page navigates the offered months (UTC, with counts, current marked), appends *Show more* in place without putting a cursor in the URL, states an empty month and a partial oldest month in words, and passes §8 at desktop and 430 px. |
| AC-0203-11 | Everything listed in §13.6 is unchanged. |

### 13.8 Required verification

- Store tests for the page query (limit and flag), the month query (bounds, partial oldest month,
  keyset, tie-break by `id`), counts in the same snapshot, and the index being used, run in both
  storage modes as the store suite always is.
- API tests for both routes: visibility, every `400`, the cache key, the component ceiling, and
  public redaction of history incidents.
- Generated TypeScript parity after the `openapi.yaml` change; vitest for the history view and the
  page link; `vue-tsc` and the production build; `make spa-snapshot`.
- Live Playwright at desktop and 430 px: more than ten past incidents, the link, month navigation,
  *Show more*, an empty month, and an unlisted page's token carried through.
- `-race`, `go vet`, `make docs-check`, `git diff --check`; mutations recorded in the iteration
  report for the limit, the flag, the keyset and the month bounds.
