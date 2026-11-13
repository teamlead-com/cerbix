# func-incident-impact — an incident's impact is declared by its Service, derived by the machine, and only rises (FR-042 / NFR-035)

> **STATUS: revision 2 — DRAFT, not approved.** Revision 1 went through a builder check against the
> code (2026-11-13): three planning questions and seven code contradictions, folded in as D8, D12–D14,
> §4.3–§4.6, I2/I6/I8 and §7. Written 2026-11-13 from the owner's answers to six
> design questions (recorded in §3 as OWNER decisions). Decisions the author took without an owner
> answer are marked **AUTHOR'S CALL**; one of them (D7) deliberately departs from the letter of an owner
> answer and says why. The SPA surfaces in §8 need an approved mock before any frontend code.
>
> Ordering (OWNER, 2026-11-13, option A): FR-042 is built **before** FR-041
> ([`func-phone-alerts.md`](func-phone-alerts.md)), because FR-041c's quiet hours decide "may this wake
> someone" from the impact this document defines.
>
> Invariants are numbered **within this document** (I1, I2, …).

## 1. Why

`incidents.impact` (`none` / `minor` / `major` / `critical`, `internal/domain/incident.go`) is shown
on the public status page — headline, colour, worst-impact copy
([`func-status-pages-overall-status.md`](func-status-pages-overall-status.md)) — and in the incident
lists. Nothing in cerbix decides what it should be:

| Origin | Impact today | Where |
| --- | --- | --- |
| Monitor auto-incident | always `major` | `internal/ingest/reconciler.go` (`Impact: domain.ImpactMajor`) |
| Service auto-incident | always `major` | `internal/store/serviceincidents.go` `OpenServiceIncidentTx` (`'major'` literal) |
| Manual (`POST …/incidents`) | chosen by the author, default `minor` | `internal/api/handlers_incidents.go` |
| Alertmanager receiver | from the `severity` label | `internal/api/handlers_alertmanager.go` `alertImpact` |

And once written it never changes: there is no endpoint and no SQL that updates `impact`.

So a service that is merely **degraded** is published as **Major impact** to every status-page
visitor, a payments outage and an internal-dashboard outage carry the same word, and a degraded
service that goes fully down keeps whatever it opened with. For a product whose claim is that it
never states a number it cannot defend, a hard-coded severity is the same defect in a different
column. FR-041c (quiet hours) would also inherit it: with every automatic incident `major`, "wake me
only for what matters" cannot be expressed.

## 2. Requirements

- **FR-042**: The impact of a **service** auto-incident is derived from the service's own declared
  mapping from alert state to impact, evaluated against the state that opened the incident and every
  later state it reaches while open; it **only rises**; the mapping in force is recorded on the
  incident; no person can change an automatic incident's impact; a project declares the impact its
  **monitor** auto-incidents open with; status-page subscribers are emailed when an incident's impact
  rises to `critical`.
- **NFR-035**: Impact stays a **classification**, not a control: no SLO, error-budget, burn-rate,
  gate, coverage, delegation, suppression or escalation computation reads it, before or after FR-042
  (proven by regression). The only new reader is FR-041c, which is specified there.

## 3. The decisions

### Owner decisions (answers of 2026-11-13)

**D1 — impact is declared on the SERVICE, as a mapping from alert state to impact. OWNER (Q1).**
A monitor is a measurement; impact is a consequence for the people who use a service. The same
failing database is `critical` for *payments* and `minor` for *analytics*, and only the service knows
which one it is.

**D2 — while an incident is open, its impact only rises; history keeps the peak. OWNER (Q2).**

**D3 — no person changes an automatic incident's impact. OWNER (Q3).** The owner's reason: a person
could lower it on purpose — to make a status page look calmer, or (after FR-041c) to keep a phone
quiet. The machine-derived value is the only value. Consequently there is **no** impact-edit
endpoint, for any role, global admin included. The way to change what an incident will be classified
as is to change the service's mapping — which is audited (§4.4) and governs only what happens next.

**D4 — monitors outside a service use ONE project default, not a per-monitor setting. OWNER (Q4).**
`projects.default_incident_impact`, default `major` (today's value, so nothing moves on upgrade for
monitor incidents).

**D5 — status-page subscribers are emailed when impact rises to `critical`. OWNER (Q5, author's
proposal accepted).** Only that rise. Rises to `minor` or `major` change the page and the timeline,
and notify webhooks, but do not email subscribers.

### Author's calls (open at review)

