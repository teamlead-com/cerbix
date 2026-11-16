# func-incident-impact — an incident's impact is declared by its Service, derived by the machine, and only rises (FR-042 / NFR-035)

> **STATUS: revision 4 — DRAFT, not approved.** Revision 4 (2026-11-16) answers the second external
> review (CHANGES REQUIRED, P0 0 / P1 3 / P2 2 / P3 2, all in text revision 3 added — §11): the rise
> step looks before it locks and its concurrency is described as `REPEATABLE READ` actually behaves
> (§4.3); the system door takes only monitor auto-incidents and validation is reconciled with an
> impact the store resolves (§4.2); `acknowledge()` in the SPA must be changed to keep the basis
> (§4.6); the project write, its audit target and the basis encoding and CHECK are exact (§4.4,
> §4.6, §5). No decision changed.
>
> Revision 3 (2026-11-16) answered the first external
> review of revision 2 (CHANGES REQUIRED, P0 2 / P1 7 / P2 7 / P3 3 — dispositions in §11) and records
> the owner's decisions of 2026-11-16: D6, D7 and D12 are now OWNER decisions, and D16–D18 are new
> ones. What changed in substance: `impact_basis` has a shape per source, a service-name snapshot and
> a contract that keeps it off every public surface (D8, D15, §4.6); a rise is a locked step with a
> post-lock clock instead of one UPDATE, and may happen more than once per incident (§4.3); the
> subscriber rule belongs to `internal/subscribe` (§4.5); `none` is spelled "Impact not specified"
> wherever impact is rendered as words (§4.6); the project PATCH and the partial service PATCH are
> specified (§4.4); exact strings are given (§4.2, §4.3, §4.5).
>
> Revision 1 went through a builder check against the code (2026-11-13): three planning questions and
> seven code contradictions, folded in as D8, D12–D14, §4.3–§4.6, I2/I6/I8 and §7. Written 2026-11-13
> from the owner's answers to six design questions (recorded in §3 as OWNER decisions). Decisions the
> author took without an owner answer are marked **AUTHOR'S CALL**. The SPA surfaces in §8 need an
> approved mock before any frontend code.
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

### Owner decisions (2026-11-16, on the author's calls of revision 2 and on the first review)

**D6 — the default mapping is `degraded → minor`, `down → major`, `unknown → none`. OWNER
(2026-11-16).** `unknown` means cerbix cannot see the service; claiming an impact it cannot observe
would be the exact "confident number" the product refuses — `none` reads "Impact not specified"
everywhere impact is rendered as words (§4.6). An incident opens on `unknown` only for a service
that owns paging and set `page_on_unknown` (`internal/domain/servicealert.go`), which is off by
default.

**This changes behaviour on upgrade**, in two ways the CHANGELOG upgrade note must both name
(roadmap §5 "Releases" rule); incidents already open keep their impact (D12):

- a service that pages on `degraded` opens its new incidents as `minor`, not `major`;
- a service with `page_on_unknown` opens an incident for `unknown` as `none`, not `major`.

Consequence for FR-041c, recorded there as an open question and **not** decided here: quiet hours
let a phone alert through only for a `critical` incident or an escalation step
([`func-phone-alerts.md`](func-phone-alerts.md) D17, §6.4), so under the default an `unknown` onset
does not ring at night unless the service has an escalation policy.

**D7 — the mapping lives in the service's ALERTING policy, not in the definition revision. OWNER
(2026-11-16), with a condition: the frozen mapping must be visible on the incident page and returned
by the API (met by D8, D15, §4.6). It departs from the letter of the owner's Q1 answer of 2026-11-13
("in the versioned definition"), which this decision replaces.**
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

**D12 — an incident open at upgrade keeps its impact and never rises. OWNER (2026-11-16; builder
check Q1).** It has no frozen mapping (`impact_basis` is NULL). Applying the service's live mapping
would be exactly the mid-incident edit I4 forbids, and inventing a basis would be a guess written as
a fact. An outage that spans the upgrade stays at the impact it opened with; the next incident is
classified. The same holds for an incident an older binary opens after a rollback: it has no basis,
and it never rises.

**D16 — no mechanism for mixed versions; an upgrade note instead. OWNER (2026-11-16, review P1-3).**
A rise is an ordinary `incident_event` with one additive field (§4.3). A process that predates
FR-042 and delivers the outbox ignores that field and emails subscribers for **every** rise, with
today's subject, not only for a rise to `critical`. The roles that deliver the outbox are `all`,
`api` and `scheduler` (`servicesForRole`, `internal/cli/cli.go`: `coreDelivery`); `worker` and
`agent` do not. The mail is true — the same note is on the public timeline — and nothing is lost,
so the cost is a few extra mails while versions overlap or after a rollback with rise events still
queued. The runbook and the CHANGELOG upgrade note say so; no gate, no new topic.

**D17 — a manual incident's impact is fixed at creation, for everyone. OWNER (2026-11-16).** D3
forbids changing an automatic incident's impact; D10 leaves a manual incident where its author put
it. Together with "no impact-edit endpoint" this means an author who chose the wrong impact cannot
correct it — they resolve the incident and open another. That is today's behaviour (no code edits
`impact`), kept on purpose and listed in §9.

**D18 — a service alerting PATCH that carries `impact_by_state` carries all three keys. OWNER
(2026-11-16, review P2-2).** Absent field: the stored mapping is untouched. Present: `degraded`,
`down` and `unknown` are all required, each a value of the impact enum; a missing key, an unknown
key or a `null` is a 400. No per-key merge — a merge over stored values resets nothing, but a merge
over defaults silently would, and the two are indistinguishable to a caller.

### Author's calls (open at review)

