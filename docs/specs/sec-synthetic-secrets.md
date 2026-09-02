# sec-synthetic-secrets — a credential inside a scenario is a secret everywhere (FR-028 / NFR-023)

> **Lifecycle: STAGES 0 AND 1 IMPLEMENTED (2026-09-02); stage 2 designed and unbuilt.** Design approved
> by the reviewer at revision 3 and amended by the owner's §10 ruling; the implementation notes below
> record what the code actually does, so a reader never has to infer it from the design. Opened 2026-09-02 after an operator asked whether a
> synthetic monitor could carry a bearer token, and the answer turned out to be "yes, in cleartext, to
> everyone". Reviewed adversarially on the party before this text existed; every finding of that round
> is folded in and named where it changed the design. This is a **spec-versus-code defect**, not an
> enhancement: `func-oncall-synthetic-pull.md` FR-SYN-1 already says "the scenario/secrets in `config`
> (encryption like the other types)" and its §217 says "secrets (scenarios, tokens) — via
> `secret.Cipher`, included in `reencrypt`". Neither is true of the tree. FR-028 and NFR-023 get their
> rows in `docs/status.md` when this design is approved.
>
> **Round 2 found two blockers, both verified in the tree before they changed this text.** Rename of an
> inventory secret does NOT survive a scoped `setting_key` — `repointSecretRefs` fails the whole rename
> when the flat config does not carry the ref key, deliberately (D6a). And the test-before-save path
> sends the literal scenario to a worker while §9 called that a non-goal, contradicting FR-028 (D10).
> Round 2 also refused the owner-question recommendation of revision 1 with an argument that holds:
> `Cipher.Encrypt` returns plaintext unchanged when no key is configured, so a red readiness signal
> repairs nothing already stored (§10). Revision 1's answers to (a) and (b) were accepted with a
> refinement each: the three sets are a policy CLASSIFICATION rather than an authorization mechanism
> (D4), and "location" had to become a canonical descriptor before invariant 11 could be tested at all
> (D8a).
>
> **Round 3 approved the architecture after three corrections, each an internal contradiction of my own
> text rather than a design fault:** D5 inherited "cipher-nil tolerant" from the push-token backfill
> while §10 requires the opposite (an implementer copying the precedent faithfully would have made the
> gate green over untouched plaintext); NFR-023 banned any transient plaintext in a display read, which
> is the very read invariant 6 requires of a writer; and D6's closing sentence still claimed rename
> works unchanged three lines above D6a proving it cannot. All three are fixed here. **The design is
> approved; §10 is the owner's to answer before any code.**
>
> **Round 4 — the owner answered §10 by rejecting its premise (2026-09-02).** A service must not go
> down because of an unset variable or one misconfigured monitor, so readiness is never coupled to the
> at-rest key or to a migration. §10 is rewritten around blast radius, D5 loses the readiness gate, and
> invariant 4 changes accordingly. The pattern is not new — `internal/worker/worker.go` already answers
> an unopenable credential with a per-job typed diagnostic and flips readiness only on a persistent
> systemic failure.

## 0. What is built, as of 2026-09-02

**Stage 0 — done.** No probe result carries a request URL, for any type:
`internal/prober/failure.go` composes a bounded failure class plus a host taken from the URL by that
code. Applied to `http`, `promql`, `synthetic` and the RabbitMQ management probe; the websocket probe
dials `host:port` and is asserted as a case rather than assumed clean.

**Stage 1 — done.** The one secret set became three classifications in `internal/domain/monitor.go`
(encrypted-at-rest, write-only-on-read, writer-only-display) and the reader became an explicit mode
named by WHAT it decrypts (`readSafe`, `readWriterOnly`, `readAll`). The scenario is ciphertext at
rest, covered by rotation, and backfilled at startup by `BackfillMonitorConfigEnc` — idempotent,
compare-and-set, and NON-fatal, per §10. A viewer receives no scenario from the detail or the list; a
writer does, through a store call chosen after the authorization decision. A partial update carries the
stored ciphertext forward. On a keyless instance a scenario write is refused with `ErrNoAtRestKey` and
nothing else changes.

**The trap the review predicted, caught by a test rather than by production:** the materializer reads
through the same boundary as a display list, so adding `scenario` to the encrypted set stripped it from
every job and every synthetic probe would have run with no scenario. The materializer now uses the
writer-only mode — the scenario is execution input, the credential is not, and the credential still
travels as an envelope.

