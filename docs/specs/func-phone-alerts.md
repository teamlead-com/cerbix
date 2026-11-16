# func-phone-alerts — an alert that reaches a person: SMS and voice calls (FR-041 / NFR-034)

> **STATUS: revision 2 — DRAFT, not approved.** Revision 1 went through a builder check against the
> code (2026-11-13): seven planning questions and five code contradictions, all folded in as D15–D21,
> §4.5, §5.1, §5.3, §7 and P15–P18. Written 2026-11-13 from the owner's
> answers to twelve design questions (all twelve accepted as proposed, recorded in §3 as OWNER
> decisions). Decisions the author took without an owner answer are marked **AUTHOR'S CALL** and are
> open to change at review. Nothing here is implementable until the owner approves a revision, and the
> SPA surfaces in §9 need an approved mock before any frontend code (CLAUDE.md, spec-before-code).
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
  - **FR-041a** — the two channel types, verified numbers, the provider abstraction, instance settings.
  - **FR-041b** — acknowledgement and snooze by SMS reply and by keypress; voice retry on no answer.
  - **FR-041c** — quotas, storm guard, fallback, per-person quiet hours, per-project provider override.
- **NFR-034**: Every existing channel, every existing suppression rule and every existing escalation
  ladder behaves **byte-identically** before and after FR-041, proven by regression; telephony
  credentials and phone numbers are secrets-grade data (never in an API response to a viewer, never
  in a log line, never in a dispatch carrier); the inbound endpoints accept **only** provider-signed
  requests.

Each lettered part is its own iteration (OWNER, Q12), in the order a → b → c, all three **after**
FR-042 ([`func-incident-impact.md`](func-incident-impact.md)) — OWNER, 2026-11-13, option A. Each ships alone and is
useful alone: after (a) the phone rings; after (b) it can stop the ladder; after (c) it cannot run up a
bill.

## 3. The decisions

### Owner decisions (answers of 2026-11-13)

**D1 — the audience is any operator of a cerbix instance, not a single hosted deployment. OWNER (Q1).**
Each instance connects its own provider account. cerbix ships no telephony account, no shared sender
number and no billing.

**D2 — phone is a CHANNEL TYPE, usable everywhere a channel is. OWNER (Q2).**
`sms` and `voice` are new `ChannelType`s. A monitor can link them, a service alert can route to them,
an escalation step can target them, an on-call schedule can rotate them. Nothing in the routing,
suppression or escalation code learns that a phone exists — it delivers to a channel, as today.

**D3 — one provider interface; Twilio first, plus a generic HTTP SMS gateway. OWNER (Q3).**
`internal/telephony` defines a `Provider` with `SendSMS`, `PlaceCall`, and (FR-041b) `VerifyInbound`.
Two implementations ship:
- `twilio` — SMS and voice (TTS via TwiML `<Say>`), inbound replies, keypress, status callbacks.
- `generic_http` — **SMS only**: an operator-defined request template (method, URL, headers, a body
  with `{{to}}`, `{{from}}`, `{{text}}`) for regional SMS gateways. It cannot place calls and cannot
  receive replies; a `voice` channel cannot be created while the active provider is `generic_http`
  (400 with that reason), and FR-041b reply features report themselves unavailable rather than broken.

Further providers are added by implementing the interface; none is promised here.

**D4 — no country is privileged. OWNER (Q4, no constraint given).** Numbers are stored and validated
as E.164. Whether a destination is reachable is the provider's answer, surfaced verbatim (redacted, §7)
in the delivery result — cerbix keeps no country allow-list of its own.

**D5 — acknowledgement by reply and by keypress is in scope, and off until a public URL exists.
OWNER (Q5).** Every phone message carries a link to the incident. Reply-to-ack (`ACK <code>`) and
press-1-to-ack additionally need the provider to reach cerbix, so they activate only when the telephony
settings hold a `public_base_url` AND the provider supports inbound. When they are inactive, the
message **does not mention them** (P9: a message never instructs an action that cannot work).

**D6 — snooze is in scope. OWNER (Q6).** `SNOOZE <code> <minutes>` (SMS) or keypress 2 (voice,
fixed 30 minutes) defers the incident's escalation ladder; it does not acknowledge. §5.3 gives the exact
semantics.

**D7 — the number belongs to a USER, verified by code; a shared number lives on the channel and is
verified too. OWNER (Q7).** See D11 for how this meets the on-call model.

