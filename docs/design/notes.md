# cerbix — Design language (approved direction)

Approved via design-track review (dashboard hero, iter-0008 prep). This is the source
of truth for the Vue SPA theme; derive tokens 1:1 from here.

## Brief

- **Character:** product minimalism (Linear/Vercel) with the data substance of status
  pages where it counts.
- **Themes:** light and dark, equal care. English UI.
- **Subject:** internal uptime/SLA monitoring — a watchdog (name ≈ Cerberus) watching many
  services at a glance.

## Signature

The **uptime-signal** motif: a thin segmented availability strip (90-day project timeline
+ per-monitor mini bars) and a live status pulse on Operational. This is the one bold
element; everything else stays quiet and precise.

## Type

- **UI / prose:** system sans — `-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, ...`.
- **Data / signature:** monospace — `ui-monospace, "SF Mono", "JetBrains Mono", Menlo, ...`
  — used for ALL data: uptime %, latency, codes, ids, heartbeats, the `cerbix` wordmark.
  Always `font-variant-numeric: tabular-nums`.
- Scale: h1 21px/650, KPI 26px mono, labels 11px uppercase +0.07em, body 14px, data 13–16px mono.

## Color tokens

Cool-tinted neutrals biased toward the iris accent (not pure grey). Brand accent is
distinct from the semantic status hues. Status colors are reserved, always shipped with a
dot + text label (never color alone).

### Light
```
--bg #fafafb  --surface #ffffff  --surface-2 #f5f5f8  --inset #f0f0f4
--border #e9e9ef  --border-strong #dadae4
--ink #17171f  --ink-2 #55556a  --ink-3 #8a8a9d
--accent #5854f2  --accent-2 #7a77ff  --accent-weak rgba(88,84,242,.10)
--up #12a05c  --down #e0393f  --degraded #b97800  --maint #3a7de5  --pending #8a8a9d
```

### Dark
```
--bg #0b0b0f  --surface #141419  --surface-2 #1a1a20  --inset #101015
--border #262630  --border-strong #34343f
--ink #f2f2f6  --ink-2 #a4a4b6  --ink-3 #6c6c7e
--accent #7d79ff  --accent-2 #928fff  --accent-weak rgba(125,121,255,.15)
--up #35c67f  --down #ff5f64  --degraded #e0a53a  --maint #5c9bff  --pending #6c6c7e
```

Radii: 8 / 6 / 4 px. Shadow: subtle 1px + soft ambient. Hairline cool borders everywhere.

## Layout

240px sticky sidebar (org/project switcher + nav) · 56px sticky topbar (breadcrumb + theme
toggle + search + New monitor + avatar) · content max-width ~1180px. Cards: 1px border,
subtle shadow, hover lift (translateY -1px + border-strong). 8px grid. Monitor grid
`auto-fill minmax(320px, 1fr)`. Collapses to single column ≤900px (sidebar hidden behind a
menu button), 1-col grid ≤560px.

## Quality floor (carry into Vue)

Both themes via CSS custom properties: `@media (prefers-color-scheme)` default + a theme
toggle stamping `data-theme` on `<html>` that wins both directions. Visible keyboard focus.
`prefers-reduced-motion` disables the pulse. Charts: area fill + 2px line + emphasized
endpoint + faint baseline/grid; status never color-alone.

## Screens (design-track checklist)

- [x] Project dashboard (hero) — approved
- [x] Monitor detail (SLA windows, latency chart, error budget, heartbeat log, incident)
- [x] Login (local + OIDC) + org/project switcher (switcher shown in shell)
- [x] Monitor create/edit (type selector + conditions editor + live preview)
- [x] Members & roles (capability matrix + invite + permission reference)
- [x] SLA & SLO (project summary, error-budget burn chart, SLO table, maintenance)
- [x] Incidents (list, status timeline, update composer, postmortem) — design-ahead (backend FR-012 pending)
- [x] Status page (public, public: components + 90d uptime + incident history + subscribe) — design-ahead (FR-012)

Full product surface mocked. Artifacts (claude.ai/code/artifact):
login c26d01a2 · dashboard 5fdea16c · monitor detail 1f9a0cd7 · new-monitor 8045c799 ·
members cee5d943 · sla&slo b9bf0580 · incidents d5baac16 · status page d4019300.
Next: iter-0008 Vue implementation 1:1 from these. Screens whose backend exists
(login/dashboard/monitor/form/members/sla) implement now; incidents + status page ship
after their backend iteration (FR-012).

## Frontend implementation (iter-0008)

`frontend/` — Vue 3 + Vite + TS + Tailwind, built & served in Docker (no local Node), the
globex-frontend way (D-0019). Tokens live in `src/style.css` (CSS vars, both themes) and are
exposed to Tailwind via `tailwind.config.js` (`bg-surface`, `text-ink`, `bg-accent`,
`text-up/down/degraded/maint`, `font-mono`, …) so components use the same classes in both
themes. Theme toggle: `src/composables/useTheme.ts` (stamps `data-theme`, persists).

