# ops-logging — the logger, and a file an operator can grep (FR-027 / NFR-022)

> **Lifecycle: DESIGN — revision 1, NOT approved.** Opened 2026-09-01 from an operator complaint:
> searching the history through `journalctl` is too slow on a systemd install. The owner chose the
> middle option — cerbix opens the file and REOPENS it on `SIGHUP`, logrotate keeps owning rotation.
> No requirement row exists in `docs/status.md` yet: FR-027 and NFR-022 get theirs when this design is
> approved, per the rule iter-0164 followed for FR-025 and iter-0166 for FR-026. The sections below
> that describe the CURRENT logger are not a proposal — they document what already holds.

## 1. What this is, in one paragraph

cerbix logs to stdout and nowhere else (`internal/logging/logger.go`, six call sites in
`internal/cli/cli.go`). Under systemd that means journald, and journald is a poor place to SEARCH: the
journal is compressed, `journalctl -u cerbix | grep` decompresses and formats every record on the way
past, and an operator hunting one request id across a week waits for it. Under Docker it is already a
file (`docker/docker-compose.prod.yml` pins the `json-file` driver at 100 MB × 5), so this requirement
is about the systemd path. FR-027 lets an operator name a file; cerbix writes to it, keeps writing to
stdout unless told otherwise, and reopens the file on `SIGHUP` so ordinary logrotate works without
`copytruncate` and without losing the lines that land during a rotation. cerbix does not rotate, does
not compress, does not expire: logrotate has done that correctly for twenty years and this requirement
does not compete with it.

## 2. Requirements

- **FR-027 — a log file an operator can grep.** `log.file` names an absolute path, and cerbix uses it
  VERBATIM: nothing is appended, substituted or derived. When it is set, every record the logger emits
  is written there as well as to stdout (`log.stdout`, default true, may turn stdout off). The file is
  created if missing, appended to if present, and REOPENED when the process receives `SIGHUP` — so a
  rotation is `mv` + `HUP` and no line is lost or written to a file that no longer has a name. Giving
  each role on a host its own path is the OPERATOR's responsibility; this requirement discharges its
  half by saying so plainly wherever the key appears. When `log.file` is empty the process behaves
  exactly as it does today.
- **NFR-022 — logging never becomes the outage.** A write failure — a full disk, a revoked
  permission, a deleted directory — never terminates the process, never blocks a request path beyond
  the write itself, and is never silent: the record falls back to stderr and the failure is counted.
  The file sink adds no ordering or interleaving guarantee that stdout does not already have, and the
  reopen is atomic with respect to writes: a record is never split across two files or written to a
  descriptor mid-close.

## 3. The decisions

**D1 — the empty path is the current product.** `log.file: ""` is the default, and on that path the
logger is constructed exactly as today. A requirement that changes the default logging of every
existing installation would be a migration, not a feature.

**D2 — both sinks, not either/or.** With a file configured, stdout keeps receiving the same records
unless `log.stdout: false`. The reason is operational: `systemctl status cerbix` shows the last
journald lines, and an operator diagnosing a service that will not start reads exactly that. Trading
it away to gain a searchable file is a bad trade when both are available. Cost, stated: double the
write syscalls per record. `log.stdout: false` exists for whoever decides that matters.

**D3 — cerbix does not rotate.** No size, no age, no retention, no compression, no `.1.gz`. logrotate
already does this, is already installed on the hosts this requirement targets, and is better tested
than anything this repository would write. cerbix owns exactly one half of the contract: reopening.

**D4 — `SIGHUP` reopens, and the hazard is named here rather than discovered.** Go's default action
for `SIGHUP` is to TERMINATE the process. A `postrotate` stanza that sends `HUP` to a cerbix that
predates this requirement kills the service. The runbook must therefore sequence the rollout — the new
binary first, the logrotate file second — and the sentence saying so is part of this requirement, not
an afterthought. `SIGHUP` with no file configured is a no-op, not an error and not an exit.

**D5 — a failed write degrades, it does not kill.** On a write error the record is written to stderr
instead, `cerbix_log_file_write_errors_total` is incremented, and the process continues. The sink
retries the file on the next record — a full disk that gets space back needs no restart. Errors are
not logged through the logger itself (that is the recursion this class of bug is famous for); the
fallback write to stderr IS the report.

