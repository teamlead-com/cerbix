# func-phone-alerts — an alert that reaches a person: SMS and voice calls (FR-041 / NFR-034)

> **STATUS: revision 5 — DRAFT, awaiting the owner's approval.** The confirming review pass
> (2026-11-17, at `23184bf`) is APPROVED with no findings (§14). Revision 5 (2026-11-17) answers the third external
> review, of revision 4 (CHANGES REQUIRED, P0 0 / P1 3 / P2 4 / P3 1 — dispositions in §14). No
> decision changed; the
> corrections are local: D32 no longer reads a payload field that is never set and closes the race
> with a lock; a phone channel kept a route by its fallback is described the same way everywhere;
> a collision of `incidents.ref` is left to the retries that already exist (§8).
>
> Revision 4 (2026-11-17) answered the second external review, of revision 3 (CHANGES REQUIRED,
> P0 0 / P1 10 / P2 6 / P3 2). No owner decision changed. What changed in substance then: whether telephony is in effect for a project
> is now part of what makes a phone channel a route, and telephony settings live in the database
> only (D31, D36); the cost of D31 is restated — it can be a whole outage, not a first alert; a
> monitor's delivery is attached to its incident when the incident opens, and a recovery no longer
> assumes the reconciler ran first (D32); a `sending` row met by an early retry, the storm guard's
> count and the `repeat_last` formula are closed (D33, §6.2, §5.3); the keyed hash is specified
> against `internal/secret` as it is (D35).
>
> Revision 3 (2026-11-16) answered the first external review of revision 2 (CHANGES REQUIRED,
> P0 4 / P1 14 / P2 6 / P3 2) and recorded the owner's decisions of 2026-11-16 (D13, D17 and
> D22–D30). What changed in substance then:
> phone events are no longer written to the incident timeline, which is public (D22, §5.5); a phone
> channel that cannot deliver no longer counts as a route, and a delivery nobody received neither
> credits coverage nor loops (D31, §4.6); a monitor's incident is no longer raced (D32); a delivery
> is reserved before the provider is called (D33); the Twilio protocol, the inbound responses and
> their correlation are specified (§5.1); snooze has a model that survives replacement and
> `repeat_last` (§5.3); quotas are atomic and the storm guard moved to FR-041a (D26, §6); numbers
> are stored as ciphertext plus a keyed hash and shown masked to everyone (D35); the schema, the
> route table and the test matrix are complete (§8, §9, §11).
>
> Revision 1 went through a builder check against the code (2026-11-13): seven planning questions
> and five code contradictions, folded into revision 2 as D15–D21, §4.5, §5.1, §5.3, §7 and
> P15–P18. Written 2026-11-13 from the owner's answers to twelve design questions (recorded in §3
> as OWNER decisions). Decisions the author took without an owner answer are marked **AUTHOR'S
> CALL** and are open to change at review. Nothing here is implementable until the owner approves a
> revision, and the SPA surfaces in §9 need an approved mock before any frontend code (CLAUDE.md,
> spec-before-code).
>
> Invariants are numbered **within this document** (P1, P2, …), so they never collide with FR-021's
> or FR-023's numbering.

## 1. Why

cerbix can decide precisely WHEN a person must act — a confirmed DOWN, a burning budget, an escalation
step on a service's own incident (FR-023) — and then delivers that decision only to email, a webhook,
Slack or Telegram (`internal/domain/notification.go`, four `ChannelType`s). None of those wakes anyone
at 03:00. The escalation ladder is therefore only as loud as the quietest channel on its last rung:
the product's most carefully specified paging logic ends in a chat message.

What is missing is the **last mile**: a channel that rings a phone, and a way for the person who
answered to stop the ladder without opening a laptop.

## 2. Requirements

- **FR-041**: A notification can be delivered as an **SMS** or a **voice call** to a **verified** phone
  number, through an operator-configured telephony provider; a person reached that way can
  **acknowledge** or **snooze** the incident from the phone; phone delivery is **bounded** in cost and
  volume and never drops an alert silently.
  - **FR-041a** — the two channel types, verified numbers, the provider abstraction, instance
    settings, the delivery record, the incident short reference, and the **storm guard** (D26).
  - **FR-041b** — inbound endpoints; acknowledgement and snooze by SMS reply and by keypress; voice
    retry on no answer.
  - **FR-041c** — monthly quotas, fallback channel, per-person quiet hours, per-project provider
    override.
- **NFR-034**: Every existing channel, every existing suppression rule and every existing escalation
  ladder behaves **identically** before and after FR-041 for a project that has no phone channel,
  proven by regression (P14 states exactly what is compared); telephony credentials and phone
  numbers are secrets-grade data (never in an API response in full, never in a log line, never in a
  dispatch carrier); the inbound endpoints accept **only** provider-signed requests.

Each lettered part is its own iteration (OWNER, Q12), in the order a → b → c, all three **after**
FR-042 ([`func-incident-impact.md`](func-incident-impact.md)) — OWNER, 2026-11-13, option A. Each
ships alone and is useful alone: after (a) the phone rings, carries a link, and cannot be made to
ring sixty times an hour; after (b) it can stop the ladder; after (c) it has a budget, a fallback
and a night. FR-041a is large — migrations, two providers, settings, verification, the short
reference, the delivery path and five SPA surfaces (§9, items 1–5) — and the builder may split it
into a backend iteration and an SPA iteration.

## 3. The decisions

### Owner decisions (answers of 2026-11-13)

**D1 — the audience is any operator of a cerbix instance, not a single hosted deployment. OWNER (Q1).**
Each instance connects its own provider account. cerbix ships no telephony account, no shared sender
number and no billing.

**D2 — phone is a CHANNEL TYPE, usable everywhere a channel is. OWNER (Q2).**
`sms` and `voice` are new `ChannelType`s. A monitor can link them, a service alert can route to them,
an escalation step can target them, an on-call schedule can rotate them. The routing, suppression
and escalation code delivers to a channel, as today; what it must learn is narrow and listed in D31
(a phone channel that cannot deliver is not a route).

**D3 — one provider interface; Twilio first, plus a generic HTTP SMS gateway. OWNER (Q3).**
`internal/telephony` defines a `Provider` with `SendSMS`, `PlaceCall`, and (FR-041b) `VerifyInbound`.
Two implementations ship:
- `twilio` — SMS and voice (TTS via TwiML `<Say>`), inbound replies, keypress, status callbacks. The
  exact protocol is §5.1.
- `generic_http` — **SMS only**: an operator-defined request (method `POST` or `PUT`, URL, headers,
  a body with the placeholders `{{to}}`, `{{from}}`, `{{text}}`) for regional SMS gateways. It
  cannot place calls and cannot receive replies; a `voice` channel cannot be created while the
  **effective** provider of its project is `generic_http` (400 with that reason), and FR-041b reply
  features report themselves unavailable rather than broken. Placeholder encoding is fixed by the
  body's content type (§4.4) — never raw concatenation.

Further providers are added by implementing the interface; none is promised here.

**D4 — no country is privileged. OWNER (Q4, no constraint given).** Numbers are stored and validated
as E.164. Whether a destination is reachable is the provider's answer — cerbix keeps no country
allow-list of its own. What is kept of that answer is its **code**, not its text (D34).

**D5 — acknowledgement by reply and by keypress is in scope, and off until a public URL exists.
OWNER (Q5).** Every phone message carries a link to the incident when one exists. Reply-to-ack
(`ACK <code>`) and press-1-to-ack additionally need the provider to reach cerbix, so they activate
only when the telephony settings hold a `public_base_url` AND the provider supports inbound. When
they are inactive, the message **does not mention them** (P9: a message never instructs an action
that cannot work).

**D6 — snooze is in scope. OWNER (Q6).** `SNOOZE <code> <minutes>` (SMS) or keypress 2 (voice,
fixed 30 minutes) defers the incident's escalation ladder; it does not acknowledge. §5.3 gives the exact
semantics.

**D7 — the number belongs to a USER, verified by code; a shared number lives on the channel and is
verified too. OWNER (Q7).** See D12 for how this meets the on-call model.

**D8 — per-person quiet hours. OWNER (Q8).** In FR-041c. §6.4.

**D9 — voice speaks a TTS message; one retry on no answer. OWNER (Q9).** Language is an instance
setting (default `en-US`) with a per-user override. The retry is placed 120 s after a `no-answer`,
`busy` or `failed` status callback, once per delivery, and is independent of the next ladder step.
The retry needs status callbacks, so it is FR-041b, not FR-041a.

**D10 — limits with fallback, never a silent drop. OWNER (Q10).** §6. The storm guard is in
FR-041a (D26); monthly quotas and the fallback channel in FR-041c.

**D11 — provider credentials live in instance settings, encrypted, with a per-project override.
OWNER (Q11).** Instance settings in FR-041a; the per-project override in FR-041c. Encryption: D29.

### Owner decisions (2026-11-16, on the first external review)

**D13 — acknowledgement by phone is attributed, so only USER-BOUND deliveries can acknowledge or
snooze. OWNER (2026-11-16; proposed by the author, supported by the review).** `acknowledged_by` is
a user id and incident writes are audited by principal (FR-026). A shared `number`-mode phone is
passed from shift to shift and names nobody: it gets the incident link but **no ACK code**, and its
replies are ignored (with a metric). Mapping such a phone to a user in the channel config was
considered and refused — it would write one person's name over whoever held the phone. A reply from
a `user`-mode delivery acts as that user, through the principal door of the store (§5.2), and only
if that user **still** holds the permission the HTTP endpoint requires on that project at reply
time.

**D17 — what breaks quiet hours. OWNER (2026-11-16).** During a user's quiet hours a `user`-mode
phone channel delivers an alert only when one of these holds; everything else goes to the fallback
channel (§6.3, §6.4):

1. the alert belongs to an **open** incident whose impact is `critical`
   ([`func-incident-impact.md`](func-incident-impact.md): declared by the service, only rises, no
   person edits it);
2. the alert is an **escalation step**, whatever the incident's impact — a ladder is the owner's
   statement "if nobody answered, wake the next person", and a ladder that stops at the bedroom
   door is not one;
3. the alert is a service alert for state `unknown`, and the service's mapping frozen on the
   incident grades `down` as `critical` (D23).

