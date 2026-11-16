# func-phone-alerts — an alert that reaches a person: SMS and voice calls (FR-041 / NFR-034)

> **STATUS: revision 3 — DRAFT, not approved.** Revision 3 (2026-11-16) answers the first external
> review of revision 2 (CHANGES REQUIRED, P0 4 / P1 14 / P2 6 / P3 2 — dispositions in §14) and
> records the owner's decisions of 2026-11-16 (D13, D17 and D22–D30). What changed in substance:
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
reference, the delivery path and three SPA surfaces — and the builder may split it into a backend
iteration and an SPA iteration.

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
encrypted (D35), and `cerbix secrets reencrypt` (`internal/store/reencrypt.go`) covers them.

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
is a configuration answer, recorded as `provider_rejected` and not retried.

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
  (domain.ChannelDelivery, error)` on the `NotifyDeliverer` interface. The counts are **added** to
  the text path's counts wherever one is consumed, which today is the service-alert path only
  (`deliverServiceAlert`, `internal/outbox/outbox.go`); D31 says what they mean.
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
and it is NOT counted as `Delivered` (D20). Only `provider_unreachable` returns an error and lets
the outbox retry — and that retry re-delivers the event's text channels, as a failing webhook does
today.

**D20 — what counts as `Delivered` for service coverage. AUTHOR'S CALL (builder check Q7).**
`ChannelDelivery.Delivered > 0` credits service coverage and silences the members' own alerts
(D-0179). A phone delivery counts as delivered only when the provider accepted it, OR its fallback
channel (§6.3) accepted the redirected alert. Everything else is NOT delivered — so the members
keep paging on their own channels. The polarity is deliberate: when in doubt, page.

**D21 — incidents get a short reference for the phone. AUTHOR'S CALL (builder check, §4.4).** Incident
ids are UUIDs and the SPA route is `/incidents/:id`; a UUID link spends ~60 of 160 characters.
`incidents.ref` — 8 characters, Crockford base32, unique, generated by a database DEFAULT (D27),
backfilled by the migration — plus an SPA route `/i/:ref`. The route needs a server resolver, which
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

1. **Deliverable by configuration.** A phone channel counts as a route — in both SQL predicates, in
   the resolution of an escalation step's channels, and in `ChannelDelivery.Resolved` at send — only
   when it is enabled and its recipient exists: in `number` mode the number is verified; in `user`
   mode the recipient is a human user who is an eligible recipient of the project (§4.3) and has a
   verified number. All of it is in typed columns (§8), so the predicates stay SQL. A service whose
   only channel is an unverified phone is therefore *unroutable*, exactly as a service with no
   channel is: its onset is withheld, its members page for themselves, and fixing the channel
   produces the announcement.
2. **Not credited.** A phone delivery credits coverage only as D20 says.
3. **Not condemned either.** A channel that is a route but was not phoned — `telephony_disabled`,
   `storm_limited`, `quota_exceeded`, `quiet_hours_deferred`, `provider_rejected`,
   `voice_unsupported_by_provider`, `outcome_unknown` — leaves the service alert *announced and
   uncredited*: no `MarkServiceAlertDelivered`, no `MarkServiceAlertUndeliverable`. Condemning it
   would make the evaluator re-announce on its next pass, fifteen seconds later, to the same
   channel with the same answer, all night — and §6.4 refuses even one morning replay. The existing
   condition (`err == nil && res.Resolved == 0`) stays as it is and keeps meaning "there was nobody
   to tell"; rule 1 is what makes it true for phone.

The cost is stated, not hidden: with a phone channel as a service's **only** route, an alert that
is deliberately not phoned — telephony switched off, quiet hours without a fallback, a spent quota —
is not re-sent when the obstacle goes away, and a member's first DOWN may have been held during the
service's confirmation window. The members' later alerts and reminders are not held (coverage was
never credited). The remedies are a fallback channel (FR-041c), a second channel, or
`renotify_seconds`; the SPA warns where a phone channel is a service's only route and has no
fallback, and the telephony settings warn before telephony is switched off while such services
exist (§9).

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
- **a recovery** finds the reconciler ahead of it: the incident is already resolved. The link of a
  recovery SMS points at the monitor's most recently resolved auto-incident; a recovery carries no
  ACK code.

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
- a retry that finds a final row does nothing for that channel; one that finds
  `provider_unreachable` calls again and updates the same row (its `attempts` grows);
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
project viewer. So a delivery row stores the HTTP status and a short provider code (Twilio's numeric
error code; nothing for `generic_http`), validated as `[A-Za-z0-9_.-]{1,32}`, and no free text.
Transport failures use `notify.DeliveryFailure` as every other channel does.

**D35 — numbers are stored as ciphertext and a keyed hash, and shown masked to everyone. AUTHOR'S
CALL (review P1-11, P1-13).** A number is kept in three forms: AEAD ciphertext (to send), a keyed
hash derived from the at-rest key (to match an inbound sender and to enforce uniqueness and the
storm guard without decrypting), and a mask (`+90•••••6869`) stored at write time (to display).
Reads are decrypt-free: no API response carries a number in full, the owner's own `GET /api/v1/me`
included — revision 2 promised the owner the full number in one section and a mask in another, and
the mask wins; a user who needs to change the number re-enters it. `phone_deliveries` stores the
hash and the mask, never the ciphertext. Key rotation re-encrypts the ciphertexts and recomputes
the hashes of `user_phones` and channels; delivery rows keep their old hashes, so ACK codes open at
the moment of a rotation are closed by it (the link still works).

## 4. FR-041a — the channels

### 4.1 Instance telephony settings

A new `telephony` block in `instance_settings`, resolved DB → config → defaults like `mail`
(`internal/settings`). Global-admin only, live-reconfigurable, YAML is a bootstrap seed.

| Field | Meaning |
| --- | --- |
| `enabled` | master switch. Off: no phone channel can be created (D27) and existing ones are not phoned (`telephony_disabled`) |
| `provider` | `twilio` \| `generic_http` |
| `twilio.account_sid`, `twilio.auth_token`*, `twilio.from` | `from` is an E.164 number or a Messaging Service SID |
| `generic_http.method`, `.url`, `.headers`*, `.content_type`, `.body_template`, `.from` | method `POST` \| `PUT`; URL validated as a webhook URL is; `content_type` is `application/json` or `application/x-www-form-urlencoded` and fixes the placeholder encoding (§4.4); headers may carry credentials, so the whole map is secret |
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
to `generic_http` while `voice` channels exist is allowed and warned about: they stay, and are not
phoned (`voice_unsupported_by_provider`).

A **"Send test SMS"** action sends a fixed text to the calling admin's own **verified** number only.

### 4.2 Verified numbers

- `PUT /api/v1/me/phone` `{ "number": "+905…" }` stores it **unverified** and sends a 6-digit code
  by SMS. `POST /api/v1/me/phone/verify` `{ "code": "…" }` marks it verified.
  `DELETE /api/v1/me/phone` removes it. `GET /api/v1/me` gains
  `phone: { number_masked, verified }` (D35).
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
  `POST …/notification-channels/{id}/verify/send` and `…/verify`. Such a channel is **created
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

**Deleting a user.** `recipient_user_id` references `users` with `ON DELETE SET NULL`, and the
delete transaction (`DeleteUser`, `internal/store/users_admin.go`) disables the user's channels
with `disabled_reason = recipient_deleted`. A disabled channel is skipped by every resolver, as any
disabled channel is today; the trace is the reason on the channel, shown in the channel list.

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
  the brackets of the tag — or UCS-2 code units, 70 and 67.
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
Names are XML-escaped inside `<Say>`; a name containing markup is spoken, never interpreted.

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
| `provider_unreachable` | transport failure; the outbox retries | a |
| `provider_rejected` | the provider answered no (incl. a 3xx); final | a |
| `outcome_unknown` | a `sending` row found stale by a retry (D33) | a |
| `voice_unsupported_by_provider` | a `voice` channel under `generic_http` | a |
| `telephony_disabled` | the master switch is off | a |
| `storm_limited` | the storm guard refused it (§6.2) | a |
| `quota_exceeded` | a monthly quota is spent (§6.1) | c |
| `quiet_hours_deferred` | the recipient's quiet hours, non-critical alert (§6.4) | c |

A channel that is not a route (D31 rule 1) produces **no row for an alert it was never resolved
for**; when it stops being a route between an alert's recipient snapshot and its delivery, the row
carries `not_deliverable` with the cause (`no_verified_number`, `recipient_not_eligible`). Separate
columns, so that one fact never overwrites another: `fallback_status` (`none`, `delivered`,
`failed`, `unavailable` — FR-041c, §6.3), `notes` (`no_incident_ref`, `no_public_url`,
`link_omitted`), `provider_status` and `provider_code` (D34), `segments`, `duration_s`, `attempts`.

Read through `GET /api/v1/incidents/{id}/phone-activity` (§5.5) and
`GET …/notification-channels/{id}/phone-deliveries` (last 50; `project:read`; reasons and masked
numbers only).

### 4.6 What the rest of the system sees

One place for the consequences of D31, D20 and D33, because they are spread over three packages:

| Where | Change |
| --- | --- |
| `routablePredicate`, `resolveServiceRecipientsTx` | a phone channel counts only when deliverable by configuration |
| escalation step channel resolution | the same predicate; a step whose channels are all non-routes behaves as a step with no channel does today |
| `ChannelDelivery.Resolved` | counts a phone channel only when deliverable at send |
| `ChannelDelivery.Delivered` | counts a phone channel only as D20 says |
| `deliverServiceAlert` | sums the phone counts into the text counts; its two conditions are unchanged |
| monitor, burn and step deliveries | no coverage accounting today, none added; an error from `DeliverPhone` (only `provider_unreachable`, or a failed fallback — §6.3) fails the event as a text channel's error does |

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
  (`ack_open`), closed by the acknowledge and resolve paths and by the scheduler's purge pass: a
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
    channel goes into the **target**: `incident <id> · via=sms` / `· via=voice`;
  - **the result must be told apart.** `AcknowledgeIncidentByPrincipal` returns one `ErrNotFound`
    for a missing incident and for a resolved one, and no word on whether this call was the first
    acknowledgement. The phone path gets a result — acknowledged now; already acknowledged, and by
    whom (display name); already resolved — so the reply can say which;
  - an `incident.acknowledge` by phone also inserts a `phone_actions` row (§5.5) in the same
    transaction.

### 5.3 Snooze

`SNOOZE <code> <minutes>` (1–120) or keypress 2 (30 min) **pauses the ladder's clock** (AUTHOR'S
CALL, builder check Q4). `AdvanceEscalations` measures every step offset from `dueBase` and fires
all steps that have come due in one pass, so "skip while snoozed" would dump every step that fell
inside the snooze at its end.

**Who and when.** The rules of acknowledgement: a user-bound delivery, the sender's current
verified number, the acknowledge permission at reply time (P11), the principal door
(`SnoozeIncidentByPrincipal`), an audit row `incident.snooze` with `· via=sms|voice` and the
minutes, a `phone_actions` row. Only an open, unacknowledged incident can be snoozed; otherwise the
reply says why not.

**The model.** Three columns on the incident, all written under the incident's row lock:

- `escalation_snoozed_at`, `escalation_snoozed_until` — the current snooze, or both NULL;
- `escalation_paused_total` — the sum of every **settled** snooze.

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
  `dueBase`, so shifting `dueBase` alone would not hold a repeat back. The pause as of the last
  step is stored when a step is recorded (`escalation_paused_at_step`), and a repeat is due at
  `last_escalated_at + renotify_seconds + (pause(now()) - escalation_paused_at_step)`.

So a ladder of 0 / +5 / +10 min snoozed for 30 minutes at +3 fires its +5 step at +35, its +10 step at
+40 — the same spacing, shifted. The step that fires re-checks FR-023's pageability predicates then,
not at snooze time. Snooze never acknowledges, never resolves, and does not suppress non-ladder
alerts. An incident with no ladder can be snoozed and the snooze changes nothing; the reply says
"no escalation is scheduled".

### 5.4 Voice retry

On a status callback `no-answer`, `busy` or `failed` for a voice delivery, the call is placed again
120 s later, at most once (P8). A retry is not a ladder step and does not move `escalation_step`.

- **Only for a delivery that belongs to an incident.** A burn alert and a monitor without
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
  it. A check that fails closes the retry row with its reason and places no call.

### 5.5 What the operator sees

Acknowledgements and snoozes made from a phone are rows of `phone_actions` (§8): who, when, `sms`
or `voice`, which delivery, the minutes of a snooze. With the incident's `phone_deliveries` they
make the **Phone** panel of the incident page: every attempt with its reason, every action with its
actor. `GET /api/v1/incidents/{id}/phone-activity` (`project:read`) returns both lists. The public
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
  the status callback's duration, rounded up, adjusts it. Without callbacks (no `public_base_url`)
  a call counts one minute — the settings say so beside the quota.
- **What else counts.** Verification codes, the test SMS and inbound replies count against the
  **instance** SMS quota; channel-number verification against the project's. None of them is
  subject to the storm guard — they have their own limits (§4.2, §5.2).
- **Warning.** At 80 %, once per month per scope: project admins for a project quota, global
  admins for the instance quota.

### 6.2 Storm guard (FR-041a)

At most **5** phone messages per destination number per incident per hour, and at most **20** per
destination number per hour across incidents (**AUTHOR'S CALL**, both configurable in §4.1).
Beyond that the delivery is `storm_limited`. A flapping monitor must not ring one phone sixty
times.

- Counted from `phone_deliveries` by the number's hash, over deliveries that reached the provider
  (`sent`, `provider_rejected`); serialised per number by an advisory lock on the hash, taken with
  the reservation (D33), so two workers cannot both take the last slot (P13).
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

Every delivery that is **deliberately not phoned** — `telephony_disabled`, `storm_limited`,
`quota_exceeded`, `quiet_hours_deferred` — is delivered to the fallback channel with the reason
prepended. The outcome is `fallback_status`, beside `reason` and never instead of it:

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
- Deferred alerts are **not** queued and replayed in the morning (a stale DOWN at 08:00 is noise —
  **AUTHOR'S CALL**, supported by the review).
- A `number`-mode channel has no quiet hours: a shared phone belongs to whoever is on shift.

### 6.5 Per-project provider override (FR-041c)

A project admin may store a project telephony block with the same fields as §4.1 (minus
`public_base_url`, which is per instance because the inbound URL is). Delivery for that project's
channels uses it — it is the project's **effective** configuration for D27's barrier, for D3's
voice rule and for the inbound routes (§5.1), whatever the instance block says. Quotas then count
against the project only. Encrypted per project, AEAD-bound to the project id like project secrets,
and covered by `cerbix secrets reencrypt`. A "send test SMS" action exists here too, to the calling
admin's own verified number.

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
  monitor_id NULL, service_id NULL, user_id NULL, number_hash, number_masked, kind sms|voice,
  alert_kind, reason, notes, provider_status NULL, provider_code NULL, provider_ref NULL, segments,
  attempts, created_at, updated_at)`, and (b) `ack_code NULL`, `ack_open boolean NOT NULL DEFAULT
  false`, `duration_s NULL`, `retry_at NULL`, `retry_claimed_at NULL`, `retry_of uuid NULL UNIQUE`,
  and (c) `fallback_status`. Retention 90 days. Indexes: unique `(outbox_event_id, channel_id)
  WHERE outbox_event_id IS NOT NULL` (D33) — `outbox_event_id` is a plain uuid, not a foreign key;
  unique `(number_hash, ack_code) WHERE ack_open`; `(number_hash, created_at)` for the storm guard.