Run (all Docker):
- `make dev`   → Vite hot-reload on :5173, proxies /api,/auth to the backend
- `make build` → produce `dist/` (SPA served by the backend from embed.FS; the
  backend Docker image builds+embeds it — no separate nginx image)
- `make gen-api` → regenerate TS types from `../openapi.yaml`

Scaffold + login (wired to `/auth/local/login`, `/api/v1/me`) done & verified in Docker.

**iter-0009 (done):** typed API client (`openapi-fetch` + generated `src/api/schema.d.ts`
via `make gen-api`), Pinia session store, router auth guard, `AppShell` + shared components
(`StatusPill`, `UptimeBar`, `Sparkline`, `MonitorCard`, `Kpi`), and the **Dashboard** and
**Monitor detail** views wired to the real API. Verified end-to-end through the frontend
nginx proxy (login → /me → create org/project/monitor → pipeline drives it up → SLA +
heartbeats). Remaining to port: new-monitor form, members, SLA & SLO page.

**iter-0010 (done):** ported the remaining backend-backed screens 1:1. A Pinia
`workspace` store (orgs/projects, current selection, `localStorage`-persisted) backs a
working org/project switcher in `AppShell`; every view reads the current project from it
and reloads on switch. New views: **Monitors** (list/table), **New monitor** (type
switch HTTP/TCP/Push + conditions editor with per-type presets + live preview →
`POST /projects/{id}/monitors`), **Members & roles** (list + add-by-user + capability matrix), **SLA & SLO** (project windows + per-monitor SLO table with inline
objective set → `PUT …/sla-target`, + maintenance schedule/delete). Monitor detail gained
a guarded **delete**. Incidents/Status-page remain sidebar entries marked *soon* (FR-012).
Verified end-to-end through the frontend nginx proxy (create monitor via the form
endpoint → pipeline up → set SLO → add member → maintenance CRUD → 401 guard).

**iter-0017 (done):** wired the **Incidents** Vue views to the incident API. Enabled the
Incidents sidebar entry; added `IncidentsView` (project incidents table with status/impact
badges), `NewIncidentView` (title/impact/status/opening-body → `POST /projects/{id}/incidents`),
and `IncidentDetailView` (header badges, timeline of updates, update composer with status
advance + quick **Resolve**, and a postmortem panel that publishes once resolved). Status/impact
labels+colors centralised in `src/lib/incident.ts` (token-driven). Verified end-to-end through
the frontend nginx proxy (open → update → resolve → publish postmortem → list, 401 guard). Only
the public **Status page** view remains (its backend — render + feeds — already exists).

**iter-0018 (done):** wired the **Status page** Vue views, completing the designed UI surface.
`StatusPagesView` (authed mgmt: list org pages, create with visibility, select → add/remove
components monitor-backed or manual, public + feed links) and `PublicStatusView` (standalone
public render at `/status/:slug` — summary banner + components with 90d uptime + active incidents
+ RSS/Atom/JSON subscribe links; handles unlisted `?token=` and the hidden/404 state). Enabled the
Status pages nav; added a `meta.public` route that bypasses the auth guard so the public page is
viewable without a session. Component status labels/colors centralised in `src/lib/statuspage.ts`.
Verified e2e through the frontend proxy (create page+component → public render no-cookie with a live
component + active incident → feeds → `/status/:slug` SPA route 200 → internal hidden 404).
**All eight designed screens are now implemented and wired; FR-011 UI is complete.**

## Log — what's been tried

- v1 dashboard: iris accent + mono-data + 90-day signal strip. Direction approved ("that's it").

## FR-021 Service Reliability (design track — APPROVED 2026-08-16)

Mock produced **before** any frontend code, per the process gate in CLAUDE.md, and **approved by
the owner**. Source lives in the repo rather than only as an artifact, because this surface will
be iterated during implementation: `docs/design/mock-service-reliability.html`
(artifact `ff3f7e2b-7c12-4d0d-ba32-5b1b38bf3007`). Implement 1:1 from it.

Five screens: Services list · Service detail (two-layer health, timeline, materialization) ·
Coverage & segments (the honesty states) · Declaration editor (monitors[] vs sli[]) ·
Revisions & provenance. A **Spec notes** toggle in the topbar overlays the rule each screen
renders, so the mock can be reviewed against `docs/specs/func-service-reliability.md` or judged
as plain UI with the annotations off. The toggle is a review affordance and does **not** ship.

Tokens are taken 1:1 from this file; the feature adds **no new colour**.

### Two additions to the design language (approved)

The language had no form for two states this feature must show, and both are expressed as
**shape and opacity on the existing tick grid** rather than as new hues — which is what makes
them impossible to misread as a status:

- **UNKNOWN** — a short tick on the same baseline in `--pending`. At a few pixels wide a hatch or
  a second hue is unreadable, and reusing the empty `--inset` "no data" fill would hide the one
  state this feature exists to make visible. A deficient tick reads as deficient evidence at any
  width and in both themes.
- **PROVISIONAL** — the same tick at reduced opacity: visible on the timeline, excluded from
  every number.

### Three placement rules learned from review (approved)

