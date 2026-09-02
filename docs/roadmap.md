# cerbix — roadmap

What is next, in the order it should happen, and why. Live requirement status is
[`status.md`](status.md); the decisions behind every item are in [`decisions.md`](decisions.md); this
document holds only the ORDER and the reasoning for it. It is edited in place — an item leaves it by
being done, or by being declined with its reason moved to §5.

**Where the tree stands (2026-09-03).** Every requirement in `status.md` is `DONE` except FR-026 and
NFR-021, which entered as `TODO` when their design was approved (D-0214). iter-0166 and iter-0167 are
closed; no iteration is open. Two releases since this paragraph last said anything true: **`v0.1.7`
(2026-09-02)** — promql in Monitoring-as-Code bundles, optional basic auth, the Go pin that closed the
`govulncheck` findings — and **`v0.1.8` (2026-09-03)**, which is FR-028 / NFR-023 end to end: a
credential inside a synthetic scenario is a secret at rest, on read and in the record, plus the editor
that declares one without an operator typing a credential. Both release bodies are their CHANGELOG
section; for `v0.1.8` the image tags `0.1.8`, `0.1` and `latest` point at one digest and the GitHub
release is `latest`. What remains is one designed-and-unbuilt requirement, a dependency sweep, and a
design awaiting review.

---

## 1. Now — the canary, then the requirement that is already designed, then the sweep

**R5 — the typed external canary (FR-029 / NFR-024).** The owner's decision on 2026-09-03: this goes
first and R2/R3 wait. It is the largest single feature since FR-021 — a new monitor type
(`async_canary`), one closed workflow kind (`async_transaction_v1`), capability-aware dispatch, a
nested typed bundle schema, an embedded fixture registry, an in-flight lease, an idempotency contract
and an SSRF policy — so it is SIX phases and not one iteration
([`func-async-canary.md`](specs/func-async-canary.md), revision 8, **design APPROVED** 2026-09-03 after
six review rounds and seventeen P0s; D-0218). FR-029 and NFR-024 are `TODO` rows in `status.md`, phase
A is closed, and **phase B is the next unit of work**: the domain types, the typed parser,
canonicalization and the semantic hash, with the type NOT yet admitted to bundles.
Two architecture rulings are already made and are what keep it from being a rewrite: it is a
CAPABILITY of the existing `worker`/`agent` roles rather than a fifth role, and one probe stays one
heartbeat rather than splitting submit from await. §11's owner questions are all answered, including the sign-off on the
one deviation from the brief: `secret_ref` names a binding declared in `workflow.secrets`, because the
flat key is what keeps rename, rotation and delete-counting on the path `password_ref` already runs
on.

**R2 — implement FR-026 / NFR-021 (incident audit).** *Deferred behind R5 by the owner, 2026-09-03 —
written down so "designed and unbuilt" does not quietly become "forgotten".* The design is approved at revision 4 and the
spec is the contract: `func-incident-audit.md`. It is the only requirement in the tree that is
specified and unbuilt, and it closes the last item D-0171 left open. Its shape is small — no
migration, no route, no read — but it carries two behaviour corrections (D8a, D8b) that need their own
regression care, and two fixture-tested AST guards that are new machinery for this repo. One
iteration.

**R3 — the dependency sweep.** **Eight** open dependabot PRs as of 2026-09-03, all newer than the last
sweep (iter-0159, 2026-08-19): the go-modules group (2 updates), the frontend group (3 updates),
`docker/setup-buildx-action` 4.2.0 → 4.3.0, the `golang` base image 1.26.6 → **1.27.0**, and four
MAJORS — TypeScript 5.9 → 7.0, Vite 6.4 → 8.2, vue-router 4.6 → 5.2, jsdom 26 → 30. (This paragraph
said "nine branches" and named a different base-image bump until it was checked against the API; the
count moves on its own, so treat it as a shape rather than an inventory.) iter-0159's rule applies
unchanged: patch and minor together, each major alone with its own verification, and `make
spa-snapshot` read back afterwards. The `golang` bump is NOT what closed R1 — that was a toolchain pin
in `go.mod`, which no dependabot branch touches; this one moves the image the container is built on,
and it is worth its own verification for that reason.

**Order — R5, then R2, then R3, with R4 independent of all three.** The owner set this on 2026-09-03,
against my recommendation to take R3 first; recorded that way rather than smoothed over, so the cost
is visible if it bites. What the cost is: four frontend MAJORS (TypeScript 7, Vite 8, vue-router 5,
jsdom 30) stay unmerged across a multi-phase feature that touches the frontend last, so the sweep will
land on a larger surface than it would today. What the benefit is: the canary is the only item here
with a waiting external use case, and its design questions are answered while they are fresh. R4 is
independent of all three — it touches the logger and the deployment documents — and cannot start until
its own design is approved.

**Retired from this section**, with what actually closed each — the labels stay put so the commits and
review threads that name them keep resolving.