**D8 — the incident records what it was classified under. AUTHOR'S CALL.** Every incident created
after FR-042 gets `incidents.impact_basis` (jsonb), in one of two shapes, chosen by `source`:

```
{ "source": "service_mapping",
  "mapping": { "degraded": "...", "down": "...", "unknown": "..." },   // resolved: all three keys
  "state": "<the state that set the CURRENT impact>",
  "service": { "name": "...", "slug": "..." } }                        // as of open

{ "source": "project_default" | "manual" | "alertmanager" }            // nothing else
```

The `service` snapshot is there because `incidents.service_id` is `ON DELETE SET NULL`
(migration 00084): an incident outlives its service, and "declared by service *payments*" must still
be readable then — the reason `incident_member_snapshots` stores names too. `basis.source` is NOT
derived from `incidents.source`: Alertmanager incidents and token-created manual incidents are both
stored as `source = 'api'` (`handlers_alertmanager.go`, `handlers_incidents.go`), so each creating
path supplies its basis source explicitly (§4.2 lists the four). Later rises are computed against
**that frozen mapping**, not against an edit made while the incident is open — the same rule FR-023
applies to an escalation ladder frozen at open (`incident_escalation_snapshots`). An operator cannot
raise or lower an open incident by editing the mapping mid-outage (D3's spirit, closed from the
side).

**D15 — `impact_basis` is not a field of `domain.Incident`. AUTHOR'S CALL (review P0-1).**
`domain.Incident` is embedded in the public status-page response, redacted by a list of exclusions
(`PublicRedacted`, `internal/domain/incident.go`), and is the `incident` object of every webhook
payload (`domain.IncidentEvent`). A field added there is public until somebody remembers to strip
it — the reason `authedIncidentView` (`internal/api/handlers_incidents.go`) keeps impact links and
member snapshots **beside** the embedded incident (its comment names invariant 59). The basis names
an internal policy and the incident's source, which a public row must not show
([`func-status-pages-incidents.md`](func-status-pages-incidents.md)). So the basis is read by its
own store call and returned only as `authedIncidentView.impact_basis`; the incident list, the
acknowledge response, the public page, the feeds and webhooks never carry it (I11).

**D9 — a mapping change does NOT bump `alert_config_generation`. AUTHOR'S CALL.** The generation
dis-arms paging delegation until re-evaluated (migration 00082); impact does not change who is paged,
so dis-arming would cost pages for nothing.

**D10 — manual and Alertmanager incidents keep their creation-time impact and are never raised.
AUTHOR'S CALL.** A manual incident's impact is the author's statement at creation (see D17 for what
that costs). An Alertmanager incident's impact is the sender's `severity`; FR-042 adds no
re-classification on a repeated firing (out of scope, §9). Neither has a service state to rise from.

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

**D19 — where the two new settings and the basis line live in the SPA. AUTHOR'S CALL (review
P2-6); the mock may move them.** The project default is a new tab **Incidents** in the *Project*
group of `frontend/src/views/SettingsView.vue`, after *Release gate*: shown to everyone who sees the
project's settings, read-only without `project:manage`. The basis line is on the incident page
(`IncidentDetailView.vue`) only — the right-hand panel of `IncidentsView.vue` is fed by the list,
which does not carry the basis (D15).

**D20 — rises are counted in the evaluator's summary and logged; no new Prometheus metric. AUTHOR'S
CALL (review P2-5).** `ServiceAlertEvaluation` gains `IncidentsRaised`; the leader's
`service_alerts_evaluated` line (`internal/scheduler/scheduler.go`) prints it and is emitted when it
is non-zero — that line is conditional, and none of its present conditions is "a rise". A counter is
declined: a rise is already a row in `incident_updates` and an outbox event, and it is not a
condition anyone alerts on.

## 4. Behaviour

### 4.1 The mapping

```
impact_by_state: { degraded: minor|major|critical|none,
                   down:     minor|major|critical|none,
                   unknown:  minor|major|critical|none }
```

The **resolved** mapping always has all three keys. Where a key may be omitted, and what omission
means, differs by door:

| Door | Omitted |
| --- | --- |
| Stored value (`services.impact_by_state`) | `NULL` = the D6 defaults; a stored object always has all three keys |
| Monitoring-as-Code `alerting.impact_by_state` | block absent → D6 defaults; a key absent inside it → that key's D6 default — the bundle is the whole declaration, as an omitted `page_on` is `{down}` (`internal/fileprovider/service.go`) |
| API PATCH | field absent → untouched; field present → all three keys required (D18) |

Two mappings are **equal** when their resolved forms are equal: `NULL` and an explicit copy of the
defaults are the same policy, so writing one over the other is no change — no audit row, no
`updated_at` (as `UpdateServiceAlertPolicy` already treats a no-op).

Validation: a closed enum per key; **no monotonicity rule** is imposed between keys (an operator may
grade `degraded` `critical` for a service where partial failure is already unacceptable), but the
SPA warns when `degraded` > `down`. A state that does not page (`page_on` excludes it) never opens
an incident, so its mapping is inert until it does — shown as such in the editor, not refused.

### 4.2 Opening

Four paths create an incident, and each writes `impact` and `impact_basis` in the statement that
inserts the row:

| Path | `impact` | `impact_basis.source` |
| --- | --- | --- |
| Service auto-incident — `OpenServiceIncidentTx` (`internal/store/serviceincidents.go`) | `mapping[opening state]` instead of the `'major'` literal | `service_mapping`, with the resolved mapping, the opening state and the service's name and slug |
| Monitor auto-incident — the reconciler through `CreateIncidentBySystem` (`internal/store/incidents.go`) | the project's `default_incident_impact` | `project_default` |
| Manual — `POST …/incidents` (`internal/api/handlers_incidents.go`) | the author's choice, default `minor` (unchanged) | `manual` |
| Alertmanager receiver (`internal/api/handlers_alertmanager.go`) | from `severity` (unchanged) | `alertmanager` |

