# func-service-escalation — a Service escalates its own outage (FR-023 / NFR-018)

> **STATUS: DELIVERED** in [iter-0157](../iterations/iter-0157.md), acceptance rows AC-0157-* in
> [status.md](../status.md). The owner commissioned this §16.9 item on 2026-08-19 and the review gate it
> announced was met; the "nothing here is implementable" line outlived the delivery and is corrected
> rather than deleted. §8's non-goals still bind — one of them, retroactive escalation, was found
> contradicted by the implementation and fixed in [iter-0161](../iterations/iter-0161.md) §13.
>
> Invariants here are numbered **within this document**. FR-021's 1–91 and FR-022's 1–16 are separate
> spaces; a bare "invariant 9" in cerbix means FR-021's unless it names its requirement.

## 1. Why this is possible now and was not before

FR-021 phase 5 deferred escalation policies for services under **owner decision 5**, and §16.9 states the
blocker precisely: *"A later phase needs a durable non-incident occurrence with started/resolved/ack/progress/repeat
state before 'the ladder' means anything for a service."* The code says the same thing at the point where it
refuses to act — `resolveServiceRecipientsTx` in `internal/store/servicealerteval.go` carries the comment
*"A ladder is defined relative to an incident start with acknowledgement and progress state, and a service
alert opens no incident — so 'the ladder applies unchanged' was not implementable."*

**FR-022 removed that blocker, literally in the terms it was written in.** A service alert now opens an
incident with `started_at`, `acknowledged_at`, `escalation_step` and `last_escalated_at` — the same row, the
same columns and the same acknowledge endpoint the monitor ladder has run on since D-0100. What is left is
not a missing subsystem; it is a candidate query shaped around one anchor and a payload that can only name a
monitor.

That is also why this requirement is small and why it must still be specified: everything dangerous about it
is in the PREDICATES — when a step is allowed to advance, and what happens when the answer cannot be
determined.

## 2. Requirements

- **FR-023**: A Service with an escalation policy escalates its own auto-opened incident: steps fire on the
  policy's schedule, progress is durable, acknowledgement stops it, and every step names the SERVICE. What
  pages is the service's own ladder — never a second copy of a member's.
- **NFR-018**: A MONITOR's ladder is byte-identical before and after FR-023 — its candidates, its steps, its
  progress, its suppression at delivery time and its payload — proven by regression rather than by reading.

## 3. The decisions, resolved

**D1 — the policy is `services.escalation_policy_id`, the column that already exists and is deliberately
unread. DECIDED.**
Phase 5 added it (the file provider writes it, the API accepts it) and the evaluator refuses to consult it,
with a comment saying why. FR-023 gives it the meaning it was reserved for. A service without a policy
escalates nothing, exactly as a monitor without one does.

**D2 — the ladder runs off the SERVICE INCIDENT, not off the alert episode. DECIDED.**
The incident carries the four fields D5 said were missing, and an episode does not. This also settles who may
escalate without a new rule: `source = 'auto'`, the same predicate the monitor ladder uses, so an incident a
human opened is never escalated by a machine — consistent with FR-022's D1b, where `source = 'auto'` is what
keeps the machine out of a person's conclusions.

**D3 — "still pageable" replaces "the monitor is down", and it FAILS CLOSED. DECIDED, and this is the one
decision worth arguing about.**
The monitor ladder advances only while `m.enabled AND m.status = 'down'`. The service analogue is: the
service still OWNS paging, its live verdict is still firing, and that verdict is FRESH (the §16 DB-clock
lease has not expired). If any of those cannot be established — no state row, an expired lease, a read error —
the ladder does **not** advance.

This is the OPPOSITE polarity from delegation's fail-open, and the asymmetry is the point. At delivery time,
ambiguity means the member pages: erring toward *a page existing*. In the ladder, ambiguity would mean
paging MORE people about a state nobody can currently confirm is still happening: erring toward *a page
multiplying*. Both rules choose the same thing — a page exists, and it does not multiply on a guess.

**D4 — acknowledgement stops it, unchanged. DECIDED.**
The same field, the same endpoint, the same meaning. This is free precisely because D2 chose the incident.