- (b) `phone_actions (id, project_id, incident_id FK ON DELETE CASCADE, user_id, delivery_id,
  action ack|snooze, via sms|voice, minutes NULL, created_at)`.
- (a) `incidents.ref text NOT NULL UNIQUE DEFAULT <a database function>` (D21, D27): eight Crockford
  base32 characters, generated in the database so that an `INSERT` that does not name the column —
  every insert of an older binary — still succeeds. Backfilled by the migration.
- (b) `incidents.escalation_snoozed_at timestamptz NULL`, `escalation_snoozed_until timestamptz
  NULL`, `escalation_paused_total interval NOT NULL DEFAULT '0'`, `escalation_paused_at_step
  interval NOT NULL DEFAULT '0'` (§5.3). With all four at their defaults `pause` is zero and both
  ladder queries compute what they compute today.
- (b) `telephony_routes (route_key PK, scope instance|project, project_id NULL)` for §5.1.
- (c) `phone_quota_usage (scope_kind instance|project, scope_id, month, kind sms_segments|
  call_minutes, used, PRIMARY KEY (scope_kind, scope_id, month, kind))`.
- (a) `instance_settings.telephony` (JSON, secret fields encrypted); (c) `project_telephony`.
- None of this touches `heartbeats`; every table is an ordinary table in both storage modes.

