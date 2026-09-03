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

## FR-021 phase 5 — alerting ownership (approved 2026-08-17)

Source: `docs/design/mock-alerting-ownership.html`. Approved by the owner after the design's second
revision, which is the version that matters: the first mock showed a single ownership toggle, and two
design rounds established that a toggle is a DECLARATION and not coverage.

Five screens: Ownership switch (the declaration, the per-signal ARMED / PENDING / DEGRADED badge with
its reason, the before/after of who pages, the confirmation delay stated as nominal, and the routing
that says plainly which field is not consumed yet) · A delegated monitor (its own real status pill
kept, a dashed delegation chip naming the owner, which signals are delegated and which are NOT, and
the `⏸ Suppressed:` note in the incident timeline) · Tolerated failure (a member DOWN under `any`
while the service is HEALTHY — what was recorded, what was not paged, and what to change if that is
wrong) · The alerts (live transition vs sealed burn with its watermark, and a CLOSE that names its
reason and goes to the onset's recipients) · Refusals (unknown, maintenance, a held burn window that
dis-arms, nobody to notify, and a recovery that is never suppressed).

### The additions to the design language (approved)

**Arming badges, and no new hue.** `armed` borrows `--up`, `pending` borrows the `--pending` hue with
a dashed ring (the phase-4 dormant grammar), `degraded` borrows `--degraded`. Alerting is a question
of who is paged, not a new state of a thing, so it introduces no colour of its own. `armed` is the
only badge that means anything is suppressed, and it is the only one carrying the accent weight.

**A delegated monitor is never greyed out.** It keeps its real status pill — DOWN reads as DOWN — and
gains a dashed chip naming the owner. Dimming it would make the system show something other than what
it knows, which is what §11 and §14 exist to prevent.

### Fidelity notes for implementation

The per-signal badge must render the SERVER's arming reason, never a client reconstruction from a
timestamp: two implementations of "fresh" is how a badge says armed while delivery suppresses nothing.
Screen 1's held-burn warning is the mandatory case — a green ownership toggle beside a dis-armed burn
signal is the exact confusion the badges exist to remove.

## Instance audit — the global admin's own history (iter-0155 — APPROVED by the owner 2026-08-18, built in `5ecde7c`)

Source: `docs/design/mock-instance-audit.html`. **Approved by the owner on 2026-08-18 and built the same day** —
`InstanceAuditPanel.vue` + spec, live E2E scenario in `settings.spec.ts` (iter-0155 §2.1, AC-0155-1). This heading
read "AWAITING OWNER APPROVAL" until 2026-08-28, ten days after the approval and the build: a stale record that
made the implementer report the item as pending twice. The brief below is as written before approval.

`RecordAudit` stores an empty `OrgID` as NULL, and those rows are what a GLOBAL admin's actions leave
behind (`user.global_admin`, `user.delete`, file-provider and outbox operations). They have been
written since D-0109 and shown nowhere: the audit list in `MembersPanel` is org-scoped by construction,
so the installation's own history is invisible in the product that records it.

What the mock decides, so the implementation invents nothing:

- **Placement** — Settings → Administration, a tab beside Users / File providers / Outbox. That group
  already means "instance, not tenant"; the feature adds no navigation.
- **Row grammar is the org audit's, unchanged** — dot, actor, action prose, monospace target, relative
  time, "Show more". One reader learns one shape.
- **One new device: a dashed `instance` chip**, reusing the dashed grammar phases 4 and 5 gave to "a
  different KIND of thing" (a dormant binding, a delegated monitor). It says these rows have no
  organization — not that an organization is missing.
- **A machine actor renders as "machine"**, exactly as the org list renders a NULL actor. An audit row
  whose actor cannot be resolved must never borrow a human's name.
- **Nothing is deletable or editable.** An audit trail with a delete button is not an audit trail.
- **Org-scoped rows are NOT mixed in** — deliberately. That would make this the "everything" view and
  answer, in one glance, a question this panel is not allowed to answer: who did what inside a tenant.
  The API enforces the same split: `GET /api/v1/admin/audit` is a distinct read (`org_id IS NULL`),
  not the org listing with a wider filter, so no authz slip can widen one into the other.

## Project objective — the promise about the whole, not the mean of its parts (iter-0155 — APPROVED by the owner 2026-08-18, built in `2e2eec4`)

