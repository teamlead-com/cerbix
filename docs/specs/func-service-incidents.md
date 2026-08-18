# func-service-incidents — a Service can own an incident (FR-022 / NFR-017)

> **STATUS: SPEC, decisions resolved, awaiting adversarial review and a UI mock.** The owner commissioned
> FR-022 and delegated its six decisions to the recommendations of the design-gate input; each is recorded
> below as **DECIDED (delegated)** with the reasoning that made it the recommendation, so a later reader
> can see it was a judgement and whose. Nothing is implementable until this file has been reviewed and
> the mock approved — the gate that governed FR-021 phases 3–5 and iter-0155's two panels.
>
> Invariants here are numbered **within this document**. FR-021's 1–91 are a separate space; a bare
> "invariant 12" in cerbix means FR-021's unless it names FR-022.

## 1. Why this is not a small feature

Phase 5 gave a Service the ability to PAGE (D-0168) and deliberately stopped there: an alert notifies and
opens no incident. Everything an incident touches was built around a MONITOR being the anchor —
`incidents.monitor_id`, the auto-incident lifecycle, the escalation ladder's progress, the `⚡ Context:`
and `⏸ Suppressed:` system notes, postmortems — and §14's correlation annotates per-monitor incidents with
links computed over the SERVICE graph. The question is not "add a nullable column". It is **what an
incident is an incident OF, once two kinds of thing can own one**, when every read path answers that today
by assuming there is only one kind.

## 2. Requirements

- **FR-022**: A Service can own an incident. An operator can see, acknowledge, annotate and resolve an
  incident whose subject is a service, with its impact links and its postmortem, through the same surfaces
  a monitor incident uses. A service that PAGES may open one automatically, under the same armed-coverage
  and confirmation rules that decide whether it pages at all.
- **NFR-017**: Adding the second anchor changes no answer the product already gives about the first. A
  monitor incident's lifecycle, escalation progress, notes, status-page rendering and postmortem are
  byte-identical before and after FR-022, proven by regression rather than by reading — the standard §17
  set for FR-021 phase 4's three intentional public-output changes.

## 3. The six decisions, resolved

**D1 — a service alert auto-opens an incident, on a LIVE onset only. DECIDED (delegated).**
Auto-opening is the feature the deferral was about; an operator-only version would leave the pager and the
timeline disagreeing. It rides the machinery that already exists: `confirm_evaluations`, ARMED coverage per
signal, and the DB-clock lease. It does NOT open on a burn breach — a budget signal is not an outage, and
§16.4's own text calls the burn pair a reporting signal that trails the watermark.

**D2 — ONE table, with an exclusive anchor. DECIDED (delegated).**
`incidents` gains `service_id` under a composite tenant-safe FK `(service_id, project_id)`, and a CHECK
that at most ONE anchor is set — `at most`, not exactly one, because a manual project-level incident
carries neither today and that must keep working. One table keeps one lifecycle, one audit trail, one
postmortem shape and one renderer. The cost is that every existing read must learn the discriminator, and
phase 4 already paid for the alternative: reading `monitor_id != ""` as an implicit discriminator is what
made a converted component publish its old monitor's status (D-0167).

**D3 — a service incident does NOTHING to its members' incidents. DECIDED (delegated).**
Suppression here would silently change what a monitor incident IS, which is exactly what NFR-017 forbids.
Two timelines, both true: the member's incident is about the member, the service's is about the promise.
Revisit only with an owner decision of its own.

**D4 — the incident reaches the status page; its impact links do NOT. DECIDED (delegated).**
An incident is already public for monitors, so hiding a service's would make the public page less true
than the private one. Impact links stay non-public: §15.0 kept internal topology out of the public
projection and phase 4 DECLINED to opt in (FR-021 invariant 59). A raw unauthenticated JSON regression
pins both halves.

**D5 — the escalation ladder does NOT ride a service incident in this requirement. DECIDED (delegated).**
§16.9 defers escalation policies for services and names what they need first: a durable non-incident
occurrence with started/resolved/ack/progress/repeat state. A service incident is close to that shape,
which is precisely why bundling them would smuggle a subsystem in through a feature. Stated as a non-goal,
re-evaluated after this lands.

**D6 — the postmortem snapshots its member set at open time. DECIDED (delegated).**
A postmortem is read after the world moved, and a member may be deleted by then. Phase 5 solved the same
problem for recipients with an IMMUTABLE snapshot on the episode; the same device here, for the same
reason: a postmortem that renders "3 members" and cannot name them is a document nobody trusts.

## 4. What this changes about FR-021 — stated, not discovered later

