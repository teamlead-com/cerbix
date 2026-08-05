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

## Open questions

- The set of channels for the MVP and message templates.