**D6 — the path is the operator's, verbatim, and the collision is documented rather than engineered
away (owner, 2026-09-01).** Two roles on one host writing to one file interleave their records once a
record exceeds the atomic-append size, and cerbix cannot tell that case from a restart. There were two
ways to answer it, and the owner considered both.

The rejected one was to take a DIRECTORY and derive the file name from the role and region
(`cerbix-api.log`, `cerbix-worker-<region>.log`). It makes the collision impossible for different
roles — and it makes cerbix invent a filesystem layout nobody asked for. The operator who writes
`/var/log/cerbix/api.log` in a config and finds `/var/log/cerbix/cerbix-api.log` on disk has met
implicit behaviour, and implicit behaviour is a worse defect class than a documented obligation: it
surprises at the moment someone is already debugging something else, and it makes every downstream
thing — the logrotate pattern, the shell history, the runbook — depend on a rule they have to learn.

So the path is used exactly as written, and the uniqueness obligation stays with the operator. What
this requirement owes in return is HONESTY about it, and that is a deliverable rather than a courtesy:
the config comment, the `overview.md` table row, the unit example in `INSTALL.md` and the runbook all
state that each role needs its own path and what happens if two share one. A hazard the product
names in the three places an operator reads is not a trap; a hazard it silently rewrites is.

**D7 — absolute paths only, mode 0640, and the directory belongs to the operator.** A relative
`log.file` is refused by config validation, because the process's working directory is not something
an operator should have to reason about. cerbix creates the FILE (0640) and never the directory —
under systemd the directory is `LogsDirectory=cerbix`, which creates `/var/log/cerbix` with the unit's
ownership and survives `ProtectSystem=strict`. A missing or unwritable directory is a startup error
naming the path, not a silent fall back to stdout.

**D8 — the writer is one small type with a mutex.** `Write` takes the lock, writes, releases;
`Reopen` takes the same lock, closes the old descriptor and opens the path fresh. That is what makes
NFR-022's "never mid-close" true, and it is the whole concurrency design — slog handlers already
serialize their own records, and this writer serializes across them.

**D9 — Docker is out of scope but not broken.** Setting `log.file` inside a container writes to the
container filesystem, which disappears with the container unless a volume is mounted. The compose
files are not changed and the `json-file` driver keeps doing what it does; the runbook says this in
one sentence so nobody discovers it by losing logs.

## 4. What must move WITH the implementation, not after it

- `INSTALL.md` — the systemd unit gains `LogsDirectory=cerbix` and a `log.file` under it; the
  distributed example shows TWO roles with TWO paths, which is how the obligation of D6 is taught
  by example. The hardening comment that says "cerbix writes nothing to disk" stops being true and
  must say what changed.
- `docs/runbook.md` — the logrotate file, the rollout order of D4, the search recipes that motivated
  the requirement, the one-line Docker note of D9, and what two roles sharing one path actually
  produces, so the reader learns it here rather than from an interleaved file.