Source: `docs/design/mock-project-objective.html`. **Approved by the owner on 2026-08-18 and built the same day** —
the objective editor with the Clear the mock demanded, in `SlaView.vue` (iter-0155 §2.3). This heading read
"AWAITING OWNER APPROVAL" until 2026-08-28; same stale record as the instance audit's. The brief below is as
written before approval.

The SLA page already shows an "Error budget remaining · 30d" card that is a MEAN across monitors which
happen to have objectives. That answers "how are my monitors doing". A project objective answers a
different question — "what did we promise for this project" — and until now it had nowhere to live even
though `sla_targets` has carried the project scope since migration 00077.

What the mock decides:

- **The two cards sit side by side and each says which question it answers.** "mean across SLO monitors"
  beside "Project objective". Two numbers that could be mistaken for one another must be labelled, not
  separated by hoping the reader notices.
- **An objective is per WINDOW; only a window that has one shows numbers.** Other windows show a dash,
  never 30d's objective borrowed — a budget without the promise it was measured against is a number
  nobody made.
- **The Burn column is always a dash at this scope, and there is NO burn control at all** — not a
  disabled one. A disabled switch says "not yet"; an absent one says "not here". Paging at project scope
  needs an arming rule, a routing answer and close semantics, and until those exist the database refuses
  the row (`sla_targets_project_no_burn_chk`).
- **Rejection happens before the request**, mirroring the server's rule client-side (`lib/objective.ts`,
  D-0165): the open interval (0,100), four decimals, half-up. The operator reads why, not a 400.
- **Clear is explicit**, and clearing takes the budget with it: the row returns to dashes, never to 100%.
- **Not shown on purpose:** a project-level burn history (cannot exist without paging semantics) and a
  per-monitor breakdown of the project budget (that is the objectives table directly below).

## FR-022 — service incidents, SPA deltas only (AWAITING OWNER APPROVAL)

Source: `docs/design/mock-service-incidents.html`. **APPROVED by the owner on 2026-08-19** ("мок ок"), and
built in iter-0156 §2.10. Per the owner's instruction the mock covers ONLY what changes in the SPA: four
surfaces gain something and nothing else moves. Backend work proceeded without it and no Vue file was touched
until the sign-off — including through a round in which a REVIEWER wrote "approved on behalf of the owner",
which is not the owner and did not unblock anything.

- **The subject is a CHIP**, not a column and not a title prefix. Chips are already how phases 4 and 5 say
  "this row is a different KIND of thing"; a column would reshape every existing row, and a prefix would put
  the discriminator inside the text people search. A project-level incident carries no chip — which is what
  makes the chip a discriminator rather than decoration.
- **A monitor incident is untouched, visibly.** Every panel shows the monitor case beside the service case,
  because NFR-017 is a promise the UI has to keep too.
- **"opened automatically" is stated with what confirmed it** — "service DOWN confirmed over 2 evaluations",
  the same number that governs paging. An operator who finds an incident nobody typed must be able to see
  the machine that opened it and why.
- **Impact chips stay on the authenticated detail only.** They already do for monitors (§14); the public page
  gets the incident and NOT the graph, because §15.0 keeps internal topology unpublished and phase 4 declined
  to opt in (FR-021 invariant 59).
- **The service page links, it does not embed.** One timeline, one owner; two renderings of one timeline drift.
- **Not shown and not built:** a resolve-all control, per-member severity, or escalation progress on a service
  incident (D5 defers the ladder). A control for a feature that does not exist is a promise the backend refuses.

## FR-023 — a Service escalates its own outage (mock, 2026-08-19)

Source: `docs/design/mock-service-escalation.html`. **APPROVED by the owner on 2026-08-19** ("мок ок"), built in iter-0157 §2.10. SPA deltas only, per the owner's
standing instruction; the backend of FR-023 is already in the branch and needed no mock.

> **Corrected on the owner's first look: the first draft used a palette I invented.** The tokens are now
> lifted VERBATIM from `frontend/src/style.css` — the same block every approved mock carries — and checked
> against it token by token rather than by eye. A mock that reviews a colour decision nobody made is worse
> than no mock: it spends the reviewer's attention on my invention instead of on the product.