**An older binary on this schema** (D27): its incident inserts work (the `ref` default); its ladder
queries ignore the snooze columns and fire deferred steps early; its dispatcher fails an event that
has a phone channel among its recipients. The runbook's order — telephony on last, phone channels
off first — is what keeps the third from happening.

## 9. Surfaces

API — OpenAPI first, then `npm run gen:api`. Request bodies are registered in
`internal/api/requestcontract_test.go`.

| Part | Method and path | Permission | Notes |
| --- | --- | --- | --- |
| a | `GET`, `PUT /api/v1/settings/telephony` | global admin | secrets as `*_set` booleans; blank keeps |
| a | `POST /api/v1/settings/telephony/test-sms` | global admin | to the caller's verified number |
| a | `PUT`, `DELETE /api/v1/me/phone`; `POST /api/v1/me/phone/verify` | signed-in human | |
| a | `PATCH /api/v1/me/phone/preferences` | signed-in human | language (a); quiet hours (c) |
| a | `GET /api/v1/me` | — | gains `phone: { number_masked, verified, language, quiet_hours }` |
| a | channel create / update / delete | D28 | the typed fields of §4.3 |
| a | `POST …/notification-channels/{id}/verify/send`, `…/verify` | `project:manage` | `number` mode |
| a | `GET /api/v1/projects/{projectID}/phone-recipients` | `project:write` | §4.3 |
| a | `GET …/notification-channels/{id}/phone-deliveries` | `project:read` | last 50 |
| a | `GET /api/v1/incidents/{id}/phone-activity` | `project:read` | deliveries; actions from (b) |
| a | `GET /api/v1/incidents/by-ref/{ref}` | `project:read`, after the 404 step | D21 |
| b | `POST /api/v1/telephony/{route_key}/sms`, `/voice/gather`, `/status` | provider signature | form bodies, TwiML answers (§5.1) |
| b | `POST /api/v1/settings/telephony/regenerate-routes` | global admin | |
| c | `GET`, `PUT`, `DELETE /api/v1/projects/{projectID}/telephony`; `…/telephony/test-sms` | `project:manage` | §6.5 |
| c | `GET /api/v1/settings/telephony/usage`; `GET /api/v1/projects/{projectID}/telephony/usage` | global admin; `project:read` | quota read-out |

