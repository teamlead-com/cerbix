# Changelog

All notable changes to **cerbix** will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

---

## [v0.1.9] - 2026-09-03

Four things in one release, in the order the owner set: a **typed external canary** for async API
journeys, an **audit trail for writes that change something** — incidents and now monitors — the
**dependency sweep** that had been waiting behind both, and one small improvement that came last, an
**optional description on a monitor**. The canary is the largest single feature since FR-021 and it
ships **complete**: all six phases, including the capability announcement and the typed UI form that an
earlier draft of these notes listed as missing. No breaking change. Five migrations, all additive.

### ⚠️ Upgrade Notes

- **The order of a canary rollout no longer matters.** A canary reaches only an executor that
  ANNOUNCED it can run one, so declaring a canary before a region is upgraded is safe: the run is
  refused with one bounded DOWN per attempt — `no_capable_runner` when nothing there announced a canary
  runner, `capability_mismatch` when what is there speaks another version — and it starts working by
  itself when the executors arrive. An earlier draft of these notes told you to upgrade the region
  first; that instruction is retired, not merely relaxed.
- **An AMQP `worker` older than this release never receives a canary at all.** Canaries ride their own
  queue (`checks.canary.<kind>@<version>.<region>`), which only a current binary binds, and on the
  HTTP-pull transport a job carries the capability it requires while a claim carries what the agent
  declared. That is the point: in a mixed fleet the old executor cannot take a job it would fail.
- **A repeated incident acknowledgement is now a genuine no-op.** It used to rewrite the incident's
  `updated_at` on every retry. The acknowledger and the instant stay the FIRST ones, and the
  modification time no longer moves. If anything of yours sorts incidents by `updated_at` and relied on
  a re-acknowledgement bumping them, it will not any more.
- **Two identical Alertmanager deliveries that arrive at the same moment now both answer HTTP 200** —
  one reporting `opened: 1` (or `resolved: 1`) and the other `ignored: 1`. The loser of that race used
  to answer 500, while the sequential retry a millisecond later answered 200. If you alert on 5xx from
  the receiver, that source of noise is gone.
- **The `golang` build image moves to 1.27.0** and `docker/setup-buildx-action` to 4.3.0. Neither
  changes the language version in `go.mod`.

### ✨ Added

**A typed external canary for async API journeys (FR-029 / NFR-024)**

A new monitor type, `async_canary`, runs ONE asynchronous transaction end to end and reports it as an
ordinary monitor: submit, take a correlation id, await a terminal outcome, assert the declared fields,
validate the cleanup boundary — and return ONE heartbeat carrying up/down, total latency, the failed
stage and a bounded code class. It is declared in a Monitoring-as-Code bundle as a nested, typed
`workflow:` block with closed unions and no free-form field anywhere: no `settings` map, no JSON string,
and a key the schema does not name refuses the whole bundle by name.

- **The contract is a type, not a convention.** Two submit kinds (`http_json`, `multipart_fixture`), two
  completion kinds (`sse`, `poll_json`), two correlation sources, three assertion kinds, two restricted
  grammars, and every bound with a number. `{{ correlation_id }}` is legal in exactly one field — the
  completion URL — because nothing has produced an id at submit time.
- **Credentials are bindings, never values.** A binding is declared once under `workflow.secrets` and
  referenced by name at each position; the value is resolved at dispatch, delivered in the existing
  credential envelope, held in memory for one execution and wiped. A credential-bearing header accepts a
  binding and nothing else. **What is not protected, stated plainly:** a credential pasted into an
  ordinary header or under an innocuous body key is not detectable and is not refused — the same
  residual FR-028 named.
- **The URL policy is strict and has no off switch.** HTTPS only; loopback, link-local, private ranges
  and cloud metadata are refused AFTER DNS resolution and re-validated on every redirect hop. There is
  no setting that relaxes it, in v1 or as a hidden flag, because a flag reachable in production is the
  policy's own bypass — tests reach a local fixture through an injected dialer at the seam, never
  through a product option. The executor also drops EVERY binding-backed header on a cross-host
  redirect, which `net/http` does not do: it strips `Authorization` and has never heard of `x-api-key`.
- **One run per monitor, four per region**, decided by the scheduler at dispatch. A refused run writes
  one ordinary DOWN with a bounded reason (`region_saturated`, `already_in_flight`, `no_capable_runner`,
  `capability_mismatch`) and the monitor's own
  `failure_threshold` decides the flip, so the sample counts as unavailable in the SLI like any other
  DOWN. Every submit carries an `Idempotency-Key` derived from the monitor, its execution revision and
  the scheduled window — whether a second submit with that key creates a second task is the target's
  contract, not cerbix's.
- **Nothing it touches leaks.** No heartbeat, log line, error or metric label carries a URL, a body, a
  header value, a secret, the correlation id or the result object path.
- **New metrics** `cerbix_canary_stage_total` and `cerbix_canary_dispatch_refused_total`, both
  low-cardinality and carrying no monitor id, URL or correlation id.

**An executor receives a canary only if it announced it can run one** (the last of the six phases).
The announcement is a set of `<kind>@<version>` tokens, and an executor makes it from the runner it
actually has: a pull agent in its heartbeat and again on every claim, an AMQP worker by consuming
`checks.canary.[v3.]<token>.<region>`, and `--role=all` by construction. The scheduler dispatches only
into a region that announced the token the DOCUMENT needs, and a region that announced nothing gets
`no_capable_runner` while a version skew gets `capability_mismatch` — two reasons because the fix
differs: start a runner, or finish the upgrade. The filter is not treated as the whole barrier, for the
reason the credential envelopes already established: a capability CHECK does not stop a consumer from
consuming, so an incapable executor is made unable to RECEIVE the job — a queue it does not bind on
AMQP, and a per-claim capability filter on the pull transport.

**A canary is declared in the SPA, in a bundle, or through the API.** The typed form is typed only:
no JSON editor on the create view or the read view, the five stages laid out as stages, bindings as
stage 0, and every refusal met AT THE FIELD rather than as a 400 after Create. A saved canary reads
back into the same form, with the binding halves recombined from the document's marker and the flat
reference key's name.

**A monitor says what it is for.** A monitor carries an optional `description` — plain text, at most
200 characters counted as Unicode code points, so a Cyrillic sentence is as long as a Latin one to the
person writing it. It appears under the name on the monitor list (one line, the whole text as its
tooltip), as the beginning of a line on a dashboard panel, and in full on the monitor's own page; it is
editable on the create and edit forms with a live count, and declarable in a Monitoring-as-Code bundle.
Every monitor that exists reads back with an empty description and every surface renders exactly as it
did — the absence of an element is asserted per surface, not assumed. It is deliberately absent from
public status pages, notification payloads and search.

### 🔒 Security

**An incident write says who wrote it (FR-026 / NFR-021)**

FR-022 promised, as its invariant 14, that "every write is audited with actor and tenant, in the
mutating transaction". That was false of the PRODUCT rather than merely unimplemented: an incident
write left **no** `audit_logs` row, for a monitor incident or a service one. A member could resolve
someone else's incident, publish a postmortem or acknowledge a page, and the organization's audit trail
said nothing.

- **Every incident mutation made by a PRINCIPAL now writes exactly one audit row in the same
  transaction** — the manual create, the Alertmanager receiver's create and its resolve, a status
  change, a note, the first acknowledgement, and a postmortem create or update. A committed change
  without its row cannot exist, and a rolled-back one leaves none.
- **The vocabulary is five words** — `incident.create`, `incident.status`, `incident.note`,
  `incident.acknowledge`, `incident.postmortem` — and the target carries ids and both ends of a
  transition and **never a body**: no note text, postmortem text or alert annotation reaches the trail.
- **Machine writes are excluded by decision and enumerated**: the reconciler's auto-open and
  auto-resolve, the service auto-incident and its resolve, the `⚡ Context:` note, both `⏸ Suppressed:`
  writers, `🚀 Changes:` and `🕸 Impact:`. Their record is the incident's own timeline, which names
  `system` as the author. Auditing them would bury a tenant's log under a flapping service's heartbeat.
