# cross-cli-command-tree — Cobra ownership and structured help

> **Lifecycle: IMPLEMENTED / OWNER-APPROVED — CORRECTIVE CYCLE CLI-0196 CLOSED BY THE OWNER ON
> 2026-10-02, D-0265; one local corrective commit authorized, push unauthorized.** The original design and
> implementation were approved and committed locally on 2026-10-02, but later verification confirmed six
> parser-boundary defect classes. The corrective implementation fixed command position, explicit false help,
> hidden completion protocol reachability, Windows mousetrap lifecycle, version flag-error compatibility, and
> compound help. The first corrective high-effort review returned `CHANGES REQUIRED` for repeated-help policy
> and global-mutation wording; both were fixed. Its targeted re-review returned `CHANGES REQUIRED` for version
> invalid-help/positional precedence; that version-local fix and permanent regression matrix were added. The
> final targeted independent review is **APPROVED** with Critical `0`, Important `0`, and Minor `0`.
> The corrective implementation is based on `c810b65901446de37221f0b7f747ef9ed6e8f607`; Cobra remains at
> v1.10.2, pflag remains transitive, and Viper remains absent. Fresh targeted/full/race/build/vet/Windows/docs/
> module/diff/scope and rebuilt-binary gates are green. The owner approved and closed the corrective cycle on
> 2026-10-02. Corrective implementation commit `07474d0b2537ccc9026de5883ac8fcf3d9a44d37` records the
> approved implementation and closure and was fast-forwarded into local `main` on 2026-10-02 without a merge
> commit. Push remains unauthorized; no PR, remote merge, tag, release, deploy, or restart was performed, and
> the corrective worktree and branch remain preserved.
>
> The design changes command parsing and help ownership only. Documented command paths, canonical long
> flag names, flag types, defaults, required/optional meaning, environment variables, stdout/stderr
> contracts, and semantic exit codes remain unchanged. Help changes as specified in §7, and previously
> ignored positional arguments are rejected with usage exit code `2` before side effects as specified in
> §10.2.

## 1. Problem and purpose

At the `f6320fc` base commit, the binary had one public command family but three interface mechanisms:

1. a manual top-level `switch` in `internal/cli/cli.go`;
2. command-local standard-library `flag.FlagSet` parsers;
3. hand-written usage strings in the root, `gate`, and `change` dispatchers.

Those mechanisms do not form one command tree. The root can list commands, but `cerbix help serve`
ignores `serve` and prints the root usage. Leaf `-h` / `--help` paths return usage exit `2` for most
commands, `gate --help` and `change --help` are treated as unknown subcommands, and
`version --help` executes `version`. Required flags, defaults, environment contracts, exit codes, and
examples are distributed between code, comments, README, overview, runbook, and feature specs.

This specification gives the complete CLI interface layer one owner: Cobra. It defines a stable,
plain-text command catalogue at the root and command-specific help at every group and leaf while keeping
business/runtime execution in the existing cerbix packages.

## 2. Scope

This change covers:

- the root command and the complete public command hierarchy;
- command, subcommand, argument, and flag parsing;
- root, group, and leaf help;
- required-flag metadata and validation;
- the boundary between parsed typed options and existing execution logic;
- centralized application-owned exit-code mapping;
- deterministic stdout/stderr selection;
- characterization, help, compatibility, side-effect, and semantic-exit tests;
- the direct Cobra module dependency and dependency-graph guards;
- synchronized CLI documentation, decision, iteration, status, traceability, and changelog records.

The public command tree is exactly:

```text
cerbix
├── serve
├── migrate
├── reencrypt
├── adopt-fact-month
├── enqueue-service-repair
├── gate
│   └── check
├── change
│   └── record
└── version
```

The existing command inventory is therefore eight runnable leaf paths and two non-runnable command
groups:

| Path | Kind | Existing execution owner |
| --- | --- | --- |
| `cerbix serve` | leaf | runtime role wiring in `internal/cli/cli.go` |
| `cerbix migrate` | leaf | embedded goose migration entrypoint |
| `cerbix reencrypt` | leaf | stored-secret key-rotation rewrite |
| `cerbix adopt-fact-month` | leaf | D-0161 operator partition adoption |
| `cerbix enqueue-service-repair` | leaf | durable admin repair enqueue |
| `cerbix gate` | group | contains `check` only |
| `cerbix gate check` | leaf | FR-024 remote HTTP client |
| `cerbix change` | group | contains `record` only |
| `cerbix change record` | leaf | FR-025 remote HTTP client |
| `cerbix version` | leaf | build-information JSON encoder |

## 3. Non-goals

This migration does not:

- add Viper or another configuration registry;
- change `internal/config`, YAML schema, settings precedence, or environment expansion;
- add automatic environment binding to config keys;
- add `--token`, a remote-server `--url`, `--insecure`, or any TLS verification bypass;
- move `CERBIX_URL`, `CERBIX_TOKEN`, or `CERBIX_CA_FILE` into flags or config files;
- add a public completion command, implicit version flag, shorthand flags other than standard help, ANSI
  color, an interactive pager, shell prompts, or confirmation flows;