**Service.** Same transaction as the announcement (FR-022 invariant 7 unchanged). The opening state
is `candidateState` from the evaluator — the state the alert was emitted for — so the evaluator's
policy reads (`evaluateServiceAlertsOn`, `lockAlertConfigRowsTx` in
`internal/store/servicealerteval.go`) gain `impact_by_state`. The opening timeline note names that
state. Today it reads `service DOWN confirmed` for every state, which was merely loose while every
incident was `major` and becomes false beside "Impact raised … the service is down" five minutes
later:

```
Opened automatically: service DOWN confirmed over <n> evaluation(s).
Opened automatically: service DEGRADED confirmed over <n> evaluation(s).
Opened automatically: service UNKNOWN confirmed over <n> evaluation(s).
```

**Monitor.** The project default is read **inside** the creating transaction — the insert selects
it from `projects` when the incident is `source = 'auto'` with a monitor anchor — so the value and
the row that records it cannot disagree with a concurrent project PATCH, the reconciler makes no
extra read per down transition, and `ingest.ReconcileStore` does not grow. The reconciler stops
passing `domain.ImpactMajor`, which has two consequences the builder must meet:

- **Validation.** `createIncident` calls `Incident.Validate()` before it opens the transaction, and
  `Validate` refuses an empty impact (`internal/domain/incident.go`). A monitor auto-incident now
  arrives without one. It must be accepted, and **every other** incident must still be refused
  without a valid impact — by a rule in `Validate` (empty allowed only for `source = auto` with a
  monitor anchor, the way `IncidentUpdate.Validate` allows an empty status), or by validating after
  the default is resolved inside the transaction. The builder chooses; §7 tests both sides.
- **The system door takes one kind of incident.** `CreateIncidentBySystem` accepts a
  monitor-anchored `auto` incident with an **empty** impact, and refuses everything else: an impact
  supplied by the caller (it would be silently overridden or silently win, and neither is a
  default), a missing monitor anchor, another source. Production has one caller, the reconciler; the
  door's present defensive branch for a service-anchored incident goes, because such an incident
  could not be given a `service_mapping` basis there — `OpenServiceIncidentTx` is the only way to
  open one. The store's test fixtures that use this door for anything else (the door has 62 call sites in test files
  today) move to `CreateIncidentByPrincipal` or
  `OpenServiceIncidentTx`; a fixture that needs a monitor incident of a particular impact sets the
  project default.

**Manual and Alertmanager.** Both go through `CreateIncidentByPrincipal`, which gains an explicit
basis source, `manual` or `alertmanager`; a call that supplies neither is refused, so a fifth path
cannot be added without stating what it is.

### 4.3 Rising

While a service auto-incident is open, every evaluation whose candidate state is confirmed and
pageable (D13) runs one **step** in the evaluator's transaction — placed before the withheld/notify
branching, so it is NOT limited to the onset branch (where a degraded→down change reaches
`OpenServiceIncidentTx` and its `ON CONFLICT … DO NOTHING`) nor to the D-0187 `reannounce` branch
(which re-sends the same state). The step is a sibling of `resolveServiceIncidentTx`
(`internal/store/serviceincidents.go`) and follows its order, for its reasons (D-0177) — with one
difference that matters: the resolve runs once per incident, this runs on **every** evaluation
while the incident is open, and almost always has nothing to do. So it looks before it locks:

0. Read the service's open auto-incident **without a lock**: `SELECT id, impact, impact_basis FROM
   incidents WHERE service_id = $1 AND source = 'auto' AND status <> 'resolved'`. Stop, writing
   nothing, when there is no row (a withheld onset with no open incident opens nothing — FR-022
   invariant 5 unchanged), when `impact_basis` is NULL (D12), when its `source` is not
   `service_mapping`, or when `rank(basis.mapping[state]) <= rank(impact)`. The mapping is the
   **frozen** one on the incident; the service's live mapping is not read (I4). This read is safe
   to trust for "nothing to do" because impact never falls (D2), the frozen mapping never changes
   (D8) and `resolved` is terminal: a stale answer can only say "raise" where the lock then says
   "already raised", never the reverse.
1. Only when a rise is due: `SELECT id, status, impact FROM incidents WHERE id = $1 AND status <>
   'resolved' FOR UPDATE`, and compare the ranks again on what it returns.
2. Read the clock **after** the lock: `SELECT statement_timestamp()`. The evaluator's own `asOf` is
   taken when its snapshot opens and is earlier than anything this step waited for.
3. `UPDATE incidents SET impact = <to>, impact_basis = jsonb_set(impact_basis, '{state}', …),
   updated_at = <clock>`. `updated_at` is not optional: the public page's "Updated …" line and
   card order, and the feeds' `<updated>`, read it.
4. Insert the timeline note with the incident's **current** status (from step 1 — a rise does not
   move the lifecycle), `author = 'system'`, `created_at = <clock>`.
5. `nextIncidentEventSeqTx`, then `enqueueOutboxAtTx(…, <clock>)` with an `incident_event` of type
   `incident.updated` (`domain.EventIncidentUpdated`) carrying the updated incident, the note, the
   next `Seq`, and `impact_raised: { "from": …, "to": … }` — an additive, optional field of
   `domain.IncidentEvent`.

Rank: `none < minor < major < critical`. Nothing else writes `impact` after insert (I3).

**Idempotency is the rank comparison** — not a marker lookup. The `⚡ Context:` / `⏸ Suppressed:`
guard (`AppendIncidentContext`, `NOT EXISTS … body LIKE marker%`) allows one such note per incident
for its whole life, and an incident may rise more than once: with `page_on = {degraded, down}`,
`page_on_unknown` and the default mapping, `unknown` (none) → `degraded` (minor) → `down` (major)
is two rises. A re-evaluation of the same state stops at step 0 and writes nothing.

**Concurrency, as the evaluator's isolation level makes it.** The evaluator runs one
`REPEATABLE READ` transaction per slice of up to 50 services (`beginAlertSnapshot`,
`serviceAlertSliceLimit`, `internal/store/servicealerteval.go`). In that level a `FOR UPDATE` that
waited for another transaction's committed change to the row does **not** read the new version: it
fails with SQLSTATE 40001. The leader does not retry it — `session.EvaluateServiceAlerts` returns
the error, the scheduler logs `service_alert_eval_failed`, counts an arm error and evaluates again
on its next `serviceAlertEvery` tick (`internal/scheduler/scheduler.go`). So:

- an operator who acknowledges or annotates the incident in the instant a rise is being written
  costs that slice one pass; the rise lands on the next tick, over a snapshot that already holds
  the operator's change, and its note carries the status the operator left;
- this is why step 0 does not lock. A lock on every tick would make every acknowledge during an
  outage a 40001 for a slice of fifty services, for an incident that had nothing to raise —
  the reason `alertPolicyMatchesTx` (`internal/store/fileapply_alerting.go`) looks before it locks;
- two evaluators cannot both raise: the second fails at step 1 the same way, and on its next pass
  step 0 finds the ranks equal. A deposed leader's transaction is rolled back with its session.

The note. Marker `⬆ Impact raised:`, distinct from the existing `🕸 Impact:`
(`internal/domain/serviceimpact.go`); the SPA marker table (`frontend/src/lib/incident.ts`, where
the key `impact` is taken by `🕸`) gets a new key, `impact_raised`. Timeline notes are public, so it
appears on the status page as the `⚡`/`🕸` notes do. Exact bodies, by the state that caused the rise:

```
⬆ Impact raised: <from> → <to> — the service is degraded.
⬆ Impact raised: <from> → <to> — the service is down.
⬆ Impact raised: <from> → <to> — the service's state is unknown.
```

`<from>` and `<to>` are the stored enum words (`none`, `minor`, `major`, `critical`).

A fall changes nothing: the peak stands (D2). Note that under the default `page_on = {down}` a fall
from `down` to `degraded` is not a fall inside an open incident — `degraded` is not pageable, so the
evaluator closes and resolves the incident. "The peak stands" matters for services that page on
`degraded` too.

### 4.4 Configuring

OpenAPI first (`openapi.yaml`), then the generated client (`frontend/src/api/schema.d.ts`).

**Service mapping — API.** The `ServiceAlerting` schema gains `impact_by_state`, always returned
resolved (three keys). It is served in two places, and both change: `GET`/`PATCH
/api/v1/projects/{projectID}/services/{serviceID}/alerting`, and the `alerting` object embedded in
`GET /api/v1/projects/{projectID}/services/{serviceID}` (`internal/api/handlers_services.go`), from
which `ServiceAlerting.vue` takes its initial values. The PATCH follows D18 and travels to the store
as a patch (`ServiceAlertPolicyPatch`), merged under the row lock as the other fields are; what can
be judged without the stored value — three keys, enum values — is a 400 before the store is called.
A file-managed service keeps answering 409 `managed_by_file`.

**Service mapping — Monitoring-as-Code.** The service `alerting` block gains `impact_by_state`
(omission rules in §4.1). Like the rest of that block it stays **out of** the service declaration
hash (`internal/fileprovider/service.go`): a hash change calls `putServiceDeclarationTx`, which
always writes a revision and an epoch — hashing it would break I10. It is applied through the
alerting path (`internal/store/fileapply_alerting.go`), and that path has **three** column lists the
field must join: the unlocked pre-filter `alertPolicyMatchesTx`, the locked re-read, and the UPDATE.
Leaving it out of the pre-filter alone makes a bundle that changes only the mapping read as "nothing
changed" and be dropped without a trace.

**Service mapping — audit.** `alertPolicyDiff` names the change as one segment, in the fixed key
order, both sides resolved:
`impact_by_state:degraded=minor,down=major,unknown=none→degraded=minor,down=critical,unknown=none`.
An equal mapping (§4.1) contributes no segment.

**Project default — API.** `domain.Project` and the `Project` schema gain
`default_incident_impact`; every endpoint returning a project carries it — `GET
/api/v1/organizations/{orgID}/projects`, `POST /api/v1/organizations/{orgID}/projects` (which does
not accept it: a new project starts at `major`), and `GET /api/v1/projects/{projectID}`. A **new**
`PATCH /api/v1/projects/{projectID}` (today only GET and DELETE exist) accepts that one optional
field:

- **Order of refusals** — the D-0212 two-step, as `getProject` does it: unknown or invisible project
  (`VisibleProject(orgID, projectID)`) → 404; visible without `project:manage` on **that project**
  → 403. Then the body: a value outside the impact enum → 400.
- **A project that has file-managed monitors or services is patched like any other** — not 409.
  `DeleteProject` refuses such a project because deletion would orphan what a bundle owns; this
  field is owned by no bundle (D14), and a 409 here would leave those projects no way to set it.
- **The write** — one transaction: `SELECT … FROM projects WHERE id = $1 FOR UPDATE` for the slug
  and the stored value, then, only when the value differs, `UPDATE projects SET
  default_incident_impact = $2, updated_at = now()` — `projects` has no `updated_at` trigger, the
  statement sets it, as the SLA-target write does (`internal/store/sla.go`). `domain.Project` and
  the store's project column list gain the field, which is what puts it in every response above.
- **No-op** — an empty object, or the value already stored: 200 with the project, no write, no
  `updated_at` change, no audit row.
- **Audit** — in the same transaction as the write, through `insertProjectAudit`
  (`internal/store/incidentaudit.go`), not the best-effort `h.audit`. Action
  `project.default_incident_impact`, beside the existing `project.sla_target`,
  `project.sla_target_clear` and `project.delete`. Target `<project slug> <old>→<new>`, passed
  through `incidentAuditTarget`, so a token caller — the route is open to a token holding
  `project:manage`, and its `actor_user_id` is NULL — is named: `… · actor=<token label>`.

The project default is not in Monitoring-as-Code (D14).

### 4.5 Subscribers

Subscriber mail is owned by `internal/subscribe` (`Notifier.Deliver` and `render`,
`internal/subscribe/subscribe.go`): today it mails every confirmed subscriber of the project on
every `incident_event`, with subject `[<status>] <title>`. `incidentFanout`
(`internal/cli/cli.go`) only calls the webhook dispatcher and then the notifier, and stays that —
the rule below is **not** written there, because webhooks must keep receiving every rise and
because §7 tests the rule in the package that owns it.

The rule, in `Notifier.Deliver`: an event with `impact_raised` set is mailed **only** when
`impact_raised.to = critical`; every event without it is mailed exactly as today (I8). The mail for
a rise to `critical`:

```
Subject: [critical] <title>