- **No new colour at all.** An escalation step is not a new state — it is the existing DOWN story told
  again to more people — so a fired step carries the DOWN hue and the progress pill keeps the degraded hue
  it already has. Steps not yet due are OUTLINED, never tinted: a step that has not happened is not a state.
- **The policy control goes in the paging panel, with its OWN save button.** It belongs where ownership and
  `Pages for` live — but the declaration and the policy are two writes with two audit actions, and one button
  that half-applies is worse than two buttons. The empty option CLEARS the policy, because "this service
  escalates nothing" has to be sayable, not merely reachable by never having chosen.
- **The progress pill needs no code.** `⛑ escalated to step N` already reads `incident.escalation_step`, and
  FR-023 advances that for a service incident too. It is in the mock so its absence is not read as a gap.
- **The per-step list is drawn and NOT built, deliberately.** The API exposes the current step index and
  nothing per step; rendering three rows from one integer would be a guess presented as a record. If the list
  is wanted it needs a read model, which is its own requirement.
- **Absent on purpose:** a renotify field (D8 — services have no such column), an "escalate now" button (a
  page nobody could explain from the record afterwards), a pause control (the ladder pauses itself and
  acknowledgement is the operator's stop — a third mechanism would give three answers to "why is it quiet"),
  and any graph hint (§14 annotates, never suppresses, and the ladder does not consult it).

## FR-024 — Release gate (mock, DEFERRED WITH THE REQUIREMENT — D-0202, 2026-08-28)

> **Deferred, not approved and not rejected.** The owner parked FR-024 on 2026-08-28 (D-0202) before looking at
> the mock visually. The reviewer's static contract pass approved revision 3 (party [59]); the visual/contrast/
> reflow pass never happened. If the requirement resumes, this mock needs the owner's look in both themes before
> any Vue code — the standing gate — and a check that the API it draws still matches the spec.

Source: `docs/design/mock-reliability-gate.html` (artifact `c8a2fca8-dbb6-4519-b11e-58f0b0c3d0ec`). Produced
**after** the design gate was approved at spec revision 13 (party [47], D-0201) and **before** any frontend code, per the
standing gate. Tokens are lifted VERBATIM from `frontend/src/style.css`; the shell, the `.note` overlay and the screen
switcher are 1:1 with `mock-alerting-ownership.html`. **No new colour.**

Six screens: Release gate · not configured (the new card on the service detail, the CLI's exit 4) · Policy editor (the
whole D11/D14 document: window, five clause assignments, threshold, seal-lag bound in minutes, unknown behaviour;
client-side refusals mirroring the server; the 409 `revision_conflict` banner) · Decision & override (latest decision with
its `reasons[]` and evidence, `unoverridden_action`, the active override, the create form refused by `override_active`) ·
Decision history (the project-scoped ledger listing, live-keyset note, range-too-wide refusal, one record by id) ·
Override history (the status function's four statuses, the by-id record's nullability, the stale-screen 409) · Unknown &
refusals (`seal_stale`, `never_sealed`, 429/503 as transport, the five-answer legend).

### What the mock decides, so the implementation invents nothing

- **Placement:** a card on the service detail between **Alerting** and **Dependencies** — facts → who is woken → who may
  deploy. The gate is an operational control (D13), not part of the declaration, so it is not inside the Declaration card.
- **The five states borrow existing hues:** ALLOW `--up`, WARN `--degraded`, BLOCK `--down`; UNKNOWN and NOT_CONFIGURED
  are `--pending` with the DASHED ring (phase-2 "deficient evidence" / phase-4 "different kind of thing"). Status is dot +
  text, never colour alone.
- **An override is the accent chip**, never a status hue: it is an operator's act in force ("declared here" grammar).
  Closed overrides (expired, revoked, inert) are dashed chips — no longer in force.
- **Clause assignments are a three-way segmented control** with `ignore` OUTLINED only (it is not a state); `block` and
  `warn` carry their own hues when selected.
- **The seal-lag bound is entered in minutes**, stored as `max_seal_lag_seconds`; the floor (5 min) is explained where it is
  refused, with the derivation (bucket 60 s + grace 120 s + two buckets).
- **The decision history is a PROJECT screen**, not a service tab: the ledger outlives services (D10), and a deleted
  service's rows render with the snapshotted name and a dashed `service deleted` chip.
- **Absent and null render the same in the table (a dash) and differently in the raw record** — the D7 presence contract is
  kept where it is machine-read, and not turned into a UI puzzle.
- **Not shown and not built:** a "preview the decision" button (a decision is recorded), an override `action` picker (D9
  fixes it), a per-environment policy, and anything reading raw heartbeats.

The **Spec notes** toggle is a review affordance overlaying the D-numbers each screen renders; it does not ship.

### Revision 2 (party [55], static contract pass by the reviewer — not a sign-off)

Seven corrections before the owner is asked: `facts_fresh_until` now follows D7's ONE formula in both examples (13:58 —
the expired ticket lease, not the 14:15 seal horizon; 08:55 — a seal horizon in the past is a horizon, not a dash);
NOT_CONFIGURED carries `reason: not_configured` + the documentation link in the raw record and the CLI; the CLI shows D16's
contract literally — ONE stdout line `state=… [action=…] [override=…] decision=…`, reasons on stderr, `--json` verbatim; a
role matrix decides that controls a role cannot use are NOT rendered (the `ServiceAlerting.vue` `readOnly` pattern) —
viewer read-only, editor without the override card, project_admin everything; the UNKNOWN copy keeps the word UNKNOWN
("state UNKNOWN, action BLOCK, exit 2"); **Delete policy…** has its continuation — a confirmation stating the four
consequences and the `409 revision_conflict` refusal; the by-id JSON is valid; `--focus` is in all three token scopes and
drives `:focus-visible`; the self-correcting hint on screen 3 is direct copy.

