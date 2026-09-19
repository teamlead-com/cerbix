# Spec: Notifications (func-notifications)

> Skeleton. To be filled in during the iter before implementing notifications.

## Purpose

Alerts about monitors going down/recovering and about incident changes via various channels.

## Scope

- `Notifier` interface; channels: Telegram, Slack, Email (SMTP), generic webhook.
- `NotificationChannel` at the project level; binding channels to monitors.
- Sending rules: up→down, down→up, dedup/debounce, quiet hours (later).

## Requirements (draft)

- FR: CRUD of notification channels within a project.
- FR: sending on monitor and incident status changes.
- NFR: a failure of one channel does not break the others; retries with backoff.
- NFR (**security**): channel secrets are not logged.

## Tenant isolation invariants

- A monitor may link only to a notification channel in the monitor's project. The store derives the
  project from the monitor/channel pair; callers do not supply or infer it independently.
- An escalation-policy target (`channel` or `schedule`), every on-call participant, and every
  on-call override channel must belong to the owning project.
- These are persistence invariants, not HTTP conveniences: store methods return the same typed
  refusal to API, CLI and future background writers, while composite foreign keys or JSONB tenant
  guards reject direct SQL that bypasses the store.
- Missing, malformed and foreign routing references receive one generic transport refusal; the API
  does not expose whether a referenced id exists in another tenant.
- An upgrade never deletes or silently rewrites routing data. Migration `00106` backfills tenant
  identity and fails fast if existing rows violate the invariant so an operator can repair them
  explicitly.

## Open questions

- The set of channels for the MVP and message templates.