Incident impact raised to critical: <title>

Status: <status>
Impact: critical (was <from>)

<the ⬆ Impact raised: note>
```

Delivery semantics are today's: subscriber mail is best-effort inside the event's fan-out, so a
webhook failure that retries the event can re-send it — FR-042 neither adds nor removes that. No
maintenance or dependency suppression applies to `incident_event`, before or after. Mixed versions:
D16.

### 4.6 Surfaces

**The basis (D15).** `GET /api/v1/incidents/{incidentID}` returns `impact_basis` on
`authedIncidentView` / the `AuthedIncident` schema, the two shapes of D8 as a `oneOf` on `source`.
It is read by its own store call, scoped by project at the store boundary as
`ListIncidentImpacts(ctx, projectID, incidentID)` is (`WHERE id = $1 AND project_id = $2`) — the
handler's access check is not the only fence. Two fields, encoded so that three states stay apart:

| State | `impact_basis` | `impact_basis_unavailable` |
| --- | --- | --- |
| recorded | the object | absent |
| not recorded (D12) | `null` — always serialized, never omitted | absent |
| could not be read | `null` | `true` (`omitempty`) |

The incident still renders when the read fails, as it does for impact links
(`impacts_unavailable`).

Nothing else returns the basis — not the incident list, not `POST …/acknowledge` (which answers
the base `Incident`), not the public status page, not the feeds, not a webhook payload. The
acknowledge response needs a change in the SPA: `acknowledge()` in `IncidentDetailView.vue`
**replaces** the loaded incident with the response and carries over only `impacts` and
`impacts_unavailable`, so today it would drop the basis and the page would print "basis not
recorded" after a click. It must carry `impact_basis` and `impact_basis_unavailable` over as well.
(That it also drops `acknowledged_by_name` is an existing defect, noted and not this spec's.)

The incident page shows one line under the impact, by case:

| Basis | Line |
| --- | --- |
| `service_mapping` | `<impact> — declared by service <name> for state <state>` (the snapshot name; a link to the service only while `service_id` is still set) |
| `project_default` | `<impact> — project default for monitor incidents` |
| `manual` | `<impact> — set by the author when the incident was created` |
| `alertmanager` | `<impact> — from the alert's severity label` |
| NULL | `<impact> — basis not recorded (opened before impact classification)` |
| unreadable | `<impact> — basis unavailable` |

**`none` in words.** D6 makes `none` an ordinary value of automatic incidents, and only the page
headline spells it properly today (`overallStatusPresentation`, `frontend/src/lib/statuspage.ts`:
"Impact not specified"). Everywhere else it is printed from the badge table
(`frontend/src/lib/incident.ts`, `none → "None"`) or from the raw enum. These change, public and
operator side alike, so that no surface reads "None", "None impact" or "none impact" (I13):

- `PublicStatusView.vue` — the count pills, the group heading shown above eight active incidents,
  and the card badge;
- `PastIncidentRow.vue` — the "… · none impact" tail of a past incident, on the page and in
  `/status/:slug/history`;
- `IncidentsView.vue` — the list badge and the "<impact> impact" badge of the selected incident;
- `IncidentDetailView.vue` — the impact badge;
- `NewIncidentView.vue` — the option of the impact selector.

The wording is "Impact not specified" as a phrase and "Not specified" as a badge; the mock fixes
each place.

This is about the SPA. Subscriber mail (`Impact: none`, `internal/subscribe`) and the feeds
(`status: … · impact: none`, `internal/api/handlers_feed.go`) print the stored word and keep
doing so: §4.5 requires every non-rise mail to stay byte-identical, and a feed is read by programs.

**The public page otherwise.** It reads incidents at request time (behind the 5 s shared cache,
`internal/api/statuspagecache.go`) and computes worst impact in `overallStatusPresentation`; a rise
needs nothing more there. The feeds (`internal/api/handlers_feed.go`) print `status · impact` from
the incident row and take their `<updated>` from `updated_at`, so a rise shows up because §4.3
step 4 moves it.

**Webhooks.** `incident.updated` with the additive `impact_raised`; the `incident` object carries
the new impact and, by D15, no basis.

**Settings.** The service alerting editor gets the three selectors; the project gets its default
(D19). Mock first (§8).

## 5. Schema (sketch — the builder owns the exact DDL)

