# func-service-incidents — a Service can own an incident (FR-022 / NFR-017)

> **STATUS: DESIGN-GATE INPUT, not an approved design.** Nothing here is implementable yet. This file
> exists because D-0169 closed FR-021 with §16.9 as an explicit non-goal and said each deferred item
> opens its OWN requirement when commissioned — so this is that requirement, written to be attacked
> before a line of code, per the practice that found four P0s in FR-020 and two design rounds' worth in
> FR-021 §16 before either shipped.
>
> The owner asked for Service incidents first among the six §16.9 items. What follows is the scope I
> believe is right, the decisions only the owner can make, and — most importantly — the three places
> where I think this feature can quietly break something that already works.

## 1. Why this is not a small feature

Phase 5 gave a Service the ability to PAGE (D-0168) and deliberately stopped there: an alert notifies
and opens no incident. Every part of the product that an incident touches was built around a MONITOR
being the anchor:

- `incidents.monitor_id` is the anchor for the auto-incident lifecycle, the escalation ladder's
  progress, the `⚡ Context:` and `⏸ Suppressed:` system notes, and postmortems;
- §14's correlation annotates incidents with `probable_root` / `affected` links computed over the
  service graph, and its canonical paths are between SERVICES while the incidents themselves are
  per monitor;
- the status page renders incidents for components, and a component may now be backed by a service
  (phase 4, D-0167) — so a service incident would have two candidate renderings and one of them is
  already shipped and public.

So the question is not "add a nullable `service_id` to `incidents`". It is: **what is an incident an
incident OF, once two kinds of thing can own one** — and every read path in the product answers that
question today by assuming there is only one kind.

## 2. Requirements, as proposed

- **FR-022**: A Service can own an incident. An operator can see, acknowledge, annotate and resolve an
  incident whose subject is a service, with its impact links and its postmortem, through the same
  surfaces a monitor incident uses.
- **NFR-017**: Adding the second anchor changes no answer the product already gives about the first. A
  monitor incident's lifecycle, its escalation progress, its notes, its status-page rendering and its
  postmortem are byte-identical before and after this feature, proven by regression rather than by
  reading — the same standard §17 set for FR-021 phase 4's three intentional public-output changes.

## 3. Decisions only the owner can make

Each is a fork where I can implement either side, and picking wrongly costs a migration or a public
output change later.

1. **Does a service alert OPEN an incident automatically, or does an operator?** Phase 5's answer was
   "notifies, opens nothing" (owner decision 3). Auto-opening is the feature people expect and the
   reason the deferral existed; it also means a flapping service can open incidents, so it needs the
   same confirmation and suppression discipline the pager has — which exists and is armed per signal.
   *My recommendation:* auto-open on a LIVE onset only, reusing `confirm_evaluations` and the ARMED
   coverage rule, never on a burn breach (a budget signal is not an outage).
2. **Is a service incident the same row as a monitor incident (`incidents` gains an exclusive second
   anchor), or its own table?** One table keeps one lifecycle, one audit trail, one postmortem shape,
   one status-page renderer — and forces every existing query to learn a discriminator, exactly as
   phase 4's `components.source` did. A second table keeps every existing query untouched and doubles
   the lifecycle, the notes and the correlation join.
   *My recommendation:* ONE table with an exclusive-anchor CHECK, and the discriminator read
   explicitly everywhere — because phase 4 already proved the alternative (`monitor_id != ""` as an
   implicit discriminator) publishes the wrong thing on a converted component.
3. **What does a service incident do to its MEMBERS' incidents?** A service is down because members
   are. Three defensible answers: nothing (two independent timelines), suppression (members' incidents
   are not opened while the service's is open — the delegation rule extended from paging to incidents),
   or absorption (member incidents become updates on the service's).
   *My recommendation:* nothing in the first phase. Suppression here would silently change what a
   monitor incident IS, and NFR-017 exists to forbid exactly that.
4. **Does a service incident appear on the status page?** Phase 4 DECLINED to project impact links, and
   §15.0 keeps internal topology non-public. An incident is different — it is already public for
   monitors — but a service incident naming member services would leak the graph the same decision
   refused to publish.
   *My recommendation:* yes for the incident itself on a service-backed component, and NO for its
   impact links, with a regression over the raw unauthenticated JSON.
5. **Escalation.** §16.9 also defers escalation policies for services, and that item needs a durable
   non-incident occurrence first. If a service incident exists, the ladder could ride it — which makes
   Service incidents the cheaper path to service escalation and changes the order of the remaining
   five items.
   *My recommendation:* out of scope here, stated as a non-goal, and re-evaluated once this lands.
6. **Retention and postmortems.** A service incident's postmortem references members that may be
   deleted later. Phase 4's answer for a similar case was an immutable snapshot (the alert's recipient
   snapshot) or a retained-but-dormant row.
   *My recommendation:* snapshot the member set at open time, exactly as an alert episode snapshots
   recipients, because the postmortem is read after the world moved.

## 4. Where I think this breaks something that works

Named up front, because these are the three places a review should attack first.

- **`⏸ Suppressed:` and `⚡ Context:` notes are idempotency markers keyed by prefix** on
  `incident_updates`. A second anchor doubles the space of "which incident gets the note", and the
  phase-5 suppression note is written on the MONITOR's incident by the outbox worker. If a service
  incident exists, that note has two plausible homes and writing it to both would double-annotate an
  outage.
- **§14 correlation is per incident and computes over services.** With service incidents, an incident's
  correlation could name the service that IS the incident's subject — a self-link — and the canonical
  path algorithm has no case for it. The witness bound and the redelivery byte-equivalence tests are
  the ones to extend first.
- **The status-page component precedence table (§15.0) is TOTAL today.** Adding a service incident to a
  service-backed component introduces a state the table has no row for, and that table's totality is
  what invariants 66–68 rest on. Any change to it is an intentional public-output change and needs the
  §17 treatment: a regression pinning the OLD behaviour as removed.

## 5. What this requirement will need before code

1. This file reviewed adversarially and its six decisions answered by the owner.
2. Acceptance invariants, numbered, in the style of FR-021 §19 — and mapped to tests in
   `docs/traceability.md` as they land, because iter-0155 showed what an unmapped invariant list is
   worth (nothing: 36 of 91 numbers appeared in no document at all).
3. A required test matrix written BEFORE the code, per §16.10's precedent — which iter-0155 also showed
   is only worth writing if something maps it (`make docs-check` now enforces that).
4. An approved UI mock, because every surface here has one: the incident detail, the status page and
   the service page.

## 6. Non-goals of FR-022, stated now

Escalation policies for services; retroactive alerting; cross-project delegation; per-member severity
inside a service page; suppression of anything beyond the three topics FR-021 named. All five remain
§16.9 items with their own owner decisions, and none of them becomes cheaper by being bundled here.