- add runtime config reload;
- change API/OpenAPI, database schema, migrations, process roles, frontend, or the generated SPA;
- rewrite server wiring, migration behavior, encryption behavior, repair behavior, gate evaluation, or
  change-record transport behavior;
- perform config-file, network, database, RabbitMQ, or other runtime access while rendering help;
- execute deploys or mutate production settings/data.

## 4. Dependency decision

The only new direct dependency is:

```text
github.com/spf13/cobra v1.10.2
```

The version was selected from the stable versions returned by the official module command on
2026-10-02:

```text
go list -m -versions github.com/spf13/cobra
```

`v1.10.2` is the newest stable version in that result. Its module declares `go 1.15`, so it is
compatible with cerbix's `go 1.25.13` module floor. The final version is pinned in `go.mod` and
`go.sum`; `@latest` is not part of the implementation procedure.

Viper is prohibited both as a direct dependency and as a cerbix configuration abstraction. After the
module update:

```text
go mod why -m github.com/spf13/cobra
```

must show the `internal/cli` import path, while:

```text
go mod why -m github.com/spf13/viper
```

must report that the main module does not need Viper. If Viper appears unexpectedly, implementation
stops until the dependency path is understood and removed or separately approved.

## 5. Ownership and construction

### 5.1 Cobra is the only parser and command tree

`newRootCommand` constructs the root and adds commands produced by:

- `newServeCommand`;
- `newMigrateCommand`;
- `newReencryptCommand`;
- `newAdoptFactMonthCommand`;
- `newEnqueueServiceRepairCommand`;
- `newGateCommand` and `newGateCheckCommand`;
- `newChangeCommand` and `newChangeRecordCommand`;
- `newVersionCommand`.

Each constructor owns its Cobra `Use`, `Short`, `Long`, `Example`, argument validation, and flag
registration. A flag is registered once. There is no native `flag.FlagSet`, secondary parser, manual
subcommand dispatcher, duplicate usage constant, or top-level command switch after migration.

The root has:

```go
SilenceUsage:  true
SilenceErrors: true
```

and explicitly receives deterministic writers:

```go
cmd.SetOut(stdout)
cmd.SetErr(stderr)
```

Cobra's automatic completion command is disabled. The root has no `Version` field, so Cobra cannot add
an implicit `--version` flag. Suggestions are disabled so an unknown command does not gain a new
Levenshtein-dependent stderr suffix. Before any `Execute`, cerbix also rejects Cobra's hidden
`__complete` and `__completeNoDesc` protocol names when they occupy command position; the strings remain
valid values of ordinary string flags. Cobra's documented Windows mousetrap is disabled by the intentional
process-global package-initialization assignment `cobra.MousetrapHelpText = ""`. Package initialization runs
before Cobra's Windows `preExecHook`, so `Main(args) int` remains the lifecycle and exit owner even when a
Windows binary is launched from Explorer. This is not per-command configuration and is the one intentional
process-global Cobra write; `cobra.EnableCommandSorting` remains untouched.

A single parser-boundary inspection runs before `Execute`. Its responsibility is deliberately narrow:

- enforce the public raw grammar `cerbix <command> [flags]`, with `check` immediately after `gate` and
  `record` immediately after `change`;
- reject root flags before a top-level command and group/leaf flags before a nested command with usage exit
  `2`, before any executor, config loader, HTTP client, database, or broker access;
- preserve positional and unknown-nested-command precedence over true help;
- classify help only when an exact help token occupies a flag position according to the selected command's
  registered flag arity; string values equal to `-h`, `--help`, `--help=false`, `--`, `__complete`, or
  `__completeNoDesc` are not reclassified;
- route flag parse failures through the target command's Cobra `FlagErrorFunc` instead of replacing
  command-specific compatibility policy with a generic usage error.

The custom `help` command is inserted into Cobra's searchable tree during root construction, before that
inspection. Cobra still owns the registered commands, flags, flag parsing, and command execution; the
boundary does not execute runtime work or maintain a second public command inventory.

### 5.2 Typed options separate parsing from execution

Every leaf binds flags into a command-specific options value, for example:

```go
type serveOptions struct {
    ConfigPath string
    Role       string
    Region     string
}
```

Equivalent option types own migrate, re-encrypt, fact adoption, repair enqueue, gate check, and change
record inputs. Constructors perform only interface-layer work: registration, argument/required/value
validation, and invocation of a parse-independent executor.

Executors retain the current runtime/business paths. In particular, this migration does not redesign:

- `runServe`'s role-dependent wiring, shutdown, database, dispatcher, API, scheduler, worker, or agent
  behavior;
- `store.Migrate`, secret re-encryption, fact adoption, or repair enqueue mechanics;
- gate/change target construction, TLS client, request shape, no-redirect/no-retry behavior, response
  decoding, raw JSON passthrough, or summary grammar.

The implementation may rename the old `run*` functions while extracting options, but the execution
logic remains recognizable and independently callable without reparsing command-line tokens.

### 5.3 Configuration boundary remains local to commands

The data flow is:

```text
Cobra flags
    ↓
typed command options
    ↓
existing config.Load(path), only in commands that need YAML
    ↓
existing runtime/settings precedence
```

There is no root `PersistentPreRunE`, global config registry, or eager config load. `gate check`,
`change record`, `version`, and every help path never call `config.Load`.

## 6. Flag ownership and validation

### 6.1 Canonical flags

| Command | Required flags | Optional flags and defaults |
| --- | --- | --- |
| `serve` | `--config string` | `--role string` = `all`; `--region string` = empty/core |
| `migrate` | `--config string` | none |
| `reencrypt` | `--config string` | none |
| `adopt-fact-month` | `--config string`; `--month string` | `--timeout duration` = `10m` |
| `enqueue-service-repair` | `--config string`; `--project string`; `--service string`; `--from string`; `--to string` | none |
| `gate check` | `--project string`; `--service string` | `--json bool` = false; `--timeout duration` = `10s` |
| `change record` | `--project string`; `--service string`; `--kind string`; `--phase string`; `--source string`; `--external-id string` | `--ref string`; `--url string`; `--decision string`; `--at string`; `--json bool` = false; `--timeout duration` = `10s` |
| `version` | none | none other than standard help |

No flag is persistent. No command inherits config, credential, URL, TLS, or output flags from the root.

### 6.2 Required metadata has one source

A single Cobra flag-registration helper registers each flag and records its placeholder, order, and
required annotation on that registered flag/command metadata. Required validation, synopsis generation,
and the help renderer read that same metadata. Requiredness, placeholders, and defaults are not restated
in a separate usage list; pflag remains Cobra's transitive implementation dependency, not a direct cerbix import.

Required string values remain non-empty contracts. Explicitly passing an empty value does not satisfy a
required flag. Optional `change record --at ""` and `--decision ""` remain distinguishable from omission
through the Cobra flag's `Changed` state, preserving FR-025's verbatim-body contract.

### 6.3 Value validation

Validation happens before config or remote access:

- `serve --role` accepts exactly `all`, `api`, `scheduler`, `worker`, or `agent`;
- `adopt-fact-month --month` is parsed as UTC `YYYY-MM`;
- duration values must parse, and `--timeout` must be positive where currently required;
- repair `--from` and `--to` must be RFC3339 and `to > from`;
- every leaf rejects positional arguments under the target contract in §10;
- `change record` continues to send kind, phase, source, external id, URL, and timestamp values verbatim
  to the server after requiredness/timeout validation; the CLI does not acquire a second copy of the
  FR-025 domain enums or text rules.

The documented canonical long-flag spelling is `--name`. Standard-library `flag` incidentally accepted
undocumented single-dash long forms such as `-config`; those spellings are not compatibility invariants.
The existing standard help shorthand `-h` remains supported. No other shorthand is added.

## 7. Help contract

### 7.1 General rules

Help is plain UTF-8 text with no ANSI sequences and no pager. It is deterministic and suitable for
golden tests. Every exact valid help invocation exits `0`, writes help to stdout, writes nothing to
stderr, and performs no config, file, network, database, RabbitMQ, or runtime-service access. Argument and
nested-command validation precede help rendering: a positional before `--help`, or a misspelled nested
command followed by help/leaf flags, exits `2` with the usage diagnostic and empty stdout. Help intent is
derived from the selected command's registered Cobra flags rather than raw substring matching:
`--help`/`-h`/`--help=false`/`--` remain valid values of string flags, while a later exact or assignment help
token is interpreted only after those values are accounted for. `--help`, `--help=true`, `-h`, and
`-h=true` are supported true-help forms. For root, groups, all side-effect leaves, and the `help` command,
help policy is **any-false-wins**: the presence of any parsed `--help=false` or `-h=false` assignment is a
deterministic usage error with exit `2`, empty stdout, and zero executor calls, regardless of ordering,
repetition, a preceding true-help, or a later true-help. This is deliberately not pflag last-value semantics.
Help-looking tokens consumed as registered string-flag values do not participate in the policy.

`version` alone preserves the measured compatibility seam. False-help is a compatibility no-op; if any
parseable true-help is also present, true-help wins regardless of order and version help is rendered. An
unknown flag combined with any true-help remains usage exit `2`. Positional validation has highest priority:
any positional exits `2` with empty stdout and zero encoder calls before help or JSON output. Without a
positional or true-help, false-help (including an unknown flag plus false-help) prints version JSON and exits
`0`. Output-writer failures on all public true-help forms share one typed exit-`1` contract.

Compound help is idempotent: `cerbix help --help` is equivalent to `cerbix help`, and
`cerbix help <path> --help` is equivalent to `cerbix help <path>`. The same applies to
`--help=true`, `-h`, and `-h=true`, including group and nested paths. `help ... --help=false` is not a
success-help form and exits `2`.

The following are required success paths:

```text
cerbix --help
cerbix help
cerbix help --help
cerbix help serve
cerbix help serve --help
cerbix help gate check --help
cerbix serve --help
cerbix gate --help
cerbix gate check --help
cerbix change --help
cerbix change record --help
```