- `services.impact_by_state jsonb NULL` — `NULL` = D6 defaults; CHECK: an object with exactly the
  keys {degraded, down, unknown}, values in the impact enum. Not in the `service_alert_config_bump`
  trigger (D9) — that trigger is `BEFORE UPDATE OF` a closed column list (migration 00082), and
  00091 already left `renotify_seconds` out of it.
- `projects.default_incident_impact text NOT NULL DEFAULT 'major'` with the impact CHECK.
- `incidents.impact_basis jsonb NULL` — `NULL` for every incident that predates the migration
  (rendered "basis not recorded"); never backfilled with a guess. CHECK, when not NULL — closed,
  so a shape nobody specified cannot be stored (all of it is plain jsonb operators, no subquery):
  an object whose `source` is one of the four; for `service_mapping` the top-level keys are exactly
  {source, mapping, state, service}, `mapping` satisfies the same three-key check as
  `services.impact_by_state`, `state` is one of {degraded, down, unknown}, and `service` is an
  object with exactly the string keys `name` and `slug`; for the other three sources the only key
  is `source`.
- Down migration: drops the three columns; no data elsewhere depends on them.
- None of this touches `heartbeats`; the three tables are ordinary tables in both storage modes.
- **An older binary on this schema** writes none of the new columns and names its columns
  explicitly, so nothing it does is refused: its incidents get a NULL basis (D12), its projects the
  default. What it cannot do is apply the subscriber rule (D16).

## 6. Acceptance invariants

- **I1** — A service auto-incident opens with `mapping[opening state]` from the service's mapping at
  that instant, and records that mapping in `impact_basis`.
- **I2** — While open, the impact equals the maximum, by rank, of the frozen mapping applied to every
  confirmed pageable state since open — whether or not that state's alert was announced (D13); it
  never decreases. An incident with no `impact_basis` never rises (D12).