**D6 — the default mapping is `degraded → minor`, `down → major`, `unknown → none`. AUTHOR'S CALL.**
`unknown` means cerbix cannot see the service; claiming an impact it cannot observe would be the
exact "confident number" the product refuses — `none` renders as "Impact not specified".
**This changes behaviour on upgrade** for services that page on `degraded`: their new incidents open
as `minor`, not `major`. That is the correction this requirement exists for; the CHANGELOG entry
carries an upgrade note (roadmap §5 "Releases" rule), and incidents already open keep their impact.

**D7 — the mapping lives in the service's ALERTING policy, not in the definition revision.
AUTHOR'S CALL — departs from the letter of the owner's Q1 answer ("in the versioned definition").**
Every write of `service_definition_revisions` opens a new **evaluation epoch** and fans out a metric
recompute (`internal/store/services.go`, `insertEpoch` + `metricEventEpochFanout`): the revision is
"what the reliability number means". Impact does not change any number (NFR-035), and putting it
there would split a service's SLO history every time an operator re-grades severity. The mapping is
the sibling of `page_on` — "which states page" and "what a paging state means" — and lives beside it
on `services`, written through the same `ServiceAlertPolicy` read/patch path, audited by the same
`alertPolicyDiff`, and declared in the same Monitoring-as-Code `alerting` block.

What the owner's answer was protecting — "why was this incident `critical`?" — is kept by **D8**,
which freezes the mapping onto the incident, so the answer is on the incident and does not depend on
the service's current or historical configuration.

**D8 — the incident records the mapping it was classified under. AUTHOR'S CALL.** On open, the
service's mapping is copied into `incidents.impact_basis` (jsonb:
`{ "mapping": {...}, "source": "service_mapping" | "project_default" | "manual" | "alertmanager",
"state": "<state that set the current impact>" }`). `basis.source` is NOT derived from
`incidents.source`: Alertmanager incidents and token-created manual incidents are both stored as
`source = 'api'` (`handlers_alertmanager.go`, `handlers_incidents.go`), so each creating handler
supplies its basis source explicitly. Later rises are computed against **that frozen
mapping**, not against an edit made while the incident is open — the same rule FR-023 applies to an
escalation ladder frozen at open (`incident_escalation_snapshots`). An operator cannot raise or lower
an open incident by editing the mapping mid-outage (D3's spirit, closed from the side).

**D9 — a mapping change does NOT bump `alert_config_generation`. AUTHOR'S CALL.** The generation
dis-arms paging delegation until re-evaluated (migration 00082); impact does not change who is paged,
so dis-arming would cost pages for nothing.

**D10 — manual and Alertmanager incidents keep their creation-time impact and are never raised.
AUTHOR'S CALL.** A manual incident's impact is the author's statement at creation (D3 forbids only
*changing* it). An Alertmanager incident's impact is the sender's `severity`; FR-042 adds no
re-classification on a repeated firing (out of scope, §9). Neither has a service state to rise from.

**D12 — an incident open at upgrade keeps its impact and never rises. AUTHOR'S CALL (builder check
Q1).** It has no frozen mapping (`impact_basis` is NULL). Applying the service's live mapping would be
exactly the mid-incident edit I4 forbids, and inventing a basis would be a guess written as a fact.
An outage that spans the upgrade stays at the impact it opened with; the next incident is classified.

**D13 — a rise follows the CONFIRMED state, not the paging announcement. AUTHOR'S CALL (builder check
Q3).** The evaluator withholds an onset when the service is unroutable or has no governing revision
(the arming check before `emitServiceAlertTx`). Tying the rise to emission would leave the public page
understating an outage during a routing gap — and who is paged is irrelevant to impact (D9). So the
rise runs whenever the evaluator's candidate state is confirmed (streak ≥ `confirm_evaluations`), is in
the service's paging set (`page_on`, plus `unknown` when `page_on_unknown`), and an auto-incident is
open — **before** the withheld/notify branching, in the same transaction as the state row.

**D14 — the project default is not declarable in Monitoring-as-Code. AUTHOR'S CALL (builder check
Q2).** A bundle names its org and project but owns no project setting and has no project-level hash
(`internal/fileprovider/parse.go`). Making it own one means ownership rules (absent key = reset or
leave alone, 409 on the UI write, provider scope) for a single field. The project default is set
through the API/UI only; a later requirement may give bundles project settings as a whole.

**D11 — a monitor auto-incident does not inherit from the services the monitor belongs to.
AUTHOR'S CALL.** A monitor can be a member of several services with different mappings, and the
service already opens its OWN incident, correlated to the monitor's through impact links
(`CorrelateIncident`). The monitor's incident takes the project default (D4). The service's incident
is where the user-facing impact is stated.

