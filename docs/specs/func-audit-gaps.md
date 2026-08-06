# Spec: Audit gap package — saved-but-never-used functionality (func-audit-gaps)

## Purpose

A 2026-08 repo-wide audit (three sweeps: settings end-to-end, config/docs, API↔SPA
surface) found functionality that exists on one side of the stack and is missing on the
other: settings that persist but change nothing, API data never rendered, UI decoration
without a backend, and two unbounded tables. This spec packages the fixes as one plan,
implemented easiest → hardest, one iteration each. SPA-facing iterations start from an
approved UI mockup per the project convention.

## Iterations (easiest → hardest)

| # | Iteration | Scope | SPA mock |
|---|---|---|---|
| 1 | iter-0044 | **Docs & config example accuracy.** `config.example.yaml` gains the missing `local.login_rate_limit_per_minute`; `docs/overview.md` fixes: `oidc.admin_emails` → `bootstrap_admin_emails` (+ mention `post_logout_redirect_url`), `admin_email/password` moved from the `local` row to `security`, the worker example command gains `serve`, the role list gains `agent`, a `pull` row is added, `rabbitmq` row gains `management_url`. | no |
| 2 | iter-0045 | **Expired-row purge + dead surface.** The scheduler maintain tick calls `DeleteExpiredSessions` and `DeleteExpiredAuthFlows` (both exist, tested, and were never wired — `sessions`/`auth_flows` grow without bound). `ListMembershipsByOrg` leaves the api Store interface (no caller; `ListOrgMembers` is the real one). | no |
| 3 | iter-0046 | **Live minimum password length.** `auth_policy.min_password_len` saved from the UI is enforced nowhere — both password paths read the startup YAML. Password set/change and reset-confirm consult the live settings snapshot; YAML stays the pre-first-save seed, matching every other policy field. | no |
| 4 | iter-0047 | **SPA display gaps.** Monitor detail: real `method` instead of hardcoded "GET", plus the missing config rows (`failure_threshold`, `confirm_interval_seconds`, `renotify_seconds`, `grace_seconds`, escalation policy, `updated_at`). Incident detail: link to the source monitor, `external_key`, and "acknowledged by" with the actor resolved to a name (backend enrichment — the field is a bare UUID). API tokens/webhooks tables: "created by" (same enrichment). SSE-drop indicator from the already-tracked `live.connected`; initial workspace loading state. | yes |
| 5 | iter-0048 | **Branding logo.** `logo_url` is publicly served but has no form input and no consumer. Branding form gains the input; the logo renders in the sidebar brand block, the login card and the public status-page header (fallback: current glyph). | yes |
| 6 | iter-0049 | **Push monitor onboarding.** The SPA never shows `push_token` or the heartbeat URL, making push monitors unusable without curl. Monitor detail gets a "Push endpoint" panel (URL + token, copy buttons, grace note) for `type=push`. | yes |
| 7 | iter-0050 | **Global silence until.** The backend honors `global_silence.until` but the UI cannot set it, and saving the toggle silently clears an API-set expiry. The alerting form gains an optional until (datetime) and always round-trips the full object. | yes |
| 8 | iter-0051 | **Webhook & channel enable/disable.** The "Status" column is decoration: no PATCH routes exist. `PATCH /webhooks/{id}` and `PATCH /notification-channels/{id}` (enabled toggle first), UI switches in both tables; delivery paths already filter on `enabled`. | yes |
| 9 | iter-0052 | **Status pages: working preview + component fields.** "View public page" 404s for `internal` (the default) and for `unlisted` (token not appended). The authed render endpoint exists and is unused: the public view falls back to it for signed-in members; the unlisted link carries `?token=`. The component form gains `group`, `description`, `position`; the public render DTO gains `description`. | yes |
| 10 | iter-0053 | **Agent tokens UI.** Full CRUD API + spec, zero UI. Settings → Administration → "Agent tokens": issue (token shown once), list, revoke. | yes |
| 11 | iter-0054 | **Status-page subscribers view.** Owners cannot see subscribers at all. `GET /status-pages/{pageID}/subscribers` (org-manage) + a subscribers block in the status-page editor (count, confirmed/pending, delete). | yes |

## Non-goals

Resolving `acknowledged_by`/`created_by` to full user profiles (email/display name is
enough); exposing `monitors.last_notified_at` (tracked as a possible follow-up);
`go_version` in the sidebar; feed/render endpoints beyond the preview fix.

## Acceptance (package-wide)

Per iteration: `-race` suite green (both storage modes when a migration is involved),
vue-tsc build green for SPA work, live E2E of the specific gap, an iteration report, a
decision record, and a traceability row. The audit is the baseline: after the package,
re-running the three audit sweeps must return no findings of the class "saved/served but
never used".