`-h` and `--help` are tested for every leaf. `cerbix help <path>` and
`cerbix <path> --help` render the same command metadata.

### 7.2 Root catalogue

The root output is compact and contains no leaf synopsis or complete flag list:

```text
cerbix — self-hosted service reliability platform

Usage:
  cerbix <command> [flags]

Runtime:
  serve                   Run cerbix services.
  migrate                 Apply database migrations and exit.
  reencrypt               Re-encrypt stored secrets.

Reliability maintenance:
  adopt-fact-month        Adopt a retained fact partition.
  enqueue-service-repair  Queue a bounded service repair range.

CI/CD:
  gate check              Evaluate whether a release may proceed.
  change record           Record a deploy, rollback or flag change.

Other:
  version                 Print build information.

Run `cerbix <command> --help` for command-specific flags and examples.
```

The renderer derives entries from the Cobra tree. Root and nested command order is recorded in local
command metadata; construction never mutates Cobra's process-global sorting setting. Hidden and
unavailable commands are excluded. Root command groups define stable group order; leaf paths and summaries
come from available descendant Cobra command metadata. Nested leaves are flattened as `gate check` and
`change record`. There is no second hard-coded command inventory. Tests require every runnable leaf exactly
once and reject an ungrouped, duplicated, hidden, or unavailable catalogue entry.

### 7.3 Group help

`gate` and `change` are non-runnable groups. Their help names the group purpose, shows
`cerbix <group> <command> [flags]`, lists its child command from the Cobra tree, includes standard help,
and points to child help. Executing a group without a subcommand remains a usage error with exit `2`;
explicit group help exits `0`.

### 7.4 Leaf help

A leaf uses only applicable sections in this stable order:

```text
cerbix <path> — <short purpose>

<long description, when needed>

Usage:
  ...

Required flags:
  ...

Options:
  ...

Environment:
  ...

Exit codes:
  ...

Examples:
  ...
```

The synopsis, flag names, types, placeholders, defaults, order, and required classification are rendered
from registered Cobra flag metadata. Help text does not maintain another copy of flag/default syntax. Examples and explanatory sections
are attached to the command that owns them.

Required command-specific content:

- `serve`: `--config`, `--role`, `--region`; roles `all`, `api`, `scheduler`, `worker`, `agent`; `--region`
  applies to worker and agent, with empty meaning core; actual local-versus-distributed semantics from code/docs.
- `migrate`: config path, embedded migration purpose, no runtime-service startup, and existing outcome
  semantics.
- `reencrypt`: config path, primary/previous key rotation sequence, database requirement, and only
  caveats already supported by code/runbook.
- `adopt-fact-month`: config, `YYYY-MM`, timeout/default, positive and overflow-safe timeout validation
  before config/DB access, copy-authoritative physical partition adoption, idempotency, no dry-run/
  confirmation, and maintenance-window lock caveat.
- `enqueue-service-repair`: config/project/service/from/to, RFC3339, durable admin enqueue, range
  normalization/coalescing, scheduler-owned later recomputation, and no immediate automatic recompute.
- `gate`: child `check` discoverability.
- `gate check`: required project/service; JSON and timeout options; `CERBIX_URL`, `CERBIX_TOKEN`, optional
  `CERBIX_CA_FILE`; no credential flags or TLS bypass; safe example; exit 0/1/2/4 table.
- `change`: child `record` discoverability.
- `change record`: six required and six optional command flags (plus standard help); the same three environment variables;
  recorded/replayed behavior; contract-refusal versus transport exit classes; safe example.
- `version`: indented JSON build-information output and no command flags except help.

Examples use shell-safe concrete placeholder UUIDs/URLs and contain no `<...>` redirection
metacharacters, token value, secret, customer, deployment, or production identifier.

## 8. Output and side-effect contract

### 8.1 Streams

- Help and successful `version` JSON use stdout.
- Successful migration, re-encryption, recovery, and serve logs keep their current logger/stream choices.
- Gate/change summaries and `--json` response bytes use stdout exactly as today.
- Gate reasons, contract refusals, usage diagnostics, transport errors, and runtime diagnostics use
  stderr exactly as today.
- `--json` remains byte-identical to the server body; no newline is added.
- Runtime errors never trigger automatic Cobra usage output.

Root, group, and leaf help writers are command-scoped; tests can provide buffers without replacing
process-global `os.Stdout`/`os.Stderr`. Existing runtime functions are adapted to receive the command
writers where necessary rather than bypassing `cmd.SetOut` / `cmd.SetErr`.

### 8.2 Help side effects

Constructing commands registers metadata only. Neither constructors nor help renderers may:

- call `config.Load` or read a config/CA file;
- resolve or dial `CERBIX_URL`;
- open PostgreSQL or RabbitMQ;
- initialize runtime services, signals, migrations, ciphers, dispatchers, schedulers, workers, or agents;
- require `CERBIX_URL`, `CERBIX_TOKEN`, `CERBIX_CA_FILE`, or any production environment.