**D8 — per-person quiet hours. OWNER (Q8).** In FR-041c. §6.4.

**D9 — voice speaks a TTS message; one retry on no answer. OWNER (Q9).** Language is an instance
setting (default `en-US`) with a per-user override. The retry is placed 120 s after a `no-answer`,
`busy` or `failed` status callback, once per delivery, and is independent of the next ladder step.
The retry needs status callbacks, so it is FR-041b, not FR-041a.

**D10 — limits with fallback, never a silent drop. OWNER (Q10).** In FR-041c. §6.

**D11 — provider credentials live in instance settings, encrypted, with a per-project override.
OWNER (Q11).** Instance settings in FR-041a (the same at-rest treatment as `MailSettings.SMTPPassword`,
`internal/store/instance_settings.go`); the per-project override in FR-041c.

### Author's calls (open at review)

**D12 — a user's phone reaches an on-call schedule through a USER-BOUND CHANNEL. AUTHOR'S CALL.**
`OnCallSchedule.Participants` are **channel ids**, and overrides name a `channel_id`
(`internal/domain/escalation.go`, `oncall_overrides`). Schedules rotate channels, not users. Rather
than teach schedules about users (a model change across schedules, overrides, tenancy checks and the
SPA), a phone channel has one of two **recipient modes**:
- `user` — config holds `recipient_user_id`; the number is resolved **at send time** from that user's
  verified number. Alice's phone is the channel "SMS — Alice"; the schedule rotates it like any other.
- `number` — config holds an E.164 `number` (a shared duty phone, a NOC line), verified at creation.

Resolving at send time means a user who changes or loses a number never leaves a stale copy inside
channels. A `user` channel whose user has no verified number, or is no longer a member of the
channel's project, **fails the delivery** with that reason (P6) — it never falls back to an old number.

**D13 — acknowledgement by phone is attributed, so only USER-BOUND deliveries can acknowledge.
AUTHOR'S CALL.** `acknowledged_by` is a user id and incident writes are audited by principal
(FR-026). A reply from a shared `number`-mode phone has no principal, so it gets the incident link but
**no ACK code** and its replies are ignored (with a metric). A reply from a `user`-mode delivery
acknowledges as that user, through the same store path as `POST /incidents/{id}/acknowledge`
(`AcknowledgeIncidentByPrincipal`), and only if that user **still** holds the permission the endpoint
requires on that project at reply time.

**D14 — the provider HTTP calls go through the notification egress guard. AUTHOR'S CALL.**
Same `notification_egress` policy as webhook/Slack/Telegram (`internal/cli/cli.go`, egress client).
`generic_http` is an operator-chosen URL and gets exactly the webhook treatment.

**D15 — phone delivery runs where the outbox runs: every role with `coreDelivery` (`all`, `api` AND
`scheduler`, `internal/cli/cli.go` `servicesForRole`). AUTHOR'S CALL.** All three load the telephony
settings and credentials. No prober involvement, no dispatch carrier: telephony credentials never
leave core (consistent with the two-keyring rule — a prober never holds a notification credential).

**D16 — no new outbox topic and no outbox payload change; phone gets its own delivery method.
AUTHOR'S CALL (revised after the builder check).** Suppression (silence, maintenance, dependency
`DownAncestors`, escalation flat-down) is decided per topic before channel fan-out and applies
unchanged. But `notify` today receives only a pre-rendered string (`Deliver`, `DeliverText`,
`DeliverProjectText`, `DeliverChannels` in `internal/notify/notify.go`), and a phone message needs
facts that string does not carry. So:
- The text paths **skip** `sms`/`voice` channels; they keep delivering every other channel exactly as
  today (P14).
- The outbox worker, for the topics phone receives (D18), builds a structured `PhoneAlert` — kind,
  subject name, project, incident id/ref/impact when one exists, outbox event id — and calls a new
  `DeliverPhone(ctx, channelIDs, PhoneAlert)` on the `NotifyDeliverer` interface. The incident is
  looked up **at delivery time** (`monitor_transition` carries no incident id; the reconciler opens the
  auto-incident separately and it may not exist yet). No open incident → the message has no incident
  ref, no ACK code (§5.2), and counts as non-critical for quiet hours.
- Outbox rows and payloads are unchanged; the interface grows, so the three fakeStores and the notify
  fakes break compilation by design (CLAUDE.md).

