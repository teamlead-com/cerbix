# cerbix — roadmap

What is next, in the order it should happen, and why. Live requirement status is
[`status.md`](status.md); the decisions behind every item are in [`decisions.md`](decisions.md); this
document holds only the ORDER and the reasoning for it. It is edited in place — an item leaves it by
being done, or by being declined with its reason moved to §5.

**Where the tree stands (2026-09-01).** Every requirement in `status.md` is `DONE` except FR-026 and
NFR-021, which entered as `TODO` when their design was approved (D-0214). iter-0165 is closed; no
iteration is open. **`v0.1.6` shipped on 2026-09-01** — 85 commits over `v0.1.5`, carrying two whole
requirements (FR-024 reliability gate, FR-025 change intelligence), two migrations (`00093`, `00094`),
two CLI verbs (`cerbix gate check`, `cerbix change record`) and the notification-channel edit; the
release body is the CHANGELOG section and the image tags `0.1.6`, `0.1` and `latest` point at it. The
release backlog that dominated this list is therefore empty, and what remains is one red pipeline, one
designed requirement and a dependency sweep.

---

## 1. Now — the red pipeline, then the requirement that is already designed

**R1 — make the Security workflow green, and keep it that way, BEFORE the next tag.** Both of its jobs
fail on every push to `main`, and have since before the FR-024/FR-025 arc — `v0.1.6` shipped with the
signal already red, which is the reason this is R1 rather than a footnote. A check that is always red
is a check nobody reads, and the next release would inherit that.

- **`govulncheck ./...`** reports vulnerabilities in the Go standard library against the version the
  workflow pins through `go-version-file: go.mod` (`go 1.25.12` today). The fix is to move the pin to a
  patch release that carries the fixes and re-run, not to silence the job. Reproduced locally against a
  different toolchain (1.26.4), where it reports eight standard-library findings fixed in 1.26.5/1.26.6
  plus two in imported packages and one in a required module that the code does not call — the exact
  list under CI's pin will differ, the class will not.
- **`gitleaks`** scans the full history (581 commits) and reports seven findings. The two that were
  inspected are base64 32-byte keys in `internal/config/secrets_test.go` — test fixtures caught by the
  `generic-api-key` rule. **The other five have not been reviewed**, and until they are, "false
  positives" is a guess. The order matters: review all seven first, ROTATE anything genuine before
  touching the config (the scan reads history, so a secret deleted by a later commit still leaks), and
  only then allowlist the fixtures in `.gitleaks.toml` — by path or fingerprint, never by disabling the
  rule.

Cheap either way, and it is the one item that gates a release rather than being one.

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
each major alone with its own verification, and `make spa-snapshot` read back afterwards. Related to
R1 but not the same work — R1's Go finding is a toolchain pin, which no dependabot branch touches.

**Order.** R4 is sequenced last of the four but is independent of the other three: it touches the
logger and the deployment documents, nothing R1–R3 own. R1 first because it gates the next release and because every day it stays red is a day the
habit of ignoring it hardens. R2 second because it is designed, bounded, and closes a named gap. R3
third because it is interruptible: each branch is its own commit and the sweep can be paused between
majors without leaving anything half-done.

**Retired from this section.** *Cut `v0.1.6`* — done on 2026-09-01: the release carries the CHANGELOG
section as its body (a workflow step now extracts it for any tag), the upgrade notes name the
`incidents` index build and the leader's new maintenance pass, and `latest` moved in both GitHub and
ghcr.

**R4 — a log file an operator can grep (FR-027 / NFR-022).** Searching the history through
`journalctl` is too slow on a systemd install: the journal is compressed and every record is
decompressed and formatted on the way past a `grep`. The owner picked the middle option — the operator names an
absolute path in `log.file`, cerbix uses it VERBATIM and REOPENS it on `SIGHUP`, so ordinary logrotate keeps owning rotation and
no line is lost to `copytruncate`; stdout keeps receiving the same records so `systemctl status` stays
useful. Design is written and awaiting review (`docs/specs/ops-logging.md`, revision 1); no
requirement row until it is approved. Estimated at half an iteration — the cost is not the write, it
is the signal handler across five roles, the test that proves the reopen honestly (rename, HUP, assert
the new inode grows and the old one does not), and the operational sequencing: Go's default action for
`SIGHUP` is to TERMINATE, so a logrotate file installed before the new binary kills the service.
Giving each role on a host its own path stays the operator's job — deriving a name from the role was
considered and rejected, because a file appearing somewhere other than where the config says is
implicit behaviour, and the product's half of that bargain is to say so wherever the key appears
rather than to rewrite the path quietly. Docker is unaffected — the `json-file` driver already writes
rotated files there.

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
- **Releases** — a tag when a requirement closes or a migration lands, so the gap the v0.1.6 item
  fixed does not rebuild. **The next tag owes an upgrade note**: a monitor target carrying credentials
  in its URL userinfo (`https://user:pass@host`) is now refused on every surface, not only in bundles
  (D-0145 addendum). An installation relying on it will see its next monitor EDIT rejected — stored
  monitors keep running, because nothing re-validates on read. **And a second one from FR-028 stage 2:**
  a synthetic monitor whose `authorization`-class header holds a LITERAL now fails validation on its
  next write — it keeps probing, and the refusal names the step and the header.
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
