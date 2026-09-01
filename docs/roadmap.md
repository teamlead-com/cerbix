# cerbix — roadmap

What is next, in the order it should happen, and why. Live requirement status is
[`status.md`](status.md); the decisions behind every item are in [`decisions.md`](decisions.md); this
document holds only the ORDER and the reasoning for it. It is edited in place — an item leaves it by
being done, or by being declined with its reason moved to §5.

**Where the tree stands (2026-09-01).** Every requirement in `status.md` is `DONE` except FR-026 and
NFR-021, which entered as `TODO` when their design was approved (D-0214). iter-0165 is closed; no
iteration is open. `v0.1.5` was cut on 2026-08-28 and **85 commits have landed since**, including two
whole requirements (FR-024 reliability gate, FR-025 change intelligence), two migrations (`00093`,
`00094`), two CLI verbs (`cerbix gate check`, `cerbix change record`) and the notification-channel
edit. The product is further ahead of its last release than it has been at any point in this project.

---

## 1. Now — the release, then the requirement that is already designed

**R1 — cut `v0.1.6`.** The largest gap in the tree is not a missing feature, it is that nothing since
2026-08-28 has been released. Two migrations land with it, so the upgrade notes are not optional:
`00093` creates the gate's daily-partitioned decision ledger and its registry, `00094` the change
schema. `CHANGELOG.md` is written at release time by this project's convention, and its release-notes
style is set by the `v0.1.5` entry. Blocking nothing, blocked by nothing — and every day it waits, the
notes get harder to write honestly.

**R2 — implement FR-026 / NFR-021 (incident audit).** The design is approved at revision 4 and the
spec is the contract: `func-incident-audit.md`. It is the only requirement in the tree that is
specified and unbuilt, and it closes the last item D-0171 left open. Its shape is small — no
migration, no route, no read — but it carries two behaviour corrections (D8a, D8b) that need their own
regression care, and two fixture-tested AST guards that are new machinery for this repo. One
iteration.

**R3 — the dependency sweep.** Nine dependabot branches are unmerged and all of them are newer than
the last sweep (iter-0159, 2026-08-19): the go-modules group, the golang base image, a GitHub action,
the frontend group (6 updates), `vue-tsc`, and four MAJORS — TypeScript 5.9 → 7.0, Vite 6.4 → 8.2,
vue-router 4.6 → 5.2, jsdom 26 → 30. iter-0159's rule applies unchanged: patch and minor together,
each major alone with its own verification, and `make spa-snapshot` read back afterwards. Cheap per
branch, and it gets more expensive the longer four majors sit.

**Order.** R1 first because it is the only item whose cost grows with delay and whose risk is entirely
in the past (the code is already green). R2 second because it is designed, bounded, and closes a named
gap. R3 third because it is interruptible: each branch is its own commit and the sweep can be paused
between majors without leaving anything half-done.

---

## 2. Next — the debts this arc created

**N1 — decide what to do with the notification-channel edit.** It shipped on the owner's explicit
lightweight path: no `func-*` spec, no iteration report, no decision record, no traceability row, no
independent review. The code is tested (store, API, six vitest cases, a live E2E) and the runbook
documents it, so the debt is process, not correctness. Two honest options: fold it into the next
iteration's report as a named exception, or leave it and record WHY the lightweight path was taken —
either closes the hole; leaving it unnamed does not.

**N2 — the two follow-ups D9 declined.** An incident-scoped audit panel (`GET …/incidents/{id}/audit`
plus a block on the incident page) needs a target index or an incident column; a project-scoped audit
read needs a `project_id` column on `audit_logs` and its own RBAC, because reading the trail is
org-manage today. Both were declined for FR-026 as bigger than the gap being closed. Neither has been
asked for by a user; they are candidates, not commitments, and they belong AFTER FR-026 ships so the
rows they would read actually exist.

**N3 — the E2E environment skips.** Two specs skip conditionally — `mail.spec.ts` without the mail
profile, `file-providers.spec.ts` without a file-managed monitor. Both are honest guards rather than
defects, but a suite that reports "1 skipped" every run trains a reader to ignore the number. Either
make the stack always satisfy them or state in the report which skip is expected and why.

---

## 3. Later — the things the specs already point at

These are named in specs as follow-up REQUIREMENTS rather than non-goals. None is scheduled.

- **A project-level inherited gate policy** (FR-024 D13, second step): a policy declared once for a
  project and inherited by its services, instead of per-service declaration.
- **Worst-of-all-windows gate evaluation** (FR-024 D2): today a policy names ONE SLO window; the
  alternative evaluates every window and takes the worst.
- **Retention for `audit_logs`** — unbounded today for every action, not only incidents. FR-026 does
  not change that and says so; the table grows forever, and nothing in the product prunes it.

---

## 4. Standing work with no end state

- **Dependencies** — a sweep roughly monthly, under iter-0159's rules.
- **Releases** — a tag when a requirement closes or a migration lands, so the gap R1 is fixing does
  not rebuild.
- **The living documents** — `make docs-check` is the only mechanical guard; the failure it cannot
  catch is prose that is true of nothing, which is what the FR-025 closing arc kept finding.

---

## 5. Declined, with the reason, so nobody re-derives it

Not a backlog. These are POSITIONS, and a request to reopen one needs a new argument, not a reminder.

| Item | Where | Why not |
|---|---|---|
| Retroactive alerting | FR-021 §16.9 | a rule enabled today says nothing about last week |
| Per-member severity inside a service page | FR-021 §16.9 | the alert links to the service; which member is diagnostics |
| Cross-project delegation | FR-021 §16.9 | an open question nobody has asked (D-0172) |
| Suppression beyond the three named topics | FR-021 §16.9 | SLA reports, incident webhooks and status-page output stay untouched |
| Auditing machine incident writes | FR-026 D1 | the incident timeline is their record; a flapping service would bury the log |
| DORA metrics, a change calendar, freeze windows | FR-025 §9 | the gate is the control; cerbix keeps no deployment catalog |
| Causal attribution ("caused by") | FR-025 D7 | the product knows "preceded", and says so |
| Automatic rollback or any action on an external system | FR-024 §9, FR-025 §9 | an invariant, not a deferral |
| Observability positioning (APM, tracing, log aggregation, a metrics backend) | D-0174 | cerbix ingests its own probe results and no external telemetry |