Tests invoke every help path with absent credentials, nonexistent config/CA paths, and no live server or
database. Remote-help tests also use a counting HTTP stub and require zero requests.

## 9. Exit-code ownership

Cobra never calls `os.Exit`. The only process exit remains:

```go
func main() {
    os.Exit(cli.Main(os.Args[1:]))
}
```

`Main(args []string) int` remains the public package entrypoint. It:

1. builds the root command with process stdout/stderr;
2. sets `args`;
3. executes the command;
4. maps the returned error to an application-owned exit code.

A typed error carries at least the exit code, underlying error, and whether a diagnostic was already
printed. Exact names are implementation details, but there is one centralized mapping. Cobra parse,
unknown-command, unknown-subcommand, missing-required, and argument errors map to usage exit `2`.
Runtime executors return typed exit errors rather than integers or process exits.

Semantic mappings remain:

| Command/outcome | Exit |
| --- | ---: |
| root/group/leaf explicit help | 0 |
| valid `version` | 0 |
| successful serve/migrate/reencrypt/recovery command | 0 |
| config, database, broker, key, runtime, migration, adoption, or enqueue failure | 1 |
| CLI usage/validation failure | 2 |
| `gate check`: ALLOW or WARN, including UNKNOWN whose configured action is ALLOW/WARN | 0 |
| `gate check`: transport, timeout, TLS, auth, server, redirect, 429, or malformed response | 1 |
| `gate check`: BLOCK, including UNKNOWN whose configured action is BLOCK | 2 |
| `gate check`: NOT_CONFIGURED | 4 |
| `change record`: recorded or replayed | 0 |
| `change record`: transport, timeout, TLS, auth, redirect, 429, 5xx, or malformed response | 1 |
| `change record`: 400/404/409 contract refusal | 2 |

The response/status classification functions remain independent of Cobra so HTTP stub tests can exercise
every semantic class directly.

## 10. Compatibility matrix

### 10.1 Characterization baseline

The pre-Cobra tree at `f6320fc1777e3ecd7201b06c21df64f2df936a12` passed:

```text
go test ./... -count=1
```

including `internal/cli`; DB-gated cases skipped normally because `CERBIX_TEST_DATABASE_DSN` was absent.
A separately built baseline binary was then run against captured stdout/stderr. The observed interface is:

| Invocation/class | Baseline | Target |
| --- | --- | --- |
| `cerbix` | exit 2; root usage on stderr; no runtime access | exit 2; new root catalogue on stderr; no runtime access |
| `cerbix -h`, `--help`, `help` | exit 0; flat root usage on stdout | exit 0; grouped root catalogue on stdout |
| `cerbix help serve` | exit 0; incorrectly prints root usage | exit 0; detailed `serve` help |
| runtime/recovery leaf `-h` / `--help` | exit 2; stdlib flag usage on stderr | exit 0; detailed help on stdout |
| `gate --help`, `change --help` | exit 2; treated as unknown subcommand | exit 0; group help on stdout |
| `gate check --help`, `change record --help` | exit 2; stdlib flag usage on stderr | exit 0; detailed help on stdout |
| `version --help` | exit 0; incorrectly executes version JSON | exit 0; detailed version help; no version execution |
| unknown top-level command | exit 2; diagnostic + root usage on stderr | exit 2; diagnostic + new root catalogue on stderr; no suggestions |
| unknown nested subcommand | exit 2; diagnostic + group usage on stderr | exit 2; diagnostic + group usage on stderr; no suggestions |
| unknown/extra `help` path | exit 0; ignored every supplied help path and printed root usage | exit 2; `help: unknown command path` on stderr; no runtime access |
| unknown flag / invalid duration on flag-owning commands | exit 2; diagnostic/usage on stderr; no runtime access | exit 2; deterministic diagnostic on stderr; no runtime access |
| root flag before top-level command; group/leaf flag before nested command | exit `2` as unknown command/subcommand before side effects | same canonical command-position boundary, before Cobra traversal or runtime access |
| any parsed `--help=false` / `-h=false` on root, groups, side-effect leaves, or `help`, including repeated/conflicting help flags | exit `2` before side effects | any-false-wins usage exit `2`, empty stdout, zero executor/config/HTTP calls; string values are excluded |
| `version --bogus`; version false-help forms without a positional | exit `0`; version JSON | unchanged through a Cobra flag-error compatibility seam; false-help is a no-op, while any parseable true-help wins regardless of order |
| version unknown/false-help/true-help forms followed by a positional | baseline ignored some positionals | positional-first exit `2`, empty stdout, encoder not called, per §10.2 |
| compound `help <path>` plus true help | old manual help ignored path details | idempotent root/path help, stdout only, writer failures exit `1` |
| missing required flag | exit 2; command diagnostic on stderr; no runtime access | exit 2; command diagnostic on stderr; no runtime access |
| invalid role/month/RFC3339/range/positive timeout | exit 2 before config/network | same semantic class and side-effect boundary |
| gate/change valid parse with missing/bad environment | exit 1; environment diagnostic; no request where validation fails | same |
| gate/change valid request path | existing one-request, no-redirect/no-retry behavior | unchanged |
| runtime/recovery valid parse with missing config | exit 1 from existing config loader | unchanged |
| normal gate/change stdout/stderr and JSON | current summary/reason/raw-body grammar | byte/line compatible |
| gate/change semantic HTTP/action mapping | existing 0/1/2/4 and 0/1/2 maps | unchanged |
| public completion command | absent | absent |
| hidden `__complete` / `__completeNoDesc` protocol command position | absent | rejected with exit `2` before `Execute`; no protocol stdout/directive stderr |
| Windows Explorer lifecycle | application parser returned an exit code | Cobra mousetrap disabled; `Main(args) int` remains the exit owner |
| implicit root version flag | absent | absent |