Only the voice retry (FR-041b) needs its own durable schedule, and it uses `phone_deliveries` (§8),
not the outbox.

**D18 — phone receives only alerts that ask a person to act. AUTHOR'S CALL (builder check Q2).**
"Usable everywhere a channel is" would otherwise include the weekly SLA report and region-worker
alerts (both fan out to every enabled project channel via `DeliverProjectText`), DOWN reminders and
burn CLEAR messages — a weekly phone call reading an SLA report.

| Alert | `sms` | `voice` |
| --- | --- | --- |
| Monitor confirmed DOWN (first transition) | yes | yes |
| Monitor recovered (UP) | yes | no |
| Service alert FIRING | yes | yes |
| Service alert resolved | yes | no |
| Escalation step (incl. `repeat_last`) | yes | yes |
| SLO burn FIRING | yes | yes |
| DOWN reminder (renotify), burn CLEAR, SLA report, region-worker alert, subscriber mail | no | no |

**D19 — one provider call per (outbox event, channel), whatever the outbox retries. AUTHOR'S CALL
(builder check Q3).** The outbox retries the WHOLE event when any channel errors, re-delivering to
every channel — harmless for Slack, a second charge and a fresh ACK code for a phone. `phone_deliveries`
has a unique key `(outbox_event_id, channel_id)` (escalation steps: plus `step`, `repeat` ordinal); a
retry finds the row and does not call the provider again. A provider **rejection** is final: it is
recorded and returned to the outbox as success-for-retry-purposes, but it is NOT counted as `Delivered`
(D20). Only `provider_unreachable` returns an error and lets the outbox retry.

**D20 — what counts as `Delivered` for service coverage. AUTHOR'S CALL (builder check Q7).**
`ChannelDelivery.Delivered > 0` credits service coverage and silences the members' own alerts
(D-0179). A phone delivery counts as delivered only when the provider accepted it, OR its fallback
channel (§6.3) accepted the redirected alert. A quiet-hours deferral or a quota stop **without** a
delivered fallback is NOT delivered — so the members keep paging on their own channels. The polarity
is deliberate: when in doubt, page.

**D21 — incidents get a short reference for the phone. AUTHOR'S CALL (builder check, §4.4).** Incident
ids are UUIDs and the SPA route is `/incidents/:id`; a UUID link spends ~60 of 160 characters.
`incidents.ref` — 8 characters, Crockford base32, unique, assigned at insert, backfilled by the
migration — plus an SPA route `/i/:ref` that resolves and redirects to `/incidents/:id` behind the
normal session (an unauthenticated visitor gets the login page, never a 404 that confirms existence).

**D17 — "critical" for quiet hours means: an alert for an incident whose impact is `critical`, or an
escalation step. Depends on FR-042. AUTHOR'S CALL (revised).** Revision 2 said "`major` or
`critical`", but every automatic incident opened as `major` and impact could never change, so the rule
let everything through. [`func-incident-impact.md`](func-incident-impact.md) (FR-042, built first —
OWNER, 2026-11-13) makes impact a per-service declaration that only rises and that no person can edit,
so `critical` now means what the service's owner declared it to mean. Escalation steps break through
because the ladder is the product's "wake a person" mechanism. Everything else goes to the fallback
channel (§6.4).

## 4. FR-041a — the channels

### 4.1 Instance telephony settings

A new `telephony` block in `instance_settings`, resolved DB → config → defaults like `mail`
(`internal/settings`). Global-admin only, live-reconfigurable, YAML is a bootstrap seed.

| Field | Meaning |
| --- | --- |
| `enabled` | master switch; off → phone channels refuse to deliver with reason `telephony_disabled` |
| `provider` | `twilio` \| `generic_http` |
| `twilio.account_sid`, `twilio.auth_token`* , `twilio.from` | `from` is an E.164 number or Messaging Service SID |
| `generic_http.method`, `.url`, `.headers`*, `.body_template`, `.from` | headers may carry credentials, so the whole map is secret |
| `public_base_url` | the URL the provider can reach (FR-041b); also builds the incident link in the message |
| `voice_language` | BCP-47, default `en-US` |

`*` encrypted at rest with `security.encryption_key`, never emitted to a client (write-only, like
`smtp_password`: the edit form re-collects them). `public_base_url` falls back to
`mail.public_base_url` when unset; with neither, messages carry no link and say so in the delivery
result (`no_public_url`).