## 4. Behaviour

### 4.1 The mapping

```
impact_by_state: { degraded: minor|major|critical|none,
                   down:     minor|major|critical|none,
                   unknown:  minor|major|critical|none }
```

All three keys are always present after resolution (omitted → D6 default). Validation: a closed
enum per key; **no monotonicity rule** is imposed between keys (an operator may grade `degraded`
`critical` for a service where partial failure is already unacceptable), but the SPA warns when
`degraded` > `down`. A state that does not page (`page_on` excludes it) never opens an incident, so
its mapping is inert until it does — shown as such in the editor, not refused.

### 4.2 Opening

`OpenServiceIncidentTx` writes `impact = mapping[opening state]` and `impact_basis` instead of the
`'major'` literal, inside the same transaction as the announcement (FR-022 invariant 7 unchanged).
The opening state is `candidateState` from the evaluator — the state the alert was emitted for.

The reconciler's monitor auto-incident writes `impact = project.default_incident_impact` and
`impact_basis.source = "project_default"`.

### 4.3 Rising

While a service auto-incident is open, every evaluation whose candidate state is confirmed and
pageable (D13) runs one statement in the evaluator's transaction — placed before the
withheld/notify branching, so it is NOT limited to the onset branch (where a degraded→down change
reaches `OpenServiceIncidentTx` and its `ON CONFLICT … DO NOTHING`) nor to the D-0187 `reannounce`
branch (which re-sends the same state):

```
UPDATE incidents SET impact = <frozen mapping>[new state], impact_basis.state = new state
 WHERE service_id = $1 AND source = 'auto' AND status <> 'resolved'
   AND impact_rank(<frozen mapping>[new state]) > impact_rank(impact)
```

Rank: `none < minor < major < critical`. Nothing else writes `impact` after insert (I3). A rise:
- writes a system incident update with the unique marker `⬆ Impact raised:` (`⬆ Impact raised:
  minor → major — the service is down.`), the idempotency pattern of `⚡ Context:` / `⏸ Suppressed:`.
  It must not collide with the existing `🕸 Impact:` marker (`internal/domain/serviceimpact.go`), and
  the SPA marker table (`frontend/src/lib/incident.ts`, where the key `impact` is taken by `🕸`) gets a
  new key, `impact_raised`. Timeline updates are public, so this note appears on the status page like
  the `⚡`/`🕸` notes do;
- enqueues an `incident_event` of type `incident.updated` (`domain.EventIncidentUpdated`) carrying the
  update, in the same transaction, so webhooks receive it with the incident's next `Seq` (D-0177
  ordering unchanged);
- carries `impact_raised: { from, to }` on that event (additive, optional field of `IncidentEvent`).

A fall changes nothing: the peak stands (D2). Note that under the default `page_on = {down}` a fall
from `down` to `degraded` is not a fall inside an open incident — `degraded` is not pageable, so the
evaluator closes and resolves the incident. "The peak stands" matters for services that page on
`degraded` too.

### 4.4 Configuring

- API: the existing service alerting read/patch gains `impact_by_state`; OpenAPI first, then the
  generated client. Project: `GET /api/v1/projects/{id}` gains `default_incident_impact`, and a
  **new** `PATCH /api/v1/projects/{id}` (today only GET and DELETE exist) accepts that one field,
  gated by `project:manage`; `domain.Project` gains the field.
- Monitoring-as-Code: the service `alerting` block gains `impact_by_state`. Like the rest of that
  block it stays **out of** the service declaration hash (`internal/fileprovider/service.go`): a hash
  change calls `putServiceDeclarationTx`, which always writes a revision and an epoch — hashing it
  would break I10. It is applied through the alerting path, as `page_on` is. The project default is
  not in MaC (D14).
- Audit: the service change goes through `alertPolicyDiff` (it names old and new mapping); the
  project change needs a **new** audit action (`project.update`), since no project-settings action
  exists today.

### 4.5 Subscribers

When mail is configured, `incidentFanout` (`internal/cli/cli.go`) emails confirmed subscribers on
every `incident_event` (without mail, `subs` is nil and nothing is emailed — unchanged). It gains one
rule: an event caused by an impact rise (`impact_raised` set) is emailed **only** when
`impact_raised.to = critical`, with subject `[critical] <title>` and a body that states the rise.
Every other event is emailed exactly as today (I8). Delivery semantics are today's: subscriber mail is
best-effort inside the event's fan-out, so a webhook failure that retries the event can re-send it —
FR-042 neither adds nor removes that.