*R1 — the red Security workflow* — closed in `v0.1.7` (2026-09-02). Both of its jobs had failed on
every push to `main` since before the FR-024/FR-025 arc, and `v0.1.6` shipped with the signal already
red, which is why it was R1 rather than a footnote. `govulncheck`: the Go pin moved to a patch release
carrying the standard-library fixes, which moved the scan AND the release binaries because all three
workflows read the version from `go.mod` — the job was never silenced. `gitleaks`: all seven findings
were read one by one, not only the two that had been when this item was written, and each is a
fabricated test fixture (two sequential-hex AES keys, the same two base64-encoded, and an
`actor_label` in a committed CLI transcript, which is the server's derived name for a bearer and never
the token). Nothing needed rotating; the allowlist entries are anchored to those exact literals, so any
other high-entropy string in the same files still fails the scan. Green on `main` since, including on
the `v0.1.8` push.

*Cut `v0.1.6`* — done on 2026-09-01: the release carries the CHANGELOG section as its body (a workflow
step now extracts it for any tag), the upgrade notes name the `incidents` index build and the leader's
new maintenance pass, and `latest` moved in both GitHub and ghcr.

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

**N4 — the synthetic binding editor. DONE in iter-0167 (2026-09-03), kept here for one line of
history.** The mock was approved by the owner at revision 1
([`docs/design/mock-synthetic-binding.html`](design/mock-synthetic-binding.html), five screens) and the
editor was built to it, so FR-028 stage 2 is now editor-reachable and not only API-reachable. The work
also fixed a defect older than itself: `canSubmit` demanded a target the form hides for `synthetic`, so
that type could not be created from the SPA at all — no unit test and no E2E had ever submitted it. The
reviewer's three requirements, all delivered: show the
binding → inventory mapping EXPLICITLY (which project secret fills which binding, by name, with the
used-by state visible); carry D7's rule INLINE where the operator types a credential-bearing header,
not as a refusal after a failed save; and state the "save before test" contract in the editor, since a
scenario with bindings is deliberately not testable before it is stored. A fourth followed from D9 and is
visible rather than discovered: a synthetic monitor still cannot be exported to a bundle.

**N5 — the second `{{secret:…}}` consumer, if one ever appears.** The binding grammar, the placeholder
and the substitution live in `internal/domain/syntheticbindings.go` and are synthetic-only by explicit
type gate (D6b). Nothing else in the product templates a credential today. If a second consumer is ever
proposed, the type gate is the thing to widen deliberately — not the prefix match that produced the
round-5 P0.

**N6 — a canary cannot log in: D7 refuses an EXTRACTED token in a credential-bearing header.** Raised
by the owner asking how to run an external canary, which is exactly the shape that hits it. A
credential-bearing header must hold exactly one `{{secret:<binding>}}`, so the classic journey — `POST
/login` → `extract` the session token → `Authorization: Bearer {{token}}` — is refused on every write
surface. That is not a bug in the rule as written; it is the rule doing what D7 says, and the cost was
not weighed when D7 was written because nobody had a login-then-act scenario in front of them.

What exists today: a long-lived credential from the inventory as a binding (fine when the target
accepts an API key or basic auth), or the extracted token in a NON-credential header — legal, and
exactly the residual D7 tells operators to avoid, so it is a workaround that spends the rule's own
credibility.

The proposed fix, which needs a D7 amendment and a review round rather than a quiet patch: allow a
credential-bearing header to interpolate variables EXTRACTED in the same scenario, because the rule's
purpose is that no STORED credential sits in the document and an extracted value is stored nowhere —
it comes from the target, at run time, and dies with the probe. Enforceable shape: the value may
contain `{{secret:<binding>}}` and/or `{{var}}` where `var` is declared by an earlier step's `extract`,
plus a bounded auth-scheme word (`Bearer`, `Basic`, `Token`), and nothing else. A literal token is
still refused, because a literal is neither of those two things. Until this is decided, the canary
recipe in `docs/runbook.md` should not pretend the login flow works.

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
  fixed does not rebuild. **Both upgrade notes this list was holding are DISCHARGED**: the URL-userinfo
  refusal shipped with its note in `v0.1.7` (D-0145 addendum), and the literal-in-a-credential-header
  refusal shipped with its note in `v0.1.8` — which also carries a third the list never anticipated,
  that probe failure messages changed shape for EVERY monitor type, so anyone matching `heartbeat.msg`
  substrings in alert rules has to look. **The next tag owes nothing today.** Add the obligation here
  the moment a breaking change lands, not at release time: this list existed because the v0.1.6 cut
  nearly shipped one unannounced.
- **AC-SYN-2: a synthetic monitor on a GEO worker.** iter-0167 gave the type its first browser
  coverage, on `core` only, so the criterion is narrowed and still undischarged. It needs a case on
  the geo stack (`make geo-up-all` / `make geo-test`), where region affinity is what is actually being
  claimed — `synthetic` carries no region rule of its own, so this is about proving the general rule
  holds for it rather than about synthetic-specific code.
- **A load-dependent flake in FR-024's gate maintenance** —
  `TestGateMaintCrashAfterEveryRemovalStatementConverges/after_drop.commit` fails as `crashed=false
  err=<nil>`: the crash injected after the drop's commit does not fire. Reproduced 2 of 5 runs with the
  machine under a competing docker build, 12 of 12 clean when idle, and reproduced at `b3c99b6` in a
  clean worktree — so it predates iter-0166 and is not caused by FR-028. It is a red CI gate under load
  and it has no owner yet; whoever takes it should start from whether the crash hook's statement
  boundary is what the pass actually executes when the purge is due at once (`PurgeEvery = 0`).
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