- `docs/overview.md` — `log.file` and `log.stdout` in the configuration table, and the row says in
  its own words that each role needs its own path (D6's half of the bargain, not a footnote).
- `docs/traceability.md`, `docs/status.md` — the discharge map and the requirement rows.

## 5. Config surface

```yaml
log:
  level: info                        # unchanged
  format: json                       # unchanged
  # Absolute path, used verbatim. Empty (default) = stdout only, today's behaviour.
  # EACH ROLE ON A HOST NEEDS ITS OWN PATH: two processes appending to one file
  # interleave their records, and cerbix cannot detect it.
  file: "/var/log/cerbix/api.log"
  stdout: true                       # keep writing to stdout when a file is configured
```

Validation: `file` must be absolute when non-empty; `stdout: false` with an empty `file` is refused,
because it asks for a process that logs nowhere. The comment above is normative — it ships in the
example config, because D6 makes the warning part of the deliverable.

## 6. Acceptance invariants (FR-027)

1. `log.file: ""` produces byte-identical output and behaviour to the current binary — same handler,
   same writer, no file opened.
2. With a file configured, every record reaches BOTH sinks; with `log.stdout: false`, only the
   file.
3. The path is used VERBATIM: the file appears at exactly the configured path, with nothing appended,
   substituted or derived from the role, the region, the host or the time.
4. The obligation D6 leaves with the operator is stated wherever the key appears — the example config,
   the `overview.md` row, the `INSTALL.md` unit and the runbook — and the distributed unit example
   shows two roles with two paths. A change that adds the key somewhere without the sentence is a
   defect against this requirement, not a documentation nicety.
5. The file is created when missing (0640) and appended to when present; contents survive a restart.
6. `SIGHUP` closes the old descriptor and opens the path again: after `mv cerbix.log cerbix.log.1;
   kill -HUP`, new records land in a NEW file at the original path and the renamed file stops growing.
7. `SIGHUP` with no file configured is a no-op: the process does not exit, and nothing is logged
   about it beyond a debug line.
8. A write error sends the record to stderr, increments `cerbix_log_file_write_errors_total`, and does
   not terminate the process; a later successful write resumes the file with no restart.
9. Records are never interleaved within themselves under concurrent writers, and never written to a
   descriptor being closed by a concurrent reopen (proved under `-race`).
10. A relative `log.file`, and `stdout: false` with no file, are refused by config validation with a
    message naming the key.
11. A missing or unwritable directory is a startup error naming the path, not a silent fall back to
    stdout.
12. The five roles behave identically: the sink is chosen once where the logger is built, and nothing
    about it varies by role.

## 7. Required test matrix (written before the code)

*The writer:* write, rename, reopen, write again → the second record is in the new file and the
renamed one is unchanged, asserted by inode and by content · concurrent writers plus a reopen under
`-race` → no torn record, no write to a closed descriptor · a file made unwritable mid-run → the
record appears on stderr, the counter moves, the process lives · the file becoming writable again →
records return to it with no restart · close is idempotent.

*The path:* the file appears at exactly the configured path — asserted by reading THAT path, not by
globbing a directory · nothing is appended for the role or the region: the same config produces the
same filename under every `--role` · a path under a directory that does not exist, and one under a
directory that is not writable, are startup errors naming the path.

*Config:* empty path → no file opened (asserted by the absence of the file, not by a flag) · relative
`log.file` → refused, message names the key · `stdout: false` with empty `file` → refused.

*The documented obligation (invariant 4):* a test reads the shipped example config, the `overview.md`
configuration row and the `INSTALL.md` unit, and fails if `log.file` appears in any of them without
the per-role sentence beside it. The warning is a deliverable, so it is checked like one — the same
way `make docs-check` checks the claims that matter elsewhere in this repository.

*Wiring:* each of the five roles builds the same sink from the same config · `SIGHUP` with a file →
reopen happens once per signal · `SIGHUP` without a file → process alive, no error · the existing
`SIGINT`/`SIGTERM` shutdown path is unchanged (regression).

*Format parity:* a record written to the file is byte-identical to the same record on stdout — the
file sink is a writer, not a second formatter.

## 8. Operational hazards, stated so they are not discovered

- `SIGHUP` kills a cerbix that predates this requirement (D4). Rollout order is binary, then logrotate.
- `copytruncate` becomes unnecessary and should be REMOVED from any existing logrotate file for
  cerbix; leaving it means a truncate under a live descriptor and a file that appears to contain
  nothing but null bytes until the next write.
- `ProtectSystem=strict` refuses writes outside the unit's own directories; without
  `LogsDirectory=cerbix`, `log.file` fails at startup with permission denied, which is the correct
  failure but reads as a cerbix bug if the unit is not read first.
- Two roles pointed at one path interleave their records once a record exceeds the atomic-append
  size, and cerbix cannot tell that from a restart. This is the operator's to avoid and the product's
  to state clearly (D6): one path per unit.
- A container writing to `log.file` without a mounted volume loses the file with the container (D9).

## 9. Non-goals of FR-027

Rotation, compression, retention and expiry (D3 — logrotate owns them); a syslog or journald-native
sink; shipping logs anywhere (no agent, no HTTP, no queue); per-role or per-LEVEL files, and any derivation of the name from role, region, host or time (D6 — the path is the operator's, verbatim); a second
format for the file; sampling or rate limiting; changing what is logged, at what level, or the secret
ban already enforced by `forbidigo`; and reopening on anything other than `SIGHUP` (no inotify, no
size watch, no timer).
