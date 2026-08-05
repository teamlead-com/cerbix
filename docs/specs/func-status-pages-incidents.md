# Spec: Status pages and incidents (func-status-pages-incidents)

> Skeleton. To be filled in during the iter before implementing status pages.

## Purpose

Status pages, an incident model with a timeline, postmortems,
subscriptions/webhooks/feeds, API-first for integrations.

## Scope

- `StatusPage` (org-level by default, opt. project-level); visibility
  `internal`|`public`|`unlisted`.
- `Component` (status automatically from a monitor or manual; grouping/ordering).
- `Incident` (`investigating→identified→monitoring→resolved`, impact, affected components,
  source `auto|manual|api`) + `IncidentUpdate` timeline.
- `Postmortem` (markdown), publishing via GUI and API.
- `Subscriber` + outgoing webhooks + RSS/Atom/JSON feeds.

## Requirements (draft)

- FR: rendering of a public/internal/unlisted page (overall status, components, 90-day bar,
  active incidents, maintenance, history).
- FR: incident creation automatically (a monitor going down), manually, via API. Auto-incidents
  are **optional per monitor** (`monitors.auto_incident`, default `true` — behavior
  preserved): opening is gated by the flag, resolution is unconditional (an already opened auto-incident
  is closed on recovery even if the flag was turned off). D-0066.
- FR: postmortem via GUI and API; a webhook on every incident change.
- NFR (**security**): `internal` by default; `public` does not expose anything beyond what is explicitly
  configured; topology does not leak outward by default.

## Open questions

- Format/schema of outgoing webhooks (for future incident analysis tooling).
- Custom domains for status pages (opt., later).