**Stage 2 — not built.** D6 will store the ref NAME in a flat config key rather than inside the
scenario JSON, which keeps `repointSecretRefs`, `monitor_secret_refs` and rotation working untouched
and makes D6a unnecessary.

## 1. What this is, in one paragraph

A synthetic monitor's scenario is a JSON document in one config key (`internal/domain/synthetic.go`,
`SyntheticScenarioKey`): up to fifty steps, each with a URL, a header map, a body, extract rules and
assert rules. Operators put credentials in it, because that is what an authenticated multi-step journey
needs. Exactly one monitor config key is protected — `domain.SecretMonitorConfigKeys` holds `password`
and nothing else — and that single set means both "encrypt at rest" and "omit on read". So a bearer
token in a scenario is stored in cleartext in `monitors.config`, returned to every principal who can
READ the monitor, skipped by key rotation (`internal/store/reencrypt.go` re-encrypts only that set),
and echoed into `heartbeat.msg` whenever a step's transport fails, URL and query string included. FR-028
closes the exposure in three stages that ship independently, and NFR-023 says what must not break on the
way: a scenario is the check's definition, so whoever may edit the monitor must still be able to read
and change it.

## 2. Requirements

- **FR-028 — a scenario's secrets are secret at rest, on read, and in the record.** Stage 0: no probe
  result carries a step's URL — the failure is named by step and by error class. Stage 1: the scenario
  is encrypted at rest under `secret.Cipher`, covered by rotation and by a startup backfill, and
  withheld from any principal that cannot write the monitor. Stage 2: a secret inside a scenario is a
  NAMED BINDING resolved from the project secret inventory, never a literal, on every write surface —
  which also removes the reason a synthetic monitor cannot be declared in a Monitoring-as-Code bundle.
- **NFR-023 — the definition stays visible to whoever owns it, and nothing degrades silently.** An
  operator who may write the monitor sees and edits the whole scenario; the encryption boundary is not a
  redaction applied after decryption but a reader that never decrypts what it must not hand out; a
  SAFE reader and every scheduler snapshot decrypt no credential plaintext at all, while the
  authenticated WRITER reader may decrypt the SCENARIO only — never `password` — and only after
  `ActionProjectWrite` (party round 3: the earlier wording banned the very read invariant 6 requires);
  and an
  installation that cannot encrypt is told so rather than storing a token in cleartext behind a green
  status.

## 3. The decisions

**D1 — three stages, shipped separately, because they close different holes at different prices.**
Stage 0 is a leak into a stored, rendered string and costs an afternoon. Stage 1 stops the database and
API exposure and needs the read boundary reshaped. Stage 2 removes the credential from the scenario
altogether and changes FR-020's contract. Bundling them would hold the cheap fix hostage to the
expensive one.

**D2 — stage 0: a probe result names the step and the error class, never the URL.** `heartbeat.msg`
today is `stepLabel(i, st) + ": " + err.Error()`, and a transport error from `net/http` embeds the
request URL. Reproduced before writing this: a step against a dead port produced
`step 1: Get "https://127.0.0.1:1/x?token=…": dial tcp …: connection refused`, so a query-string
credential is in a stored, UI-rendered field. The fix is not to redact the message — pattern-matching a
secret out of prose is the guessing this repository refuses — but to STOP COMPOSING it: the step label,
a classified cause (dns, connect, tls, timeout, http status, assert, extract), and nothing echoed from
the request. This alone makes NFR-SYN-2 true, which it is not today.

**D3 — stage 1 replaces the one secret set with three, by MEANING (party round 1).** The review found
that the obvious move breaks the product: `scanMonitorMode(row, false)` DELETES every
`SecretMonitorConfigKeys` member without decrypting (`internal/store/monitors.go`), and
`MaterializeExecutionConfigs` (`internal/store/materialize.go`) reads through that same boundary — so
adding `scenario` to today's set would deny it to the writer AND strip it from the job, and every
synthetic probe would fail with no scenario configured. The sets are therefore:

| set | members | meaning |
|---|---|---|
| encrypted at rest | `password`, `scenario` | ciphertext in the column; covered by rotation and backfill |
| write-only on every read | `password` | never leaves the server, in any mode |
| writer-only display | `scenario` | decrypted for a principal who may write the monitor; absent otherwise |