A **"Send test SMS"** action sends a fixed text to the calling admin's own **verified** number only.

### 4.2 Verified numbers

- `PUT /api/v1/me/phone` `{ "number": "+905…" }` → stores it **unverified**, sends a 6-digit code by
  SMS. `POST /api/v1/me/phone/verify` `{ "code": "…" }` → marks it verified. `DELETE /api/v1/me/phone`.
  `GET /api/v1/me` gains `phone: { number_masked, verified }`.
- One number per user (a second `PUT` replaces it and clears verification).
- Code: 6 digits, stored as a hash, valid 10 min, 5 attempts, then invalid; sending is limited to
  3 codes per user per hour and 10 per number per day (a number cannot be bombed through many accounts).
- A shared `number`-mode channel is verified the same way, by the person creating it, before it can be
  enabled: `POST …/notification-channels/{id}/verify/send` and `…/verify`.
- Verification codes are the **only** text cerbix sends to an unverified number (P1).

### 4.3 The channel types

`ChannelType` gains `sms` and `voice`. Config:

| Key | Mode | Notes |
| --- | --- | --- |
| `recipient` | both | `user` \| `number` |
| `recipient_user_id` | user | must be a member of the channel's project at create/update and at send |
| `number` | number | E.164; **write-only**: added to `SecretChannelConfigKeys`, so `Redacted()` blanks it for every caller, admins included, and `MergeChannelConfig` keeps the stored value on a blank patch — the existing secret mechanics, no role-aware redaction |
| `number_masked` | number | server-computed on read (`+90•••••6869`), never accepted on write |
| `fallback_channel_id` | both | FR-041c, §6.3 |