- **No read is added.** The rows appear in the organization's existing audit listing and never in the
  instance one. `docs/runbook.md` now answers "who resolved this incident" directly.
- Two AST guards hold the door surface, each driven by a fixture that contains the violation. The first
  version of one guard exempted the Alertmanager receiver by name — and the exemption hid the exact
  defect the requirement exists to prevent, because Alertmanager posts with a project-write token and a
  token is a principal.

**And a monitor write says who wrote it, by the same rules**

Creating, editing or deleting a monitor through the API or the SPA left no audit row either. It now
writes exactly one, in the mutating transaction, with three words — `monitor.create`,
`monitor.update`, `monitor.delete`.

- **The target names the document and never its contents**: the monitor's id, slug, type and region;
  `enabled true→false` when the write was a pause or a resume, because that is the edit an operator
  asks about; and, for an async canary whose workflow declares `cleanup.kind: none` with
  `acknowledged: true`, the clause `cleanup=none acknowledged` — which is what makes that
  acknowledgement visible in the trail, as its requirement promised. No config value, credential
  reference, target URL, scenario or workflow body appears in a target, for any type.
- **What the row says is read from the row the writer HOLDS**, not from what the caller passed: the
  `FOR UPDATE` statement for a delete, the returned row for an update. Independent review found the
  first version taking a whole monitor from the caller — a concurrent edit could make the audited region
  stale, and a careless caller could file the row under another project's organization. The delete door
  takes an id now, so there is nowhere to put a foreign project.
- **A monitor applied from a Monitoring-as-Code bundle writes no row**: it is a machine write, and its
  record is the bundle. Deleting a project leaves the organization's trail intact.

### 🔧 Changed

- **A pull monitor with a timeout past 30 seconds is no longer re-claimable mid-probe.** The agent's
  claim lease was hardcoded at 30 s, so any pull monitor slower than that could be handed to a second
  agent while the first was still working. The lease is now per job. This is older than the canary; the
  canary is what made it visible.
- `async_canary` joins the `monitors_type_check` database constraint. Every Go test passed while the
  real table refused the type, because no store test writes to it — a live E2E found it, and the type
  vocabulary is now asserted against the database itself so the next new type cannot repeat it.
- **A red CI job now names the tests that failed**, as annotations the check-runs API serves without
  repository-admin rights. Two failures that had been unreadable from outside turned out to be tests
  measuring the MACHINE rather than the product: one read a legitimate deferral of a partition drop —
  correct behaviour while any older transaction is open — as a crash that never happened, and one gave a
  repair slice two seconds and failed when the closing write missed it, where production keeps the range
  under a sixty-second lease and resumes. Both tests are corrected; no product code changed.

### 📦 Dependencies

Seven of the eight open dependabot branches, verified one group and one major at a time (iter-0170).

- go-modules: `rabbitmq/amqp091-go` 1.13.0 → 1.14.0, `google.golang.org/grpc` 1.83.0 → 1.83.1
- github-actions: `docker/setup-buildx-action` 4.2.0 → 4.3.0 (SHA-pinned)
- docker: the `golang` build image 1.26.6-bookworm → **1.27.0-bookworm**
- frontend: `@types/node` → 26.4.1, `eslint` → 10.9.1, `vue-tsc` → 3.3.11, and three MAJORS —
  `jsdom` 26 → **30**, `vite` 6.4 → **8.2**, `vue-router` 4.6 → **5.3**. The last two are one decision,
  not two: vue-router 5 declares `peerOptional vite@"^7.3.0 || ^8.0.0"` and will not install on Vite 6.
- **Not taken: TypeScript 7.** `vue-tsc` 3.3.11 cannot drive the native compiler port —
  `npm run type-check` dies with `ERR_PACKAGE_PATH_NOT_EXPORTED` before checking a file. Upstream of
  this repository; the bump waits for a `vue-tsc` that supports it.

The committed SPA snapshot (`internal/web/dist`) is regenerated with this: every asset filename hash
moved, because Vite 8 hashes differently.

### API · Metrics · Schema

- **API:** `Monitor.description` on read, create and update (`maxLength: 200`, counted as code points;
  omitted on update leaves it unchanged, `""` clears it, and a longer value is 400 naming the field).
  The monitor `config` description documents the `async_canary` `workflow` document and its
  `canary_secret_<binding>_ref` keys; the server canonicalizes the document on write, so an
  API-created canary and a bundle-declared one store byte-identical documents. The agent protocol
  gains one capability declaration in both directions: `workflow_kinds` in the heartbeat and
  `X-Cerbix-Workflow-Kinds` on every job claim, bounded and grammar-checked before it reaches a
  column.
- **Metrics:** `cerbix_canary_stage_total` and `cerbix_canary_dispatch_refused_total`, both
  low-cardinality and carrying no monitor id, URL or correlation id. The refusal metric's reasons are
  the four bounded ones a heartbeat can carry.
- **Schema:** five additive migrations — `00095` the canary in-flight lease, `00096` the per-job pull
  claim lease, `00097` `async_canary` in `monitors_type_check`, `00098` `pull_jobs.workflow_kind` (NULL
  for every job any agent may run), `00099` `monitors.description` (`NOT NULL DEFAULT ''`, which is the
  whole compatibility promise in one default). No index added: nothing reads by a description, and
  search over it was declined.

<sub>Decisions D-0218 … D-0234 · FR-029, NFR-024, FR-026 (+ its §10 monitor amendment), NFR-021 and
FR-030 DONE · iterations iter-0168 … iter-0173, all CLOSED, every range approved by the independent
reviewer · full `-race` suite green (33 packages), vitest 46 files / 514 tests, browser suite 66 passed
/ 1 skipped, geo topology suite 12 passed · hosted CI green on the pushed head, all seven jobs,
including `Backend (timescaledb hypertables)` — the job whose two failures D-0225 could not read until
the annotations step made a red run legible</sub>

---

## [v0.1.8] - 2026-09-03

A credential inside a synthetic monitor's scenario used to be stored in cleartext, returned to every
principal who could READ the monitor, skipped by key rotation, and echoed into the heartbeat message
with the whole request URL whenever a step's transport failed. This release closes all four, in three
stages that were built and reviewed separately, and gives the editor a way to declare a credential
without ever typing one. It BREAKS a synthetic monitor that keeps a token in an authorization-style
header — read the upgrade note. No migration.

This was a **spec-versus-code defect, not a new feature**: `func-oncall-synthetic-pull.md` FR-SYN-1
already promised "encryption like the other types" and its §217 promised inclusion in `reencrypt`, and
D-0090 promised the failure message "never echoes bodies/headers". None of the three was true of the
code, and the SYN requirements had no rows in `docs/status.md` at all, which is how a false promise
survived unnoticed. They have rows now.

### ⚠️ Upgrade Notes