`SecretMonitorConfigKeys` does not survive as a catch-all: a name that means three things is how this
defect was born.

**D4 — the three sets are a policy CLASSIFICATION; the reader is a MODE, and neither is an
authorization decision (party round 2).** The authorization decision is `ActionProjectWrite`, taken
where it is taken today; only after it does the handler SELECT a reader. The classifications stay
declarative and closed, and their allowed overlaps are asserted rather than assumed — `password` is
encrypted-at-rest plus write-only-on-read, `scenario` is encrypted-at-rest plus writer-only-display,
and no other combination is legal. No caller may infer a mode from request contents or from the monitor
type: a mode inferred from the payload is an authorization decision made by the attacker. `scanMonitorMode(row, decryptConfigSecrets bool)` becomes
an explicit enum with exactly three values, each named for the authorization that precedes it: the SAFE
reader (display and snapshots — decrypts neither), the WRITER reader (decrypts the scenario only), and
the EXECUTION reader (decrypts the scenario only, to build a job). No mode decrypts `password` into a
display or snapshot path, which is the property `scanMonitorNoSecrets` exists to hold and which must
survive this change. The mode is chosen AFTER the authorization decision, never inferred from the
request.

**D5 — stage 1 needs a backfill, not only rotation (party round 1).** `reencrypt` is an operator
command run at key rotation; nothing runs it on upgrade, so every scenario written before stage 1 would
stay plaintext indefinitely. The precedent is `BackfillPushTokenEnc` (`internal/store/reencrypt.go`):
idempotent and compare-and-set. What stage 1 inherits from it is exactly that — the idempotence and the
CAS — and NOT its cipher-nil tolerance (party round 3): the push function returns success on a keyless
instance, and a scenario backfill that did the same would report a green gate over untouched plaintext.
With no key and any scenario present, the backfill and readiness fail with a typed reason, per §10.
Stage 1 adds the backfill for the scenario as an ordinary
idempotent task with a metric and a log line — NOT gated on readiness. Revision 3 coupled it to
readiness and §10 withdraws that: a half-migrated column is a per-row fact, and a monitoring service
that reports itself unready over a migration takes away the visibility it exists to provide.

**D6 — stage 2: a secret is a NAMED BINDING, declared in the scenario and resolved from the inventory
(party round 1).** Not `secret_ref_1..n`: positional slots read badly in YAML and make reordering steps
change identity. The scenario declares bindings under a strict grammar; a step may reference only a
declared binding, and only from a permitted location. The reference is stored as a scoped
`setting_key` — `scenario.secret.<binding>` — so `monitor_secret_refs` keeps its tenant-safe foreign key
and delete counting and rotation fencing retain their ref-row behaviour. Rename does NOT come for free —
it requires D6a, and this sentence used to claim otherwise. A binding NAME is not a secret and stays visible:
that is what keeps the scenario readable.

**D6a — rename does NOT keep working, and the spec must say what to build (BLOCKER, party round 2).**
Revision 1 claimed a scoped `setting_key` leaves rename, delete and rotation untouched. Rename is
broken by it: `repointSecretRefs` (`internal/store/secrets.go`) reads each ref row's `setting_key`,
looks it up in the monitor's FLAT config, and fails the ENTIRE rename when the value is not the old
name — deliberately, because "a silent skip would leave a ref row pointing at a name the config never
carried". A scenario ref row has no such flat key, so one stage-2 monitor would break every inventory
rename in its project. Stage 2 therefore requires a scenario-aware repoint: inside the existing rename
transaction and its lock order, decrypt and parse the scenario, find the binding by NAME, replace its
ref name, re-encode and re-encrypt with the same compare-and-set discipline, and keep the
fail-on-divergence rule — a binding that does not carry the old name is the same broken invariant and
must fail the rename. Delete counting and the rotation fence can keep reading ref rows unchanged.

**D7 — literals are refused from every secret-capable surface, and secrets are forbidden in the URL.**
Not headers only (party round 1): a credential fits in a body and in an assert value just as well, and
restricting the rule to headers would ban nothing while sounding like a rule. The URL is stricter still —
no binding may resolve into a URL or query string, because that is the surface D2's leak came from and
the one an error message, a proxy log and an access log all copy.