- **I3** — `incidents.impact` is written by the four creating paths of §4.2 and by the rise step of
  §4.3, and by nothing else; there is no API that edits it. Creating routes do accept an impact
  (`impact`, or Alertmanager's `severity`) — the invariant is about an **existing** incident.
- **I4** — Editing a service's mapping while it has an open incident does not change that incident's
  impact, now or on a later rise.
- **I5** — A monitor auto-incident opens with its project's `default_incident_impact`; a project that
  never set one produces `major`, as before.
- **I6** — Every rise writes exactly one `⬆ Impact raised:` update and one `incident_event`, in the
  transaction that raised it; a re-evaluation or re-announcement of the same state writes neither.
  An incident may rise more than once, and then has one note and one event **per rise**.
- **I7** — Manual and Alertmanager incidents are never raised.
- **I8** — With mail configured, an impact-rise event is emailed to subscribers if and only if the new
  impact is `critical` (with today's at-least-once re-send on event retry); every non-rise incident
  event is emailed exactly as before.
- **I9** (NFR-035) — With every service on the default mapping, every SLO, budget, burn, gate,
  coverage, delegation, suppression and escalation output is byte-identical before and after FR-042.
- **I10** — Changing a mapping does not create a definition revision, an evaluation epoch, or bump
  `alert_config_generation`.
- **I11** — `impact_basis` is returned by the authenticated incident detail endpoint and by nothing
  else: not the incident list, the acknowledge response, the public status page, the feeds, or a
  webhook payload (D15).
- **I12** — A rise stamps the incident's `updated_at`, its note's `created_at` and its outbox row
  with one instant read after the incident row is locked; the note carries the incident's status as
  locked and does not change it. An evaluation that has nothing to raise takes no lock on the
  incident (§4.3 step 0).
- **I13** — No **SPA** surface renders impact `none` as "None", "None impact" or "none impact"; it
  reads "Impact not specified" / "Not specified" (§4.6). Mail and feeds keep the stored word.
- **I14** — Every incident created after FR-042 has an `impact_basis` of the shape its source
  requires (D8); the stored `service` snapshot keeps the basis line readable after the service is
  deleted.

## 7. Test matrix (written before the code)

Store tests need no storage-mode branching — `incidents`, `services` and `projects` are ordinary
tables — and are covered in both modes by the ordinary suite (`make race-both`, and the CI matrix).

| Layer | Cases | Invariants |
| --- | --- | --- |
| Domain | mapping validation; resolution of omitted keys per door (§4.1); equality of `NULL` and explicit defaults; the basis shapes | I1, I14 |
| Store — open | per state × mapping; basis shape for each of the four creating paths, and the refusal of a create with no basis source; the opening note names the state; the project default read in the creating transaction against a concurrent project PATCH; the system door refuses a supplied impact, a missing monitor anchor and another source; an empty impact is refused on a manual and on an Alertmanager incident | I1, I5, I14 |
| Store — rise | degraded→down; down→degraded (no fall); unknown→down; **two rises in one incident** (unknown→degraded→down: two notes, two events); repeated same state (neither); degraded→down while the onset is WITHHELD by the arming check; NULL basis; a basis whose source is not `service_mapping`; no open incident | I2, I6 |
| Store — rise, clock and lock | (a) the step called directly in a `READ COMMITTED` transaction behind a manual writer that **commits** a status change (the shape of `TestAServiceIncidentResolvesAfterTheWaitItActuallyDid`): `updated_at`, note `created_at` and the outbox row are after the wait and equal, and the note's status is the one the writer left; (b) the same interleave through `evaluateServiceAlertsOn` (`REPEATABLE READ`): the pass fails with 40001 and writes nothing, the next pass raises and its note carries the writer's status; (c) an evaluation with nothing to raise, while another transaction holds the incident row locked, completes without waiting | I12 |
| Store — frozen mapping | mapping edited while an incident is open, then a worse state: the rise uses the frozen mapping | I4 |
| Store — I10 | count revisions, epochs and `alert_config_generation` around a mapping write | I10 |
| Store — never raised | one service goes from degraded to down with three incidents open in its project — its own auto-incident, a manual one and an Alertmanager one: the service incident rises, the other two keep impact and timeline. The rise in the same test is what makes it fail before the code exists | I7 |
| Store — I3 | a scan of the store package's SQL for statements that write `incidents.impact`, compared against an asserted list — the creating inserts and the rise step — so a new writer fails the test | I3 |
| API | alerting PATCH: absent field, full mapping, partial mapping → 400, unknown key → 400, `null` → 400, file-managed → 409, the embedded `alerting` of the service GET; project PATCH: 404 before 403, a `project_admin` of that project allowed, a file-managed project → 200, no-op → no audit row, audit row in the transaction, with `· actor=<label>` for a token caller; `updated_at` moves on a change and not on a no-op; every route that mutates an existing incident refuses or ignores `impact` | I3, D18 |
| API — basis confinement | detail returns the basis in each shape and NULL; the list, the acknowledge response, the public status-page JSON, each feed format and a captured webhook payload do not contain the key; the three encodings of §4.6 (object, `null`, `null` with `impact_basis_unavailable`); the store read refuses an incident id under another project | I11 |
| API — contract | `patchServiceAlertingRequest` and the project PATCH body registered in `internal/api/requestcontract_test.go`; `frontend/src/api/schema.d.ts` regenerated and unchanged by a second generation | — |
| Subscribe (`internal/subscribe`) | rise to minor, major → no mail; rise to critical → the §4.5 mail; opened / updated / resolved without `impact_raised` → today's mail, byte for byte | I8 |
| MaC | hash stability with and without the new keys; **re-applying a bundle whose only change is `impact_by_state`** updates the row and writes the audit segment (the pre-filter must not drop it); omitted block and omitted key resolve as §4.1; the UI PATCH on that service → 409 | I10 |
| NFR-035 regression | two services identical but for the mapping (defaults against `down → critical`), driven through the same outage: SLI, budget, burn, coverage, gate decision, delegation, suppression and escalation outputs are equal; plus a source scan asserting that the only readers of `incidents.impact` outside rendering, audit and the rise step are the ones this spec names | I9 |
| SPA unit | every surface of §4.6 for `none`; the six basis lines; the `impact_raised` marker; the degraded > down warning; `acknowledge()` keeps `impact_basis` and `impact_basis_unavailable` | I13 |
| E2E, dev stack | a service with `down → critical`, forced down: the public page shows "Critical impact", the incident page shows the basis line | — |

## 8. SPA (mock required before code)

1. Service → Alerting (`ServiceAlerting.vue`): three impact selectors beside `page_on`, the
   inert-state hint, the degraded > down warning.
2. Settings → *Project* group → new tab **Incidents**: "Impact of monitor incidents", one selector,
   read-only without `project:manage` (D19).
3. Incident detail: the basis line in its six forms (§4.6); `⬆ Impact raised:` notes render like
   the other system notes.
4. `none` in words, on every surface listed in §4.6 — the mock shows each: public count pill, group
   heading, card badge, past-incident row, operator list badge, selected-incident badge, incident
   page badge, the new-incident selector.

## 9. Non-goals

- Any human edit of an automatic incident's impact (D3), including "raise only".
- Any edit of a **manual** incident's impact after creation (D17): a wrong choice is corrected by
  resolving the incident and opening another.
- A gate, a new outbox topic or any other mechanism that keeps a pre-FR-042 process from mailing
  subscribers on a rise below `critical` (D16).
- A Prometheus metric for rises (D20).
- Impact rising or falling with duration, burn rate or number of affected services (time-based or
  topology-based escalation of severity).
- Re-classifying an open Alertmanager incident on a later firing with a different `severity`.
- Using impact to choose an escalation policy, route alerts, or gate deploys (NFR-035) — the one
  planned reader is FR-041c's quiet hours.
- Back-filling `impact_basis` for incidents opened before FR-042, or raising them (D12).
- Declaring the project default in Monitoring-as-Code (D14).

## 10. Open questions for review

None for the owner: D6, D7 and D12 were decided on 2026-11-16, with D16–D18.

The author's calls — D8, D9, D10, D11, D13, D14, D15, D19, D20 — have each been agreed by the
reviewer (D9–D14 in the first pass, D10 on the condition D17 states; D8, D15, D19, D20 in the
second). They stay marked AUTHOR'S CALL until the owner approves the revision.

Interplay with FR-043 (recovery threshold), checked by the first pass and recorded so it is not
re-derived: a monitor held `down` by a recovery run keeps its auto-incident open longer, at the
project default, which never rises; a service's live health reads raw results, not
`monitors.status`, so a service incident may close before the monitor's does. Neither changes
anything here.

## 11. Review history

**Builder check of revision 1 (2026-11-13).** Three planning questions and seven code
contradictions, folded into revision 2 as D8, D12–D14, §4.3–§4.6, I2/I6/I8 and §7.

**External review 1 of revision 2 (2026-11-16, against `main` at `b823aca`): CHANGES REQUIRED,
P0 2 / P1 7 / P2 7 / P3 3.** It confirmed the four creating paths of §1 and found no fifth, the
premise of D7 (`putServiceDeclarationTx` always opens an epoch and fans out a recompute), D9 against
the 00082 trigger, D10's `source = 'api'` claim, NFR-035 by reading every computation, and that an
older binary's writes are not refused by the new schema.

| Finding | Disposition |
| --- | --- |
| P0-1 — no API contract for `impact_basis`; a field on `domain.Incident` would reach the public page and webhooks | **Confirmed.** The reviewer's first option is taken and made the rule: the basis is not on `domain.Incident` at all (D15), which also settles the acknowledge response. §4.6, I11, §7 |
| P0-2 — the basis shape is undefined for three of four sources, has no label for two, and loses the service's name when the service is deleted | **Confirmed.** Two shapes and a service snapshot (D8), four writers (§4.2), CHECK (§5), six lines (§4.6), I14 |
| P1-1 — the `⚡ Context:` idempotency pattern allows one note per incident and would drop the second rise's note | **Confirmed** (`AppendIncidentContext`). Idempotency is the rank comparison under the lock; I6 allows several rises. §4.3 |
| P1-2 — one UPDATE cannot return `from`, skips `updated_at`, reads the clock before the lock, and has no NULL-basis guard | **Confirmed.** Replaced by a step on the model of `resolveServiceIncidentTx`. §4.3, I12 |
| P1-3 — a process that predates FR-042 mails subscribers on every rise | **Confirmed, with a correction:** the roles that deliver the outbox are `all`, `api` and `scheduler`, not `worker`. The owner chose an upgrade note over a mechanism (D16) |
| P1-4 — "the status page needs no change" is false: `none` prints as "None" / "none impact" outside the headline | **Confirmed.** §4.6 lists the surfaces, including two the review did not name (`IncidentDetailView.vue`, `NewIncidentView.vue`); I13, §8 |
| P1-5 — the subscriber rule was assigned to `incidentFanout` | **Confirmed.** It belongs to `internal/subscribe`; the fan-out is unchanged. §4.5 |
| P1-6 — wrong package for the contract test; endpoints missing; the MaC pre-filter | **Confirmed**, all three. §4.4, §7 |
| P1-7 — with `unknown → none`, FR-041c's quiet hours do not ring for a service that pages on unknown | **Confirmed, and not decided here.** D6 stands (owner); the consequence is stated in D6 and added to `func-phone-alerts.md` §13 as an open question for that spec's review |
| P2-1 — the project PATCH: refusal order, file-managed projects, no-op, audit; and project audit actions do exist | **Confirmed.** §4.4; the action is `project.default_incident_impact`, not `project.update` |
| P2-2 — partial PATCH of the mapping is undefined | **Confirmed.** The owner chose "all three keys or 400" (D18). §4.1, §4.4 |
| P2-3 — where the project default is read; the upgrade note names one of two behaviour changes | **Confirmed.** §4.2 (inside the creating transaction), D6 (both changes) |
| P2-4 — no exact strings; the opening note says "service DOWN" for every state | **Confirmed** (`OpenServiceIncidentTx`). §4.2, §4.3, §4.5 |
| P2-5 — a rise is invisible to the evaluator's telemetry | **Accepted in part.** `IncidentsRaised` and the log line, no metric (D20). The review's example — a rise during a withheld onset — is already logged, because the line is emitted when anything was withheld; the gap is that the line does not say a rise happened |
| P2-6 — §8 does not place the project setting or the basis line | **Accepted.** D19, §8 |
| P2-7 — no test rows for I7 and I9; I3's test misdescribed; no MaC re-apply test | **Accepted.** §7 is now a table with an invariant column |
| P3-1 — `⬆ Impact:` in §8 against `⬆ Impact raised:` elsewhere | **Fixed.** §8 |
| P3-2 — "both storage modes" suggests a dual-mode harness | **Fixed.** §5, §7 |
| P3-3 — D6, D7, D12 and §10 not updated for the owner's decisions | **Fixed.** §3, §10 |

The reviewer's remark on D3 + D10 — nobody can raise a manual incident either — is D17 and a line
in §9.

**External review 2 of revision 3 (2026-11-16, read at `9400298`, checked against `31815ae` after
FR-043 merged): CHANGES REQUIRED, P0 0 / P1 3 / P2 2 / P3 2.** Of the first pass's 19 findings it
found 15 closed and 4 closed in design but wrong in a sentence revision 3 added; those four are
the first four rows below. It agreed with D8, D15, D19 and D20, confirmed the outbox roles of D16,
the surface list of §4.6 as complete, the MaC column lists, the mail template against `render`, and
that §8 is enough for a mock. Every finding was checked against the code on `main`.

| Finding | Disposition |
| --- | --- |
| R3-P1-1 — the rise step locked on every evaluation, and §4.3 described a `REPEATABLE READ` lock wait as "reads the raised row" or "is retried" — it fails with 40001 and the leader does not retry | **Confirmed** (`beginAlertSnapshot`; the scheduler's `service_alert_eval_failed` path has no retry). Step 0 reads without a lock; the concurrency paragraph is rewritten; the clock test is split in three. §4.3, I12, §7 |
| R3-P1-2 — the reconciler dropping its impact fails `Incident.Validate()` before the transaction opens; the system door's contract for other callers is undefined | **Confirmed** (`createIncident` validates first). §4.2: the validation requirement, and the system door narrowed to monitor auto-incidents with an empty impact; fixtures move |
| R3-P1-3 — §4.6 said `acknowledge()` merges and keeps extra fields; it replaces, keeping only the impact links | **Confirmed** (`IncidentDetailView.vue`). The sentence is replaced by the required change. §4.6, §7 |
| R3-P2-1 — the project audit target drops a token caller's label; no `FOR UPDATE`, no `updated_at` | **Confirmed.** §4.4: the write, `incidentAuditTarget`, `updated_at` |
| R3-P2-2 — the basis read is not project-scoped at the store; the two null states are not encoded; the CHECK is open | **Accepted.** §4.6 (signature and the three encodings), §5 (closed CHECK) |
| R3-P3-1 — I13's "no surface" covers mail and feeds, which print the raw word and must | **Fixed.** I13 says SPA; §4.6 says why mail and feeds stay |
| R3-P3-2 — the never-raised test is green before the code exists | **Fixed.** §7: a service incident rises in the same test |