- **A synthetic monitor whose credential-bearing header holds a LITERAL now fails validation on its
  next write.** The affected headers are the finite credential-bearing set — `authorization`,
  `proxy-authorization`, `cookie`, `x-api-key`, `api-key`, `x-auth-token`, `auth-token`,
  `x-access-token`, `access-token`, `private-token` — whose value must now be exactly one
  `{{secret:<binding>}}` placeholder. **Existing monitors keep probing**: nothing re-validates on
  read, so the refusal appears the next time such a monitor is EDITED, and it names the step and the
  header without echoing the value. Move the token into the project secret inventory and reference it
  as a binding ([b3c99b6](https://github.com/teamlead-com/cerbix/commit/b3c99b6))
- **No migration, and no readiness coupling.** The scenario stays in `monitors.config`; only its form
  changes, to ciphertext. An instance with no `security.encryption_key` starts, reports ready and keeps
  probing every monitor it was probing: what it refuses is a scenario WRITE it cannot protect. Legacy
  plaintext scenarios stay plaintext until a key is supplied — that cost is stated rather than traded
  for an outage ([cc90e4a](https://github.com/teamlead-com/cerbix/commit/cc90e4a))
- **Probe failure messages changed shape for every monitor type, not only synthetic.** A transport
  failure now reads as a bounded class plus the target's host (`dns: no such host (api.internal)`)
  instead of Go's error text. If you have alert rules or dashboards matching on `heartbeat.msg`
  substrings, check them. Nothing in the product parses that field to make a decision ([e6a3db8](https://github.com/teamlead-com/cerbix/commit/e6a3db8))

### 🔒 Security

**A credential in a scenario is a secret at rest, on read, and in the record (FR-028 / NFR-023)**

- **Stage 0 — no probe result carries a request URL, for any type.** `net/http` embeds the request URL
  in every error it returns, so `Msg: err.Error()` published whatever the target's query string
  carried — reproduced on an ordinary `http` monitor and on `promql`, not only on synthetic. Every
  failure is now composed from a bounded class plus a host, asserted per type through the real prober
  registry with a secret planted in the URL, the query, a header and the body ([e6a3db8](https://github.com/teamlead-com/cerbix/commit/e6a3db8))
- **Stage 1 — the scenario is ciphertext at rest, and withheld from anyone who cannot write the
  monitor.** One secret set became three classifications by MEANING (encrypted-at-rest,
  write-only-on-read, writer-only-display) and the store reader became an explicit MODE named by what
  it decrypts. A viewer receives no scenario at all — not plaintext, not ciphertext — through a store
  call chosen after the authorization decision rather than by redacting a decrypted document. Key
  rotation covers scenarios, and an idempotent, compare-and-set, NON-fatal startup backfill converts
  existing rows ([cc90e4a](https://github.com/teamlead-com/cerbix/commit/cc90e4a))
- **Stage 2 — a declared credential is a NAMED BINDING resolved from the project inventory.** The
  document carries `{{secret:<binding>}}` and the secret's NAME lives in a flat config key,
  `scenario_secret_<binding>_ref`, so rename, delete-counting and rotation run on the path
  `password_ref` already runs on. The value is delivered in the credential envelope, substituted into
  the scenario by the executor, and the substituted copy is dropped when the probe ends ([b3c99b6](https://github.com/teamlead-com/cerbix/commit/b3c99b6))
- **A moved placeholder cannot survive a valid envelope.** The scenario and its reference keys became
  EXECUTION BINDING KEYS, so `EnvelopeV2`'s body digest covers the stored document: an attacker with a
  valid envelope who rewrites a step's URL produces a different document and the AEAD fails before any
  request. A binding therefore REQUIRES a body-bound envelope, and a region on an older carrier gets a
  per-monitor `carrier_too_old` rather than a job that looks protected and is not ([b3c99b6](https://github.com/teamlead-com/cerbix/commit/b3c99b6))
- **The binding belongs to a synthetic monitor and to nothing else**, decided once at the write
  boundary and gated in the store, in both derived key sets, and at the dispatch gate — which refuses
  rather than ignores such a job on any other type, permanently, on carrier integrity ([084b49d](https://github.com/teamlead-com/cerbix/commit/084b49d))
  ([4fdedff](https://github.com/teamlead-com/cerbix/commit/4fdedff))

**What this does NOT claim, stated because a security note that overstates is worse than none.** A
literal secret is not detectable by shape, so the enforceable rule is the header-NAME one. A credential
pasted into a header nobody would call a credential header, or into a body, is still legal: it is
encrypted at rest, withheld from viewers and kept out of probe results, and it travels to the prober
inside the ordinary job rather than in an envelope. Buying the stronger property needs a restrictive
typed request model for the scenario, which is an owner's decision and separate work — and a heuristic
over VALUES is refused rather than deferred ([900aa1b](https://github.com/teamlead-com/cerbix/commit/900aa1b))

### New Features

**Declaring a binding in the UI**

- **The synthetic monitor form has a Scenario secrets panel** above the steps: pick a project secret,
  name the binding, and the row then shows which secret fills it, where it is used, and the flat key it
  is stored as. A binding name is never displayed without the secret it resolves to ([3a67106](https://github.com/teamlead-com/cerbix/commit/3a67106))
- **A credential-bearing header stops being a free-text field.** Its value control is a binding
  selector from the first keystroke of the header name — empty and disabled with "Add a binding first"
  until one exists — so the rule is met before a token is pasted rather than after a failed save
  ([95ee945](https://github.com/teamlead-com/cerbix/commit/95ee945))
- **"Save before test" is stated at the button.** A scenario carrying bindings is deliberately not
  testable before it is saved: that path builds an unsaved monitor with no envelope, so a placeholder
  would travel to the target as literal text. The Test control is disabled with the reason and the way
  forward; a credential-free scenario still tests unchanged ([3a67106](https://github.com/teamlead-com/cerbix/commit/3a67106))
- **What is NOT protected is shown too**: a pasted-looking value in an ordinary header gets a hint
  offering the inventory — never a refusal, because cerbix cannot tell a credential from data there
  ([3a67106](https://github.com/teamlead-com/cerbix/commit/3a67106))

### Bug Fixes

- **A synthetic monitor could not be created from the SPA at all.** `canSubmit` required a target for
  every active type while the form deliberately hides that field for `synthetic` — whose
  `NeedsTarget()` is false — so **Create monitor was permanently disabled and unexplained**. Found by a
  new component test, and it survived because no unit test and no browser test had ever submitted this
  type ([3a67106](https://github.com/teamlead-com/cerbix/commit/3a67106))
- **The form's default scenario opened INVALID.** The scaffold shipped
  `Authorization: Bearer {{token}}`, which the new rule refuses, so merely choosing Synthetic
  produced a form whose own example could not be saved. It now demonstrates `extract` → interpolate
  with an id used in the next step's path, which is what `extract` is for ([95ee945](https://github.com/teamlead-com/cerbix/commit/95ee945))
- **A malformed binding reference was silently ignored.** `scenario_secret_Login_ref` — capitalised, so
  outside the grammar — meant the operator declared a binding, saw no error, and shipped a scenario the
  credential was never wired into. It is refused by name now ([084b49d](https://github.com/teamlead-com/cerbix/commit/084b49d))

### Improvements

**Documentation that stopped lying**

- **FR-SYN-1, NFR-SYN-2 and AC-SYN-2 corrected in place**, and the five SYN requirements given the
  `docs/status.md` rows they never had — including two gaps stated rather than implied: no test names
  the whole-scenario deadline, and no browser test puts a synthetic monitor on a GEO worker
  ([4e6def8](https://github.com/teamlead-com/cerbix/commit/4e6def8))
- **The binding key is documented in `openapi.yaml`** on all three monitor config schemas. The feature
  was API-reachable and undocumented, which makes it unusable rather than merely un-designed
  ([084b49d](https://github.com/teamlead-com/cerbix/commit/084b49d))
- **The runbook gained an FR-028 section** covering what is protected at rest, on read and in the
  record; the detection query and one-edit repair for a stale reference; and — as plainly — what is not
  protected ([258adb5](https://github.com/teamlead-com/cerbix/commit/258adb5))

**Repository hygiene**

- **The SPA build runs as the developer, not as root.** Every docker build wrote
  `frontend/node_modules` and its caches as root, so any tool later run by a developer failed with
  EACCES on a tree it owned nothing in — it stopped an independent reviewer from running the frontend
  tests at all. `make spa-snapshot` now passes `--user` and an in-container npm cache ([c321931](https://github.com/teamlead-com/cerbix/commit/c321931))
- **The first browser coverage a synthetic monitor has ever had** (`e2e/tests/synthetic-bindings.spec.ts`):
  declare a binding through the UI, meet the refusal, see the test blocked, save, and find the flat key
  and the placeholder on the wire with no value anywhere ([3a67106](https://github.com/teamlead-com/cerbix/commit/3a67106))

### API · Metrics · Schema

- **API:** the monitor `config` description documents `scenario_secret_<binding>_ref` and its rules on
  create, update and test; a `scenario_secret_*` key on any non-synthetic type is refused with 400
  naming the key; test-before-save answers 400 naming the binding for a scenario that declares one.
- **Metrics:** unchanged. A binding that cannot be materialized surfaces as the existing per-monitor
  reason (`carrier_too_old`, `missing_reference`, `decrypt_failed`), never as a readiness flip.
- **Schema:** no migration and no new column. `monitor_secret_refs` carries a binding through the same
  `setting_key` shape as `password_ref`. The scenario's VALUE in `monitors.config` becomes
  self-describing ciphertext, converted in place by the startup backfill.

<sub>15 commits · decisions D-0216 (+ addendum) and D-0217 · FR-028 and NFR-023 DONE, FR-SYN-1/2/3 and
NFR-SYN-1/2 given their first rows · iterations iter-0166 and iter-0167, both CLOSED and both approved
by the independent reviewer after five and four rounds · full `-race` suite green (33 packages, with
`internal/store` re-run idle to separate a pre-existing load-dependent FR-024 flake), vitest 38 files /
418 tests, browser suite 60 passed / 1 skipped</sub>

---

## [v0.1.7] - 2026-09-02

PromQL grew up: it is expressible in a Monitoring-as-Code bundle and can authenticate to a Prometheus
behind basic auth. One security fix rides with it, and it BREAKS a configuration that works today —
read the upgrade note first. No migration.

### ⚠️ Upgrade Notes

- **A monitor target may no longer carry credentials in its URL userinfo.**
  `https://user:pass@prom.internal:9090` is refused on every surface now, not only in bundles. It used
  to work — Go's `net/http` turns such a URL into an `Authorization: Basic` header on its own — and the
  password was stored as plaintext in `monitors.target`, which the API's redaction does not blank, so
  every VIEWER of the project could read it in the monitor list. **Stored monitors keep probing**:
  nothing re-validates on read, so the refusal appears the next time such a monitor is EDITED. Move the
  credential to the type's own settings (`promql` now has them) or to a target without userinfo before
  editing ([f827d96](https://github.com/teamlead-com/cerbix/commit/f827d96))
- **No migration, no configuration change.** A `promql` monitor with no `auth_mode` behaves exactly as
  before, at validation and at the dispatch gate — the default is resolved, never written, so no
  canonical hash moves and no monitor is rescheduled ([754e5dc](https://github.com/teamlead-com/cerbix/commit/754e5dc))
- **Worth knowing if you deploy the release BINARIES:** the `v0.1.6` assets were compiled with Go
  1.25.12, which carries seven known standard-library vulnerabilities this release closes by moving the
  pin. The container image was never affected — it is built on `golang:1.26.6` — so an installation
  running the image was not exposed by this ([be75d51](https://github.com/teamlead-com/cerbix/commit/be75d51))

### New Features

**PromQL: bundles and basic auth**

- **A `promql` monitor can live in a Monitoring-as-Code bundle.** It carries `query` — required,
  bounded at 1024 characters, never blank. The type was excluded because a 2026-08 classification
  grouped it with the credentialed types; the prober had in fact never had a credential slot, so what
  stood between it and a bundle was one typed field ([39245fe](https://github.com/teamlead-com/cerbix/commit/39245fe))
- **Optional HTTP basic auth against Prometheus**, through `auth_mode: none | basic`. `basic` requires
  a `username` and a credential — a project-secret reference in a bundle, value or reference in the
  UI/API — and the prober sends the header only when a username is configured, so an unauthenticated
  Prometheus is never handed an empty one. Bearer tokens and mTLS are deliberately NOT supported: basic
  is what Prometheus implements natively, and for anything else the answer remains a regional agent
  plus an unauthenticated path for the prober ([754e5dc](https://github.com/teamlead-com/cerbix/commit/754e5dc))
- **In the SPA:** the PromQL section gained an Authentication selector, a username field and the same
  credential control the database types use — value or project secret, with the dangling-secret warning
  ([05db534](https://github.com/teamlead-com/cerbix/commit/05db534))

### Security

- **The Go pin carries the standard-library fixes `govulncheck` asks for.** Under the version the
  workflows install from `go.mod` (1.25.12) it reported SEVEN called vulnerabilities — `crypto/tls` ×2,
  `net/http` ×2, `encoding/xml`, `encoding/asn1`, `html/template` — reached through ordinary product
  paths: the HTTP prober's `Client.Do`, the mailer's TLS handshake, the operational server's
  `ListenAndServe`. All seven are fixed in 1.25.13, and one line moves the scan AND the release
  binaries, because all three workflows read the version from `go.mod` ([be75d51](https://github.com/teamlead-com/cerbix/commit/be75d51))
- **The full-history secret scan is clean, without weakening it.** Its seven findings were read one by
  one and are fabricated test fixtures — two sequential-hex AES keys, the same two base64-encoded, and
  an `actor_label` in a committed CLI transcript, which is the server's derived name for a bearer and
  never the token. Nothing needed rotating. The allowlist entries are anchored to those exact literals,
  so any other high-entropy string in the same files still fails the scan ([7a9350d](https://github.com/teamlead-com/cerbix/commit/7a9350d))

### Improvements

**Correctness**

- **The file provider no longer conflates "has settings" with "is credentialed".** One entry point
  routes a type to the credential registry, to its own schema, or to an error naming the key — which is
  what the specification has said in words since the beginning ([39245fe](https://github.com/teamlead-com/cerbix/commit/39245fe))
- **A blank-after-trim `query` is refused.** A presence check accepts `"   "`, and a whitespace-only
  expression makes the prober report "no query configured" on every run: a monitor that is configured,
  scheduled and permanently meaningless ([754e5dc](https://github.com/teamlead-com/cerbix/commit/754e5dc))
- **A rejection reason is decided by a typed error, not by matching message text**, so a reworded
  validator can no longer silently change which reason a bundle is refused with ([754e5dc](https://github.com/teamlead-com/cerbix/commit/754e5dc))

**Repository hygiene**

- **The documented test command is the one that finishes.** CI has passed `-timeout 40m` since
  iter-0163 because `internal/store` needs about eleven minutes under `-race`; `make race`, `CLAUDE.md`
  and `AGENTS.md` did not, so a developer running the documented command met
  `panic: test timed out` naming whichever test was running — a name that sends the reader after the
  wrong bug ([9608fbf](https://github.com/teamlead-com/cerbix/commit/9608fbf))
- **Dependabot got quieter without hiding anything**: monthly instead of weekly, all MAJOR bumps in one
  PR per ecosystem instead of one each, lower open-PR limits, and a cooldown so a version released
  yesterday is not proposed today ([18e3564](https://github.com/teamlead-com/cerbix/commit/18e3564))
- **A roadmap exists** (`docs/roadmap.md`) and the PRD points at it; the red Security workflow is its
  first item ([6e89c84](https://github.com/teamlead-com/cerbix/commit/6e89c84), [1efd5dc](https://github.com/teamlead-com/cerbix/commit/1efd5dc))
- **A flake left the suite by being understood rather than retried.** A scheduler test waited on the
  gate pass's sink events and then asserted on the leader gauge, which the leadership loop publishes on
  an unordered path — so under load it read the state before the step-down landed. The assertion now
  waits for the signal it asserts on, with the same requirement and its own deadline ([aa3cbfb](https://github.com/teamlead-com/cerbix/commit/aa3cbfb))

### API · Metrics · Schema

- **API:** the monitor `config` description now names promql's settings (`query`, and with
  `auth_mode: "basic"` a username plus `password` | `password_ref`); a monitor target with URL userinfo
  is refused with 400 on create and update.
- **Metrics:** unchanged.
- **Schema:** no migration. `promql` joins the credential registry, so `monitor_secret_refs` covers its
  `password_ref` through the same normalization every credentialed type uses.

<sub>14 commits · decisions D-0145 (addendum) and D-0215 · no requirement row: the type boundary is
FR-017's, and extending it is an addendum to its decision rather than a new requirement · full `-race`
suite green under the new pin, browser suite 59 passed / 1 skipped, `govulncheck` and `gitleaks` both
clean</sub>

---

## [v0.1.6] - 2026-09-01

Two requirements that turn reliability facts into something a pipeline can act on: a **release gate**
that answers whether the error budget allows a deploy, and **change intelligence** that records the
deploy went and lets the service's own facts say what followed. Eighty-five commits since `v0.1.5`.

### ⚠️ Upgrade Notes

- **Two additive migrations, no data repair, nothing to stop.** `00093` creates the gate's policy,
  override and decision tables plus the partition registry; `00094` creates the change tables and adds
  `api_tokens.actions`. Run `cerbix migrate` with the new binary and start as usual — every role applies
  migrations on startup anyway. On a database where no service has a gate policy, nothing evaluates and
  nothing is written ([16eecaf](https://github.com/teamlead-com/cerbix/commit/16eecaf), [c6c74dd](https://github.com/teamlead-com/cerbix/commit/c6c74dd))
- **`00094` builds a UNIQUE index on `incidents`.** The constraint `(id, project_id)` is what lets an
  incident↔change link be tenant-safe by the schema rather than by a query. It is built
  non-concurrently and takes a brief exclusive lock on `incidents`; the guard skips the build entirely
  if an equivalent constraint already exists ([c6c74dd](https://github.com/teamlead-com/cerbix/commit/c6c74dd))
- **The scheduler leader gains one background pass.** Gate-ledger partition maintenance runs on its own
  fenced advisory session — daily partitions created a week ahead, dropped past
  `gate.decision_retention_days` (default 90). Watch `cerbix_gate_decisions_writable_horizon_seconds`
  and `cerbix_gate_decisions_partitions_pending_drop`; a healthy pass keeps the first well above zero
  ([a6a9915](https://github.com/teamlead-com/cerbix/commit/a6a9915))
- **Existing API tokens are unchanged.** The new `actions` allow-list is optional: omitted or `null`
  means the token's role decides, exactly as before. A token is only ever NARROWED by it — the list is
  intersected with the role, never added to it ([7260e68](https://github.com/teamlead-com/cerbix/commit/7260e68))
- **`gate.*` and `change.*` ship with defaults**, so no YAML edit is required. The ones worth knowing:
  gate decisions retained 90 days and purged hourly; change groups retained by whole identity; the
  record endpoint bounded at 300 requests/minute per process and 30 per principal ([16eecaf](https://github.com/teamlead-com/cerbix/commit/16eecaf),
  [c6c74dd](https://github.com/teamlead-com/cerbix/commit/c6c74dd))

### New Features

**Reliability Gate — a deploy asks whether the error budget allows it (FR-024)**

- **One call, one machine-readable answer.** `cerbix gate check --project <id> --service <id>` (or
  `POST /api/v1/projects/{p}/services/{s}/gate`) returns an observed `state` — `ALLOW`, `WARN`,
  `BLOCK`, `UNKNOWN`, `NOT_CONFIGURED` — an effective `action`, every matching reason and the evidence
  under it. The CLI's exit code follows the action (`0` allow/warn, `2` block, `4` not configured,
  `1` transport/auth), so a CI step is one command; credentials come from the environment only
  ([d926568](https://github.com/teamlead-com/cerbix/commit/d926568), [229b33e](https://github.com/teamlead-com/cerbix/commit/229b33e))
- **What blocks is declared, not guessed.** A per-service policy names ONE SLO window and assigns each
  clause of a closed vocabulary — budget exhausted, budget consumed over a threshold, page-burn firing,
  ticket-burn firing, an open service incident — to `block`, `warn` or `ignore`, with a mandatory
  `unknown_behavior` and a seal-lag bound past which the budget is UNAVAILABLE rather than quoted stale
  ([8cb12d6](https://github.com/teamlead-com/cerbix/commit/8cb12d6))
- **The gate derives nothing.** The service row, the policy, the active override, the report, the burn
  latches, the incident and the ledger write all happen in ONE `REPEATABLE READ` transaction whose
  first snapshot-bearing statement is the decision's `evaluated_at` — so a gate answer and the service
  page cannot disagree about the same instant ([8cb12d6](https://github.com/teamlead-com/cerbix/commit/8cb12d6), [b7518b9](https://github.com/teamlead-com/cerbix/commit/b7518b9))
- **An override changes the action and never the facts.** At most seven days, one active per service,
  bound to the policy revision, project-admin only; the decision still records the state that was
  observed and that somebody overrode it ([229b33e](https://github.com/teamlead-com/cerbix/commit/229b33e))
- **Every decision is an immutable ledger row**, in a daily-partitioned bounded table, readable by id
  after the service is renamed or deleted — the moment the evidence is actually wanted. Partition
  maintenance runs as a fenced pass with a 30-second lifecycle and ownership proved by marker rather
  than by OID ([a6a9915](https://github.com/teamlead-com/cerbix/commit/a6a9915), [24e901f](https://github.com/teamlead-com/cerbix/commit/24e901f))
- **In the SPA:** a `Release gate` card on the service page with the policy editor, the latest decision
  and the override panel; a `Gate decisions` browser with a server-side state filter and keyset paging;
  the by-id record; and the per-service override history. Opening a page never creates a decision — only
  a pipeline does ([9793758](https://github.com/teamlead-com/cerbix/commit/9793758), [84adac1](https://github.com/teamlead-com/cerbix/commit/84adac1), [3fc19e3](https://github.com/teamlead-com/cerbix/commit/3fc19e3))

**Change Intelligence — the pipeline says what it changed (FR-025)**

- **A change is a fact about time, not a catalog.** `cerbix change record` (or one `POST`) reports a
  `deploy`, `rollback` or `flag` in one of its phases — `started`, `succeeded`, `failed`, `cancelled` —
  under an external identity `(source, external_id)`, optionally naming the gate decision the release
  rested on. Phases are append-only and idempotent: an identical retry returns the original row, a
  contradictory one is refused by name, and two runners reporting different endings for one run cannot
  both pass ([c6c74dd](https://github.com/teamlead-com/cerbix/commit/c6c74dd), [74ce187](https://github.com/teamlead-com/cerbix/commit/74ce187))
- **The service timeline** is a bounded `[from, to)` read of change groups with an opaque cursor that
  never returns a group twice, each group carrying its live gate decision and the incidents it preceded
  ([7260e68](https://github.com/teamlead-com/cerbix/commit/7260e68))
- **Incident correlation says "preceded", never "caused".** When a service auto-incident opens, the
  changes within the correlation window on that service and on its `probable_root` upstreams are linked
  and named in one `🚀 Changes:` note. It is fail-open in both directions: the incident opens and
  resolves exactly as before whatever the correlation does ([7260e68](https://github.com/teamlead-com/cerbix/commit/7260e68), [3b43b70](https://github.com/teamlead-com/cerbix/commit/3b43b70),
  [f5654d7](https://github.com/teamlead-com/cerbix/commit/f5654d7))
- **Before/after SLI around a change**, computed from SEALED buckets through the same query the
  reliability page uses — never a second implementation. Each side is a figure, or withheld with the
  page's own word, or `pending` until the seal reaches it; a delta only when both sides are figures
  ([c6c74dd](https://github.com/teamlead-com/cerbix/commit/c6c74dd), [5cace76](https://github.com/teamlead-com/cerbix/commit/5cace76))
- **A CI token can be narrower than a role.** An optional `actions` allow-list on an API token is
  intersected with the role in `authz.Can`, so a pipeline token can be exactly "ask the gate, record a
  change" and nothing else ([7260e68](https://github.com/teamlead-com/cerbix/commit/7260e68), [a1cfe00](https://github.com/teamlead-com/cerbix/commit/a1cfe00))
- **In the SPA:** a `Changes` card beside the release gate with terminal-only marks on the facts strip,
  the timeline view, the comparison view, and `Preceded by` on the incident page ([d2e0f3c](https://github.com/teamlead-com/cerbix/commit/d2e0f3c),
  [42ae4ab](https://github.com/teamlead-com/cerbix/commit/42ae4ab))

**Notification channels are edited in place**

- **A channel's name and config are editable**, so rotating a bot token or a hook URL no longer means
  delete-and-recreate — which silently dropped every monitor link, escalation step and alert route
  pointing at the channel. A secret left blank keeps the stored value, because the API never sends one
  out; the merged config is validated, so an edit cannot leave a channel undeliverable ([3e76791](https://github.com/teamlead-com/cerbix/commit/3e76791))

### Improvements

**UI**

- **The alerting panel keeps an operator's unsaved edits** when a late prop arrives instead of
  discarding them ([dd83bfa](https://github.com/teamlead-com/cerbix/commit/dd83bfa))
- **The gate ledger's state filter is the server's**, so a page of results is a page of matches and the
  cursor continues the filtered set ([84adac1](https://github.com/teamlead-com/cerbix/commit/84adac1))

**Documentation & Gates**

- **Every surface now says what the product is.** D-0174's positioning — a service reliability platform,
  not "uptime & SLA monitoring" — reached the README at iter-0160 and stopped there; the CLI's help, the
  OpenAPI description, the overview, the onboarding doc, the systemd unit and the OIDC client all still
  carried the pre-FR-021 framing. Claims that the repository is private are gone with them; they told a
  reader to authenticate for things that need no authentication ([92edc5b](https://github.com/teamlead-com/cerbix/commit/92edc5b))
- **The PRD describes the product as it is now** — services, their incidents, their escalation ladder
  and change intelligence — and points at a roadmap that exists ([4066527](https://github.com/teamlead-com/cerbix/commit/4066527), [59d05c4](https://github.com/teamlead-com/cerbix/commit/59d05c4))
- **`make docs-check` compares the FR-025 acceptance map as a SET** and refuses the spellings the design
  retired, in the specification and in every living document ([056eff5](https://github.com/teamlead-com/cerbix/commit/056eff5), [b320290](https://github.com/teamlead-com/cerbix/commit/b320290))
- **The incident-audit gap has a requirement.** FR-026 / NFR-021 are specified and approved at revision
  four after three review rounds; no product code changes until the iteration opens ([9208171](https://github.com/teamlead-com/cerbix/commit/9208171))

### API · Metrics · Schema

- **API:** eleven gate routes (the decision, the policy CRUD with `expected_revision`, the override
  lifecycle and history, the project-scoped ledger read and listing); four change routes (record,
  timeline, compare, an incident's preceding changes); `ApiToken.actions`; `PATCH
  /api/v1/notification-channels/{id}` now accepts `name` and `config` as well as `enabled`; the
  service detail carries its `sla_targets` inventory.
- **Metrics:** the gate family — `cerbix_gate_decisions_total{state,action,overridden}`,
  `cerbix_gate_decision_duration_seconds` (this project's first histogram),
  `cerbix_gate_evaluate_rejected_total`, `cerbix_gate_evaluate_errors_total`,
  `cerbix_gate_maintenance_errors_total` and four ledger gauges; and the change family —
  `cerbix_change_correlations_total`, `cerbix_change_correlation_errors_total`,
  `cerbix_change_compare_total`, `cerbix_change_record_rejected_total` ([db16dfa](https://github.com/teamlead-com/cerbix/commit/db16dfa)).
- **Schema:** migrations `00093`–`00094` — gate policies, overrides, the daily-partitioned decision
  ledger and its ownership registry; `service_changes`, `incident_changes`, `api_tokens.actions`, and
  `UNIQUE (id, project_id)` on `incidents`.

<sub>85 commits · decisions D-0188…D-0214 · independent review: every FR-024 range approved, FR-025
approved as four effective slices plus the live-evidence correction · full account in
`docs/iterations/iter-0163.md`, `docs/iterations/iter-0164.md`, `docs/iterations/iter-0165.md`</sub>

---

## [v0.1.5] - 2026-08-28

### ⚠️ Upgrade Notes

- **Stop the outbox owners before migrating.** Roles `all`, `api` and `scheduler` run the outbox worker; `worker` and `agent` do not and can keep running. Run `cerbix migrate` once with the new binary, then start the owners. Migration `00088` hands ownership of a class of outbox rows to the database and cannot reach a delivery an old worker already has in flight. Skipping the stop loses nothing; it risks out-of-order delivery for at most one already-claimed batch (≤50 rows) per old owner ([fc6608d](https://github.com/teamlead-com/cerbix/commit/fc6608d), [88ec4ad](https://github.com/teamlead-com/cerbix/commit/88ec4ad))
- **Migration `00090` repairs incident data written by earlier versions.** Incidents resolved and then walked backwards, and service incidents stranded open after their alert ended, are resolved with a `🔧 Repaired:` timeline note. Two classes are reported in the migration output but deliberately left alone — member snapshots that may name a not-yet-governing revision, and auto-incidents with no monitor or service left — because fixing them would mean guessing at history. Queries and the manual procedure are in `docs/runbook.md`. On a database that never hit the races the migration is a no-op ([af7a7a2](https://github.com/teamlead-com/cerbix/commit/af7a7a2))
- **PostgreSQL 15 or newer is required.** 14 is not supported and will not be; `cerbix migrate` refuses before applying anything ([e53244b](https://github.com/teamlead-com/cerbix/commit/e53244b))
- **Armed services keep their coverage across the upgrade.** `00089` seeds delivery tracking as "delivered" for existing rows, so no member monitors start paging in the minute after the upgrade ([7a9e87d](https://github.com/teamlead-com/cerbix/commit/7a9e87d))

### New Features

**Service Alerting: Coverage Means Somebody Was Told**

- **A route must name a live channel.** A schedule pointing at a deleted or disabled channel no longer counts as a route; members keep paging instead of being silenced in favour of a replacement that can reach nobody ([fb9e94b](https://github.com/teamlead-com/cerbix/commit/fb9e94b), [08a8b31](https://github.com/teamlead-com/cerbix/commit/08a8b31))
- **An alert nobody can receive is withheld, not sent into the void.** Counted as `cerbix_service_alert_withheld_total{signal,reason}` with `unroutable` or `no_governing_revision`, and announced as soon as a route exists — on both the health and burn signals ([44dfdad](https://github.com/teamlead-com/cerbix/commit/44dfdad), [a3aecfc](https://github.com/teamlead-com/cerbix/commit/a3aecfc), [19d4943](https://github.com/teamlead-com/cerbix/commit/19d4943))
- **Coverage requires a delivered announcement.** A service suppresses its members only once its own alert *reached* at least one recipient — a channel row that exists is not a delivery, and a 500 from the only channel is not a delivery either ([7a9e87d](https://github.com/teamlead-com/cerbix/commit/7a9e87d), [5ec101b](https://github.com/teamlead-com/cerbix/commit/5ec101b), [35de54d](https://github.com/teamlead-com/cerbix/commit/35de54d))
- **Coverage follows the state the service is in.** A delivered DEGRADED announcement no longer covers a service that has since observed DOWN and is still confirming it ([6e6339f](https://github.com/teamlead-com/cerbix/commit/6e6339f), [46c50de](https://github.com/teamlead-com/cerbix/commit/46c50de))
- **An outage nobody heard about is announced again** once there is somebody to tell — channel deleted mid-flight, every send failed, or retries exhausted — with a fresh recipient list and a new episode. A partial delivery that reached some recipients is never re-sent to them ([23aa3cc](https://github.com/teamlead-com/cerbix/commit/23aa3cc), [425a59f](https://github.com/teamlead-com/cerbix/commit/425a59f), [2b2594c](https://github.com/teamlead-com/cerbix/commit/2b2594c))
- **One vocabulary for "why did my monitor page".** The service badge and `cerbix_alert_delegation_fail_open_total{reason}` are computed from the same clause evaluation. New values `no_owning_service`, `onset_pending`, `onset_undelivered`, `latch_inconsistent`; `stale_lease` on the burn arm now means an expired lease and nothing else ([18e3aef](https://github.com/teamlead-com/cerbix/commit/18e3aef), [3192ebb](https://github.com/teamlead-com/cerbix/commit/3192ebb), [6e0b6a4](https://github.com/teamlead-com/cerbix/commit/6e0b6a4), [4c4bae3](https://github.com/teamlead-com/cerbix/commit/4c4bae3))

**Service Escalation: Repeat Cadence**

- **Services can repeat the last escalation step.** `renotify_seconds` on the service — 0 is off and the default, otherwise 60..86400 — read live, so turning it down mid-incident takes effect immediately. Available in the UI, the API and monitoring-as-code bundles (`alerting.renotify_seconds`). Previously `repeat_last` on a service-attached policy did nothing ([90bf146](https://github.com/teamlead-com/cerbix/commit/90bf146), [425a59f](https://github.com/teamlead-com/cerbix/commit/425a59f))
- **An incident climbs the ladder it started with.** The escalation policy is snapshotted when the incident opens, so editing a policy mid-outage cannot re-time a page already in flight. Incidents open across the upgrade start their ladder at the upgrade instant instead of firing every overdue step at once ([0c6e5dd](https://github.com/teamlead-com/cerbix/commit/0c6e5dd), [d3f428f](https://github.com/teamlead-com/cerbix/commit/d3f428f), [118c55c](https://github.com/teamlead-com/cerbix/commit/118c55c))

**Outbox: Ordered and Bounded Delivery**

- **Incident webhooks are dispatched in order.** Every `incident.*` payload carries `seq`; the claim will not release an event while an earlier one for the same incident is undelivered, and a dead predecessor blocks too. Arrival order over the network is not promised — `(incident.id, seq)` is what lets a receiver dedupe and order, and the runbook describes two receiver strategies ([554e609](https://github.com/teamlead-com/cerbix/commit/554e609), [8ed191a](https://github.com/teamlead-com/cerbix/commit/8ed191a), [02bc005](https://github.com/teamlead-com/cerbix/commit/02bc005), [4762f1a](https://github.com/teamlead-com/cerbix/commit/4762f1a), [64349a0](https://github.com/teamlead-com/cerbix/commit/64349a0))
- **A delivery is bounded by the lease that authorised it.** A deposed worker can no longer keep an HTTP request or SMTP session open while the new owner sends the same event. The lease is measured in database time, the settle is never bounded, and a claim whose turn came after its lease is handed back with its attempt refunded ([b17e0e2](https://github.com/teamlead-com/cerbix/commit/b17e0e2), [425a59f](https://github.com/teamlead-com/cerbix/commit/425a59f), [2b2594c](https://github.com/teamlead-com/cerbix/commit/2b2594c))

### Improvements

**Incident Lifecycle**

- **`resolved` is terminal and status only moves forward**, enforced in the write rather than in a stale read. A plain comment keeps the current status instead of carrying whatever the client last saw ([b21b09f](https://github.com/teamlead-com/cerbix/commit/b21b09f), [183b9f8](https://github.com/teamlead-com/cerbix/commit/183b9f8), [451406b](https://github.com/teamlead-com/cerbix/commit/451406b))
- **Service incidents are a full lifecycle.** They announce open *and* resolve to webhooks and status-page subscribers, and resolve when their alert ends — including through disown and delete, which used to leave them open forever ([e405d85](https://github.com/teamlead-com/cerbix/commit/e405d85), [744980f](https://github.com/teamlead-com/cerbix/commit/744980f))
- **The postmortem names the declaration that governed the outage**, not the newest one; a foreign revision is refused ([0a8ba9b](https://github.com/teamlead-com/cerbix/commit/0a8ba9b), [335d4db](https://github.com/teamlead-com/cerbix/commit/335d4db))
- **Every incident write stamps its times after the row lock**, so a writer that waited cannot date its action before the wait ([3ad2a4b](https://github.com/teamlead-com/cerbix/commit/3ad2a4b), [5ec101b](https://github.com/teamlead-com/cerbix/commit/5ec101b))

**Status Pages**

- **A page, its feed and its subscriber mail agree on what the page reports.** A page made only of Service components used to show an incident and email nobody about it. One project axis now, with the subscriber query as its exact inverse ([c7ce059](https://github.com/teamlead-com/cerbix/commit/c7ce059))

**UI**

- **Search hits bring their workspace.** Opening a monitor or incident from another project switches to it, so edit controls are not hidden from a legitimate editor; detail views follow the URL when navigating between two of the same kind ([2dbc7ce](https://github.com/teamlead-com/cerbix/commit/2dbc7ce))
- **A partly unmeasured hour no longer renders green** on the Reliability card ([27c63f7](https://github.com/teamlead-com/cerbix/commit/27c63f7))
- **The service picker in status-page components works again**, and the escalation form says what "repeat last step" does on a service ([a26260d](https://github.com/teamlead-com/cerbix/commit/a26260d), [a331af0](https://github.com/teamlead-com/cerbix/commit/a331af0))

**Documentation & Gates**

- `make docs-check` compares the FR-021 invariant set in the spec against the discharge map exactly — missing, extra, duplicate or skipped numbers all fail; invariants 92–105 moved into the spec ([46c50de](https://github.com/teamlead-com/cerbix/commit/46c50de), [35de54d](https://github.com/teamlead-com/cerbix/commit/35de54d))
- Four documents that still described the product as it was before FR-022/FR-023 shipped are corrected, and a gate catches the class ([afc8cd1](https://github.com/teamlead-com/cerbix/commit/afc8cd1))

### API · Metrics · Schema

- **API:** `ServiceAlertPolicy.renotify_seconds`; alerting-state `reason` gains `onset_pending`, `onset_undelivered`, `latch_inconsistent`; `incident.*` webhook payloads carry `seq`.
- **Metrics:** new `cerbix_service_alert_withheld_total{signal,reason}`; `cerbix_alert_delegation_fail_open_total{reason}` also emits `error`, `record_failed`, `unspecified` for lookup-level failures.
- **Schema:** migrations `00085`–`00092` — incident escalation snapshots, `incidents.event_seq`, `CHECK ((status = 'resolved') = (resolved_at IS NOT NULL))`, `delivered_seq`/`undelivered_seq` on both service latch tables, `services.renotify_seconds`, `undelivered` as an episode close reason.

<sub>57 commits · decisions D-0175…D-0187 · independent review: product approved at `35de54d`, follow-on work at `eba2b69` · full account in `docs/iterations/iter-0161.md`</sub>

---

## [v0.1.5-beta.2] - 2026-08-19

Three defects found by running `v0.1.5-beta.1` in production rather than by a test. The first blocked the
upgrade outright.

### Fixed

- **PostgreSQL 15 is now enforced instead of assumed.** A production upgrade to `v0.1.5-beta.1` on
  PostgreSQL 14 applied `00061`…`00069` and died on `00070` with `syntax error at or near "("`: five
  migrations use the column-list `ON DELETE SET NULL (col)` form introduced in PG15. Every document, image
  and CI job already said 16, but nothing checked it and nothing said it out loud. `cerbix migrate` now
  reads `server_version_num` before the first file and refuses with the version, the requirement and the
  fact that nothing was applied. README, `runbook.md` and `overview.md` state the requirement; the runbook
  also carries the recovery note for a system left partially migrated (`00065` makes `monitors.slug` NOT
  NULL, which an older binary does not write).
- **A status-page component could not be created from a Service.** The service picker rendered a blank
  option AND carried no value, because the view read `sv.name`/`sv.id` from a list endpoint that answers
  `ServiceSummary` (the service wrapped with its rollup counts). An `as Service[]` cast on the load is what
  stopped the compiler from reporting it, and the test fixture repeated the same wrong shape, so six
  passing tests never saw it.
- **A stale claim on the service page.** The footer said availability, the error budget and the burn rate
  "arrive with the next iteration". They arrived in iter-0144 and the page already renders them; what was
  actually missing on a fresh service is sealed facts and a declared objective, which is what it says now.

---

## [v0.1.5-beta.1] - 2026-08-19

242 commits since `v0.1.0-beta.5`. This is the release where a **Service** becomes the object
reliability is defined on, measured for, and paged about — and where the product's own
positioning was corrected to match (**D-0174**).

### Added

- **Service reliability, phases 3–5 (FR-021 / NFR-016, D-0169)** — closed against an ENFORCED
  discharge map of 91 acceptance invariants and 24 required scenarios, each naming a test that
  exists; `make docs-check` fails if a number lacks a row or a row cites a test the tree lacks.
  - **dependency impact graph** — a same-project service DAG (schema-enforced tenancy, bounded,
    outside the declaration), symmetric open-time correlation into structured incident↔service
    links with 🕸 timeline notes. It annotates and links: it records **candidates**, never elects
    a culprit, and never suppresses or hides.
  - **status-page projection (§15.0)** — a component renders from ONE of three sources under a
    discriminator (`monitor`/`service`/`manual`) with the replaced binding kept dormant for
    revert. **Public-output change:** the page summary is worst-of-MEASURED plus an unmeasured
    count, and measurement ABSENT is the public status `no_data` — never `operational`.
  - **alerting ownership (§16)** — a Service can be the thing that pages, and its declared SLI
    members can stop delivering their own alerts for the same failure. Suppression is per
    SIGNAL (live health, sealed burn), per POLARITY (onset-like only; a recovery is never
    suppressed) and only while a replacement is demonstrably ARMED. Anything ambiguous **fails
    open** — the member pages.
  - **service burn alerting** — arbitrary long/short windows over one burn-math owner, with the
    hold matrix and the watermark every number was computed from.
- **Service incidents (FR-022 / NFR-017, D-0170, D-0171)** — an incident can be an incident OF a
  Service. At most one anchor, enforced by CHECK; opened in the SAME transaction as the
  announcement and resolved by its close; **never on a burn breach**; at most one open
  auto-incident per service; a member snapshot a postmortem can still name after the world moved;
  impact links through the service graph, with no link ever naming its own subject. Closed
  against 16 invariants + 16 scenarios.
- **Escalation for services (FR-023 / NFR-018, D-0172, D-0173)** — a Service with an escalation
  policy escalates its own auto-opened incident: steps from the incident's start, durable
  progress, acknowledgement or resolution ends it, every step names the SERVICE. The ladder
  **fails closed** where delegation fails open, and the service graph does **not** pause it.
  Closed against 16 invariants + 19 scenarios.
- **Project-level SLO objective** and an **instance-wide audit surface** for a global admin's own
  actions, which had been recorded for months and shown nowhere.
- **`job_id` correlation** end to end, and `observed_at` ordering that refuses a result older
  than the issue it answers.
- **A write path for a service's escalation policy** — the column existed since phase 5 and was
  reachable only at create time or from a file provider; a change to who gets woken is now
  audited with what moved, inside the mutating transaction.
- **`cerbix_service_incidents_total{action}`** and **`cerbix_escalation_steps_total{subject}`** —
  the on-call ladder had no metric at all before, only a log line.

### Changed

- **Positioning (D-0174)** — cerbix is a **service reliability platform**, not "uptime & SLA
  monitoring". The README states its NON-GOALS publicly, quoted from the specification: no
  arbitrary time-series queries, no generic telemetry, no query language, no metrics backend, no
  service catalog, no trace or log ingestion, and **no automatic root-cause analysis**.
- **`ServiceDetail.reliability`** stays `null` by design and now SAYS so: SLO, error budget and
  burn rate live on `GET …/services/{id}/reliability` with the honesty context a bare number
  would lack. The previous description claimed they were unbuilt.
- **Dependencies** — `golang.org/x/crypto` 0.55.0, `x/net` 0.58.0, `x/text` 0.41.0; build image
  golang 1.26.6; pinia 4.0.3; and four frontend majors (`@vitejs/plugin-vue` 6, `npm-run-all2` 9,
  `unplugin-auto-import` 21, `@vueuse/core` 14), each merged and verified one at a time.

### Fixed

- **A public leak found by reviewing our own invariant:** `Incident.PublicRedacted` cleared
  `project_id`, `monitor_id`, the external key and the ack actor — and not the `service_id` added
  days earlier, so every unauthenticated render of a page with a service incident shipped the
  service's internal UUID.
- **CI had never run on this line of work**: tests triggered only on `pull_request`, `docs-check`
  ran only by hand, one storage mode was covered, and readiness was awaited with `pg_isready`,
  which this project's own discipline calls a non-barrier. All four repaired.
- **Two flaky tests fixed at their cause**, not re-run: a fence test that bet a budget refusal on
  5ms of wall clock (2 failures in 12 runs under load), and scheduler telemetry waits that
  asserted more series than they waited for.

### Migrations

24 new migrations (`00061`…`00084`), forward-only and applied automatically by every role.

### Compatibility

FR-021 §17 makes backward compatibility an acceptance criterion, not a footnote: zero Services is
a valid installation state, every existing Monitor stays valid without a service, bundle format 1
stays valid, existing composites and monitor SLOs keep their semantics. **The one intentional
break is the public status-page output** described above — a consumer that read `operational` for
an unmeasured component will now read `no_data`.

---

## [v0.1.0-beta.1 … v0.1.0-beta.5] — released 2026-07-25 … 2026-08-12

> **Corrected on 2026-08-19.** This block sat under `[Unreleased]` while its contents were
> shipping across the five `v0.1.0-beta.*` tags — nobody moved it out. It is relabelled rather
> than rewritten, because it is a record of what happened, not a plan. The per-beta split is
> not reconstructed here: the tag messages carry it, and inventing a division after the fact
> would be a worse claim than admitting the block covers the whole beta train.

### Added
- **Geo-Distributed HTTP Pull Agent** (`--role agent`) with Long-Polling (`LISTEN/NOTIFY`), Edge Ring-Buffer (`bufferCap=10000`), and historical backfill (`POST /agent/backfill`).
- **Observability for Pull Transport**: Prometheus gauges `cerbix_pull_jobs_pending{region}` and `cerbix_pull_agent_lag_seconds{region}` with automatic lagging alerts.
- **Database Agent Tokens**: Table `agent_tokens` and Admin API (`POST/GET/DELETE /api/v1/agent-tokens`) for issuing, listing, and revoking agent tokens without redeploy.
- **Region Scoping**: Enforcement of `monitor.region == agent.region` on `/agent/results` and `/agent/backfill` (403 Forbidden on mismatch).
- **16 Prober Types**: Added probers for PostgreSQL, MySQL, Redis, RabbitMQ, PromQL, gRPC, WebSocket, SSH, DNS, TLS cert expiry, Composite, and Synthetic multi-step HTTP scenarios.
- **On-Call & Escalation Engine**: Escalation ladders, on-call schedules with vacation overrides, and acknowledge-to-stop incident handling.
- **Prometheus Alertmanager Receiver**: Inbound webhook receiver (`firing` -> auto-incident, `resolved` -> auto-close by fingerprint).
- **Instance Settings Framework**: Database singleton `instance_settings` for branding, auth policies, SMTP mailer, and global silence toggle.

### Changed
- **OIDC Provider Independence**: Identity provider is now any OpenID Connect issuer (Keycloak, Auth0, Okta, Google, Entra ID) discovered via `oidc.issuer`.
- **Database Schema**: Renamed `keycloak_sub` to `oidc_sub`.
- **Heartbeat Retention & Partitioning**: Switched `heartbeats` to native daily RANGE partitioning with automatic retention purging (`retention_days`).

### Security
- **SSRF Guard**: Prober target resolution validated by [`prober.Guard`](internal/prober/guard.go), blocking cloud metadata (`169.254.169.254`) and link-local ranges by default.
- **AES-256-GCM Secrets at Rest**: Keyring encryption for webhook secrets and channel credentials with zero-downtime key rotation (`cerbix reencrypt`).

---

> **Note on the two entries below.** `v0.35.0` and `v0.10.0` belong to an EARLIER numbering
> scheme and correspond to no git tag in this repository — the tag line is `v0.1.0-beta.*`.
> They are kept because they document real work; treat their version numbers as historical
> labels, not as releases anybody can check out.

## [v0.35.0] - 2026-08-01

### Added
- **Global Search**: Tenant-scoped search endpoint (`GET /api/v1/search`) across monitors, projects, and incidents.
- **p95 Latency**: SLA reporting enriched with `p95_latency_ms` via `percentile_cont(0.95)`.
- **UI 1:1 Design Sync**: Vue 3 SPA views rebuilt matching modern dark-theme design artifacts with bespoke inline-SVG charts.

---

## [v0.10.0] - 2026-07-25

### Added
- **Transactional Outbox**: Outbox delivery pipeline for notifications and incident webhooks with exponential backoff and dead-letter queue.
- **SLA & SLO**: Error budgets, maintenance window exclusion, and daily availability rollup aggregation.
- **Single Binary Multi-Role Execution**: `--role all|api|scheduler|worker` process execution model.