There is no "test this channel" action for a phone channel, as there is none for the other channel
types today; the test SMS of the settings is the only test send.

Audit actions added: `settings.telephony.update`, `user.phone.set`, `user.phone.verify`,
`user.phone.delete`, `incident.snooze`, `project.telephony.update`, `project.telephony.delete`,
`telephony.routes.regenerate`; `incident.acknowledge` gains the `via=` suffix (§5.2). Channel
writes are audited as channel writes are today, and their target never carries a number.

SPA — **a mock must be approved before any frontend code**; one mock per part:

1. (a) Settings → Administration → Telephony: provider form, test SMS, encoding, storm limits, the
   warning before switching telephony off (D31); (b) the inbound URLs; (c) quotas and usage.
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
- **P7** — No alert disappears without a trace: every phone delivery that was resolved and not
  `sent` has a row, a metric count and — when it belongs to an incident — a line in that incident's
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
  channel, and a row left in `sending` is never answered with a second call (D19, D33).
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
  message is sent: the quiet-hours decision, the ACK code and the send itself are the same whether
  the reconciler has opened the incident or not; only the link can be absent, and then the row says
  so (D32).
- **P22** — A phone channel cannot be created while telephony is not enabled for its project, and
  telephony cannot be enabled, nor a number or credential stored, without the at-rest key (D27,
  D29).