A user-bound channel may be created by that user for themselves, or by a project admin for any
project member (the member's number is never shown to the admin — only "verified / not verified").

### 4.4 The message

SMS text, rendered by `notify` from the `PhoneAlert` (D16):

```
[cerbix] DOWN payments-api (prod) — incident X7K2P9QM https://cerbix.example/i/X7K2P9QM  ACK 482193
```

- Target: one GSM-7 segment (160 chars). The service/monitor name is truncated first; the link and
  the code are never truncated. Non-GSM characters in names are transliterated or dropped, not sent as
  UCS-2 (which would halve the segment and double the cost) — **AUTHOR'S CALL**.
- The `ACK <code>` suffix appears only when D5 and D13 both allow it (P9).
- Content is limited to what a project viewer may already see. No heartbeat message bodies, no URLs of
  monitored targets, no secret values — probe `msg` text can echo target responses and is excluded.

Voice: TTS reads the same facts in words ("cerbix alert. payments A P I in prod is down.")
twice, then (FR-041b, when active) "Press 1 to acknowledge. Press 2 to snooze for 30 minutes."

### 4.5 Delivery result

There is no per-channel delivery report today (`ChannelDelivery` in `internal/domain/servicealert.go`
holds three counts, and only the `service_alert` path returns it). The record of a phone delivery is
therefore its **`phone_deliveries` row**, with a `reason` from a closed set: `sent`,
`provider_rejected`, `provider_unreachable`, `telephony_disabled`, `no_verified_number`,
`recipient_not_member`, `voice_unsupported_by_provider`, `duplicate_suppressed` (D19),
`quota_exceeded` (FR-041c), `quiet_hours_deferred` (FR-041c), `fallback_delivered` (FR-041c).
Read through `GET /api/v1/incidents/{id}/phone-deliveries` and `GET …/notification-channels/{id}/
phone-deliveries` (last 50; project viewers see reasons and masked numbers). The provider's own error
text is kept only after redaction (§7). Retry semantics are D19.

## 5. FR-041b — acknowledge, snooze, retry

### 5.1 Inbound endpoints

Mounted on a new `TelephonyRouter` beside `PublicRouter` (no session auth):

- `POST /api/v1/telephony/{route_key}/sms` — inbound SMS reply
- `POST /api/v1/telephony/{route_key}/voice/gather` — keypress result
- `POST /api/v1/telephony/{route_key}/status` — call/SMS status callback

`route_key` is an opaque random id (22 chars) generated per provider configuration — the instance one
now, each project override in FR-041c — and shown in the settings as the URLs to paste into the
provider console. It exists from FR-041b so that FR-041c's per-project accounts never force operators
to reconfigure their providers (builder check Q6). The key selects ONE credential set; the request is
then verified with that set by `Provider.VerifyInbound` before any body field is trusted (Twilio:
`X-Twilio-Signature` over the full URL + sorted params with that account's auth token). An unknown key
or an unverifiable request gets 403 and touches no row (P10). Rotating a credential set keeps its key;
"regenerate URLs" is an explicit admin action. The router is rate-limited per source IP and
globally. The endpoints exist only while telephony is enabled with a provider that supports inbound;
otherwise 404.

### 5.2 The ACK code

- Issued per **user-bound** delivery that belongs to an incident: 6 digits, unique among the codes that
  are still **open** for that destination number, stored in `phone_deliveries`.
- Open until the incident is acknowledged or resolved, or 24 h pass — whichever is first.
- `ACK 482193` acknowledges that incident. `ACK` alone works when exactly one code is open for the
  sender's number; with several, the reply lists them (`2 open: ACK 482193 payments-api, ACK 551020
  checkout`) and acknowledges nothing.
- The sender is matched by the number the provider reports, which must equal the number the code was
  issued to **and** still be that user's verified number at reply time.
- Replies are case-insensitive, trimmed; anything unrecognised gets one help reply per number per
  10 minutes (a reply loop with another automated responder must not amplify).
- Acknowledgement runs through `AcknowledgeIncidentByPrincipal` with the user as principal. The audit
  row must say it came from a phone: `AuditActor` (`internal/store/incidentaudit.go`) has
  `ActorUserID`, `ViaToken` and `Label` and accepts three principal shapes; a fourth shape — user +
  `via: sms|voice` — is added there, with its own `valid()` case and test. The permission checked is the
  acknowledge endpoint's (`ActionProjectWrite`). Acknowledging an already-acknowledged incident is a no-op with a
  confirmation reply naming who acknowledged it.
- A system incident update is written with the unique prefix `☎ Acknowledged by phone:` (the
  idempotency-marker pattern) so the timeline shows how it was acknowledged.

### 5.3 Snooze

`SNOOZE <code> <minutes>` (1–120) or keypress 2 (30 min) **pauses the ladder's clock**
(AUTHOR'S CALL, builder check Q4). `AdvanceEscalations` measures every step offset from `dueBase` and
fires all steps that have come due in one pass, so "skip while snoozed" would dump every step that
fell inside the snooze at its end. Instead:
- `incidents.escalation_snoozed_until` — while `now()` is before it, neither candidate query (monitor
  ladder, which reads its policy live; service ladder, which reads the snapshot frozen at open) selects
  the incident, and `repeat_last` re-sends are held too;
- `incidents.escalation_paused_total` — on the first pass after the snooze ends, the elapsed snooze is
  added to it, and both queries measure offsets from `dueBase + escalation_paused_total`.

So a ladder of 0 / +5 / +10 min snoozed for 30 minutes at +3 fires its +5 step at +35, its +10 step at
+40 — the same spacing, shifted. The step that fires re-checks FR-023's pageability predicates then,
not at snooze time. Snooze never acknowledges,
never resolves, and does not suppress non-ladder alerts. A second snooze replaces the first; the total
snooze per incident is capped at 4 h (**AUTHOR'S CALL**) so a ladder cannot be snoozed forever.
Written to the timeline as `⏸ Snoozed by phone:`.

### 5.4 Voice retry

On a status callback `no-answer`, `busy` or `failed` for a voice delivery that has not been retried,
and whose incident is still unacknowledged, the call is placed again after 120 s — scheduled through
`phone_deliveries.retry_at`, claimed by the scheduler leader with `FOR UPDATE SKIP LOCKED`, at most once
per delivery (P8). A retry is not a ladder step and does not move `escalation_step`.

## 6. FR-041c — bounds

### 6.1 Quotas

Per project and per instance, per calendar month (UTC), separately for SMS segments and call minutes,
configurable in the telephony settings (default: unlimited instance, unlimited project — the operator
opts in). At 80 % the project's admins get an email (once per month per threshold). At 100 % phone
delivery for that scope stops with reason `quota_exceeded`.

### 6.2 Storm guard

At most **5** phone messages per destination number per incident per hour, and at most **20** per
destination number per hour across incidents (**AUTHOR'S CALL**, both configurable). Beyond that:
`quota_exceeded` with `scope: storm`. A flapping monitor must not ring one phone sixty times.

### 6.3 Fallback, never silent

A phone channel may name a `fallback_channel_id` (any non-phone channel in the same project). Every
delivery that does not reach the phone for a **bounded** reason (`quota_exceeded`,
`quiet_hours_deferred` when non-critical, `telephony_disabled`) is delivered to the fallback channel
with the reason prepended, and a system incident update `☎ Not phoned:` records it when the alert
belongs to an incident. Without a fallback, the reason is still recorded and counted (P7).

### 6.4 Quiet hours

Per user: `quiet_hours: { tz, start, end }` on the profile. During that window, a `user`-mode phone
channel delivers only **critical** alerts (D17). Non-critical alerts go to the fallback channel; they
are **not** queued and replayed in the morning (a stale DOWN at 08:00 is noise — **AUTHOR'S CALL**).

### 6.5 Per-project provider override

A project admin may store a project telephony block with the same fields as §4.1 (minus
`public_base_url`, which is per instance because the inbound URL is). Delivery for that project's
channels uses it; quotas then count against the project only. Encrypted per project, AEAD-bound to
the project id like project secrets.

## 7. Data protection

- Numbers in logs and metrics: never. Log lines carry `channel_id`, `user_id`, a masked number
  (`+90•••••6869`) at most.
- Provider error text is passed through the existing failure-message redaction and truncated to 200
  characters before it is stored or returned.
- A user's number is visible in full only to that user. A shared `number`-mode number is write-only
  for everyone after save (masked on read, §4.3) — re-entering it requires re-verification.
- Deleting a user deletes their number and disables (does not delete) user-bound channels pointing at
  them, with the reason visible on the channel.

## 8. Schema (sketch — the builder owns the exact DDL)

- `user_phones (user_id PK FK users ON DELETE CASCADE, e164, verified_at NULL, language NULL,
  quiet_hours jsonb NULL, updated_at)`
- `phone_verifications (id, subject_kind user|channel, subject_id, e164, code_hash, attempts,
  expires_at, created_at)` — short-lived, purged by the scheduler.
- `phone_deliveries (id, project_id, channel_id, outbox_event_id NULL, step NULL, repeat_ordinal NULL,
  incident_id NULL, user_id NULL, e164, kind sms|voice, alert_kind, ack_code NULL, provider_ref,
  reason, segments, duration_s, retry_at NULL, retried bool, created_at)` — retention 90 days; unique
  `(e164, ack_code)` WHERE the code is open; unique `(outbox_event_id, channel_id, step,
  repeat_ordinal)` NULLS NOT DISTINCT for D19.
- `incidents.ref` (D21), `incidents.escalation_snoozed_until timestamptz NULL`,
  `incidents.escalation_snoozed_total interval` (the §5.3 cap), `incidents.escalation_paused_total
  interval NOT NULL DEFAULT '0'`.
- `telephony_routes (route_key PK, scope instance|project, project_id NULL)` for §5.1.
- `instance_settings.telephony` (JSON, secret fields encrypted); `project_telephony` for §6.5.
- `notification_channels_type_chk` widened to add `sms`, `voice` (the 00013 pattern: drop and re-add
  the constraint; the Down refuses while phone channels exist rather than deleting them).

## 9. Surfaces

API (OpenAPI first, then `npm run gen:api`): §4.1 settings routes, §4.2 `/me/phone` and channel
verification, §4.3 channel config, §5.1 inbound routes (documented, unauthenticated, provider-signed),
§6 quota read-out.

SPA — **a mock must be approved before any frontend code**:
1. Settings → Instance → Telephony (provider form, test SMS, quotas).
2. Profile → Phone (add, verify, language, quiet hours).
3. Channel editor: `SMS` / `Voice call` with recipient mode, verification state, fallback channel.
4. Incident timeline: the `☎` / `⏸` system notes and per-delivery status.

Metrics: `cerbix_phone_deliveries_total{kind,provider,reason}`,
`cerbix_phone_inbound_total{kind,result}`, `cerbix_phone_quota_used{scope,kind}`.

## 10. Acceptance invariants

- **P1** — No SMS or call is ever sent to a number that is not verified, except the verification code.
- **P2** — A phone delivery is subject to exactly the suppression the same alert gets on a Slack channel
  (silence, maintenance, dependency ancestors, escalation flat-down); proven by a table test that runs
  every suppression case against both channel types.
- **P3** — Telephony credentials and full numbers never appear in an API response to a non-owner, a log
  line, a metric label, an audit diff, an export bundle or a dispatch carrier.
- **P4** — A `user`-mode channel resolves the number at send time; changing or deleting the number
  takes effect on the next delivery with no channel edit.
- **P5** — `generic_http` cannot be the provider of a `voice` channel; the refusal is at write time and
  at send time.
- **P6** — A delivery that cannot be made returns a reason from the closed set in §4.5; none is
  reported as `sent`.
- **P7** — No alert disappears without a trace: every non-`sent` phone delivery is visible in the
  delivery report, the metric, and — when it belongs to an incident — the incident timeline.
- **P8** — A voice retry happens at most once per delivery, never after acknowledgement, and never moves
  the ladder.
- **P9** — A message never offers an action that cannot work: no `ACK` text or keypress prompt without
  an active inbound path and a user-bound delivery.
- **P10** — An inbound request that fails provider verification changes nothing and is answered 403.
- **P11** — Acknowledgement by phone requires the code's number to equal the user's CURRENT verified
  number and the user to hold acknowledge permission on the project at reply time.
- **P12** — Snooze defers only the ladder, is capped, and the deferred step re-checks FR-023's
  pageability predicates when it fires.
- **P13** — Quota and storm limits are enforced before the provider is called, across processes (a DB
  count, not an in-memory counter).
- **P14** (NFR-034) — A project with no phone channel produces byte-identical outbox rows, deliveries,
  escalation progress and payloads before and after FR-041. (Outbox payloads are unchanged for every
  project; the incident `ref` column and the `paused_total` offset of zero change no existing output.)
- **P15** — A phone channel receives exactly the alert kinds in D18's table; a test enumerates every
  topic and asserts the table.
- **P16** — An outbox event retried N times produces at most one provider call per channel (D19).
- **P17** — A snoozed ladder resumes with its step spacing preserved (§5.3); no step fires twice and no
  step is skipped.
- **P18** — A phone delivery that reached nobody (no provider acceptance, no delivered fallback) never
  credits service coverage (D20).

## 11. Test matrix (written before the code)

Store tests on both storage modes for every new table; domain tests for message rendering (segment
length, truncation order, transliteration, P9 text presence); a fake `Provider` recording calls for
P1–P8; a signed/unsigned inbound table for P10–P11 with Twilio's published signature vectors; a
scheduler test for retry claim under two leaders (P8); the P2 suppression table; the P14 regression
on existing channel fakes (`internal/outbox`, `internal/api`, `internal/scheduler` fakeStores); E2E
on the dev stack against a stub provider container (no real SMS in CI) covering verify → channel →
DOWN → delivery row → inbound ACK → incident acknowledged.

## 12. Non-goals

- A telephony account, sender number or billing operated by cerbix.
- Two-way chat over SMS beyond `ACK`, `SNOOZE` and help.
- Resolving or reopening an incident from the phone (acknowledge is reversible; resolve is not).
- Push notifications / a mobile app.
- Changing on-call schedules to rotate users instead of channels (D12).
- Any change to the existing Alertmanager receiver (`POST /api/v1/projects/{id}/alerts/alertmanager`);
  its incidents page by phone like any other incident.
- Queuing quiet-hours alerts for morning replay (§6.4).

## 13. Open questions for review

- D13: should a shared `number`-mode phone be able to acknowledge if it is mapped to a user in the
  channel config? (Proposed: no in revision 1.)
- D17: should an escalation step for a NON-critical incident break quiet hours? (Proposed: yes — the
  ladder is opt-in per service/monitor, so a ladder is already an owner's statement that it should.)
- §4.4: transliteration vs. accepting UCS-2 for names in non-Latin scripts.
- D17 / §6.4 against FR-042's default mapping (raised by the first external review of
  [`func-incident-impact.md`](func-incident-impact.md), 2026-11-16; not decided there): FR-042 D6
  (OWNER) opens an incident for service state `unknown` with impact `none`. A service that set
  `page_on_unknown` — "tell me when you cannot see it" — therefore does not ring during quiet hours
  unless it has an escalation policy, and grading `unknown` `critical` to get the call would publish
  "Critical impact" for a loss of visibility. Options: say that the night call for `unknown` is
  obtained through an escalation policy; or let quiet hours treat a paged `unknown` as breaking
  through regardless of impact. FR-042's default is not open.
