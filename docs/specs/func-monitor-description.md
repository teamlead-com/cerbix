# func-monitor-description — a monitor says what it is for (FR-030)

> **Lifecycle: IMPLEMENTED — built and closed at iter-0173 (2026-09-03).** The owner asked for it in
> one paragraph: a stranger who opens the product cannot tell a monitor's purpose from its name, so a
> monitor gets an optional description with a character limit, shown where monitors are listed, and
> nothing about existing monitors may break. The mock `docs/design/mock-monitor-description.html` was
> approved the same day with two answers: **the limit is 200 characters**, and **search does not match
> on it**. Decision record: D-0234; FR-030 is `DONE` in `docs/status.md` and discharged in the FR-030
> row of `docs/traceability.md`.
>
> **One thing this document got wrong and now states correctly** (reviewer P1/P2, 2026-09-03): the
> dashboard card does NOT keep a fixed height. A description adds at most one line to a grid row, and
> the truthful bounded contract is in §3 D3 — kept, at the owner's decision, over reserving a line on
> every card. §4's list is what MOVED with the implementation, all of it done.

## 1. What this is, in one paragraph

One optional plain-text field, `description`, on a monitor. Written once — in the create/edit form, through
the API, or in a Monitoring-as-Code bundle — and shown wherever a monitor is listed: as a second line under
the name on `/monitors`, as the beginning of a line on a dashboard panel, in full on the monitor's own
page. Empty for every monitor that exists today, and every surface renders exactly as it does now when it
is empty. It is a description, not a runbook: 200 characters is the bound that keeps it one.

## 2. Requirements

- **FR-030 — a monitor carries an optional description.** `monitors.description` is plain text, at most
  200 characters, counted as characters (Unicode code points) identically on the form and on the server;
  a longer value is refused at the field on the form and with a 400 naming the field on the API — never
  truncated silently. It is optional everywhere: an API body or a bundle that omits it means "no
  description". It is shown on the monitor list, on dashboard panels (one line, cut by the panel's width
  with an ellipsis) and on the monitor detail page (in full), and editable on the create and edit forms
  with a live count. Existing monitors, bundles and API clients are unaffected: the column defaults to
  the empty string and no surface changes when it is empty.

## 3. The decisions

**D1 — 200 characters, counted the same way twice.** The owner set the number. Counting is by code point
(`utf8.RuneCountInString` on the server, `[...s].length` on the form), not bytes: a Cyrillic sentence is
as long as a Latin one to the person writing it. Both counts are asserted against one fixture so they
cannot drift — the same lesson the canary form paid for three review rounds to learn (D-0227…D-0230). The
bound is a named constant, `domain.MaxMonitorDescriptionRunes`, published to the client through the
existing bounds-parity mechanism where practical, and the form's counter reads `n / 200`.

**D2 — empty is the default and means "none".** Stored `NOT NULL DEFAULT ''`. The API omits nothing new
on read (`description` is always present, possibly `""`) and requires nothing new on write. A bundle that
omits the key declares "no description", which is the same as today. A file-managed monitor is read-only
in the UI and the API (`ErrManagedByFile`, as for every field), so the bundle is the ONLY writer of its
description: adding the key sets it, changing it is a change (the canonical hash covers it), removing the
key clears it. Stated here so nobody expects a UI edit to survive a re-apply — it cannot be made. Leading and trailing whitespace is trimmed; a whitespace-only value is empty.

**D3 — where it appears, and how much of it.**

| Surface | File | How |
| --- | --- | --- |
| `/monitors` | `frontend/src/views/MonitorsView.vue` | a second line under the name, `--ink-2`, single line, ellipsis, full text as the cell's tooltip; NOT a sixth column — the table is five wide and a description belongs to the name it explains |
| dashboard panels | `frontend/src/components/MonitorCard.vue` | one line under the name row, `--ink-3`, 12 px, cut by the panel's own width with a CSS ellipsis, so the line can never become two whatever the text |
| monitor detail | `frontend/src/views/MonitorDetailView.vue` | the whole text, wrapped at a readable measure, between the title row and the target line |
| create / edit | `frontend/src/views/NewMonitorView.vue` | a textarea under Name, optional, with a live `n / 200` counter; over the limit the counter turns `--down`, the message says what to do, and Create/Save stays disabled |

A monitor with no description renders identically to today on every surface: no empty line and no
placeholder — no element at all.