## 11. Test matrix (written before the code)

Store tests need no storage-mode branching — every new table is an ordinary table — and run in both
modes in the ordinary suite. The fake `Provider` records calls; the E2E stub provider is a container
that accepts outbound messages and calls and can send **signed** inbound requests back.

| Part | Layer | Cases | Invariants |
| --- | --- | --- | --- |
| a | Domain — message | septet counting incl. the two-septet characters; one and several segments in both encodings; truncation order; link omitted; transliteration never empties a name; normalisation of control characters and line breaks; XML escaping in `<Say>` | P9 |
| a | Domain — `generic_http` | JSON and form encoding of each placeholder; the injected-recipient name stays one value; a template with a placeholder outside a value is refused | P1 |
| a | Store — routes | each way a phone channel is not deliverable × `routablePredicate`, `resolveServiceRecipientsTx`, step resolution; a service whose only channel becomes deliverable announces | P20 |
| a | Outbox — coverage | service alert × {delivered, each not-phoned reason, not deliverable at send}: credited / neither / condemned; the neither case evaluated five more times enqueues nothing | P18 |
| a | Outbox — D18 | every topic and every predicate of the table, against an `sms` and a `voice` channel | P15 |
| a | Outbox — retries | an event retried five times with one phone and one text channel: one accepted provider call; a `sending` row older than 60 s closes as `outcome_unknown`; `provider_unreachable` is called again on the same row; the time budget of D33 with three phone channels | P16 |
| a | Outbox — monitor race | DOWN delivered before the reconciler opens the incident, and after: same send, same quiet-hours input, link present only in the second, `no_incident_ref` in the first; recovery links the resolved incident | P21 |
| a | Suppression | the P2 table, both channel types | P2 |
| a | Storm guard | N concurrent deliveries to one number at the limit, on PostgreSQL under `-race`: exactly the allowed number of provider calls; per-incident and per-number limits; the monitor-as-subject case | P13 |
| a | API | D28's matrix, create and update; `number` channel created disabled and unenablable before verification; number change clears it; the recipients endpoint for an editor and an admin; by-ref 404 before 403; no response carries a number | P3, P22 |
| a | Settings | secrets kept on a blank write; refusal without the at-rest key; reencrypt covers the new ciphertexts and recomputes hashes | P22 |
| a | Regression | a project without phone channels through an outage with a ladder: the P14 comparison, captured before the change and compared after | P14 |
| b | Inbound | Twilio's published signature vectors; unknown key, inactive configuration and bad signature all 403 with no row touched; the URL built from `public_base_url` behind a rewritten `Host`; form parsing and the size limit; TwiML content type of every answer | P10 |
| b | ACK | the code against the delivery's subject with the incident opened before and after the send; several open codes; a changed number; a revoked permission; ten wrong codes; already acknowledged (names who); already resolved; the audit target's `via=` | P11 |
| b | Snooze | 0/+5/+10 snoozed 30 at +3; replaced at +23; the incident unselectable when the snooze ends; the 4 h cap, shortened and refused; `repeat_last` with `renotify_seconds` longer than the snooze; pageability re-checked at fire time | P12, P17 |
| b | Voice retry | two leaders claiming one row; a leader change during the call; a resolved-unacknowledged incident; a delivery without an incident; the child row keeps the parent's reference | P8 |
| b | Timeline | after an ACK, a snooze and a not-phoned delivery: no new `incident_updates` row; the public page JSON, each feed and a captured webhook contain none of it | P19 |
| c | Quotas | N concurrent deliveries against one remaining unit; a rejected delivery returns its reservation; the one-minute rule with and without a callback; what the non-alert messages count against | P13 |
| c | Fallback | each not-phoned reason × {delivered, failed then retried, unavailable}; the retry redoes only the fallback | P7 |
| c | Quiet hours | each rule of D17 and D23, D24; a window across midnight; both daylight-saving changes; `start = end` refused; `number` mode unaffected | — |
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

## 13. Open questions for review

None for the owner: the four questions of revision 2 were decided on 2026-11-16 (D13, D17, D23,
D25).

For the second review pass, the author's calls that are new or changed: D31 (routes, credit, no
re-announce) with the cost it states; D32 (the monitor race, and the 1 s wait for a link); D33 (the
reserved row and `outcome_unknown`); D34 (code, not text); D35 (ciphertext, hash, mask; open codes
closed by a key rotation); the snooze model of §5.3; counting replies and verification codes
against the instance quota (§6.1).

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