### 4.6 Surfaces

The status page needs no change: the public render reads incidents at request time (behind the 5 s
shared cache, `internal/api/statuspagecache.go`) and computes worst impact in
`overallStatusPresentation` (`frontend/src/lib/statuspage.ts`). The incident detail page shows the impact with its basis
("critical — declared by service *payments* for state `down`", or "project default"). The service
alerting editor gets the three selectors (mock first).

## 5. Schema (sketch — the builder owns the exact DDL)

- `services.impact_by_state jsonb NULL` — `NULL` = D6 defaults; CHECK: keys ⊆ {degraded, down,
  unknown}, values in the impact enum. Not in the `service_alert_config_bump` trigger (D9).
- `projects.default_incident_impact text NOT NULL DEFAULT 'major'` with the impact CHECK.
- `incidents.impact_basis jsonb NULL` — `NULL` for every incident that predates the migration
  (rendered "basis not recorded"); never backfilled with a guess.
- Down migration: drops the three columns; no data elsewhere depends on them.

## 6. Acceptance invariants

- **I1** — A service auto-incident opens with `mapping[opening state]` from the service's mapping at
  that instant, and records that mapping in `impact_basis`.
- **I2** — While open, the impact equals the maximum, by rank, of the frozen mapping applied to every
  confirmed pageable state since open — whether or not that state's alert was announced (D13); it
  never decreases. An incident with no `impact_basis` never rises (D12).
- **I3** — No code path other than I1/I2's two statements and the creation of manual, Alertmanager
  and monitor incidents writes `incidents.impact`; there is no API that edits it (a test enumerates
  the routes and asserts none accepts `impact` on an existing incident).
- **I4** — Editing a service's mapping while it has an open incident does not change that incident's
  impact, now or on a later rise.
- **I5** — A monitor auto-incident opens with its project's `default_incident_impact`; a project that
  never set one produces `major`, as before.
- **I6** — Every rise writes exactly one `⬆ Impact raised:` update and one `incident_event`, in the
  transaction that raised it; a re-evaluation or re-announcement of the same state writes neither.
- **I7** — Manual and Alertmanager incidents are never raised.
- **I8** — With mail configured, an impact-rise event is emailed to subscribers if and only if the new
  impact is `critical` (with today's at-least-once re-send on event retry); every non-rise incident
  event is emailed exactly as before.
- **I9** (NFR-035) — With every service on the default mapping, every SLO, budget, burn, gate,
  coverage, delegation, suppression and escalation output is byte-identical before and after FR-042.
- **I10** — Changing a mapping does not create a definition revision, an evaluation epoch, or bump
  `alert_config_generation`.

## 7. Test matrix (written before the code)

Store tests on both storage modes: open per state × mapping; the rise table (degraded→down,
down→degraded, unknown→down, repeated same state, degraded→down while the onset is WITHHELD by the
arming check, an incident with NULL basis) asserting impact, update marker and event count;
mapping edited during an open incident (I4); project default (I5); I10 by counting revisions, epochs
and the generation around a mapping write. Domain: mapping validation and resolution of omitted keys.
API: alerting patch round-trip, project patch, the I3 route enumeration, OpenAPI parity
(`internal/contracttest`). Subscribe: the I8 table. MaC: hash stability with and without the new
keys. E2E on the dev stack: a service with `down → critical`, force it down, assert the public status
page shows "Critical impact" and the incident page shows the basis.

## 8. SPA (mock required before code)

1. Service → Alerting: three impact selectors beside `page_on`, inert-state hint, degraded > down
   warning.
2. Settings → Project: "Impact of monitor incidents".
3. Incident detail: impact basis line; `⬆ Impact:` updates render like the other system notes.

## 9. Non-goals

- Any human edit of an automatic incident's impact (D3), including "raise only".
- Impact rising or falling with duration, burn rate or number of affected services (time-based or
  topology-based escalation of severity).
- Re-classifying an open Alertmanager incident on a later firing with a different `severity`.
- Using impact to choose an escalation policy, route alerts, or gate deploys (NFR-035) — the one
  planned reader is FR-041c's quiet hours.
- Back-filling `impact_basis` for incidents opened before FR-042, or raising them (D12).
- Declaring the project default in Monitoring-as-Code (D14).

## 10. Open questions for review

- D6: is `unknown → none` right, or should `unknown` default to `minor` ("something may be wrong")?
- D7: does the owner accept the alerting-policy location given the epoch cost, with D8 as the
  traceability record?