**D5 — the SERVICE GRAPH does NOT pause the ladder. DECIDED, against the obvious symmetry.**
The monitor ladder pauses while a transitive parent is down (D-0100), and mirroring that over the §14 service
graph looks like the consistent choice. It is not: §14 states its own position — the impact graph
*"annotates and links; never suppresses, merges or hides"* (FR-021 §13, phase 3 scope). A graph built and
sold as advisory must not quietly become a suppression mechanism because a second feature found it
convenient. If upstream-aware pausing is wanted, it changes what §14 IS, and that is its own requirement.

**D6 — the step alert names the SERVICE through a new field, and the topic is NOT fenced. DECIDED, after
the first answer was wrong.**
`EscalationStepAlert` today carries `monitor_id` / `monitor_name`, and its `Message()` renders
`"%s is DOWN (escalation step %d)"`. A service step must name the service, and an OLD worker in a
mixed-version fleet must not render `" is DOWN"` for a payload it does not understand.

My first answer was to fence `TopicEscalationStep`, and `domain.FencedTopics` says in its own doc comment why
that is wrong: *"the pre-fence topics stay legacy forever because every deployed owner already dispatches
them."* Fencing a pre-fence topic inverts the protection — a currently-deployed worker claims
`status = 'pending'` and would stop seeing escalation steps entirely during a rolling upgrade. The fence
exists to stop an old worker mishandling a NEW topic; using it on an OLD one silences a working delivery
path.

So the payload evolves compatibly instead. It gains `service_id` and `subject_name`; `Message()` prefers
`subject_name`; and `monitor_name` stays populated **with the subject's display name** — for a service step
that is the service's name — documented explicitly as a legacy render field kept so a pre-FR-023 worker
renders a correct sentence rather than a blank one. What an old worker does with a service step is therefore
stated rather than left to be discovered: it renders correctly, it attempts one monitor lookup that finds
nothing and logs a single fail-open, and it DELIVERS — the safe direction, and the same direction §16.6b
already chose for an unresolvable delegation.

**D7 — delivery-time service delegation does NOT apply to a service's own step. DECIDED.**
The worker suppresses a MONITOR's step while a service owns its paging (§16.6b). A SERVICE's step IS the
owner's page; suppressing it would mute the only alert anyone gets. The service branch therefore skips the
delegation path entirely — and skips the monitor lookup with it, so a service step never calls `GetMonitor("")`
and never logs a fail-open it did not have.

**D8 — no renotify knob for services in this requirement. DECIDED.**
Monitors carry `renotify_seconds`; services do not, and adding one is a separate decision about a separate
control surface. Stated as a non-goal rather than left as an omission a reader has to notice.

> **SUPERSEDED 2026-08-27 by D-0185**, which adds `services.renotify_seconds` (0 = off, otherwise
> 60..86400) and makes the repeat work. The non-goal is kept at its number because the correction
> below is the useful part of it: the reasoning about WHY the original justification was false
> outlives the decision it justified.
>
> **Corrected 2026-08-26 (D-0181).** This decision originally added "the policy's own steps (including its
> repeat) are the repeat mechanism", and that sentence was false. The policy's repeat runs ON the monitor's
> renotify interval — `RepeatLast && renotifySeconds > 0` — so with no interval a service ladder runs to its
> LAST STEP AND STOPS, whatever `repeat_last` says. The non-goal is unchanged and correct; what was wrong was
> the claim that something else covered it. A policy shared by monitors and services therefore behaves
> differently on each, which the escalation form now says beside the toggle, and
> `TestAServiceLadderDoesNotRepeatItsLastStep` holds it.

## 4. What must move WITH the implementation, not after it

FR-023 falsifies statements that are true today and asserted in THREE places. All three move in the change
that makes them false, the way FR-022 handled FR-021 invariant 86:

1. **§16.9's bullet** "escalation POLICIES for services — deferred by owner decision 5" gains a SUPERSEDED
   note naming FR-023 and stating what made it possible (FR-022's incident), keeping the bullet so the record
   of the deferral survives its end;
2. **the comment in `resolveServiceRecipientsTx`** that says the ladder "was not implementable" must be
   rewritten in the same change — it will otherwise remain as an explanation of a refusal the code no longer
   makes;
3. `docs/overview.md`'s alerting-ownership paragraph names what a service can and cannot do; it gains the
   ladder.

## 5. Schema

No new table. Two changes, both additive:

```
-- nothing to ALTER: services.escalation_policy_id already exists (00069) and is already
-- tenant-checked on write. FR-023 only starts READING it.

-- the payload gains service fields (domain.EscalationStepAlert): service_id, subject_name.
-- monitor_name stays, populated with the SUBJECT's name, as a documented legacy render field.
-- TopicEscalationStep is NOT fenced — see D6; fencing a pre-fence topic would strand a
-- currently-deployed worker that claims status = 'pending'.
```

The absence of a migration is itself a claim to verify: the ladder's progress lives on `incidents`, which
FR-022 already taught to hold a service anchor, and the policy lives on `services`, which has held the column
since phase 5.

## 6. Acceptance invariants (FR-023)

1. only an AUTO-opened service incident escalates; one a human opened is never escalated by a machine;
2. a service with no `escalation_policy_id` escalates nothing, and its incident is untouched;
3. acknowledging the incident stops the ladder — the same field and endpoint as a monitor's;
4. resolving the incident stops the ladder;
5. a step advances ONLY while the service still owns paging AND its live verdict is still firing AND that
   verdict is fresh; a missing state row, an expired lease or a read error does NOT advance it (fail closed);
6. progress is written on the incident (`escalation_step`, `last_escalated_at`) in the transaction that
   enqueues the step, so at-least-once redelivery cannot double-advance it;
7. the SERVICE GRAPH does not pause the ladder (D5) — an upstream service with an open incident changes
   nothing about a downstream service's steps;
8. a burn breach never escalates — a budget signal is not an outage, the same rule as FR-022 invariant 6;
9. the step alert names the SERVICE and never renders a blank subject — including for a PRE-FR-023 worker,
   which is pinned by rendering the payload through the legacy field alone;
10. delivery-time service delegation does not apply to a service's own step, and no monitor lookup is
    attempted for it;
11. instance-wide silence mutes a service step exactly as it mutes a monitor step;
12. the policy is tenant-scoped: a policy id belonging to another project cannot page, proven by DIRECT SQL
    rather than through the API;
13. deleting the service mid-outage clears the anchor (FR-022 invariant 3) and the ladder stops — an incident
    with no service never escalates on a NULL anchor;
14. a MONITOR's ladder is byte-identical to before FR-023 — candidates, steps, progress, delivery-time
    suppression and payload (NFR-018), proven by regression over each;
15. **ADDED during implementation (iter-0157 §2.7), because D1 was imprecise.** D1 said the API "accepts"
    the policy; it accepts it only at CREATE time, and there is no service update endpoint at all — so an
    existing service could not be given one. A change to this field is a change to WHO IS WOKEN, so it has
    its own route, its own transaction and an audit row naming what moved, written INSIDE that transaction;
    a no-op writes nothing at all; a policy from another project is refused BY NAME rather than as a
    constraint violation; and a file-managed service refuses, because its fields are the file's state;
16. **ADDED during implementation (iter-0157 §2.10).** An operator can attach AND clear the policy from the
    SPA — clearing is a choice, not the absence of one — and that control is independent of the paging
    declaration: separate write, separate save, separate error, and it does not disappear when the
    declaration cannot be read.

## 7. Required test matrix (written before the code)

a service DOWN with a policy: step 1 fires at its offset, naming the SERVICE ·
the same service after an ACK: no further steps · after a RESOLVE: no further steps ·
`owns_paging` turned off mid-outage: the ladder stops · the live verdict goes stale (lease expired): the
ladder stops, and RESUMES when a fresh verdict says it is still firing · a store error reading that verdict:
no step, no progress written · a service with no policy: nothing, ever ·
a human-opened service incident: nothing, ever · a burn breach: nothing, ever ·
an upstream service with an open incident: the downstream ladder is UNAFFECTED (D5) ·
a redelivered step: progress advances once · a service step at delivery: no delegation lookup, no
`GetMonitor("")`, and the page is delivered · the SAME payload rendered through the legacy field alone
(what a pre-FR-023 worker does): a correct sentence, never a blank subject · instance-wide silence: the step is muted and consumed ·
a policy from another project by DIRECT SQL: refused · the service deleted mid-outage: the ladder stops ·
attach and clear on an EXISTING service, each audited with what moved, and every refusal writing nothing ·
the same round-trip through the SPA control, including `— none —` ·
every monitor-ladder regression in the suite, unchanged and green.

## 8. Non-goals of FR-023

A renotify knob for services (D8); per-step severity; escalation for BURN alerts; retroactive escalation of
incidents opened before a policy was attached; upstream-aware pausing through the service graph (D5 — it
changes what §14 is, and belongs to a requirement about the graph); and any change to what a MONITOR's ladder
means (NFR-018 forbids it).