For a monitor's DOWN the impact is the project's `default_incident_impact` when the monitor has
`auto_incident` on (FR-042: a monitor's incident opens with it and never rises), read directly —
not from the incident row, which may not exist yet (D32). A monitor without `auto_incident` has no
incident and never breaks quiet hours.

Never breaking quiet hours: a recovery message (a good-news SMS does not wake anyone, even for an
incident that was `critical`), and an SLO burn alert (D24).

**D22 — phone events are not written to the incident timeline. OWNER (2026-11-16, review P0-2).**
`incident_updates` is public: the status page and the incident history serve every update's body to
anonymous visitors, and `IncidentUpdate.PublicRedacted` (`internal/domain/incident.go`) strips the
author but keeps the text. Notes such as "acknowledged by phone", "snoozed" or "not phoned: quota
exceeded" would publish who is on call, whose quiet hours are in force and that a quota ran out.
They are recorded instead in `phone_deliveries` and `phone_actions` (§8) and shown to operators on
the incident page as a **Phone** panel (§5.5, §9), behind `project:read`. A visibility flag on
`incident_updates` was considered and refused: it would touch the public page, the feeds, webhooks
and subscriber mail to keep three kinds of note out of them.

**D23 — the night call for `unknown` follows the service's own grade for `down`. OWNER
(2026-11-16; proposed by the review).** FR-042 opens an incident for state `unknown` with impact
`none` by default, so rule 1 of D17 would never ring for a service that set `page_on_unknown`. The
two options of revision 2 were both refused: "use an escalation policy" hides a switch the operator
already flipped behind a second one, and "a paged `unknown` always breaks through" would wake a
person for lost visibility of a service whose full outage is graded `major` and would not. The
rule: a paged `unknown` breaks quiet hours exactly when that service grades `down` as `critical` —
read from the mapping frozen on the incident (`impact_basis.mapping.down`, FR-042 D8). The public
impact stays `none`; there is no new setting; losing sight of a service never rings louder than
the service going down.

**D24 — an SLO burn alert does not break quiet hours. OWNER (2026-11-16, review P1-13).** Neither
the monitor burn alert (`slo_burn_alert`) nor a service alert with signal `burn` opens an incident
or starts a ladder, so neither rule 1 nor rule 2 of D17 can apply to it. That is accepted, not
patched: during quiet hours a burn alert goes to the fallback channel. A fast burn that matters is
normally an outage, and the outage opens its own incident.

**D25 — SMS encoding is an instance setting. OWNER (2026-11-16, review §13 Q3).**
`sms_encoding: transliterate | unicode`, default `transliterate`. Revision 2 dropped every
character outside GSM-7, which turns a monitor named in Cyrillic, Greek, Arabic or CJK into empty
brackets. `transliterate` converts names to ASCII and keeps the message in GSM-7; `unicode` sends
names as written, in UCS-2, at 70 characters a segment — and the segments are counted as they are
(§4.4, §6.1). The message frame is ASCII in both.

**D26 — the storm guard is part of FR-041a. OWNER (2026-11-16, review §19).** Between FR-041a and
FR-041c an instance would otherwise place paid calls with no limit; a flapping monitor without a
ladder could ring one phone all night. §6.2. In FR-041a a storm-limited delivery has no fallback
channel to go to (that is FR-041c): it is recorded and counted, and nothing else.

**D27 — mixed versions and rollback: a barrier and a runbook, no mechanism. OWNER (2026-11-16,
review P1-14).** A process that predates FR-041 and delivers the outbox does not know the channel
types `sms` and `voice`: its dispatcher fails the whole event, so the event's other channels are
re-delivered on every outbox retry and the event is finally dead-lettered. The harm is duplicate
messages, not lost ones. So:

- a phone channel cannot be **created** while telephony is not enabled for its project (the
  instance settings in FR-041a; the project's effective settings once FR-041c adds the override);
- the runbook and the CHANGELOG upgrade note say: enable telephony only after every role that
  delivers the outbox (`all`, `api`, `scheduler`) runs this release; before rolling back, disable
  the phone channels. The same note covers snooze: an older scheduler leader ignores it and fires
  the deferred steps at once;
- what must not depend on that discipline is made safe in the schema: `incidents.ref` has a
  database DEFAULT, so an older binary's `INSERT INTO incidents` keeps working from the moment the
  migration runs (§8).

**D28 — who may create a phone channel. OWNER (2026-11-16, review P1-6).**

| Caller | `user` mode | `number` mode |
| --- | --- | --- |
| project editor (`project:write`), signed in | only with `recipient_user_id` = themselves | no |
| project admin (`project:manage`), org admin, global admin | any eligible recipient (§4.3) | yes |
| API token | no — a token is not a person and has no number | yes, with `project:manage` |
| project viewer | no (the existing rule for every channel) | no |

Update and delete follow the same table, judged against the channel **as stored** and as patched: an
editor cannot retarget someone else's channel to themselves or theirs to someone else.

**D29 — telephony needs the at-rest key. OWNER (2026-11-16; author's proposal).** cerbix starts
without `security.encryption_key` when `secrets.enabled` is false, and the legacy settings path
then stores secrets as plaintext (`internal/secret/secret.go`). Telephony does not follow it:
enabling telephony, storing a provider credential or storing a phone number is refused with a clear
error while no key is configured. Provider credentials, user numbers and channel numbers are all
encrypted (D35), and `cerbix reencrypt` (`internal/cli/reencrypt.go`,
`internal/store/reencrypt.go`) covers them.

**D30 — manual and Alertmanager incidents do not phone. OWNER (2026-11-16; author's proposal,
correcting revision 2).** Revision 2 said the Alertmanager receiver's incidents "page by phone like
any other incident". They page nothing today: such an incident is created through the principal
door with no monitor or service anchor, enqueues only `incident_event` (webhooks and subscriber
mail), and the escalation queries select `source = 'auto'` (`internal/store/escalation.go`). FR-041
does not change that.

### Author's calls (open at review)

**D12 — a user's phone reaches an on-call schedule through a USER-BOUND CHANNEL. AUTHOR'S CALL.**
`OnCallSchedule.Participants` are **channel ids**, and overrides name a `channel_id`
(`internal/domain/escalation.go`, `oncall_overrides`). Schedules rotate channels, not users. Rather
than teach schedules about users (a model change across schedules, overrides, tenancy checks and the
SPA), a phone channel has one of two **recipient modes**:
- `user` — the channel holds `recipient_user_id`; the number is resolved **at send time** from that
  user's verified number. Alice's phone is the channel "SMS — Alice"; the schedule rotates it like
  any other.
- `number` — the channel holds an E.164 number (a shared duty phone, a NOC line), verified before
  the channel can be enabled.

Resolving at send time means a user who changes or loses a number never leaves a stale copy inside
channels. A `user` channel whose user has no verified number, or is no longer an eligible recipient
of the channel's project (§4.3), is **not a route** (D31) — it never falls back to an old number.

**D14 — the provider HTTP calls go through the notification egress guard. AUTHOR'S CALL.**
Same `notification_egress` policy as webhook/Slack/Telegram (`internal/cli/cli.go`, egress client).
`generic_http` is an operator-chosen URL and gets exactly the webhook treatment, including the
redirect refusal sec-hardening-3 added to delivery (`prober.RefuseRedirects`): a 3xx from a gateway
is a configuration answer, recorded as `provider_rejected` and not retried. The full classification
of provider answers is in §4.5.

**D15 — phone delivery runs where the outbox runs: every role with `coreDelivery` (`all`, `api` AND
`scheduler`, `internal/cli/cli.go` `servicesForRole`). AUTHOR'S CALL.** All three load the telephony
settings and credentials. No prober involvement, no dispatch carrier: telephony credentials never
leave core (consistent with the two-keyring rule — a prober never holds a notification credential).
The inbound endpoints are served by the roles that serve the API (`all`, `api`). So in a distributed
topology the process that places a call and the process that receives its callback differ; §5.1
correlates them through a row committed **before** the call (D33), never through memory.

**D16 — no new outbox topic and no outbox payload change; phone gets its own delivery method.
AUTHOR'S CALL (revised after the builder check and the review).** Suppression (silence,
maintenance, dependency `DownAncestors`, escalation flat-down) is decided per topic before channel
fan-out and applies unchanged. But `notify` today receives only a pre-rendered string (`Deliver`,
`DeliverText`, `DeliverProjectText`, `DeliverChannels`, `DeliverChannelsReporting` in
`internal/notify/notify.go`), and a phone message needs facts that string does not carry. So:
- The text paths **skip** `sms`/`voice` channels; they keep delivering every other channel exactly
  as today (P14).
- The outbox worker, for the alerts phone receives (D18), builds a structured `PhoneAlert` — kind,
  subject name, project, the monitor or service it is about, the incident when one is known, the
  outbox event id — and calls a new `DeliverPhone(ctx, channelIDs, PhoneAlert)
  (domain.ChannelDelivery, error)` on the `NotifyDeliverer` interface. Where the counts are
  consumed — today the service-alert path only (`deliverServiceAlert`,
  `internal/outbox/outbox.go`) — `Resolved` and `Delivered` of the two calls are **added**, and
  `Requested` is **not**: both calls are handed the same recipient snapshot and
  `DeliverChannelsReporting` already sets `Requested` to its length (`internal/notify/notify.go`).
  Adding it twice would make `Requested - Resolved` positive on every service alert with a phone
  recipient and raise `service_alert_recipient_missing` for recipients that are all there. D31
  says what the counts mean.
- Outbox rows and payloads are unchanged; the interface grows, so every fake that implements it or
  the store interfaces breaks compilation by design (CLAUDE.md): the fakeStores of
  `internal/outbox`, `internal/api` and `internal/scheduler`, and those of `internal/notify` and
  `internal/settings`.

Only the voice retry (FR-041b) needs its own durable schedule, and it uses `phone_deliveries` (§8),
not the outbox.

**D18 — phone receives only alerts that ask a person to act. AUTHOR'S CALL (builder check Q2;
predicates made exact after the review).** "Usable everywhere a channel is" would otherwise include
the weekly SLA report and region-worker alerts (both fan out to every enabled project channel via
`DeliverProjectText`), DOWN reminders and burn CLEAR messages — a weekly phone call reading an SLA
report. The table is by outbox topic and the payload fields that split it; a row not listed here is
not phoned.

| Topic | Predicate | `sms` | `voice` |
| --- | --- | --- | --- |
| `monitor_transition` | not `Reminder`, current status `down` | yes | yes |
| `monitor_transition` | not `Reminder`, current status `up` (recovery) | yes | no |
| `monitor_transition` | `Reminder` (renotify) | no | no |
| `service_alert` | signal `health`, firing | yes | yes |
| `service_alert` | signal `health`, not firing, close reason `recovered` | yes | no |
| `service_alert` | signal `health`, not firing, any other close reason | no | no |
| `service_alert` | signal `burn`, firing | yes | yes |
| `service_alert` | signal `burn`, not firing (CLEAR) | no | no |
| `slo_burn_alert` | firing | yes | yes |
| `slo_burn_alert` | CLEAR | no | no |
| `escalation_step` | any step, including `repeat_last` | yes | yes |
| `sla_report`, `region_worker_alert`, `incident_event`, `subscriber_confirm`, every other topic | — | no | no |

A service alert that closes for a reason other than `recovered` — `visibility_lost`,
`entered_maintenance`, `ownership_disabled`, `policy_changed`, `burn_disabled`, `rule_removed`,
`service_deleted`, `undelivered` (`internal/domain/servicealert.go`) — did not recover, and an SMS
saying "RESOLVED" would be false. The text channels keep receiving those closes as today.

**D19 — one accepted provider call per (outbox event, channel), whatever the outbox retries.
AUTHOR'S CALL (builder check Q3; mechanism replaced after the review).** The outbox retries the
WHOLE event when any channel errors, re-delivering to every channel — harmless for Slack, a second
charge and a fresh ACK code for a phone. The mechanism is D33's reserved row: a retry finds the row
and does not call the provider again unless the row says the provider was never reached. A provider
**rejection** is final: it is recorded and returned to the outbox as success-for-retry-purposes,
and it is NOT counted as `Delivered` (D20). Three things return an error and let the outbox retry:
`provider_unreachable`, a failed fallback (§6.3), and a row still in `sending` that is too young to
be judged (D33). Such a retry re-delivers the event's text channels, as a failing webhook does
today.

**D20 — what counts as `Delivered` for service coverage. AUTHOR'S CALL (builder check Q7).**
`ChannelDelivery.Delivered > 0` credits service coverage and silences the members' own alerts
(D-0179). A phone delivery counts as delivered only when the provider accepted it, OR its fallback
channel (§6.3) accepted the redirected alert. Everything else is NOT delivered — so the members
keep paging on their own channels. The polarity is deliberate: when in doubt, page.

**D21 — incidents get a short reference for the phone. AUTHOR'S CALL (builder check, §4.4).** Incident
ids are UUIDs and the SPA route is `/incidents/:id`; a UUID link spends ~60 of 160 characters.
`incidents.ref` — 8 characters, Crockford base32, unique, generated by a database DEFAULT (D27)
that does not hand out a value already in use (§8), backfilled by the migration — plus an SPA route
`/i/:ref`. The route needs a server resolver, which
revision 2 did not have: `GET /api/v1/incidents/by-ref/{ref}` returns the incident's id, project
and organization under the same two-step as `getIncident` (invisible → 404, visible without
`project:read` → 403); the SPA switches its active organization and project to them and redirects
to `/incidents/:id`. An unauthenticated visitor gets the login page first, never an answer that
confirms existence. `ref` is **not** a field of `domain.Incident`: that struct is the `incident`
object of every webhook payload and of `outbox_events.payload`, and adding a field there would
change those bytes for every project (P14). It is returned by the authenticated incident detail and
list responses only, the way FR-042 returns `impact_basis`.

**D31 — a phone channel that cannot deliver is not a route; a delivery nobody received neither
credits nor loops. AUTHOR'S CALL (review P0-3).** Today a channel that exists and is enabled is a
route, and "resolved but not delivered with no error" cannot happen. Phone makes it happen, and two
pieces of code decide on it:

- `routablePredicate` and `resolveServiceRecipientsTx` (`internal/store/alertdelegation.go`,
  `internal/store/servicealerteval.go`) decide whether a service has anywhere to announce to. If
  the answer is yes, the service covers its members and their own DOWN alerts are held while the
  service confirms;
- the outbox worker condemns a service alert that resolved **no** channel
  (`MarkServiceAlertUndeliverable`), and the evaluator then **re-announces on every pass** while the
  service is still routable (D-0187).

So the rules are:

1. **Deliverable by configuration.** A phone channel counts as a route only when all of this
   holds:
   - it is enabled and its recipient exists: in `number` mode the number is verified; in `user`
     mode the recipient is a human user who is an eligible recipient of the project (§4.3) and has
     a verified number;
   - **telephony is in effect** for its project: the effective telephony settings (the instance's
     in FR-041a, the project's override when FR-041c gives it one) are enabled, and for a `voice`
     channel their provider is not `generic_http`. A channel that fails only this clause is still a
     route when (FR-041c) it names a live non-phone fallback channel — the fallback is where its
     alerts go (§6.3).

   All of it is in typed columns and in the database (§8, D36), so the predicates stay SQL. The
   rule is applied in three places and only to rows of type `sms` or `voice`: both SQL predicates;
   the resolution of an escalation step's channels; and `ChannelDelivery.Resolved` at send. A
   service whose only channel is an unverified phone — or any phone while telephony is switched
   off — is therefore *unroutable*, exactly as a service with no channel is: its onset is withheld,
   coverage is not armed, its members page for themselves on their own channels, and fixing the
   channel or switching telephony on produces the announcement.
2. **Not credited.** A phone delivery credits coverage only as D20 says.
3. **Not condemned either.** A channel that is a route but was not phoned — `storm_limited`,
   `quota_exceeded`, `quiet_hours_deferred`, `provider_rejected`, `outcome_unknown` — leaves the
   service alert *announced and uncredited*: no `MarkServiceAlertDelivered`, no
   `MarkServiceAlertUndeliverable`. Condemning it would make the evaluator re-announce on its next
   pass, fifteen seconds later, to the same channel with the same answer, all night — and §6.4
   refuses even one morning replay. The existing condition (`err == nil && res.Resolved == 0`)
   stays as it is and keeps meaning "there was nobody to tell"; rule 1 is what makes it true for
   phone.

**A snapshot taken before a channel stopped being a route.** A service alert carries the recipients
resolved when it was enqueued. A phone channel in that snapshot which is no longer a route at send
is not counted in `Resolved`, and its row says why (`not_deliverable`, `telephony_disabled` or
`voice_unsupported_by_provider`, §4.5). `Resolved` follows **rule 1 and nothing else** — never the
row's reason: a channel that telephony being off would have disqualified, but that a live fallback
keeps a route (FR-041c), is counted in `Resolved`, carries one of the same two reasons, and is
`Delivered` when the fallback accepts it. Deciding `Resolved` from the reason would report such a
recipient as missing on every alert. If nothing else resolved, the existing condition condemns
the alert; the evaluator's next pass finds no route, withholds, and the members page for
themselves. There is no loop: an unroutable service is not re-announced.

**The cost, as it is.** While a service is healthy and routable it covers its members: a member's
own DOWN is dropped at delivery, in favour of the announcement the service is about to make
(`liveOnsetDeliveredSQL`). If that announcement then goes to a phone channel that is the service's
**only** route and is not phoned under rule 3, nobody has been told, and nothing is re-sent when
the obstacle goes away. What follows depends on what else is configured:

- a monitor's `renotify_seconds` is `0` by default (`internal/settings/service.go`), so there are no
  reminders unless the operator set them; with them, the reminders are delivered, because coverage
  was never credited;
- a service incident with an escalation ladder still fires its steps, and steps break quiet hours
  (D17);
- an SLO burn alert has no reminder and no ladder at all (`EvaluateBurnAlerts`,
  `internal/store/sla.go`).

So with a phone as the only route, no fallback, no reminders and no ladder, a storm-limited,
over-quota, rejected or quiet-hours-deferred announcement can mean **silence for the whole outage
or the whole burn episode**. Revision 3 said only the first alert was at stake; that was wrong.
What bounds it: in each of these cases the member's own page to the same phone would have been
refused for the same reason, so the loss is real only where the member links a different channel.
Telephony switched off and a voice channel under `generic_http` are no longer among these cases —
rule 1 makes them non-routes, and the members page from the start. The remedies are a fallback
channel (FR-041c), a second channel, `renotify_seconds`, or a ladder; the SPA warns where a phone
channel is a service's only route and has no fallback (§9).

**D32 — a monitor's incident is not raced. AUTHOR'S CALL (review P0-4).** `monitor_transition` is
enqueued in the transaction that records the result; the reconciler opens and resolves the
monitor's auto-incident **afterwards**, outside it, with a 250 ms retry (`internal/ingest`). The
outbox worker — possibly another process — can deliver before the incident exists. Nothing in the
phone path may depend on winning that race:

- **quiet hours** read the project's `default_incident_impact` for a monitor with `auto_incident`
  (D17), not the incident row;
- **the ACK code** is bound to the delivery, and the delivery to the monitor; the incident is
  resolved when the reply arrives — "the open auto-incident of that monitor" (§5.2) — not when the
  message is sent;
- **the link** needs the incident's `ref`. On a DOWN for a monitor with `auto_incident`, when no
  open auto-incident is found, the phone path waits for it up to 1 s (four tries, 250 ms apart,
  inside the delivery's time budget) and then sends **without** a link, recording `no_incident_ref`
  on the delivery row. The message is still sent; the code still works;
- **the row finds its incident either way**, and the two sides are serialised, because under READ
  COMMITTED neither can see the other's uncommitted work. Both take the same transaction-scoped
  lock for the monitor — an advisory lock keyed by the monitor id, not the monitor's row, which
  every check result updates:
  - the **reservation** of a monitor-DOWN delivery (D33) takes the lock, looks for the monitor's
    open auto-incident, inserts the row with that `incident_id` or with NULL, and commits. The row
    also stores `monitor_seq`, the `Seq` its event carries;
  - the transaction that **opens** a monitor's auto-incident — and only that one: not a manual
    incident, not an Alertmanager one, not a service's — takes the lock, inserts the incident,
    and sets `incident_id` on that monitor's rows that have none, are of the monitor-DOWN alert
    kind, and whose `monitor_seq` equals the monitor's current `state_sequence`.

  Whichever commits first, the other sees it. The sequence replaces a time bound: the opener is
  not told when the transition happened, and the result's own timestamp comes from the prober's
  clock, which may run ahead of the database's (`result.allowed_skew`). It also keeps out what
  must not be attached — a row of an earlier outage that never got an incident, and a burn alert
  of the same monitor, which has no incident and must not gain a voice retry (§5.4). The 1 s wait
  above is then only about the link. Without the attachment the row would stay out of the
  incident's Phone panel (P7), out of the voice retry, and its ACK code would not be closed with
  the incident (§5.2) — and would still be open when the monitor next went down;
- **a recovery is raced in the same way.** The UP event commits with the result; the reconciler
  resolves the incident afterwards, so the worker may see the incident still open. The link of a
  recovery SMS is the monitor's **open** auto-incident when there is one; otherwise its latest
  auto-incident, if that was resolved at or after the time the recovery event was enqueued;
  otherwise there is no link, and the row says `no_incident_ref` when the monitor has
  `auto_incident` on (without it, having no incident is the normal case and is not noted). The
  time is `outbox_events.created_at`, which the claim already reads (`ClaimDueOutbox`) and which is
  handed to the phone path beside the payload. It is **not** `MonitorTransition.At`: no writer of
  that payload sets the field, it is the zero time in every stored event, and starting to set it
  would change payload bytes for every project (P14). Revision 3 took "the most recently resolved
  auto-incident", which during the race is the previous outage's. The price of the rule: an
  auto-incident a person resolved by hand before the monitor recovered is not linked. A recovery
  carries no ACK code. (During an FR-043 recovery run the monitor stays `down` and no UP is
  enqueued, so the race exists only on the confirmed recovery.)

A service alert has no such race — the service incident opens in the evaluator's transaction, with
the alert.

**D33 — a delivery is reserved before the provider is called. AUTHOR'S CALL (review P1-2).** A row
is inserted into `phone_deliveries` with state `sending` — `INSERT … ON CONFLICT DO NOTHING` on
`(outbox_event_id, channel_id)` — and **committed** before any HTTP request; the provider's answer
then updates it. Consequences:

- a callback that arrives before the worker has stored the provider's reference still finds its
  row, because the callback URL carries the delivery id (§5.1);
- a process that dies after the provider accepted and before the update leaves a row in `sending`.
  A retry that finds such a row older than 60 s does **not** call again: it closes the row as
  `outcome_unknown` — not delivered (D20), visible (P7). One uncertain message is cheaper than a
  duplicate call at 03:00, and the members keep paging (D31);
- a retry that finds a `sending` row **younger** than 60 s does not call the provider either, and
  returns a retryable error for the event. The outbox's first two leases are 10 s and 20 s
  (`ClaimDueOutbox`, `internal/store/outbox.go`), so the retries after a crash arrive about 10 s
  and 30 s after the reservation — both too early to judge. Returning success there would mark the
  event delivered and leave the row in `sending` for ever; the error brings the attempt at about
  70 s, which closes it. The text channels of the event are re-delivered by those retries, as they
  are after any worker crash;
- the scheduler leader also closes `sending` rows older than 60 s as `outcome_unknown` in its
  periodic pass, for the rows no retry will come back to (a dead-lettered event, a voice retry of
  §5.4);
- a retry that finds a final row does nothing for that channel; one that finds
  `provider_unreachable` sets the same row back to `sending` — committed, `updated_at = now()`,
  `attempts` grown — and calls again;
- the 60 s are measured from `updated_at`, by the retry and by the leader's pass alike. Measured
  from `created_at`, a row on its fourth attempt is already 70 s old and the leader would close a
  call that is in flight. (The storm guard's hour and the quota's month read `created_at`.)
- the unique key is `(outbox_event_id, channel_id) WHERE outbox_event_id IS NOT NULL`. It needs no
  step or repeat ordinal — every ladder step and every `repeat_last` is its own outbox row
  (`AdvanceEscalations`) — and `outbox_event_id` is **not** a foreign key: delivered outbox rows are
  purged long before the 90 days a delivery row is kept;
- each provider request is bounded by what is left of the delivery's budget. The first outbox
  attempt has about 8 s (a 10 s lease less `leaseHeadroom`, `internal/outbox/outbox.go`), which a
  10 s HTTP timeout per channel would overrun on the second channel. A provider call gets at most
  5 s and never more than the remaining budget; phone channels of one event are called
  concurrently, at most four at a time; a channel not reached before the budget ends is
  `provider_unreachable`.

**D34 — of a provider's refusal, cerbix keeps the code, not the text. AUTHOR'S CALL (review
P1-10).** Revision 2 passed provider error text through "the existing failure-message redaction".
There is none for response bodies: `notify.DeliveryFailure` (`internal/notify/deliveryfailure.go`)
never reads a body — it replaces a transport error with a class so that no URL is quoted. Provider
error bodies routinely quote the destination number, and the delivery report is readable by every
project viewer. So a delivery row stores the HTTP status and a short provider code, validated as
`[A-Za-z0-9_.-]{1,32}`, and no free text. For Twilio the code is its numeric error code. For
`generic_http` it is read from the response only when the settings name a
`generic_http.response_code_field` — one top-level key of a JSON response body, string or number;
a value that does not match the pattern is dropped. Without that setting a gateway's refusal is its
HTTP status alone, which cannot tell a bad template from an empty balance; the settings say so
beside the field. Transport failures use `notify.DeliveryFailure` as every other channel does.

**D35 — numbers are stored as ciphertext and a keyed hash, and shown masked to everyone. AUTHOR'S
CALL (review P1-11, P1-13).** A number is kept in three forms: AEAD ciphertext (to send), a keyed
hash derived from the at-rest key (to match an inbound sender and to enforce uniqueness and the
storm guard without decrypting), and a mask (`+90•••••6869`) stored at write time (to display).
Reads are decrypt-free: no API response carries a number in full, the owner's own `GET /api/v1/me`
included — revision 2 promised the owner the full number in one section and a mask in another, and
the mask wins; a user who needs to change the number re-enters it. `phone_deliveries` stores the
hash and the mask, never the ciphertext.

**The hash needs a primitive `internal/secret` does not have.** `secret.Cipher` keeps only the AEAD
of each key and drops the key bytes (`secret.New`). It gains a keyed hash: HMAC-SHA-256 under a
sub-key derived from each at-rest key with a fixed domain label, so the AES-GCM key itself is never
used as a MAC key. Two operations: the hash under the **primary** key, used for every write; and
the list of hashes under **every configured key**, used for every lookup.

**Rotation is two steps in cerbix**, and both are covered. First the processes restart with the new
primary and the old key in `previous_keys`; then `cerbix reencrypt` runs
(`internal/cli/reencrypt.go`, `internal/store/reencrypt.go`).

- Between the two, stored hashes are under the old key. Lookups — the inbound sender, the storm
  guard, the uniqueness check of a number at write — therefore match against the hashes of all
  configured keys.
- `cerbix reencrypt` re-encrypts the ciphertexts and recomputes the hashes of `user_phones` and of
  channels, and closes every open ACK code (`UPDATE phone_deliveries SET ack_open = false WHERE
  ack_open`) in the same run. Delivery rows hold no ciphertext, so their hashes cannot be
  recomputed; a code issued under the old key could no longer be matched once that key is removed,
  and closing it is the honest end. The link in the message still works.
- Once the old key is removed from the configuration, delivery rows hashed under it no longer
  count for the storm guard. That is at most one hour of history, once per rotation.

**D36 — telephony settings live in the database only. AUTHOR'S CALL (second review, P1-2).** The
blocks that hold credentials — mail, OIDC, the auth policy — resolve database → YAML → defaults;
branding, alerting and monitor defaults already resolve database → defaults with no YAML
(`internal/settings`). Telephony joins the second group. D31's rule 1 has to
know, inside SQL, whether telephony is enabled and which provider is in force, and a predicate
cannot read a YAML file. Passing the resolved values into every query that embeds the predicate was
considered and refused: the predicate is a constant shared by the evaluator, the coverage verdict
and the burn rules, and a value cached per process would differ between the process that evaluates
and the one that delivers. So the `telephony` block has **no** YAML counterpart: it is absent until
a global admin saves it, and absent means disabled. Nothing is lost by it — telephony cannot work
before a person has verified a number in the UI anyway.

## 4. FR-041a — the channels

### 4.1 Instance telephony settings

A new `telephony` block in `instance_settings`. Global-admin only, live-reconfigurable, and —
unlike `mail` — **database only**: there is no YAML seed, and an absent block means telephony is
disabled (D36).

| Field | Meaning |
| --- | --- |
| `enabled` | master switch. Off: no phone channel can be created (D27), existing ones are not phoned (`telephony_disabled`) and are not routes unless a fallback keeps them one (D31) |
| `provider` | `twilio` \| `generic_http` |
| `twilio.account_sid`, `twilio.auth_token`*, `twilio.from` | `from` is an E.164 number or a Messaging Service SID |
| `generic_http.method`, `.url`, `.headers`*, `.content_type`, `.body_template`, `.from` | method `POST` \| `PUT`; URL validated as a webhook URL is; `content_type` is `application/json` or `application/x-www-form-urlencoded` and fixes the placeholder encoding (§4.4); headers may carry credentials, so the whole map is secret |
| `generic_http.response_code_field` | optional; the top-level JSON key of the gateway's response that holds its error code (D34) |
| `public_base_url` | the URL the provider can reach (FR-041b); also builds the incident link. `https` unless the host is loopback |
| `voice_language` | BCP-47, default `en-US` |
| `sms_encoding` | `transliterate` (default) \| `unicode` (D25) |
| `sms_max_segments` | 1–4, default 2 (§4.4) |
| `storm.per_incident_per_hour`, `storm.per_number_per_hour` | defaults 5 and 20 (§6.2) |

`*` secret: encrypted at rest (D29) and never emitted. The read returns `auth_token_set` /
`headers_set` booleans, and a write that omits a secret or sends it empty **keeps** the stored
value — the contract `putMail` already has for `smtp_password` (`internal/api/handlers_settings.go`,
`password_set`). Revision 2 described the opposite ("the form re-collects them").
`public_base_url` falls back to `mail.public_base_url` when unset; with neither, messages carry no
link and the delivery row says so (`no_public_url`).

Saving the settings is refused while `security.encryption_key` is unset (D29). Switching `provider`
to `generic_http` while `voice` channels exist is allowed and warned about: they stay, stop being
routes (D31) and are not phoned (`voice_unsupported_by_provider`). Switching `enabled` off is
warned about in the same way. Both warnings name the services that the change leaves with **no
route** — computed with rule 1, so from FR-041c a phone channel with a live fallback is not among
the reasons: those services become unroutable and their members page for themselves.

A **"Send test SMS"** action sends a fixed text to the calling admin's own **verified** number only.

### 4.2 Verified numbers

- `PUT /api/v1/me/phone` `{ "number": "+905…" }` stores it **unverified** and sends a 6-digit code
  by SMS. `POST /api/v1/me/phone/verify` `{ "code": "…" }` marks it verified.
  `DELETE /api/v1/me/phone` removes it. `GET /api/v1/me` gains
  `phone: { number_masked, verified, language }` (D35), or `phone: null` without a number;
  FR-041c adds `quiet_hours` to it.
- Language and quiet hours are **not** part of that write — replacing the number clears
  verification, and changing a language must not. `PATCH /api/v1/me/phone/preferences`
  `{ "language"?, "quiet_hours"? }` (quiet hours: FR-041c, §6.4).
- One number per user (a second `PUT` replaces it and clears verification). Machine users
  (`users.kind = 'machine'`) have no phone: the routes answer 403 for a token or a
  client-credentials principal.
- Code: 6 digits, stored as a hash, valid 10 min, 5 attempts, then invalid; sending is limited to
  3 codes per user per hour and 10 per number per day (a number cannot be bombed through many
  accounts).
- A shared `number`-mode channel is verified the same way, by the person creating it:
  `POST /api/v1/notification-channels/{channelID}/verify/send` and
  `POST /api/v1/notification-channels/{channelID}/verify`. Such a channel is **created
  disabled** whatever the request says — the create endpoint's default of `enabled: true`
  (`internal/api/handlers_notifications.go`) does not apply to it — and cannot be enabled before it
  is verified. Changing its number clears the verification and disables it in the same statement.
- Verification codes are the **only** text cerbix sends to an unverified number (P1).

### 4.3 The channel types

`ChannelType` gains `sms` and `voice`. What a phone channel holds is **not** in the `config` map,
which is a string map round-tripped by every client (`MergeChannelConfig`): a read-only key in it
would make the ordinary read-modify-write PATCH fail, and a reference in it has no foreign key.
Typed fields on the channel instead (§8), surfaced at the top level of the channel DTO:

| Field | Mode | Write | Read |
| --- | --- | --- | --- |
| `recipient` | both | `user` \| `number`, fixed at creation | as written |
| `recipient_user_id` | user | an eligible recipient; D28 says who may name whom | as written |
| `number` | number | E.164, write-only | never; `number_masked` and `number_verified` instead |
| `fallback_channel_id` | both | FR-041c, §6.3 | as written |
| `disabled_reason` | both | server only | `recipient_deleted`, `number_unverified`, or absent |
| `deliverable` | both | server only | whether the channel is a route (D31), with the reason when it is not |

**Eligible recipient.** A user of kind `human` who reaches the channel's project in one of the
three ways access is granted: a membership on the project, an organization-level membership of its
organization, or global admin. Checking project memberships alone would bar organization admins
from being on call. The check runs at create, at update and — through the D31 predicate — at every
send.

**Choosing a recipient.** The members endpoint needs `project:manage` and does not return inherited
members' ids (`internal/store/projectmembers.go`), so an editor's form has nothing to list. A new
`GET /api/v1/projects/{projectID}/phone-recipients` (`project:write`) returns the eligible
recipients as `{ user_id, display_name, email, phone_verified }` — never a number, not even
masked. For an editor it returns only the editor themselves (D28).

**Deleting a user.** `recipient_user_id` references `users` with `ON DELETE SET NULL`.
`DeleteUser` (`internal/store/users_admin.go`) is one statement on the pool today; it becomes a
transaction that **first** disables the user's channels with `disabled_reason = recipient_deleted`
and **then** deletes the user — in the other order the foreign key has already cleared
`recipient_user_id` and the update finds no row. A disabled channel is skipped by every resolver,
as any disabled channel is today; the trace is the reason on the channel, shown in the channel
list.

### 4.4 The message

SMS text, rendered by `notify` from the `PhoneAlert` (D16):

```
[cerbix] DOWN payments-api (prod) - incident X7K2P9QM https://cerbix.example/i/X7K2P9QM ACK 482193
```

- **The frame is ASCII**: the tag, the verbs, the separators, the link, the code. Revision 2's
  example used an em dash, which is outside GSM-7 and would have forced the whole message into
  UCS-2.
- **Names** (monitor, service, project) are user-chosen text and are **normalised** before they are
  rendered anywhere: control characters and line breaks removed, runs of white space collapsed.
  A name can then not start a new line that imitates the frame ("… ACK 000000").
  With `sms_encoding: transliterate` they are converted to ASCII (a character with no
  transliteration becomes `?`, so a name is never silently emptied); with `unicode` they are sent
  as written and the message is UCS-2.
- **Length is counted in the encoding's own units**: GSM-7 septets — 160 in one segment, 153 per
  segment in a multi-part message, and two septets for each of `^ { } \ [ ~ ] | €`, which includes
  the brackets of the tag — or UTF-16 code units, 70 and 67. A character outside the Basic
  Multilingual Plane is two code units (`len(utf16.Encode(...))`), not one rune.
- **Truncation order**, to fit `sms_max_segments`: the monitor or service name first (to no fewer
  than 12 characters, with `...`), then the project name, then the project is dropped. The link and
  the code are never truncated; if they alone do not fit, the link is dropped and the row records
  `link_omitted`. The number of segments actually sent is stored and is what quotas count (§6.1).
- The `ACK <code>` suffix appears only when D5 and D13 both allow it (P9).
- Content is limited to what a project viewer may already see. No heartbeat message bodies, no URLs
  of monitored targets, no secret values — probe `msg` text can echo target responses and is
  excluded.

Voice: TTS reads the same facts in words ("cerbix alert. payments A P I in prod is down.")
twice, then (FR-041b, when active) "Press 1 to acknowledge. Press 2 to snooze for 30 minutes."
The second sentence is spoken only when the alert's subject has a ladder at send time (§5.3):
offering a snooze that will be refused breaks P9. Names are XML-escaped inside `<Say>`; a name
containing markup is spoken, never interpreted.

**`generic_http`.** The destination is taken from the verified number and from nowhere else.
Placeholders are substituted as **values** in the body's content type: JSON string escaping for
`application/json`, percent-encoding for form bodies. A monitor named
`x","to":"+12025550199","x":"` therefore stays one string — with raw substitution it would have
rewritten the recipient and broken P1. A template that places a placeholder outside a JSON string
or a form value is refused when the settings are saved.

### 4.5 The delivery record

There is no per-channel delivery report today (`ChannelDelivery` in `internal/domain/servicealert.go`
holds three counts, and only the `service_alert` path returns it). The record of a phone delivery is
its **`phone_deliveries` row** (D33). Its `reason` is from a closed set, grouped by what D31 does
with it:

| Reason | Meaning | Part |
| --- | --- | --- |
| `sending` | reserved, provider not answered yet | a |
| `sent` | the provider accepted it | a |
| `provider_unreachable` | no usable answer; the outbox retries | a |
| `provider_rejected` | the provider answered no; final | a |
| `outcome_unknown` | a `sending` row found stale (D33) | a |
| `not_deliverable` | the channel's recipient is gone or unverified at send; the cause (`no_verified_number`, `recipient_not_eligible`) is in `notes` | a |
| `voice_unsupported_by_provider` | a `voice` channel under `generic_http` | a |
| `telephony_disabled` | telephony is not in effect for the project | a |
| `storm_limited` | the storm guard refused it (§6.2) | a |
| `incident_settled` | a voice retry whose incident was acknowledged or resolved before it was due (§5.4) | b |
| `quota_exceeded` | a monthly quota is spent (§6.1) | c |
| `quiet_hours_deferred` | the recipient's quiet hours, non-critical alert (§6.4) | c |

The table is the whole set (P6); nothing outside it is a `reason`.

**When a row is written.** A row exists for every alert a phone channel was **handed**: a monitor's
linked channel, a recipient in a service alert's snapshot, a target of an enqueued escalation step.
A channel that the route predicates left out (D31 rule 1) was never handed the alert and has no row
for it; its trace is the channel's own `deliverable` field with its reason (§4.3) and the warnings
of §9. So the three reasons `not_deliverable`, `telephony_disabled` and
`voice_unsupported_by_provider` appear: on a monitor's linked channel; on a snapshot recipient or
step target that stopped being a route after it was chosen; and, for the last two, from FR-041c on
a channel that **is** a route because a live fallback keeps it one (D31 rule 1) — chosen, counted
in `Resolved`, and delivered through the fallback (§6.3).

**How a provider's answer is classified**, for both providers:

| Answer | Reason |
| --- | --- |
| HTTP 2xx | `sent` |
| HTTP 408, 429, 5xx; a network error; no answer within the budget (D33) | `provider_unreachable` — retried |
| any other status, a 3xx included (D14) | `provider_rejected` — final |

For `generic_http`, 2xx **is** success: a gateway that reports a failure inside a 200 response is
not supported, and the settings say so. Retrying a 5xx can send a message twice when the gateway
accepted it and then failed; that is the same trade every webhook retry makes.

**Separate columns**, so that one fact never overwrites another: `fallback_status` (`none`,
`delivered`, `failed`, `unavailable` — FR-041c, §6.3), `notes` (`no_incident_ref`, `no_public_url`,
`link_omitted`, and the cause of `not_deliverable`), `provider_status` and `provider_code` (D34),
`callback_status` (FR-041b), `segments`, `duration_s`, `attempts`.

`reason = 'sent'` is the fact that the provider accepted the request, and a later status callback
never rewrites it. What the provider reports afterwards — `delivered`, `undelivered`, `failed`,
`busy`, `no-answer`, `completed`, `canceled` — is stored in `callback_status`, with the callback's
error code in `provider_code`. The voice retry (§5.4) and the Phone panel read `callback_status`.

Read through `GET /api/v1/incidents/{incidentID}/phone-activity` (§5.5) and
`GET /api/v1/notification-channels/{channelID}/phone-deliveries` (last 50; `project:read`; reasons
and masked numbers only).

### 4.6 What the rest of the system sees

One place for the consequences of D31, D20 and D33, because they are spread over three packages:

| Where | Change |
| --- | --- |
| `routablePredicate` (both halves: the schedule's participants and the project's channels), `resolveServiceRecipientsTx` | a row of type `sms`/`voice` counts only under D31 rule 1; rows of the four existing types are judged exactly as today |
| escalation step channel resolution (`channelInProject`, `internal/store/escalation.go`) | the D31 rule 1 condition is added **for `sms`/`voice` rows only**. The existing check does not look at `enabled` — a disabled Slack channel on a step is still enqueued and still moves `last_escalated_at`, and is dropped only at send — and that stays so, or P14 breaks. A step whose channels are all non-route phones behaves as a step with no channel does today: nothing enqueued, the ladder advances |
| `ChannelDelivery.Requested` | the length of the snapshot, counted once (D16) |
| `ChannelDelivery.Resolved` | counts a phone channel only when it is a route at send |
| `ChannelDelivery.Delivered` | counts a phone channel only as D20 says |
| `deliverServiceAlert` | adds `Resolved` and `Delivered` of the phone call to those of the text call; its two conditions are unchanged |
| monitor, burn and step deliveries | no coverage accounting today, none added; an error from `DeliverPhone` — `provider_unreachable`, a failed fallback (§6.3), or a young `sending` row (D33) — fails the event as a text channel's error does |
| scheduler leader | a periodic pass closes stale `sending` rows (D33) |

## 5. FR-041b — acknowledge, snooze, retry

### 5.1 The provider protocol and the inbound endpoints

**Outbound, Twilio.** Revision 2 named the provider and not the calls.

- SMS: the Messages resource, with `StatusCallback` set when inbound is active.
- Voice: the Calls resource with the call's TwiML passed **inline** in the `Twiml` parameter — no
  URL for the provider to fetch, so no fourth endpoint and no window in which the script is missing.
  The script is `<Say>` twice; when inbound is active it is wrapped in
  `<Gather numDigits="1" action="…/voice/gather?d=<delivery id>">`. `StatusCallback` is
  `…/status?d=<delivery id>`.
- Every callback URL carries `d`, the id of the delivery row reserved before the call (D33). The
  signature covers the full URL, so `d` cannot be altered; and because the row is committed first,
  a `failed` or `busy` callback that reaches the API before the delivering process has stored the
  provider's reference still finds it. `provider_ref` (the SID) is stored for the operator, not
  used for correlation.

**Inbound.** Mounted on a new `TelephonyRouter` beside `PublicRouter` (no session auth):

- `POST /api/v1/telephony/{route_key}/sms` — an inbound SMS
- `POST /api/v1/telephony/{route_key}/voice/gather` — a keypress
- `POST /api/v1/telephony/{route_key}/status` — a call or message status

`route_key` is an opaque random id (22 chars) generated per provider configuration — the instance
one now, each project override in FR-041c — and shown in the settings as the URLs to paste into the
provider console. It exists from FR-041b so that FR-041c's per-project accounts never force
operators to reconfigure their providers (builder check Q6). Rotating a credential set keeps its
key; "regenerate URLs" is an explicit admin action.

Order of handling, and nothing before step 2 is trusted:

1. The body is read with a size limit (64 KiB) and parsed as
   `application/x-www-form-urlencoded` — these routes are not JSON and are exempt from the JSON
   content-type guard by name.
2. The key selects ONE credential set, and the request is verified with it by
   `Provider.VerifyInbound` (Twilio: `X-Twilio-Signature` over the URL and the sorted parameters,
   with that account's auth token). The URL that is signed is rebuilt from the configured
   `public_base_url` plus the request's path and query — **never** from the `Host` or
   `X-Forwarded-*` headers, which a client controls and a proxy rewrites.
3. A key that is unknown, a configuration that is disabled or has no inbound support, and a
   signature that does not verify all get the **same** 403 and touch no row (P10). Revision 2
   answered 404 for some of these, which tells a scanner which keys exist; and it tied the routes'
   existence to the *instance* settings, which would have cut off a project whose own Twilio
   account is active while the instance's is not.

Rate limits: requests that **fail** verification are limited per client address (as the existing
limiter resolves it behind a proxy); verified requests are limited per route key. A provider sends
from a small shared address pool, so a per-address limit applied before verification would refuse
its legitimate callbacks during exactly the burst they belong to.

**Responses** are `200` with `Content-Type: text/xml` and TwiML, never JSON:

| Route | Response |
| --- | --- |
| `/sms` | `<Response><Message>…</Message></Response>` with the reply text, or an empty `<Response/>` when no reply is due |
| `/voice/gather` | `<Response><Say>…</Say></Response>` — "Acknowledged.", "Snoozed for 30 minutes.", or why not |
| `/status` | an empty `<Response/>` |

`/status` writes `callback_status` (and the error code when there is one) on the row `d` names; it
never changes `reason` (§4.5). Calls are placed **without** subscribing to intermediate status
events, so the provider reports only the terminal status of a call; a status that is not terminal
is stored and changes nothing else. `duration_s` is written once: the handler's
`UPDATE … WHERE id = $1 AND duration_s IS NULL` and the quota adjustment of §6.1 are one
transaction, and the adjustment is made only when that update changed the row. Providers repeat
callbacks, and two copies arriving together must not adjust twice.

A reply SMS is therefore the provider's response to its own request — no second outbound call, no
second delivery row. Replies are counted (§6.1) and bounded by their own rules (§5.2), not by the
storm guard.

### 5.2 The ACK code

- Issued per **user-bound** delivery (D13) of a kind a person can act on: a monitor DOWN, a service
  alert firing, an escalation step. Not on a recovery, not on a burn alert. 6 digits, unique among
  the codes still **open** for that number, stored on the delivery row with `ack_open = true`.
- **What it points at** is the delivery's subject — a monitor or a service — or its incident when
  the alert carries one (an escalation step). The incident is resolved when the reply arrives: the
  open auto-incident of that monitor or service (D32). No open incident then — a monitor without
  `auto_incident`, or one that already recovered — and the reply says there is nothing to
  acknowledge.
- **Open** until the incident is acknowledged or resolved, or 24 h pass. "Open" is a column
  (`ack_open`), closed by the acknowledge and resolve paths — for the rows of that incident and,
  for a monitor's auto-incident, also for that monitor's rows that never got an `incident_id`
  (D32 attaches them; this is the second lock on the same door) — and by the scheduler's purge
  pass. The resolve paths are two: an incident update that resolves (`addIncidentUpdate`), and the
  service incident's own close (`resolveServiceIncidentTx`, `internal/store/serviceincidents.go`),
  which does not go through the first. A
  partial index cannot call `now()` or look at another table, so revision 2's
  `UNIQUE … WHERE the code is open` was not DDL.
- `ACK 482193` acknowledges. `ACK` alone works when exactly one code is open for the sender's
  number; with several, the reply lists them (`2 open: ACK 482193 payments-api, ACK 551020
  checkout`) and acknowledges nothing.
- **The sender** is matched by the keyed hash of the number the provider reports, which must equal
  the hash the code was issued to **and** still be that user's verified number at reply time (P11).
- Replies are case-insensitive, trimmed; anything unrecognised gets one help reply per number per
  10 minutes (a reply loop with another automated responder must not amplify).
- **Guessing.** Ten wrong codes from one number within 10 minutes and further `ACK`/`SNOOZE`
  messages from it are ignored for 10 minutes, with a metric. A verification code already had such
  a limit; a code that stays open for 24 h needs one more.
- **The write.** Acknowledgement goes through the principal door with the user as principal — the
  inbound handlers live in `internal/api`, and `TestTheAPINeverCallsASystemDoor`
  (`internal/api/incidentdoors_test.go`) forbids them the system door. The permission checked is
  the acknowledge endpoint's. Three things the present door does not give, and the phone path
  needs:
  - **the audit row must say it came from a phone.** `insertProjectAudit` stores
    `actor_user_id`, `via_token`, `action` and `target`, and `incidentAuditTarget`
    (`internal/store/incidentaudit.go`) appends the actor's label to the target only for a token;
    a "fourth actor shape", as revision 2 proposed, would be validated and then stored nowhere. The
    channel is appended to the **target** the action already writes
    (`IncidentAcknowledgeTarget`): `incident <id> · acknowledged · via=sms` / `· via=voice`;
  - **the result must be told apart.** `AcknowledgeIncidentByPrincipal` returns one `ErrNotFound`
    for a missing incident and for a resolved one, and no word on whether this call was the first
    acknowledgement. The phone path gets a result — acknowledged now; already acknowledged, and by
    whom (display name); already resolved — so the reply can say which;
  - an `incident.acknowledge` by phone also inserts a `phone_actions` row (§5.5) in the same
    transaction.

### 5.3 Snooze

`SNOOZE <code> <minutes>` (1–120) or keypress 2 (30 min) **pauses the ladder's clock** (AUTHOR'S
CALL, builder check Q4). The code may be left out when exactly one code is open for the sender's
number, as with `ACK`: `SNOOZE 45` then means 45 minutes — a code has six digits and minutes at
most three, so the two cannot be confused — and `SNOOZE` alone means 30. With several open codes
the reply lists them and snoozes nothing. `AdvanceEscalations` measures every step offset from `dueBase` and fires
all steps that have come due in one pass, so "skip while snoozed" would dump every step that fell
inside the snooze at its end.

**Who and when.** The rules of acknowledgement: a user-bound delivery, the sender's current
verified number, the acknowledge permission at reply time (P11), the principal door
(`SnoozeIncidentByPrincipal`), an audit row `incident.snooze` with the target
`incident <id> · snoozed <m>m · via=sms|voice` (the minutes actually granted), a `phone_actions`
row. Only an open, unacknowledged incident **that has a ladder** can be snoozed; otherwise the
reply says why not and nothing is written — no column, no audit row, no `phone_actions` row. "Has
a ladder" is what `AdvanceEscalations` itself requires (`internal/store/escalation.go`): for a
monitor's incident, the monitor has an escalation policy; for a service's incident, the snapshot
frozen at open exists **and** the service still has a policy attached and still owns paging — an
operator who detached the policy stopped the ladder, and there is nothing left to snooze.

**The model.** Four columns on the incident, all written under the incident's row lock:

- `escalation_snoozed_at`, `escalation_snoozed_until` — the current snooze, or both NULL;
- `escalation_paused_total` — the sum of every **settled** snooze;
- `escalation_paused_at_step` — the pause as it stood when `last_escalated_at` was last written
  (used by `repeat_last`, below).

The pause in force at an instant `T` is a pure function of the row:

```
pause(T) = escalation_paused_total
         + (snoozed_at IS NULL ? 0 : min(T, snoozed_until) - snoozed_at)
```

- Both candidate queries (the monitor ladder, which reads its policy live; the service ladder, which
  reads the snapshot frozen at open) skip an incident while `now() < escalation_snoozed_until`, and
  measure step offsets from `dueBase + pause(now())`.
- Nothing is accrued "on the first pass after the snooze ends" — revision 2 did that, and an
  incident that was not selected at that moment (a monitor in a recovery run under FR-043, a
  maintenance window) would have lost the pause. An expired snooze simply stays in the two columns
  and keeps counting through `pause`.
- **A new snooze settles the old one**: it adds the old one's elapsed part —
  `min(now, snoozed_until) - snoozed_at` — to `escalation_paused_total` and then sets the new pair.
  So a 30-minute snooze replaced after 20 minutes by another 30 pauses 50 minutes in all, not 30
  and not 60.
- **The cap.** The total pause of one incident is at most 4 h (**AUTHOR'S CALL**) so a ladder
  cannot be snoozed forever. A request that would exceed it is shortened to what is left and the
  reply says so ("Snoozed for 12 minutes — the 4 hour limit is reached."); with nothing left it is
  refused.
- **`repeat_last`.** A repeat is due `renotify_seconds` after `last_escalated_at`, not after
  `dueBase`, so shifting `dueBase` alone would not hold a repeat back. A repeat is due at
  `last_escalated_at + renotify_seconds + (pause(now()) - escalation_paused_at_step)`.
  `escalation_paused_at_step := pause(now)` is written **whenever, and only when,**
  `last_escalated_at := now` is: on a step that resolved at least one target, and on every repeat
  that did (`AdvanceEscalations` moves `last_escalated_at` in both branches and in neither when no
  target resolved). Written on steps only, the pause would be added again to every later repeat:
  last step at 0, a 30-minute snooze at +2, `renotify_seconds` of 10 minutes — the first repeat
  fires at +40 and the second must fire at +50, not +80.

So a ladder of 0 / +5 / +10 min snoozed for 30 minutes at +3 fires its +5 step at +35, its +10 step at
+40 — the same spacing, shifted. The step that fires re-checks FR-023's pageability predicates then,
not at snooze time. Snooze never acknowledges, never resolves, and does not suppress non-ladder
alerts. An incident with no ladder is not snoozed: the reply says "no escalation is scheduled".

### 5.4 Voice retry

On a `callback_status` of `no-answer`, `busy` or `failed` for a voice delivery, the call is placed
again 120 s later, at most once (P8). A retry is not a ladder step and does not move
`escalation_step`.

- **Only for a delivery that belongs to an incident**, judged when the retry is due — by then a
  monitor's row has been attached to its incident (D32). A burn alert and a monitor without
  `auto_incident` have nothing whose acknowledgement could cancel a retry.
- **Scheduling**: the callback sets `retry_at` on the delivery row. The scheduler leader claims due
  rows with `FOR UPDATE SKIP LOCKED`, sets `retry_claimed_at` and **commits** — and only then
  calls the provider. Holding the transaction open across a 5 s HTTP call would pin a pool
  connection per call, and a leader change during the call would let the next leader claim the
  same row.
- **The retry is its own row**, with `retry_of` naming the first (unique, so there is one). The
  first call's provider reference, duration and late callbacks stay on the first row; revision 2
  had one row and would have overwritten them.
- **Checked again at retry time**, in this order: the incident is still open and unacknowledged
  (`status <> 'resolved'` **and** `acknowledged_at IS NULL` — an auto-incident that recovered is
  resolved without ever being acknowledged, and revision 2 would have called about it); the channel
  is still a route (D31); telephony is enabled; the storm guard, the quota and quiet hours allow
  it. A check that fails closes the retry row with its reason and places no call: `incident_settled`
  for the first, and the ordinary reasons of §4.5 for the others.

### 5.5 What the operator sees

Acknowledgements and snoozes made from a phone are rows of `phone_actions` (§8): who, when, `sms`
or `voice`, which delivery, the minutes of a snooze. With the incident's `phone_deliveries` they
make the **Phone** panel of the incident page: every attempt with its reason, every action with its
actor. `GET /api/v1/incidents/{incidentID}/phone-activity` (`project:read`) returns both lists. The public
status page, the incident timeline, the feeds, webhooks and subscriber mail carry none of it (D22,
P19).

## 6. Bounds

The storm guard is FR-041a (D26); everything else in this section is FR-041c.

### 6.1 Quotas (FR-041c)

Per project and per instance, per calendar month (UTC), separately for SMS segments and call
minutes, configurable in the telephony settings (default: unlimited instance, unlimited project —
the operator opts in). At 100 % phone delivery for that scope stops with reason `quota_exceeded`.

- **Atomic.** Several processes deliver in parallel, each with several workers; a count of delivery
  rows read before the call lets two of them spend the last unit. Usage is a counter row per
  (scope, month, kind), reserved with one statement that succeeds only if it fits
  (`UPDATE … SET used = used + $n WHERE used + $n <= limit`) — before the provider is called (P13).
- **SMS** reserves the segments the rendered message has. A rejected or unreachable delivery
  returns its reservation.
- **Voice** cannot know its length in advance. A call reserves **one minute** when it is placed;
  the status callback's duration adjusts it by `ceil(duration_s / 60) - 1`: a call that was never
  answered (`duration_s = 0`) gives its minute back. The adjustment goes to the month of the
  delivery's `created_at` (UTC), not of the callback, and is **unconditional** — the minutes were
  spoken, so it is not refused for exceeding the limit; `used` never goes below zero. Without
  callbacks (no `public_base_url`) a call counts one minute — the settings say so beside the quota.
- **What else counts.** Verification codes of users and the instance's test SMS count against the
  **instance** SMS quota; channel-number verification and a project override's test SMS (§6.5)
  against the project's. A reply to an inbound SMS counts against the scope of the route key the
  request arrived on (§5.1): the instance's, or the project's when the project has its own
  provider account. None of them is
  subject to the storm guard — they have their own limits (§4.2, §5.2).
- **Warning.** At 80 %, once per month per scope: project admins for a project quota, global
  admins for the instance quota.

### 6.2 Storm guard (FR-041a)

At most **5** phone messages per destination number per incident per hour, and at most **20** per
destination number per hour across incidents (**AUTHOR'S CALL**, both configurable in §4.1).
Beyond that the delivery is `storm_limited`. A flapping monitor must not ring one phone sixty
times.

- Counted from `phone_deliveries` by the number's hash (under every configured key, D35), inside
  the reservation transaction, over the rows that hold a slot: `sending`, `sent`,
  `provider_rejected` and `outcome_unknown`. `sending` must be in the count: the reservation
  commits before the HTTP call (D33) and its advisory lock on the hash ends with that commit, so
  for the seconds a call is in flight the reserved row is the only thing a second worker can see.
  Counting only finished rows would let every parallel worker take the last slot (P13). A call
  that ends `provider_unreachable` frees its slot.
- "Per incident" needs an incident. A delivery for a monitor whose incident is not found yet (D32)
  is counted against the **monitor**; one without any subject of that kind (a burn alert) only
  against the per-number limit.
- In FR-041a there is no fallback channel: a storm-limited delivery is recorded, counted in the
  metric and shown in the Phone panel, and that is all (D26).

### 6.3 Fallback, never silent (FR-041c)

A phone channel may name a `fallback_channel_id`: a non-phone channel of the same project. It is a
typed column with a composite foreign key on `(project_id, fallback_channel_id)`, `ON DELETE SET
NULL`, and a CHECK against self-reference; that the target is not a phone channel is checked at
write and again at send. It is a tenant reference and is registered where those are
(`internal/store/tenantreferenceowners_test.go`,
[`cross-contract-conformance.md`](cross-contract-conformance.md)).

Every delivery that is **deliberately not phoned** — `telephony_disabled`,
`voice_unsupported_by_provider`, `storm_limited`, `quota_exceeded`, `quiet_hours_deferred` — is
delivered to the fallback channel with the reason prepended. For the first two a live fallback is
also what keeps the channel a route (D31 rule 1). The outcome is `fallback_status`, beside `reason` and never instead of it:

| `fallback_status` | Meaning |
| --- | --- |
| `none` | the channel names no fallback |
| `delivered` | the fallback accepted it — this is what makes the delivery count as `Delivered` (D20) |
| `failed` | the fallback's delivery failed; the event fails, the outbox retries, and the retry redoes **only** the fallback for this row |
| `unavailable` | the fallback is deleted, disabled or itself a phone channel |

Without a delivered fallback the reason is still recorded and counted (P7), and D31 says what the
service sees.

### 6.4 Quiet hours (FR-041c)

Per user, on `PATCH /api/v1/me/phone/preferences`: `quiet_hours: { "tz", "start", "end" }` or
`null`.

- `tz` is an IANA zone name the server can load; `start` and `end` are `HH:MM` wall-clock times in
  it. A window may cross midnight (`23:00`–`07:00`). `start = end` is refused (it would mean "never"
  or "always", and neither is what a person typing it meant). The window is evaluated in wall-clock
  time, so on a daylight-saving change it is an hour longer or shorter, as the clock on the wall is.
- During the window a `user`-mode channel delivers only what D17 lets through. Everything else is
  `quiet_hours_deferred` and goes to the fallback channel (§6.3).
- For D23, a service incident without `impact_basis` (opened before FR-042) grades `down` as not
  `critical`: a paged `unknown` on it does not break quiet hours.
- Deferred alerts are **not** queued and replayed in the morning (a stale DOWN at 08:00 is noise —
  **AUTHOR'S CALL**, supported by the review).
- A `number`-mode channel has no quiet hours: a shared phone belongs to whoever is on shift.

### 6.5 Per-project provider override (FR-041c)

A project admin may store a project telephony block with the same fields as §4.1 (minus
`public_base_url`, which is per instance because the inbound URL is). Delivery for that project's
channels uses it — it is the project's **effective** configuration for D27's barrier, for D3's
voice rule and for the inbound routes (§5.1), whatever the instance block says. Quotas then count
against the project only. Encrypted per project, AEAD-bound to the project id like project secrets,
and covered by `cerbix reencrypt`. A "send test SMS" action exists here too, to the calling
admin's own verified number; it counts against the project's quota.

## 7. Data protection

- **Numbers in full: nowhere on read** (D35). API responses, the owner's own included, carry a
  mask. Logs and metrics carry `channel_id`, `user_id` and a mask at most; an audit target carries
  no number.
- **At rest**: ciphertext, keyed hash, mask (D35). Telephony cannot be enabled without the at-rest
  key (D29).
- **Provider answers**: status and code, no text (D34).
- **User text in messages** is normalised and encoded for the medium it lands in (§4.4).
- **Recipients**: the recipient list shows who has a verified number, never the number (§4.3).
- **Deleting a user** deletes their number (`ON DELETE CASCADE`) and disables their channels with
  a visible reason (§4.3).

## 8. Schema (sketch — the builder owns the exact DDL)

Marked with the part that needs it.

- (a) `user_phones (user_id PK FK users ON DELETE CASCADE, number_enc, number_hash UNIQUE,
  number_masked, verified_at NULL, language NULL, quiet_hours jsonb NULL, updated_at)`.
- (a) `phone_verifications (id, subject_kind user|channel, subject_id, number_hash, code_hash,
  attempts, expires_at, created_at)` — short-lived, purged by the scheduler.
- (a) `notification_channels` gains `recipient text NULL`, `recipient_user_id uuid NULL REFERENCES
  users ON DELETE SET NULL`, `number_enc`, `number_hash`, `number_masked`, `number_verified_at`,
  `disabled_reason text NULL`, and (c) `fallback_channel_id uuid NULL` with the composite foreign
  key of §6.3. CHECKs: the phone columns are NULL for the four existing types; `recipient` is set
  for `sms`/`voice` and decides which of `recipient_user_id` / `number_*` may be set.
  `notification_channels_type_chk` is widened to add `sms`, `voice` (the 00013 pattern: drop and
  re-add the constraint; the Down refuses while phone channels exist rather than deleting them).
- (a) `phone_deliveries (id, project_id, channel_id, outbox_event_id NULL, incident_id NULL,
  monitor_id NULL, monitor_seq bigint NULL, service_id NULL, user_id NULL, number_hash,
  number_masked, kind sms|voice,
  alert_kind, reason, notes, provider_status NULL, provider_code NULL, provider_ref NULL, segments,
  attempts, created_at, updated_at)`, and (b) `ack_code NULL`, `ack_open boolean NOT NULL DEFAULT
  false`, `callback_status NULL`, `duration_s NULL`, `retry_at NULL`, `retry_claimed_at NULL`, `retry_of uuid NULL UNIQUE`,
  and (c) `fallback_status`. Retention 90 days. Indexes: unique `(outbox_event_id, channel_id)
  WHERE outbox_event_id IS NOT NULL` (D33) — `outbox_event_id` is a plain uuid, not a foreign key;
  unique `(number_hash, ack_code) WHERE ack_open`; `(number_hash, created_at)` for the storm guard.
- (b) `phone_actions (id, project_id, incident_id FK ON DELETE CASCADE, user_id, delivery_id,
  action ack|snooze, via sms|voice, minutes NULL, created_at)`.
- (a) `incidents.ref text NOT NULL UNIQUE DEFAULT <a database function>` (D21, D27): eight Crockford
  base32 characters, generated in the database so that an `INSERT` that does not name the column —
  every insert of an older binary — still succeeds. Backfilled by the migration. The function draws
  candidates until one is not in the table, so a collision is not left to the unique constraint.
  Two inserts that draw the same value at the same moment can still meet there, and **no retry is
  added for that**. Revision 4 asked `createIncident` to retry; it cannot without a savepoint — a
  unique violation aborts the transaction it is in — and incidents are also inserted by
  `OpenServiceIncidentTx` (`internal/store/serviceincidents.go`), inside the evaluator's batch
  transaction. Every writer already has a retry above it: the reconciler tries again after 250 ms,
  the evaluator on its next pass, a person or an Alertmanager on the next request. For two
  concurrent draws of one value out of 2^40 that is enough, in the new binary and in an older one.
- (b) `incidents.escalation_snoozed_at timestamptz NULL`, `escalation_snoozed_until timestamptz
  NULL`, `escalation_paused_total interval NOT NULL DEFAULT '0'`, `escalation_paused_at_step
  interval NOT NULL DEFAULT '0'` (§5.3). With all four at their defaults `pause` is zero and both
  ladder queries compute what they compute today.
- (b) `telephony_routes (route_key PK, scope instance|project, project_id NULL)` for §5.1.
- (c) `phone_quota_usage (scope_kind instance|project, scope_id, month, kind sms_segments|
  call_minutes, used, PRIMARY KEY (scope_kind, scope_id, month, kind))`.
- (a) `instance_settings.telephony` (secret fields encrypted), with `enabled` and `provider`
  readable by the SQL predicates of D31 (D36); (c) `project_telephony`, the same.
- None of this touches `heartbeats`; every table is an ordinary table in both storage modes.

**An older binary on this schema** (D27): its incident inserts work (the `ref` default); its ladder
queries ignore the snooze columns and fire deferred steps early; its dispatcher fails an event that
has a phone channel among its recipients. The runbook's order — telephony on last, phone channels
off first — is what keeps the third from happening.

## 9. Surfaces

API — OpenAPI first, then `npm run gen:api`. A single channel is mounted flat, under
`/api/v1/notification-channels/{channelID}`, as its PATCH and DELETE are today; only the collection
is under the project. `internal/api/requestcontract_test.go` checks one registry today (the status
page's request contracts); the new request bodies need a registry of their own beside it.

| Part | Method and path | Permission | Notes |
| --- | --- | --- | --- |
| a | `GET`, `PUT /api/v1/settings/telephony` | global admin | secrets as `*_set` booleans; blank keeps |
| a | `POST /api/v1/settings/telephony/test-sms` | global admin | to the caller's verified number |
| a | `PUT`, `DELETE /api/v1/me/phone`; `POST /api/v1/me/phone/verify` | signed-in human | |
| a | `PATCH /api/v1/me/phone/preferences` | signed-in human | language (a); quiet hours (c) |
| a | `GET /api/v1/me` | — | gains `phone: { number_masked, verified, language }`; `quiet_hours` from (c) |
| a | `POST /api/v1/projects/{projectID}/notification-channels`; `PATCH`, `DELETE /api/v1/notification-channels/{channelID}` | D28 | the typed fields of §4.3 |
| a | `POST /api/v1/notification-channels/{channelID}/verify/send`; `POST /api/v1/notification-channels/{channelID}/verify` | `project:manage` | `number` mode |
| a | `GET /api/v1/projects/{projectID}/phone-recipients` | `project:write` | §4.3 |
| a | `GET /api/v1/notification-channels/{channelID}/phone-deliveries` | `project:read` | last 50 |
| a | `GET /api/v1/incidents/{incidentID}/phone-activity` | `project:read` | deliveries; actions from (b) |
| a | `GET /api/v1/incidents/by-ref/{ref}` | `project:read`, after the 404 step | D21 |
| b | `POST /api/v1/telephony/{route_key}/sms`, `/voice/gather`, `/status` | provider signature | form bodies, TwiML answers (§5.1) |
| b | `POST /api/v1/settings/telephony/regenerate-routes` | global admin | |
| c | `GET`, `PUT`, `DELETE /api/v1/projects/{projectID}/telephony`; `…/telephony/test-sms` | `project:manage` | §6.5 |
| c | `GET /api/v1/settings/telephony/usage`; `GET /api/v1/projects/{projectID}/telephony/usage` | global admin; `project:read` | quota read-out |

There is no "test this channel" action for a phone channel, as there is none for the other channel
types today; the test SMS of the settings is the only test send.

Audit actions added: `settings.telephony.update`, `user.phone.set`, `user.phone.verify`,
`user.phone.delete`, `incident.snooze`, `project.telephony.update`, `project.telephony.delete`,
`telephony.routes.regenerate`; `incident.acknowledge` gains the `via=` suffix (§5.2).

Channel writes are **not audited at all today** — `internal/api/handlers_notifications.go` and
`internal/store/notifications.go` write no audit row; revision 3 said otherwise. A phone channel
binds a person or a number to paging, so its writes are audited from FR-041a:
`notification_channel.create`, `.update`, `.delete` and `.verify`, for channels of type `sms` and
`voice`, with the target `channel <id> · <type>` and never a number. The four existing types stay
unaudited here: closing that gap is its own change, not a side effect of this one (AUTHOR'S CALL).

SPA — **a mock must be approved before any frontend code**; one mock per part:

1. (a) Settings → Administration → Telephony: provider form, test SMS, encoding, storm limits, the
   warnings before switching telephony off or to a provider without voice (§4.1); (b) the inbound
   URLs; (c) quotas and usage.
2. (a) Profile → Phone: add, verify, language; (c) quiet hours.
3. (a) Channel editor: `SMS` / `Voice call`, recipient mode, the recipient picker, verification
   state, "not a route" with its reason, the warning when it is a service's only route; (c) the
   fallback channel.
4. (a) Incident page: the **Phone** panel (§5.5); (b) acknowledgements and snoozes in it.
5. (a) `/i/:ref`: the redirect, and what a signed-in user without access to the incident sees.
6. (c) Project settings → Telephony: the override, its test SMS, the project's usage.

Metrics — labels are low-cardinality by rule (`internal/metrics/metrics.go`), so no project id is
one: `cerbix_phone_deliveries_total{kind,provider,reason}`,
`cerbix_phone_inbound_total{kind,result}`, `cerbix_phone_quota_events_total{scope,kind,event}`
(`scope`: `instance`, `project`, `storm`; `event`: `warn_80`, `exceeded`), and
`cerbix_phone_instance_quota_used{kind}` — a gauge read from the database, exported by the
scheduler leader and cleared on step-down like the other leader gauges. Per-project usage is an API
read-out, not a metric.

## 10. Acceptance invariants

- **P1** — No SMS or call is ever sent to a number that is not verified, except the verification
  code; and the destination of a `generic_http` request cannot be altered by any user-chosen text.
- **P2** — A phone delivery is subject to exactly the suppression the same alert gets on a Slack channel
  (silence, maintenance, dependency ancestors, escalation flat-down); proven by a table test that runs
  every suppression case against both channel types.
- **P3** — Telephony credentials and full numbers never appear in an API response, a log line, a
  metric label, an audit target, an export bundle or a dispatch carrier.
- **P4** — A `user`-mode channel resolves the number at send time; changing or deleting the number
  takes effect on the next delivery with no channel edit.
- **P5** — `generic_http` cannot be the provider of a `voice` channel: refused at write time against
  the project's effective provider, and not phoned at send time.
- **P6** — A delivery that cannot be made carries a reason from the closed set in §4.5; none is
  reported as `sent`.
- **P7** — No alert disappears without a trace: every alert a phone channel was handed (§4.5) and
  did not send has a row, a metric count and — when it belongs to an incident — a line in that incident's
  Phone panel.
- **P8** — A voice retry happens at most once per delivery, never for a resolved or acknowledged
  incident, never for a delivery without an incident, and never moves the ladder.
- **P9** — A message never offers an action that cannot work: no `ACK` text or keypress prompt without
  an active inbound path and a user-bound delivery.
- **P10** — An inbound request with an unknown key, an inactive configuration or a signature that
  does not verify changes nothing and gets the same 403 in all three cases; the signed URL is built
  from configuration, not from request headers.
- **P11** — Acknowledgement and snooze by phone require the sender's number to be the user's CURRENT
  verified number and the user to hold acknowledge permission on the project at reply time.
- **P12** — Snooze defers only the ladder, is capped at 4 h in total per incident, and the deferred
  step re-checks FR-023's pageability predicates when it fires.
- **P13** — Quota and storm limits are enforced before the provider is called and hold under
  concurrent deliveries from several processes: N parallel deliveries against a limit with one unit
  left produce one provider call.
- **P14** (NFR-034) — For a project with no phone channel, before and after FR-041: the rows
  enqueued in `outbox_events` (topic and payload bytes), the webhook request bodies, the text sent
  to every channel, and the escalation steps fired with their times are equal. `domain.Incident`'s
  JSON is unchanged for every project (D21). Not compared, because they are additive: new columns
  at their defaults, new API fields, new routes.
- **P15** — A phone channel receives exactly the alerts in D18's table; a test enumerates every
  outbox topic and every predicate of the table and asserts it.
- **P16** — An outbox event retried N times produces at most one **accepted** provider call per
  channel; a row left in `sending` is never answered with a second call and never stays in
  `sending` (D19, D33).
- **P17** — A snoozed ladder resumes with its step spacing preserved, including when a snooze
  replaces another and when the incident was not selectable at the moment a snooze ended; a
  `repeat_last` is held for as long as the snooze lasted; no step fires twice and none is skipped.
- **P18** — A phone delivery that reached nobody never credits service coverage (D20), and does not
  make the service re-announce: a service alert whose only route was deliberately not phoned is
  enqueued once per episode, not once per evaluation (D31).
- **P19** — No phone event appears in `incident_updates`, on the public status page, in the incident
  history, in a feed, in a webhook payload or in subscriber mail (D22).
- **P20** — A phone channel that is not deliverable by configuration is not a route: a service whose
  only channel it is withholds its onset as an unroutable service does and does not hold its
  members' alerts; making the channel deliverable produces the announcement (D31).
- **P21** — Nothing in a monitor's phone delivery depends on the incident row existing when the
  message is sent. By part: (a) the send happens either way, only the link can be absent and then
  the row says so, and the row is attached to the incident once it opens; (b) the ACK code is
  issued and works either way, and is closed with the incident either way; (c) the quiet-hours
  decision is the same either way (D32).
- **P22** — A phone channel cannot be created while telephony is not enabled for its project, and
  telephony cannot be enabled, nor a number or credential stored, without the at-rest key (D27,
  D29).

## 11. Test matrix (written before the code)

Store tests need no storage-mode branching — every new table is an ordinary table — and run in both
modes in the ordinary suite. The fake `Provider` records calls; the E2E stub provider is a container
that accepts outbound messages and calls and can send **signed** inbound requests back.

| Part | Layer | Cases | Invariants |
| --- | --- | --- | --- |
| a | Domain — message | septet counting incl. the two-septet characters; one and several segments in both encodings; a character outside the BMP; truncation order; link omitted; transliteration never empties a name; normalisation of control characters and line breaks; XML escaping in `<Say>`; no `ACK` text in part (a) | P9 |
| a | Domain — `generic_http` | JSON and form encoding of each placeholder; the injected-recipient name stays one value; a template with a placeholder outside a value is refused | P1 |
| a | Store — routes | each way a phone channel is not a route (unverified, recipient gone or not eligible, telephony off, voice under `generic_http`) × `routablePredicate` (both halves), `resolveServiceRecipientsTx`, step resolution; a service whose only channel becomes a route announces; a step with a **disabled Slack** channel is enqueued exactly as before | P20, P14 |
| a | Store — number at send | a `user` channel after its user replaced the number, verified the new one, deleted it: the next delivery goes to the new number, or the channel is no route — with no channel edit | P4 |
| a | Outbox — coverage | service alert × {delivered, each rule-3 reason of part (a), not a route at send}: credited / neither / condemned; the neither case evaluated five more times enqueues nothing; a snapshot of one text and one phone recipient raises no `service_alert_recipient_missing` | P18 |
| a | Outbox — reasons | every row of §4.5 that belongs to part (a) produced by its cause; each answer class of the classification table for both providers; no reason outside the set; none of them reported as `sent` | P6 |
| a | Trace | each not-`sent` outcome of part (a): the row, the metric count, the line in the incident's Phone panel | P7 |
| a | Outbox — D18 | every topic and every predicate of the table, against an `sms` and a `voice` channel | P15 |
| a | Outbox — retries | an event retried five times with one phone and one text channel: one accepted provider call; a `sending` row met at 10 s and at 30 s is not called and fails the event, and at 70 s closes as `outcome_unknown`; the leader's pass closes an orphaned one and leaves alone a row on its fourth attempt whose call is in flight; `provider_unreachable` is called again on the same row; the time budget of D33 with three phone channels | P16 |
| a | Outbox — monitor race | DOWN delivered before the reconciler opens the incident, and after: same send, link present only in the second, `no_incident_ref` in the first; reservation and incident opening run concurrently on PostgreSQL in both orders: the row always ends with the incident's id; a row of an earlier outage and a burn-alert row of the same monitor are not attached; recovery delivered before and after the reconciler resolves: the same incident linked; a recovery with only an older resolved incident: no link, and no note when `auto_incident` is off; the stored event's payload bytes unchanged | P21, P14 |
| a | Suppression | the P2 table, both channel types | P2 |
| a | Storm guard | N concurrent deliveries to one number at the limit, on PostgreSQL under `-race`: exactly the allowed number of provider calls; per-incident and per-number limits; the monitor-as-subject case | P13 |
| a | API | D28's matrix, create and update; `number` channel created disabled and unenablable before verification; number change clears it; nothing but a verification code is sent to an unverified number; the recipients endpoint for an editor and an admin; by-ref 404 before 403; no response carries a number; the audit rows of a phone channel's writes and none for a Slack channel's; deleting a user disables their channels with the reason | P1, P3, P22 |
| a | Settings | secrets kept on a blank write; refusal without the at-rest key; no YAML seed; `cerbix reencrypt` covers the new ciphertexts and recomputes hashes; between the key change and the reencrypt a number is still matched and still unique | P22 |
| a | Store — `ref` | the default never returns a value in use; an insert that does not name the column succeeds, through `createIncident` and through `OpenServiceIncidentTx` | — |
| a | Regression | a project without phone channels through an outage with a ladder: the P14 comparison, captured before the change and compared after | P14 |
| b | Inbound | Twilio's published signature vectors; unknown key, inactive configuration and bad signature all 403 with no row touched; the URL built from `public_base_url` behind a rewritten `Host`; form parsing and the size limit; TwiML content type of every answer | P10 |
| b | ACK | the code against the delivery's subject with the incident opened before and after the send; several open codes; a changed number; a revoked permission; ten wrong codes; already acknowledged (names who); already resolved; the audit target's `via=`; an unacknowledged incident that recovers closes its code, the row sent before the incident existed included, so the monitor's next outage has one open code; a service incident closed by `resolveServiceIncidentTx` closes its codes; `cerbix reencrypt` closes open codes; the `ACK` text and the keypress prompt present only with an active inbound path and a user-bound delivery, and "Press 2" only with a ladder | P9, P11, P21 |
| b | Snooze | 0/+5/+10 snoozed 30 at +3; replaced at +23; the incident unselectable when the snooze ends; the 4 h cap, shortened and refused; `repeat_last` with `renotify_seconds` longer than the snooze, and its **second** repeat one `renotify_seconds` after the first; a step with no target leaves `escalation_paused_at_step` alone; `SNOOZE 45` and `SNOOZE` with one open code and with two; an incident without a ladder writes nothing, a service whose policy was detached included; pageability re-checked at fire time | P12, P17 |
| b | Status | a callback never changes `reason`; a repeated callback is a no-op; (c) `duration_s = 0` returns the reserved minute, and two copies of one callback arriving together adjust the quota once | P6 |
| b | Voice retry | two leaders claiming one row; a leader change during the call; a resolved-unacknowledged incident; a delivery without an incident; the child row keeps the parent's reference | P8 |
| b | Timeline | after an ACK, a snooze and a not-phoned delivery: no new `incident_updates` row; the public page JSON, each feed and a captured webhook contain none of it | P19 |
| c | Quotas | N concurrent deliveries against one remaining unit; a rejected delivery returns its reservation; the one-minute rule with and without a callback; a call across a month boundary is adjusted in the month it was placed; an adjustment past the limit is applied; what the non-alert messages count against, a reply on a project's route key included | P13 |
| c | Fallback | each not-phoned reason, `voice_unsupported_by_provider` included × {delivered, failed then retried, unavailable}; the retry redoes only the fallback; a phone channel with a live fallback stays a route with telephony off | P7, P20 |
| c | Quiet hours | each rule of D17 and D23, D24; the same decision with the monitor's incident opened before and after the send; an incident without `impact_basis`; a window across midnight; both daylight-saving changes; `start = end` refused; `number` mode unaffected | P21 |
| c | Override | effective provider for the barrier, the voice rule and the inbound route when the instance block is off | P5, P22 |
| a–c | E2E, dev stack | (a) verify → channel → DOWN → delivery row; (b) signed inbound ACK → incident acknowledged; (c) a spent quota → fallback delivery | — |

## 12. Non-goals

- A telephony account, sender number or billing operated by cerbix.
- Two-way chat over SMS beyond `ACK`, `SNOOZE` and help.
- Resolving or reopening an incident from the phone. An acknowledgement cannot be undone either
  (there is no such operation in cerbix); resolving from a six-digit code, without looking at
  anything, invites closing an outage that is still going.
- Push notifications / a mobile app.
- Changing on-call schedules to rotate users instead of channels (D12).
- Phoning for manual or Alertmanager incidents (D30); any change to the Alertmanager receiver.
- Queuing quiet-hours alerts for morning replay (§6.4).
- Re-sending a service alert that was deliberately not phoned once the obstacle is gone (D31).
- A visibility flag on incident updates (D22).
- A mechanism that makes a pre-FR-041 process tolerate phone channels (D27).
- A YAML seed for telephony settings (D36).
- Audit rows for writes of the four existing channel types (§9).
- A `generic_http` gateway that reports failure inside a 2xx response (§4.5).

## 13. Open questions for review

None for the owner: the four questions of revision 2 were decided on 2026-11-16 (D13, D17, D23,
D25).

The second and third review passes agreed with every author's call, D36 and the three departures
of revision 4 included. For the confirming pass, what is new in revision 5: the per-monitor lock
and `monitor_seq` in D32; `outbox_events.created_at` as the recovery's time; no retry for a `ref`
collision (§8) — a different answer from the savepoint the third pass proposed.

## 14. Review history

**Builder check of revision 1 (2026-11-13).** Seven planning questions and five code
contradictions, folded into revision 2 as D15–D21, §4.5, §5.1, §5.3, §7 and P15–P18.

**External review 1 of revision 2 (2026-11-16, against `main` at `31815ae`): CHANGES REQUIRED,
P0 4 / P1 14 / P2 6 / P3 2.** It agreed with D12–D15 and D18–D21 in direction, with D16 and D17 in
part, and judged the a → b → c split sound with the storm guard moved forward. The four blocking
findings were checked against the code by the author; the rest are accepted as described, each with
the reviewer's file and line.

| Finding | Disposition |
| --- | --- |
| P0-1 — no Twilio call protocol, no inbound response format, no correlation for `/voice/gather` and `/status` | **Accepted.** §5.1: inline TwiML, the `d` parameter over a row reserved first (D33), TwiML answers |
| P0-2 — phone notes in `incident_updates` are public, collide with the one-note marker pattern, and need a system door from `internal/api` | **Confirmed** (`IncidentUpdate.PublicRedacted` keeps the body). The owner chose not to write them there at all (D22); `phone_actions` and the Phone panel instead. §5.5, P19 |
| P0-3 — resolved-but-undelivered never re-announces; a phone channel that cannot deliver still suppresses members | **Confirmed**, and resolved differently from the proposal. Condemning on `Delivered == 0` would re-announce on every evaluation — the reviewer's own follow-up note. D31: non-deliverable channels are not routes; a not-phoned route neither credits nor condemns. §4.6, P18, P20 |
| P0-4 — `monitor_transition` can be delivered before the reconciler opens the incident | **Confirmed.** D32, P21 |
| P1-1 — D18 does not split `service_alert` by signal and close reason; §12 claimed Alertmanager incidents phone | **Accepted.** D18 by predicate; D30 |
| P1-2 — the crash window after the provider call, the 8 s first budget, the unique key | **Accepted.** D33 |
| P1-3 — no `escalation_snoozed_at`; replacement, cap and `repeat_last` wrong; no permission or audit for snooze | **Accepted**, with a model that needs no accrual pass. §5.3, §8 |
| P1-4 — voice retry: lock across HTTP, one `provider_ref`, calls about resolved incidents and without an incident | **Accepted.** §5.4 |
| P1-5 — quota race; minutes unknown before the call; `fallback_delivered` overwrites the reason; the fallback reference | **Accepted.** §6.1–§6.3, `fallback_status` |
| P1-6 — membership model, the editor's missing recipient list, create rights, user deletion | **Accepted.** §4.3; the owner decided the rights (D28) |
| P1-7 — inbound: signed URL behind a proxy, 404 under a project override, per-IP limit before verification | **Accepted.** §5.1, P10 |
| P1-8 — `AuditActor` stores no `via`; the acknowledge door returns too little; snooze has no door | **Accepted.** §5.2, §5.3 |
| P1-9 — injection through names into the `generic_http` template, TwiML and the SMS text | **Accepted.** §4.4, P1 |
| P1-10 — there is no existing redaction of provider error text | **Confirmed.** D34: code, not text |
| P1-11 — empty at-rest key, reencrypt, `number_masked` inside `config`, the `smtp_password` contract, a number channel enabled at creation | **Accepted.** D29, D35, §4.1–§4.3 |
| P1-12 — `/i/:ref` has no resolver; `ref` on `domain.Incident` breaks P14 | **Accepted.** D21, P14 |
| P1-13 — burn alerts can never break quiet hours; no API for preferences; masked or full number for the owner | **Accepted.** The owner decided burn (D24); §4.2, §6.4; mask for everyone (D35) |
| P1-14 — the partial index is not DDL; `incidents.ref` breaks an older binary's inserts; mixed versions | **Accepted.** `ack_open`, the database DEFAULT, D27 |
| P2-1 — the em dash, two-septet characters, project name and long base URL | **Accepted.** §4.4 |
| P2-2 — `cerbix_phone_quota_used{scope,kind}` against the label rule and the leader gauge model | **Accepted.** §9 |
| P2-3 — no guess limit for ACK codes; which non-alert messages count | **Accepted.** §5.2, §6.1 |
| P2-4 — redirects, method and URL validation, the effective provider in P5 | **Accepted.** D3, D14, §4.1, P5 |
| P2-5 — §9 too thin for a mock | **Accepted.** §9: routes, six surfaces, audit actions |
| P2-6 — the test matrix misses P12, P13, P15–P18 and two fakes | **Accepted.** §11, D16 |
| P3-1 — D7 points at D11; D17 out of order | **Fixed.** |
| P3-2 — "acknowledge is reversible" | **Fixed.** §12 |

The reviewer's notes on the owner's nine decisions (2026-11-16) are folded in: inbound actions have
their own table (§5.5); the re-announce loop is what D31 rule 3 exists to prevent; the storm guard
in FR-041a records without a fallback and counts against the monitor when the incident is not yet
found (§6.2); the creation barrier reads the project's effective telephony, and `incidents.ref`
has a database default independent of it (D27).

**External review 2 of revision 3 (2026-11-17, against `main` at `72411cb`): CHANGES REQUIRED,
P0 0 / P1 10 / P2 6 / P3 2.** All four blockers of the first pass were judged removed at the level
of the model; of the 26 first-pass findings 16 were closed and 10 closed in part, each remainder
being one of the findings below. The reviewer agreed with every author's call — D12, D14, D15, D18,
D20, D21 outright; D16, D19, D31–D35 and the snooze model on condition of the corrections below.
Every code claim was checked by the author against `72411cb` before it was accepted.

| Finding | Disposition |
| --- | --- |
| P1-1 — adding the phone call's counts to the text call's doubles `Requested` | **Confirmed** (`DeliverChannelsReporting` sets it to the snapshot's length). `Requested` counted once. D16, §4.6 |
| P1-2 — `voice_unsupported_by_provider` missing from the fallback list; D31's cost understated: with `renotify_seconds = 0`, and always for burn, the silence is total; telephony off silences a critical outage by day | **Confirmed.** §6.3 gains the reason. Telephony in effect is part of rule 1, so both cases become non-routes. The proposal to copy a YAML seed into the database at start was replaced by having no YAML seed (D36). The cost paragraph of D31 is rewritten |
| P1-3 — "the same predicate" in step resolution would add `enabled` for every type and break P14 | **Confirmed** (`channelInProject` does not test `enabled`). The condition is for `sms`/`voice` rows only. §4.6, and a regression case |
| P1-4 — rows with no `incident_id` leak an open ACK code into the next outage, miss the Phone panel and the voice retry; the recovery link assumes the reconciler ran first | **Confirmed** (the reconciler resolves after the UP event commits). Rows are attached when the incident opens; codes are closed by incident and by monitor; the recovery link takes the open incident first. The link rule differs from the proposal — it uses the event's own time, which the payload has, not the start of the outage, which it has not — and its price is stated. D32, §5.2, §5.4 |
| P1-5 — a retry at 10 s or 30 s meets a `sending` row younger than 60 s; returning success strands it | **Confirmed** (leases of 10 s and 20 s). A retryable error, and a leader pass for orphans. D19, D33, §4.6, P16 |
| P1-6 — the storm guard does not count `sending`, and its lock ends with the reservation's commit | **Accepted.** `sending` and `outcome_unknown` hold a slot. §6.2 |
| P1-7 — `escalation_paused_at_step` is not updated on a repeat, so the pause is added to every later repeat | **Confirmed** (`AdvanceEscalations` moves `last_escalated_at` in both branches). Written with `last_escalated_at`, always and only. §5.3 |
| P1-8 — no keyed hash in `secret.Cipher`; the command is `cerbix reencrypt`; rotation is two steps | **Confirmed.** D35: HMAC under a derived sub-key, lookups under every configured key, `reencrypt` closes open codes. D29, §6.5 |
| P1-9 — channel writes are not audited today | **Confirmed.** The false sentence is gone; phone channels are audited, the existing types are not (author's call). §9 |
| P1-10 — a part-(a) test depends on (b) and (c); reasons outside the closed table; `GET /api/v1/me` differs between §4.2 and §9 | **Accepted.** P21 by part; `not_deliverable` and `incident_settled` in the table; one shape of `phone` |
| P2-1 — what `/status` changes; the quota adjustment at a month boundary and at zero duration | **Accepted.** `callback_status`; `reason` is not rewritten; §6.1 |
| P2-2 — no classification of provider answers; a bare 400 from `generic_http` | **Accepted.** The table in §4.5; `response_code_field` (D34); a failure inside a 2xx is a non-goal |
| P2-3 — elided channel paths; the audit target of acknowledge and snooze; the request-contract registry | **Accepted.** §9, §5.2, §5.3 |
| P2-4 — a `ref` collision reaches `createIncident` as a 500 | **Accepted.** The function avoids values in use; one retry in the new binary. §8 |
| P2-5 — P4 and P6 in no test row; P7 only in part (c); P1's first half unmarked | **Accepted.** §11 |
| P2-6 — bare `SNOOZE` and a snooze without a ladder; surrogate pairs; `impact_basis IS NULL`; the order inside `DeleteUser` | **Accepted**, and a snooze without a ladder now writes nothing. §5.3, §4.4, §6.4, §4.3 |
| P3-1 — "three columns" for four | **Fixed.** §5.3 |
| P3-2 — "three SPA surfaces" for five | **Fixed.** §2 |

**External review 3 of revision 4 (2026-11-17, against `main` at `651ee92`): CHANGES REQUIRED,
P0 0 / P1 3 / P2 4 / P3 1.** Of the 18 second-pass findings 15 were closed and 3 closed in part,
each remainder being one of the findings below. The reviewer agreed in full with D36, with the
recovery-link rule and its price, and with auditing phone channels only. Every code claim was
checked by the author against `651ee92`.

| Finding | Disposition |
| --- | --- |
| P1-1 — `MonitorTransition.At` is never set; the attachment on DOWN races under READ COMMITTED, has no usable time bound and would attach a burn-alert row | **Confirmed** (no writer of the payload sets `At`). The recovery's time is `outbox_events.created_at`; `no_incident_ref` only with `auto_incident` on. Reservation and opening are serialised — by an advisory lock per monitor rather than the proposed lock on the monitor's row, which every check result updates. Rows are matched by alert kind and by `monitor_seq`, not by time. D32, §8 |
| P1-2 — a channel kept a route by its fallback contradicts §4.5, the snapshot paragraph of D31 and the warnings of §4.1 | **Accepted.** `Resolved` follows rule 1, never the reason; the three places agree |
| P1-3 — the `ref` retry misses `OpenServiceIncidentTx` and cannot work inside an aborted transaction | **Confirmed**, and resolved differently: the retry is removed, not wrapped in savepoints. Every writer already has a retry above it. §8 |
| P2-1 — the 60 s of a `sending` row must run from `updated_at` | **Accepted.** D33 |
| P2-2 — intermediate status events; two copies of one callback adjust the quota twice | **Accepted.** §5.1 |
| P2-3 — "has a ladder" for a service; "Press 2" offered without one | **Confirmed** (`AdvanceEscalations` also requires the policy still attached and `owns_paging`). §5.3, §4.4 |
| P2-4 — which quota an inbound reply counts against under a project override | **Accepted.** The scope of the route key. §6.1 |
| P3-1 — part marks in §11; D36's claim about other settings blocks | **Fixed.** |

**Confirming review pass of revision 5 (2026-11-17, against `main` at `23184bf`): APPROVED,
P0 0 / P1 0 / P2 0 / P3 0.** All eight findings of the third pass closed. The two places where
revision 5 departed from the third pass's proposals were checked against the code and agreed with:
the advisory lock with `monitor_seq` (no deadlock with the check-result transaction, which takes
only the monitor's row; `state_sequence` moves only on a transition that ends the outage, and a
row left without an incident by such a transition gets no voice retry and has its code closed by
the monitor-wide rule of §5.2), and no retry for a `ref` collision (the two inserts into
`incidents` have four callers, each with a retry above it; a failed evaluator batch rolls back
whole and is recomputed on the next pass). One note for the builder, now in D32: the lock and the
attachment belong to a monitor's auto-incident only.