**FR-022 SUPERSEDES FR-021 invariant 86** ("no service alert opens, resolves or annotates an INCIDENT,
with the single explicit exception of the §16.1 suppression note on a MONITOR's incident"). That invariant
is true today and is held by `TestAServiceAlertNeverTouchesTheIncidentTables`, written in iter-0155.

When FR-022 lands, three things must move IN THE SAME CHANGE, or this repository repeats invariant 47's
history — an invariant asserting the opposite of what the code does, left standing for a phase:

1. FR-021 §16.8's invariant 86 gains a SUPERSEDED note pointing at FR-022, keeping its number because
   other documents cite it;
2. the discharge row for 86 in `docs/traceability.md` moves to the test that holds the NEW rule (a service
   alert opens an incident only under armed coverage, and only on a live onset);
3. `TestAServiceAlertNeverTouchesTheIncidentTables` is REWRITTEN rather than deleted — the way phase 5
   rewrote the phase-2 rejection test when it inverted (`TestServiceScopedBurnTargetIsSupported`).

The habit that makes this cheap: when a phase lands, grep the spec for its own number before closing it
(iter-0154 §2.1).

## 5. Schema

```
ALTER TABLE incidents
    ADD COLUMN service_id uuid,
    ADD CONSTRAINT incidents_service_fk
        FOREIGN KEY (service_id, project_id) REFERENCES services (id, project_id) ON DELETE SET NULL,
    ADD CONSTRAINT incidents_one_anchor_chk
        CHECK ((monitor_id IS NOT NULL)::int + (service_id IS NOT NULL)::int <= 1);

-- one open auto-incident per service, mirroring 00049's per-monitor index
CREATE UNIQUE INDEX incidents_service_open_auto_idx ON incidents (service_id)
    WHERE source = 'auto' AND status <> 'resolved' AND service_id IS NOT NULL;

CREATE TABLE incident_member_snapshots (   -- D6
    incident_id uuid PRIMARY KEY REFERENCES incidents (id) ON DELETE CASCADE,
    project_id  uuid NOT NULL,
    members     jsonb NOT NULL,            -- [{monitor_id, name, roles[]}] as of the open instant
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT incident_member_snapshots_tenant_fk
        FOREIGN KEY (incident_id, project_id) REFERENCES incidents (id, project_id) ON DELETE CASCADE
);
```

Three notes on the shapes. The FK is **composite** because a same-project guarantee that lives in
application code is one bug from crossing tenants (FR-021 invariant 48 exists for this). `ON DELETE SET
NULL` on `(service_id, project_id)` needs PG15's column-list form so it clears the anchor and not the
tenant key — the exact trap iter-0125 hit with a bare `SET NULL` on a NOT NULL `project_id`. And the
snapshot's own tenant FK is composite for the same reason, against the incident's `(id, project_id)`
unique target that already exists.

## 6. Acceptance invariants (FR-022)

1. an incident has AT MOST ONE anchor, enforced by CHECK: a row naming both a monitor and a service is
   unrepresentable, and a manual project-level incident with neither keeps working;
2. a service anchor cannot cross tenants — proven by a DIRECT SQL insert, not through the store;
3. deleting a service CLEARS the anchor and not the tenant key; the incident survives as a project-level
   record with its timeline intact;
4. at most one OPEN auto-incident per service, enforced by a partial unique index, so a flapping service
   cannot accumulate incidents;
5. a service alert opens an incident ONLY on a live onset, only while that signal's coverage is ARMED, and
   only after `confirm_evaluations` — the same three gates that decide whether it pages;
6. a burn breach opens NO incident, ever;
7. the open and the alert are ONE transaction: an incident without its announcement, or an announcement
   without its incident, is unrepresentable;
8. a monitor incident's lifecycle, escalation progress, notes, status-page rendering and postmortem are
   byte-identical to before FR-022 (NFR-017), proven by regression over each;
9. `probable_root` and `affected` never name the incident's own subject: a self-link is skipped, and the
   canonical path algorithm is unchanged for every other pair;
10. the §15.0 component precedence table is untouched: a service incident changes no component STATUS, it
    is rendered as an incident. The table's totality (FR-021 invariants 66–68) stands;
11. the public projection carries a service incident and NO impact links, proven by a raw unauthenticated
    JSON regression over a page whose service has correlated incidents;
12. the `⏸ Suppressed:` and `⚡ Context:` notes keep exactly one home each — the MONITOR's incident — so a
    single outage is never annotated twice;
13. a postmortem names the members the service had AT OPEN TIME, and keeps naming them after a member is
    deleted;
14. every write is audited with its actor and tenant, in the mutating transaction.

## 7. Required test matrix (written before the code)

a service DOWN under armed coverage after `confirm_evaluations`: ONE incident, ONE announcement, in one
transaction · the same service DOWN while coverage is DISARMED: no incident, the member pages ·
a burn breach: no incident · a flapping service: one open auto-incident, not four ·
a member's incident opened while the service's is open: both exist, unchanged (NFR-017) ·
resolve a service incident: the member's stays open · delete the service: the anchor clears, the tenant key
does not, the timeline stands · a cross-tenant anchor by DIRECT SQL: refused ·
correlation over an incident whose subject is a service: no self-link, other links byte-identical to the
pre-FR-022 relation · the public JSON: incident present, impact links absent ·
the `⏸` note after a suppressed member delivery: written once, on the monitor's incident ·
a postmortem read after a member is deleted: still names it ·
every monitor-incident regression in the suite, unchanged and green.

## 8. Non-goals of FR-022

Escalation policies for services (§16.9, needs its own occurrence subsystem — D5); retroactive alerting;
cross-project delegation; per-member severity inside a service page; suppression beyond the three topics
FR-021 named; and any change to what a MONITOR incident means (NFR-017 forbids it).