### Revision 3 (party [57])

- **The "Latest decision" card is a READ, never a POST.** There is no latest-decision endpoint on purpose: the card is the
  newest row of the project ledger filtered to the service over the last 30 days (`GET …/gate/decisions?service_id&from&to
  &limit=1`), titled "Latest decision · last 30 days", with an empty state ("no decision yet") on screen 1 and a link to the
  history. Opening a service page must never evaluate the gate — that would write an immutable row, spend a rate token and
  move a metric, the very "no preview" rationale of §9. SPA code invents nothing here.
- Delete arithmetic stated in two steps: the tombstone is revision 8, the next saved policy revision 9.
- The NOT_CONFIGURED history row carries `not_configured` in its Reasons column like the by-id record.
- The override by-id JSON is valid (comments outside the object); the D16 hint says "reasons and diagnostics go to stderr"
  and distinguishes `--json` (verbatim) from `jq` (a projection).

---

## FR-028 — Synthetic secret bindings (mock, APPROVED by the owner 2026-09-03)

> **Approved visually by the owner on 2026-09-03**, at revision 1, with no changes requested. The standing gate
> is lifted for this surface only: the SPA editor may now be built, to this mock. Drawn after the requirement
> closed rather than before it — FR-028 stage 2 is DONE and approved (D-0217), and the editor was its one
> deferred half. The reviewer gave three requirements for it after approving stage 2 (roadmap N4) and those
> three are the mock's spine, not decoration.
>
> **The open question is answered by the approval itself:** the mapping panel sits ABOVE the steps, as drawn.
> An empty panel is also how an operator learns the feature is there, and that is the reading the owner took.

Source: `docs/design/mock-synthetic-binding.html`. Tokens VERBATIM from `frontend/src/style.css`; shell, `.note`
overlay and screen switcher 1:1 with `mock-reliability-gate.html`. **No new colour** — a binding is not a status,
so it never borrows `--up`/`--down`; it is the accent, the voice the secret inventory already speaks in.

Five screens: Scenario with bindings (the mapping panel above the existing step builder) · Declaring a binding
(the flow starting at the header field) · Refusals & the residual · Save before test · Edges (withheld, dangling
ref, no bundle export).

### What the mock decides, so the implementation invents nothing

- **The mapping is a panel, not a tooltip.** Screen 1 opens with a table: binding → project secret → where it is
  used → the flat key it is stored as. A binding chip is NEVER rendered alone anywhere in the mock — that pairing
  is the reviewer's first requirement made structural rather than remembered.
- **D7 is taught by the control, not by a refusal.** The moment a header NAME is in the credential-bearing set,
  the value field stops being free text and offers the inventory. The literal refusal still exists (screen 3) and
  uses the server's own words, but an operator should meet the rule before they paste, not after.