**D8 — stage 2's gate is not "a longer expected-field list".** Synthetic is `CredentialForbidden`
today; the materializer resolves `password`/`password_ref` and injects envelope fields into the
top-level config, which `ParseScenario` never reads. Stage 2 adds a scenario-specific materialization
stage that runs only AFTER the envelope's structural gate, writes into ephemeral buffers and wipes
them. And the execution digest must bind the whole scenario structure TOGETHER WITH each binding's name
and location — otherwise a carrier attacker keeps a valid envelope while relocating a placeholder into a
request of their choosing, which is a worse attack than an altered field set and the one the current
typed digest cannot even represent.

**D8a — "location" is a canonical descriptor, or invariant 11 is untestable (party round 2).** The
reviewer offered two relocations that revision 1's wording would not catch: moving a token between
duplicate or case-aliased header keys, and between two template occurrences in one body. So the digest
covers a DESCRIPTOR derived from the parsed, strictly validated scenario, never raw JSON — map ordering
and whitespace make raw JSON noncanonical. The descriptor is: the ordered step index; the field kind;
for a header, the canonical lower-cased header name, with duplicate keys refused case-insensitively at
validation; for a body or assert value, a defined JSON/template pointer plus the occurrence index;
the binding name; and the complete canonical NON-SECRET structure of the scenario — methods, URLs,
literal template segments, conditions. Seal and open derive the same descriptor from the same
validator, so a moved placeholder, a rewritten target or an altered literal context fails the AEAD
before any request is made.

**D9 — Monitoring-as-Code follows from stage 2, and only from it.** The file provider refuses secrets by
KEY NAME and cannot see inside a JSON string, which is why `synthetic` is excluded today. After stage 2
a scenario contains no secret, so the exclusion loses its reason — but a bundle still needs the nested
schema the flat `settings map[string]string` cannot express. That is a separate piece of work and it is
named here so nobody reads stage 2 as delivering it.

**D10 — test-before-save gets ONE fail-closed contract (BLOCKER, party round 2).** Revision 1 listed
scenario secrets in that path as a non-goal while requiring literals to be refused on every write
surface. Both cannot hold: `POST /api/v1/projects/{id}/monitors/test` builds an unsaved monitor with
`Config: body.Config` — the literal scenario — and hands it to `RunTest`, which wraps it in an ordinary
generation-1 job (`internal/dispatch/amqp.go`), so the credential crosses the broker in cleartext. The
`CredentialedType` branch beside it does not apply, because synthetic is not a credentialed type.

The contract, chosen rather than left open: an unsaved synthetic test carrying DECLARED bindings runs
through the same authoritative materialization the saved path uses — project-scoped refs resolved
server-side, the envelope built, no plaintext in the payload — and a LITERAL in any secret-capable
location is refused with a typed response naming the location. Nothing about this path may accept a
literal "just to test it", and no broker or pull payload from it may contain one. Where the
materialization cannot run at all (no key, per §10), the response is the typed refusal, not a
best-effort test.

## 4. What must move WITH the implementation, not after it

- `docs/specs/func-oncall-synthetic-pull.md` — FR-SYN-1 and §217 promise what the code does not; they
  gain the correction and the pointer here, the way FR-022's invariant 14 was corrected in place.
- `docs/status.md` — the SYN requirements have NO rows today (zero matches for `SYN-`), which is how a
  false promise survived unnoticed; they get rows, and FR-028 / NFR-023 get theirs.
- `docs/traceability.md` — the discharge map per stage.
- `docs/runbook.md` — what an operator does with an existing synthetic monitor whose scenario holds a
  literal, and the rotation story.
- `docs/overview.md` — the config-key protection table, once there are three sets rather than one.

## 5. Schema

Stages 0 and 1: **no migration.** The scenario stays in `monitors.config`; only its value's form changes
(ciphertext), and `secret.Cipher` output is self-describing, which is what makes the backfill idempotent.

Stage 2: **no new table.** `monitor_secret_refs` already keys by `setting_key`, so a scoped key
(`scenario.secret.<binding>`) fits without DDL. Whether the per-binding envelope needs a shape change in
the dispatch payload is a stage-2 design question, not a schema one.

## 6. Acceptance invariants (FR-028)