- **The uptime-signal strip is a CARD element, not a table cell.** `UptimeBar` appears in exactly
  one place in the product — `MonitorCard.vue`, height 26 — and `MonitorsView`'s table has no bar
  at all. A 90-tick strip in a table row reads as a solid striped block and, worse, draws a
  plausible picture over a stalled service. The Services table is therefore numeric, and its last
  column is **`sealed_through`** — the watermark itself, with the lag called out when a service
  falls behind. The signature strip lives on the detail and provenance screens, where a tick is a
  real rollup whose span is labelled and, on provenance, an actual 60s bucket you can hover.
- **A tick is a ROLLUP, and every strip says which.** `one tick = 8h rollup of sealed 60s buckets`
  on the 30-day view, `one tick = one canonical 60s bucket` on provenance. An unlabelled tick
  invites the reader to think it is a bucket.
- **Services sits ABOVE Monitors in the project nav, as a peer — never nested.** Order expresses
  level of abstraction (the unit of reliability, then what measures it); nesting would express
  containment, which the model rejects: a monitor may be in the SLI of several services, may be
  in none, and zero services is a valid state forever. Placing Services *below* Monitors asserts
  the "grouping of monitors" reading that acceptance invariant 42 exists to refute.

The `NEW` badge on the nav entry is a temporary adoption affordance and must disappear once the
project has its first service; §17 forbids presenting an empty Services screen as the product's
new front door.

### Fidelity notes for implementation

The org/project switcher in the mock is 1:1 with `AppShell.vue`: gradient org avatar 22×22 at
radius 6, **organization bold on top, project small below**, chevron right. An earlier draft
inverted that hierarchy, which told the reader the project is primary when the product says the
organization is — recorded because it is the kind of error that survives a screenshot review.

Watermark numbers in the mock are coherent on purpose: a healthy service sits at
`bucket 60s + late_arrival_grace 120s`, roughly three minutes behind `as_of`, and the detail
screen shows that delta beside `sealed_through`. An operator needs to have seen a healthy lag
once, or a stalled one carries no information.

## FR-021 phase 3 — Service impact (design track — APPROVED 2026-08-17)

Mock produced **before** any frontend code and **approved by the owner**, per the standing
gate. Source: `docs/design/mock-service-impact.html`
(artifact `20874f09-1c14-41b1-bcee-bc778c761ab0`). Implement 1:1 from it. Tokens and shell are
1:1 with `mock-service-reliability.html`; the feature adds **no new colour and no new motif**
— the impact graph is lists, pills and chips the language already has. The one new glyph is
**🕸** on system timeline notes, joining the ⚡/⏸ idempotency-marker family (text, not colour).

Four screens: Service detail — dependencies (Depends on / Depended on by with the phase-2
two-layer health, edge count vs the 20 bound, file-pin chip, blocked 409 delete in Danger
zone) · Edit dependencies (project-service multiselect minus self; honesty states rendered
verbatim: cycle 400, stale `graph_generation` 409 with Reload, limit 400) · Incident — impact
(🕸 probable-root chips with root-first "via …" paths for EVERY candidate, ranked by path
depth in presentation only; affected chips; the 🕸 system note beside ⚡/⏸) · Incidents list —
deliberately WITHOUT impacts (the §14.7 read bound made visible; a designed absence).

The **Spec notes** toggle is again a review affordance overlaying §14 rules and does not ship.

## FR-021 phase 4 — Status projection (design track — APPROVED 2026-08-17)

Mock produced **before** any frontend code and **approved by the owner**, per the standing gate.
Source: `docs/design/mock-status-projection.html`
(artifact `7a555bda-8b0d-4389-987b-60518061384a`). Implement 1:1 from it. Tokens and shell are
1:1 with the earlier mocks; the feature adds **one public status and no new colour**.

Five screens: Component source (the discriminator with DORMANT bindings shown dashed, the
same-project refusal, and the two rows whose appearance changed) · Conversion preview
(now → would-see per component AND for the page summary, both CAS counters with `as_of`, and the
`page_configuration_stale` refusal) · Public page (the two-part headline, gaps-not-zeros strip,
withheld availability with its reason, all-`no_data` and empty-page edges, and the fail-closed
over-limit response) · Composite · retire (one stored link rendered both ways, the warning naming
the loss of SLO and alerts, and the list distinguishing superseded / retired / disabled) ·
Deletion cases (three deletions, three honest answers).

### The one addition to the design language (approved)

**`no_data` is the `--pending` hue plus a DASHED ring — not a new colour.** A sixth hue would
invite the reader to rank "we do not know" against "declared maintenance", and §15.0 explicitly
refuses that comparison: the summary keeps them apart as measured-vs-unmeasured instead of
ordering them. This is the same motif phase 2 used for UNKNOWN ticks — deficient evidence reads
as deficient at any size, in both themes.

### Fidelity notes for implementation

The public page's strip must render ABSENT days as gaps and partially decidable days as short
bars. A zero-height day and a zero-availability day would look identical, which is the exact
confusion `daily[]` omitting absent days exists to prevent. The window label reads
`sealed through …`, never "now" — the mock says so on the axis because an implementation that
ended the window at page load would be silently wrong.