- **"Save before test" is a sentence at the button.** The Test control is disabled with the reason and two ways
  forward (Save · Save and run now), and the same card shows the credential-free scenario still testing. The
  contract is D10's, and the UI owes it in the place the click happens.
- **The residual gets its own card, in `--degraded`, not silence.** A credential in an unlisted header or a body
  is NOT detectable and is NOT refused; the card says so and offers to turn the value into a project secret. A
  panel that only showed what is enforced would read as a guarantee the product cannot keep (D7).
- **The value never enters the browser.** The picker lists secret NAMES; the form cannot create a secret (that is
  the inventory's job) — a credential typed into a monitor editor is the habit the requirement exists to end.
- **D9 is visible where an export would be**, so the bundle limitation is read rather than discovered.
- **Not shown and not built:** binding usage analytics, a "reveal" affordance of any kind, in-place secret
  rotation from the monitor form, and any inline test that would need an envelope.

### Open for the owner's look

Whether the mapping panel sits ABOVE the steps (as drawn — the reference is read before the journey) or collapses
into a summary row until a binding exists. The mock takes the first, because an empty panel is also how the
operator learns the feature is there.

## `mock-async-canary.html` — FR-029 phase F, the typed canary form (2026-09-03)

Four screens: the typed form in full, the refusals, what a run is allowed to report, and the file-owned
view. Built AFTER phases B–E, so every rule drawn here already exists in
`internal/domain/canary.go` and `internal/prober/canary.go` — the mock chooses where each refusal
belongs on screen and nothing else.

### What the mock decides, so the implementation invents nothing

- **There is no JSON editor, on any screen, including the read view.** The stored document is one
  canonical string, and the detail panel renders it as the same typed sections the form uses. A textual
  view is a view somebody edits as text, and the whole reason this type exists is that the brief refused
  an opaque settings blob.
- **The secrets map is a numbered stage (0), above the five.** A binding is declared once and every
  position refers to it by name — the same grammar the FR-028 mock established, and the same chip: a
  binding is never shown without the project secret that fills it.
- **D7 is taught by the control**, exactly as in FR-028: typing a credential-bearing header name replaces
  the value input with the binding picker. The literal refusal exists and uses the server's words, and
  it offers the fix (choose a binding · declare one) rather than only the complaint.
- **Bounds are printed under the field from the moment the form opens**, not after the value is wrong:
  interval ≥ timeout, completion timeout ≤ the monitor's, `interval × max_attempts` ≤ completion timeout,
  `max_latency` a promise inside the limit. A canary that overlaps itself would report
  `already_in_flight` forever, and the form is where that is cheap to prevent.
- **The correlation-id placeholder is legal in exactly one field** and the refusal says why: nothing has
  produced an id at submit time.
- **A cross-host completion binding is refused in the form**, with the redirect rule stated beside it.
- **The run view carries a stage and a bounded class, and nothing else** — no URL, no correlation id, no
  object path, no secret. The threat model is a screen, not a paragraph in a spec.
- **The monitor form hides `target`** for this type rather than asking for a value it would ignore.
- **A file-owned canary is read-only**, like every other file-owned type, with host names shown and full
  URLs not.
- **Not shown and not built:** a JSON or YAML editor of any kind, an in-form "run once" (a canary is
  tested by running it from its region, so the form saves first), a fixture upload (the registry key is
  the only fixture reference there is), and any control that would relax the SSRF policy.

### APPROVED by the owner, 2026-09-03

Approved visually at revision 2, after one correction the owner found and I had not: the mock carried a
REAL project name as its example data. It had spread past the mock — 37 occurrences across the FR-029
spec, `docs/decisions.md` and three `_test.go` fixture files — because the original brief named the real
first use case and I carried that name straight into every example. All of it is now a neutral
placeholder (`Media upload journey`, `upload-token`), and `make docs-check` gained
`check_customer_names()`, which scans the WHOLE tree rather than only the living documents, because most
of the leak was in fixtures. Verified by planting a file and watching the gate fail.

The one thing the owner did not ask to change, left as drawn: the submit stage is the only one long
enough to want collapsing, and all five stay expanded — a canary is read end to end far more often than
it is edited, and a collapsed stage is where a wrong bound hides.