1. No probe result — heartbeat message, log line or metric label — contains a step's URL, query string,
   header value or body, for any failure class.
2. A scenario is ciphertext in `monitors.config` for a new write, and for every row that existed before
   the change once the backfill has run.
3. The backfill is idempotent and compare-and-set: running it twice changes nothing, and a concurrent
   writer never loses an update to it.
4. Readiness never depends on the at-rest key or on the backfill: an instance with no key, and one
   mid-backfill, both report ready and keep probing every monitor they were probing. A scenario that
   cannot be protected fails its own WRITE; one that cannot be opened fails its own PROBE with a typed
   reason and no heartbeat, status or SLA mutation.
5. A principal that may not write the monitor receives no scenario — neither plaintext nor ciphertext —
   from any read path, and the response contract says so in one documented way.
6. A principal that may write the monitor receives the whole scenario and can edit it; an empty
   submitted scenario keeps the stored one, as `password` already does.
7. No display or snapshot read decrypts `password`, before or after this change.
8. Key rotation re-encrypts scenarios; after rotating and dropping the old key, every scenario is still
   readable and every synthetic monitor still executes.
9. A synthetic monitor still runs under envelope mode: the execution reader supplies the scenario to the
   job, and the probe behaves as it did.
10. Stage 2: a literal in any secret-capable location is refused on every write surface, naming the
    location; a binding may resolve only into a permitted location, and never into a URL or query.
11. Stage 2: the execution digest is over the canonical descriptor of D8a, and it changes when the
    scenario's structure changes, when a binding is renamed, and when a binding MOVES — including
    between duplicate or case-aliased header keys and between two occurrences in one body. A valid
    envelope with a relocated placeholder fails the gate before any request is made, and duplicate
    header keys are refused case-insensitively at validation rather than resolved by precedence.
12. Stage 2: a bundle or API write naming an unknown or foreign binding is refused, and deleting a
    referenced secret is refused by the same tenant-safe rule that protects `password_ref` today.
13. Stage 2: renaming a referenced inventory secret succeeds and rewrites the binding inside the
    scenario, in the rename's own transaction; a binding whose stored ref name does not match fails the
    rename, exactly as a flat `*_ref` mismatch does today (D6a).
14. Stage 2: an unsaved synthetic test with declared bindings runs with a server-resolved envelope and
    no plaintext in any payload; one carrying a literal is refused with a typed response naming the
    location (D10).

## 7. Required test matrix (written before the code)

*Stage 0:* every failure class — DNS, connect refused, TLS, timeout, non-matching assert, failed
extract, HTTP status — asserted to contain the step label and the class and NOT the URL, with a
distinctive secret planted in the URL, the query, a header and the body of the same scenario, searched
for across the heartbeat message and the captured log output.

*Stage 1, storage:* a raw column read shows ciphertext for a new write and for a legacy row after the
backfill · the backfill run twice changes nothing · a planted failure mid-backfill leaves readiness
false and no partially-updated row claiming success · rotation then dropping the old key leaves every
scenario readable · a keyless instance reports READY and keeps probing, its scenario write is refused
with a typed reason, and a scenario it cannot open produces a per-monitor probe error with no
heartbeat, status or SLA mutation — the three assertions §10 turns on.

*Stage 1, mode separation (party round 3):* the WRITER read of a monitor succeeds against a decrypting
cipher while the SAFE list, the SAFE detail and the scheduler snapshot for that same monitor run
against a cipher that PANICS on any decrypt at all. That pins the mode boundary itself rather than the
shape of a response body, which a redaction bug would satisfy just as well.

*Stage 1, boundaries:* a viewer's detail and list responses contain neither scenario plaintext nor
ciphertext · a writer's do contain the scenario · no display or snapshot path decrypts `password`,
proved by a cipher that fails the test if asked to decrypt at all · `MaterializeExecutionConfigs`
supplies the scenario, so a synthetic monitor executes under envelope mode · the AMQP payload, the pull
agent's response and the test-before-save path each asserted for what they carry.

*Stage 1, mutation responses (party round 2):* a create and a PATCH return the scenario to the writer
that made them, while a viewer's list and detail for the same monitor carry neither plaintext nor
ciphertext.