**The dashboard's height, stated exactly** (owner's decision, 2026-09-03, after reviewer P1). A card
with a description is ONE LINE taller than one without: the line is a flex child of the card, and
`truncate` stops it becoming two lines but cannot remove the one. Inside a grid row the items stretch,
so no card is ever taller than its neighbour — the ROW is one line taller when any card in it has a
description, and an undescribed card in that row gains trailing space. This is what the approved mock
renders, and it is how the card already behaves for content it has always had: a `push` monitor has no
latency column and no error-budget meter, so it is shorter than an `http` monitor beside it. The
earlier wording here — "so the panel keeps its height and its grid" — was false of this component for
any field, and is corrected rather than defended. The alternative, reserving the line on every card,
was declined by the owner because it would change every existing dashboard for people who never write
a description.

**D4 — where it deliberately does not appear.** Public status pages (a description is written for the team
and may name internal hosts; components keep their own public wording). Alerts, notifications and incident
text (payload and templates unchanged; adding it to a DOWN notification is a reasonable follow-up that
changes what every channel sends and deserves its own look). Search (the owner's answer: no). Service
pages and incident subject chips (they link the monitor by name; the description is one click away).

**D5 — Monitoring-as-Code.** The monitor block gains an optional `description` key with the same bound;
the hash that decides whether a re-apply is a change covers it like any other field. A bundle without the
key is unchanged and still applies cleanly.

**D6 — API.** `Monitor.description` in `openapi.yaml` (`maxLength: 200`, described as code points);
accepted on create and on update (a `PATCH`-style update omitting it leaves it as it is; sending `""`
clears it); the 400 for an over-long value names the field.

**D7 — audit.** A change of description is a monitor update and is audited as one (FR-026 §10,
`monitor.update`); the description's TEXT never reaches the audit target — D13 of that amendment already
forbids document contents there.

## 4. What moved WITH the implementation (all done at iter-0173)

- `openapi.yaml` and the regenerated `frontend/src/api/schema.d.ts`.
- `docs/status.md` — the FR-030 row; `docs/traceability.md` — the FR-030 row; `docs/overview.md` — the
  monitor description gains one sentence; `docs/runbook.md` — nothing (there is no operational
  behaviour).
- `make spa-snapshot` — the committed `internal/web/dist` is what `go build` embeds.

## 5. Schema

Migration `00099_monitor_description.sql`: `ALTER TABLE monitors ADD COLUMN IF NOT EXISTS description text
NOT NULL DEFAULT ''`. No index — nothing reads by it. `monitorColumns` and `scanMonitor` gain the column
at the END, in that order, and the create and update statements gain it (the change-pattern gotcha in
`CLAUDE.md`).

## 6. Acceptance invariants (FR-030)

1. A description of exactly 200 code points is accepted; 201 is refused — by `Monitor.Validate` with an
   error naming the field, by the API with 400, and by the form at the field — for a Latin and for a
   multibyte sample alike, so the count is by code points on both sides.
2. Omitted on create → empty; omitted on update → unchanged; `""` on update → cleared; whitespace is
   trimmed and a whitespace-only value is empty.
3. Every monitor that existed before the migration reads back with `description: ""` and carries NO
   description element on any surface — no empty line on the list, no line on the card, nothing on the
   detail. Its card is unchanged in itself; in a grid row shared with a described card it stretches to
   that row's height, exactly as it already stretches beside a taller card today.
4. The monitor list shows the description under the name as one line with the full text as tooltip; the
   dashboard card shows ONE line, cut by CSS and never wrapping — asserted structurally, as exactly one
   more flex child than an undescribed card, so the height difference is a measured number rather than
   a promise; the detail page shows the whole text; the form shows the live count, the refusal state
   and keeps Create/Save disabled while over the limit.
5. A bundle with `description` sets it; a bundle without the key leaves it empty and applies unchanged;
   a bundle that changes only the description is a change; removing the key clears it; the re-apply of an
   identical bundle is not a change; and the UI cannot write a file-managed monitor's description at all.
6. The description does not appear on a public status page, in a notification payload, or in an audit
   target; search does not match on it.
7. A description change is audited as `monitor.update`.

## 7. Required test matrix (written before the code; every line exists)

- `internal/domain`: the bound at the edge and one past it, Latin and multibyte; trimming.
- `internal/store`: round trip through create and update; pre-migration rows read back empty; the
  bundle apply sets, keeps and clears per invariant 5.
- `internal/api`: 400 naming the field; omitted vs `""` on update.
- `frontend`: a component test of the form counter and refusal state (`n / 200`, `--down`, disabled
  Create) — `NewMonitorDescription.spec.ts`; the card's own geometry — `MonitorCardDescription.spec.ts`
  (one line and never two, no element when empty, and the height delta measured as exactly one more
  flex child); the published bound mirrored — `monitorBounds.spec.ts`.
- `e2e/tests/monitors.spec.ts`: create with a description through the API, see it on `/monitors` and on
  the detail page; a monitor without one shows no description element.

## 8. Non-goals

Markdown or links in the description; per-type descriptions; a description on services, status-page
components or incidents; searching or filtering by description; a description in notifications.