### 10.2 Intentional positional-argument tightening

The standard-library implementation explicitly rejects positional arguments only for `gate check` and
`change record`. After a valid flag set, `serve`, `migrate`, `reencrypt`, `adopt-fact-month`, and
`enqueue-service-repair` silently ignore extra positional tokens and proceed to config/runtime access;
`version extra` prints version JSON and exits `0`. This is an unsafe parser accident, not documented
syntax.

The Cobra target gives every leaf explicit `NoArgs` validation. Every unexpected positional argument
therefore exits `2` on stderr before config, network, database, or runtime access. This remains the only
intentional non-help compatibility tightening introduced by the design. The corrective canonical
command-position boundary is not a new syntax change: it restores the measured pre-Cobra rule that flags
cannot precede the command or nested command name.

Documented command paths, canonical `--flag` names, types, defaults, required/optional meaning,
environment variables, stdout/stderr contracts, and semantic result exit codes otherwise remain
unchanged. The positional rejection above is stated beside every compatibility claim and is not hidden
behind an unqualified “all CLI contracts remain unchanged” assertion.

## 11. Security boundaries

`gate check` and `change record` preserve the existing remote-client boundary:

- `CERBIX_URL` is the plain HTTP(S) base URL and may not contain credentials, query, or fragment;
- `CERBIX_TOKEN` is environment-only and appears only in the bearer header;
- `CERBIX_CA_FILE` optionally appends PEM roots to the system pool;
- TLS verification remains enabled with minimum TLS 1.2;
- redirects are not followed;
- no retry is added;
- no help, error, example, or completion surface prints a token;
- gate has no `--url`; change's existing `--url` remains the change body link, never the server target;
- neither command gains `--token`, `--insecure`, skip-verify, credential config, or Viper binding.

Tests keep the existing token-never-printed, TLS, CA-file, redirect, path-escaping, one-request, and
no-retry coverage and add help-side-effect assertions.

## 12. Required test strategy

Implementation follows red/green/refactor in bounded phases. Production code does not change until the
owner approves this specification.

### Phase A — characterization on the old parser

Before adding Cobra:

- add table-driven tests for root, group, leaf, error, stream, and exit behavior;
- preserve the baseline behavior that remains contractual;
- mark the help and positional rows in §10 as intentional target changes rather than silently rewriting
  expectations;
- prove parse/validation failures occur before config/network access;
- run `go test ./internal/cli/... -count=1` and record GREEN in iter-0196.

### Phase B — root RED/GREEN

- add failing golden/contract tests for the grouped root catalogue;
- prove RED against the standard-library root;
- add Cobra v1.10.2 and `newRootCommand`;
- disable completion, implicit version, sorting drift, suggestions, automatic usage/errors;
- make root help GREEN without migrating runtime leaves wholesale.

### Phase C — runtime and recovery leaves

Migrate one command at a time in this order: `version`, `migrate`, `reencrypt`, `adopt-fact-month`,
`enqueue-service-repair`, `serve`. For each:

1. add failing `-h` / `--help`, usage, required/optional/default/example, no-side-effect, and argument tests;
2. register flags once in Cobra;
3. extract typed options and a parse-independent executor;
4. run focused compatibility tests;
5. return to GREEN before the next command.

The positional tightening has an explicit table-driven contract for every affected leaf:

| Invocation | Required assertions |
| --- | --- |
| `cerbix serve extra` | exit `2`; stderr names `unexpected argument`; stdout empty; config loader and serve executor not called |
| `cerbix migrate extra` | exit `2`; stderr names `unexpected argument`; stdout empty; config loader and migration executor not called |
| `cerbix reencrypt extra` | exit `2`; stderr names `unexpected argument`; stdout empty; config loader and re-encryption executor not called |
| `cerbix adopt-fact-month extra` | exit `2`; stderr names `unexpected argument`; stdout empty; config loader, database, and adoption executor not called |
| `cerbix enqueue-service-repair extra` | exit `2`; stderr names `unexpected argument`; stdout empty; config loader, database, and repair executor not called |
| `cerbix version extra` | exit `2`; stderr names `unexpected argument`; stdout empty; version encoder not called |

Each YAML-backed command also has an otherwise-valid-flags-plus-`extra` case, proving that argument
validation still wins before the supplied config path can be read. The executor/config seams use counting
fakes or injected functions; an absent output alone is not accepted as proof that no side effect was
attempted.