*Stage 0, redirects and proxies (party round 2):* a step whose redirect target fails, and a step behind
a failing proxy, both produce a message with no URL — the wrapped error from a redirect chain carries
the NEXT url, which is the case a naive fix misses. `http status` is asserted only if it is a distinct
classification; a non-2xx is an assert-shaped outcome today and the matrix says which it is rather than
assuming.

*Stage 2, rename (party round 2):* renaming a referenced secret rewrites the binding inside the
encrypted scenario and leaves the ref row consistent · a scenario whose binding does not carry the old
name fails the rename rather than being skipped · a rename concurrent with a monitor edit resolves by
the same compare-and-set rule as `password_ref` · rotation of the referenced secret needs no monitor
edit.

*Stage 2:* the exact binding set the gate expects, per scenario · an unknown binding, a foreign one, a
literal in a header, in a body, in an assert value and in a URL — each refused, each naming the
location · a tampered carrier that relocates a placeholder fails before any HTTP request · a broker and
pull payload searched for the secret value · a missing or malformed envelope fails closed · deleting a
referenced secret refused · rotation of a referenced secret picked up without editing the monitor.

## 8. Threat model

The reader is the threat this starts with: a project VIEWER is a legitimate, low-privilege role, and
today it reads bearer tokens out of the monitor list. Next is the operator with a database dump or a
backup, for whom stage 1 is the whole answer. Stage 2 addresses the executor and the transport: until
the credential leaves the scenario, it rides to a worker in an ordinary job payload while every typed
credential travels in an envelope under a per-region key. The attacker with a valid carrier is stage
2's specific case, and D8 is the answer: binding the structure and the placement, not just the field
names. What none of the stages claim: an editor who may write the monitor can read its credentials, and
a compromised worker sees what it must send.

## 9. Non-goals

Rotating the credentials this defect exposed (an operator action, and the runbook says so); a secret
manager beyond the existing project inventory; per-step credentials for anything other than the
scenario's own bindings; encrypting the non-secret parts of a scenario; and Monitoring-as-Code support
for `synthetic`, which stage 2 unblocks but does not deliver (D9).

## 10. Blast radius: what a missing key or a broken monitor may take down (owner, 2026-09-02)

Revisions 1 through 3 asked the owner to choose between four shapes for a keyless instance, and three
of the four were wrong for a reason none of us named until the owner did: **a service must not go down
because of an unset environment variable or one misconfigured monitor.** For a reliability platform
that is not a preference — an instance that stops reporting ready takes away visibility into
everything else at the moment it is needed most.

The product already holds this line, and the code says so out loud. When an executor cannot open a
job's credential (`internal/worker/worker.go`), the job gets a TYPED probe error for that monitor and
nothing else happens; readiness flips only when `credentialTracker` sees the failure PERSIST, with the
comment "a single corrupt or retired-key payload is a per-job diagnostic, and readiness routes work to
the whole worker". `Materialized.UsedCredential` exists so that "an ordinary job never flips credential
health either way". So the answer is not a new policy, it is the policy that is already there:

1. **Readiness is never coupled to the at-rest key or to a migration.** The gate of revision 3's option
   4 is withdrawn. A missing `security.encryption_key` does not make an instance unready, and neither
   does a half-finished backfill.
2. **A scenario that cannot be protected is refused at WRITE time.** With no key configured, saving a
   scenario is refused with a typed reason naming the missing configuration. That is the smallest
   radius available: one monitor's edit fails, nothing that already runs is touched, and the product
   never accepts a secret it cannot protect. Existing monitors keep probing.
3. **A scenario that cannot be OPENED at run time is a per-monitor typed diagnostic**, exactly like a
   credential that cannot be materialized: a probe error for that monitor, no heartbeat, status or SLA
   mutation, and readiness only on a persistent, systemic pattern.
4. **The backfill is an ordinary idempotent task with a metric and a log line**, not a readiness gate
   (this supersedes the coupling D5 carried in revision 3). A row that is already ciphertext is
   protected; a row that is not keeps working exactly as it does today and is migrated on a later pass.
   Partial progress is a per-row fact and must not be reported as a service-level failure.

What this costs, stated rather than hidden: on an instance with no key, legacy plaintext scenarios stay
plaintext until an operator supplies one. The spec does not pretend otherwise — that is the honest
version of revision 3's "conditional" wording, and it is preferable to trading it for an outage.
