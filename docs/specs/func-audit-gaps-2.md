# Spec: Audit gap package 2 — the second layer (func-audit-gaps-2)

## Purpose

Re-running the three audit sweeps after `func-audit-gaps` confirmed all eleven original
findings closed and surfaced a deeper layer: defects visible only when tracing the full
UI path → API semantics. This spec packages them, easiest → hardest, one iteration each;
SPA-facing iterations start from an approved mockup (skipped only where the change has
no visual surface).

## Iterations (easiest → hardest)

| # | Iteration | Scope | SPA mock |
|---|---|---|---|
| 1 | iter-0055 | **Docs & dead-scan cleanup.** `docs/overview.md`: `agent` wrongly marked as needing RabbitMQ (×2 places), invalid inline command (missing `serve`/`--config`), `--region` missing from the serve flags cell, `reencrypt` also requires `database.dsn`, mail row key notation (`smtp_host/port/…` → real tags). `internal/config`/`config.example` comment: management-URL derives from the *scheme*, not the port. `OIDCSettings.UpdatedAt` (scanned, never used) leaves the domain struct. Test-support store functions (`SetMonitorStatus`, `LatestHeartbeat`, `GetUserByOIDCSub`, `ListMembershipsByOrg`) get explicit test-support doc comments so the next audit doesn't re-flag them. | no |
| 2 | iter-0056 | **Feed links that work.** RSS/Atom/JSON links 404 for every non-public page: the editor and the preview both hardcode the public slug URL. Unlisted links carry `?token=`; internal pages use the authed `GET /status-pages/{pageID}/feed` (built for this, never called). | no (link targets only, no new UI) |
| 3 | iter-0057 | **Audit log paging.** `MembersPanel` hardcodes `limit: 30` with no way to see more; a "Show more" control raises the window (backend already accepts any limit ≤ 500). | yes (small) |
| 4 | iter-0058 | **Escalation progress on the incident.** `escalation_step`/`last_escalated_at` are written by the escalation engine and surfaced nowhere. `domain.Incident` gains both, the store reads them, the incident header shows "escalated to step N · <time>" — what an on-call responder needs when deciding to acknowledge. | yes |
| 5 | iter-0059 | **Monitor defaults that actually apply.** The monitor form hardcodes its own defaults and always sends explicit values, so Settings → Monitor defaults never affects UI-created monitors; and `retries`/`renotify_seconds` are `int` in the create body, so an explicit `0` is indistinguishable from absent and gets silently replaced. Fix both: a lightweight authed `GET /api/v1/monitor-defaults` (effective values; the settings PUT stays admin-only) prefills the form, and the create/update handlers take `*int` for `retries`/`renotify_seconds` (absent → default, explicit 0 → 0). | yes |
| 6 | iter-0060 | **Editable escalation & on-call + safe deletes.** `PUT` endpoints for both exist, documented, with zero UI; deletes are one-click and lossy (policy delete detaches monitors via SET NULL; schedule delete cascades overrides). Edit forms in `EscalationView` + inline confirm on both deletes with an explicit consequence note. | yes |
| 7 | iter-0061 | **TOTP policy honesty for SSO.** `require_totp` is enforced on local login only; the OIDC callback issues sessions without a TOTP gate. Decision: **keep** enforcement local-only — MFA for SSO users is the IdP's job — but say so: the policy help text and the domain doc state the local-only scope, and `LoginView` finally handles `totp_setup_required` (today a locked-out user gets a flat error with no path to enrollment). | yes (small) |

## Non-goals (accepted, documented here)

`sla_targets.project_id` stays as dormant schema (a project-level objective is a real
feature, not a gap fix) — **superseded**: it became that feature in iter-0155 (AC-0155-3,
migration 00083), which is exactly the "real feature" this line was reserving it for; dead store functions stay as test support (now labeled);
`GET /organizations/{orgID}`-style read-one endpoints stay (list responses satisfy the
SPA); agent transport routes stay undocumented in openapi by design.

## Acceptance (package-wide)

Per iteration: `-race` suite green, vue-tsc green for SPA work, live E2E of the specific
gap, iteration report + decision record + traceability row. The three audit sweeps,
re-run after the package, must return no findings outside the documented non-goals.
