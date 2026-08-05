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