Existing DB-backed recovery command tests remain behind `CERBIX_TEST_DATABASE_DSN` and continue to test
the real artifact when a dedicated test database is supplied.

### Phase D — nested remote commands

Migrate `gate check`, then `change record`. Existing `httptest` servers continue to prove every semantic
exit class, stream, raw JSON path, replay, TLS/CA, redirect, timeout, and no-retry behavior. Add parent
help, nested unknown-command, required metadata, environment-only credential, and help-zero-request tests.
The existing positional behavior remains GREEN for both `cerbix gate check extra` and
`cerbix change record extra`: exit `2`, an `unexpected argument` diagnostic on stderr, empty stdout, and
zero HTTP requests.

Mutation checks must demonstrate that tests fail when:

- gate BLOCK maps away from `2`;
- gate NOT_CONFIGURED maps away from `4`;
- change 409 maps away from `2`;
- recorded/replayed maps away from `0`;
- an env credential is replaced by a flag;
- help invokes a runtime executor or reaches a counting HTTP stub.

### Phase E — removal

Only after every command is GREEN, remove the manual switch, native `flag.FlagSet` parsers, manual
nested dispatchers, duplicated usage constants, and obsolete tests. The final source guard is:

```text
rg -n 'flag\.NewFlagSet|flag\.(String|Bool|Duration|Int|Var)' internal/cli
```

It must return no native CLI parsing. Any remaining `flag` import requires a written explanation; the
expected result is no such import.

### Complete minimum matrix

Tests cover:

- root no args and post-`--` inputs; canonical raw command position for top-level and nested commands; all
  root help aliases; exact tree-derived leaf SET/group ownership; stable grouping/order; no public or hidden
  completion protocol; no implicit version flag; no suggestions; Windows mousetrap disabled;
- root/group flags before command names, `gate --help`, `change --help`, unknown/misspelled nested
  subcommands (with help or leaf flags), invalid/extra `help` paths, and positional-before-help paths failing
  with exit `2` rather than rendering help or reaching an executor;
- every leaf `-h` and `--help`; all exact/assignment true/false/invalid boolean forms; repeated/conflicting
  false→true, true→false, false→false, long→shorthand, and shorthand→long forms; any-false-wins rejection on
  every ordinary side-effect path; version false-help no-op, order-independent true-help, unknown+true-help,
  and positional-first matrices; metadata-derived synopsis, required/optional sections, placeholders and
  defaults; shell-safe examples; no runtime side effects; in-memory metadata mutations must update all
  rendered help; string-flag values equal to `--help`/`--help=false`/`-h`/`-h=false`/`--`/`__complete`/
  `__completeNoDesc` must execute normally, including a later real help token; exact and assignment help
  forms must map failing stdout writers to the same typed exit `1`;
- compound `help --help` and `help <root|group|leaf path> --help` exact/assignment/shorthand forms, including
  typed writer failures and false-help usage errors;
- unknown flags; missing required flags; the six explicit positional-tightening cases and two preserved
  gate/change positional cases above; invalid durations; role/month/RFC3339/range validation;
- every positional case asserts exit, readable diagnostic, empty stdout, and the applicable zero-call
  config/network/database/runtime boundary; adoption also covers duration-addition overflow before config;
- root/group catalogue tests prove local metadata order without global Cobra command-sorting mutation and
  exclude hidden/unavailable commands; the separate intentional process-global mousetrap assignment is
  guarded independently;
- stdout/stderr separation and runtime errors without automatic usage;
- every gate/change semantic exit code through HTTP stubs;
- environment-only credentials, CA file, TLS verification, no bypass, raw JSON, replay, and redaction of
  credentials embedded in an invalid `CERBIX_URL` diagnostic;
- config/runtime failures for YAML-backed commands;
- source/dependency guards for the sole parser, no direct pflag import, absent Viper/forbidden flags/public
  completion command, and a clean `go mod tidy -diff`.

Tests are hermetic by default and never use production resources.

## 13. Documentation and delivery records

After working implementation exists, synchronize:

- `README.md` and `docs/overview.md` with structured help discoverability;
- this specification and `docs/specs/README.md` with implementation/review evidence;
- `docs/status.md` and `docs/traceability.md` with iter-0196 evidence;
- `docs/decisions.md` with D-0265;
- the iter-0196 report with characterization, RED/GREEN, dependency, compatibility, verification, review,
  skips, rollback, and owner sign-off;
- the top `## [v0.3.5] - Unreleased` section of `CHANGELOG.md` only after implementation works.

After working implementation exists, the Unreleased `Added` entry is:

> **Structured Cobra CLI.** The command tree now provides grouped top-level help and detailed
> command-specific help. Documented commands, flags, defaults, environment variables and semantic exit
> codes remain unchanged. Leaf commands now reject previously ignored positional arguments with usage
> exit code 2 before loading configuration or performing side effects. Viper is not used.

This entry does not precede working code.

D-0265 records the architectural ownership boundary:

- Cobra owns parsing, help, flags, args, and the tree;
- `internal/config` remains the config owner;
- Viper is excluded;
- exit codes remain application-owned;
- no completion command is exposed;
- environment-only credential and TLS boundaries remain intact.

## 14. Verification and acceptance invariants

### 14.1 Required commands

Final verification runs:

```text
go test ./internal/cli/... -count=1
go build -buildvcs=false ./...
GOOS=windows GOARCH=amd64 go build -o /tmp/cerbix-windows.exe ./cmd/cerbix
go vet ./...
go test -race -count=1 -timeout 40m ./...
make docs-check
git diff --check
```

It builds a temporary Linux binary and manually checks root, alias, group, every leaf, nested/compound
help, canonical command-position failures, false-help forms, version compatibility, and hidden completion
protocol rejection with exact process exit/stdout/stderr capture. Help must pass without config, PostgreSQL,
RabbitMQ, API server, credentials, or production environment; temporary binaries are removed afterward.
Dependency guards run `go list -m all`, `go mod why` for Cobra, pflag, and Viper,
`go mod tidy -diff`, and automated/source scans for native flag parsing, direct pflag/Viper imports, and
forbidden flags.

### 14.2 Acceptance invariants

1. Cobra is the only command tree, subcommand dispatcher, argument parser, flag parser, and help owner.
2. The public hierarchy is exactly the tree in §2; no public completion command, reachable hidden
   `__complete`/`__completeNoDesc` protocol command, or implicit version flag exists.
3. Every available runnable leaf appears once in the root catalogue, hidden/unavailable commands are absent,
   and deterministic local metadata order never mutates Cobra's global sorting setting.
4. Every group and leaf has command-specific `Use`, `Short`, `Long` where applicable, examples, argument
   validation, and help.
5. Flags are registered once; required/default help derives from the registered flag metadata.
6. `Main(args []string) int` remains public and `cmd/cerbix/main.go` remains the sole application-owned
   `os.Exit` site; `cobra.MousetrapHelpText` is empty before every `Execute`.
7. Cobra uses `SilenceUsage: true`, `SilenceErrors: true`, explicit output/error writers, no suggestion
   text, and no process-global sorting mutation.
8. Exact valid help paths exit `0` on stdout and reach no config, file, network, database, broker, or
   runtime mechanism; positional/nested-command validation outranks help; help detection does not steal
   string values equal to `-h`/`-h=false`/`--help`/`--help=false`/`--`; compound help is idempotent;
   any parsed false-help assignment wins for ordinary commands regardless of repeated flag order, while
   version false-help is a no-op and any parseable true-help wins; unknown/extra help paths fail with exit `2`;
   and help writer errors share typed exit `1` for exact/assignment/shorthand `--help` and `help` syntaxes.
9. Root/group no-argument, post-`--`, flag-before-command, and all non-help usage failures keep application
   usage exit `2` and occur before executor/config/network/database/broker access.
10. Documented command paths, canonical flags, types, defaults, requiredness, environment names,
    stdout/stderr contracts, semantic exit codes, and config precedence remain unchanged.
11. `serve`, `migrate`, `reencrypt`, `adopt-fact-month`, `enqueue-service-repair`, and `version` reject
    every unexpected positional with exit `2`, a readable stderr diagnostic, empty stdout, and no config,
    database, network, or runtime executor call; `gate check` and `change record` keep that existing behavior;
    adoption refuses timeout-addition overflow before config/DB access.
12. Gate exit 0/1/2/4 and change exit 0/1/2 mappings are fully exercised and unchanged.
13. Gate/change successful and JSON stdout, reason/refusal/runtime stderr, raw response bytes, replay,
    redirect, TLS, CA, timeout, and no-retry behavior remain unchanged.
14. Credentials remain environment-only; no `--token`, remote `--url`, `--insecure`, or TLS bypass exists,
    and rejected `CERBIX_URL` values never echo embedded userinfo/query credentials.
15. Cobra v1.10.2 is the sole new direct dependency; pflag remains transitive, Viper is absent, and the
    module graph is tidy.
16. Native standard-library flag parsing and duplicate usage strings are absent from `internal/cli`.
17. README, overview, spec/index, status, traceability, decision, iteration, and Unreleased changelog text
    describe the final code and its verified evidence, including the positional tightening.
18. No API/OpenAPI, config schema, database schema/migration, runtime role, frontend, generated SPA,
    monitoring/alert, Docker runtime, or production resource changes occur.
19. Independent scoped review is APPROVED with no open Important finding before commit.
20. No commit is created before separate owner approval; no push, PR, merge, deploy, or restart occurs.

## 15. Rollback

The migration is one interface-layer change with no data migration. Before a commit, rollback is simply
discarding the worktree changes. After an owner-authorized local commit, rollback is a source revert of
that commit: restore the manual/native parser files and tests, remove Cobra from `go.mod`/`go.sum`, and
restore the prior CLI documentation. No database, API, config, frontend, or persisted-data rollback is
required.

If a partial implementation cannot preserve a command's semantic exit/output contract, work stops at
the last GREEN phase. The old parser is not removed until every leaf and nested command is GREEN, so the
migration never relies on an unreviewable all-at-once cutover.
