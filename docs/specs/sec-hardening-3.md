# sec-hardening-3 — the PostgreSQL probe's TLS and connection string, redirects and secret URLs on alert delivery, and headers in alert mail

> **STATUS: APPROVED, revision 7 (2026-11-15).** The owner approved revision 7 (`e3196e8`) on
> 2026-11-15, together with the review's P3-4 (§10.5).
> **Implemented** in [iter-0213](../iterations/iter-0213.md) / [D-0280](../decisions.md).
> - **Review of revision 2:** CHANGES REQUIRED, 0 P0 / 4 P1 / 5 P2 / 2 P3. The owner ruled on each
>   remark (§10.1).
> - **Revision 4:** recorded the owner's answers to revision 3's questions (§10.2) and reconciled
>   the reviewer's P2-5 test list (§10.3).
> - **Review of revision 4:** CHANGES REQUIRED, 0 P0 / 3 P1 / 5 P2 / 1 P3. Revision 5 answers it
>   (§10.4):
>   - five remarks are rejected as not matching the text;
>   - the rest are applied;
>   - checking one of them turned up T-5.
> - **Revision 6:** records the owner's decision on revision 5's question. `mail.from` is checked
>   at startup (§8 item 6, §4.2), with its test and upgrade note.
> - **Review of revision 6:** CHANGES REQUIRED, 0 P0 / 0 P1 / 1 P2 / 3 P3. All four remarks were
>   accepted and are applied in revision 7 (§10.5).
> - **§11:** empty.
> - **Implemented** in one iteration (iter-0213 / D-0280).
> - **Code review of the branch** (at `6a90e5a`): CHANGES REQUIRED, 0 P0 / 1 P1 / 2 P2 / 1 P3; all
>   four accepted and applied, one part of P1-1's recommendation rejected (§10.6). Second pass (at
>   `1723920`): 0/0/1/1, both accepted and applied. Final pass (at `7e3bda6`): **APPROVED** (§10.6).
>
> **Origin.** iter-0210 recorded two items under "Found on the way, left alone" while implementing
> [`sec-hardening-2.md`](sec-hardening-2.md) H-2 and H-13 ([iter-0210](../iterations/iter-0210.md)).
> - Re-verifying the first one against the code turned up a worse defect underneath it: the probe
>   does not apply `sslmode` at all (T-1a).
> - T-3 was found while writing revision 1 and taken in by the owner's ruling (§8).
> - T-1c and T-1d were found while answering the review of revision 2 (§10). T-4 was found then too,
>   and the owner took it in on 2026-11-15 (§10.2).
> - T-5 was found while reproducing remark P1-3 of the review of revision 4 (§10.4).
>
> **How the claims were checked.**
> - Every claim below was checked by reading the code.
> - Claims marked *(run)* were also checked by running the code (§10 lists the runs). None of those
>   runs touched the dev database.
>
> By the precedent of [`func-hardening.md`](func-hardening.md) and `sec-hardening-2.md`, this package
> carries **no requirement number**.
>
> **SPA surface: none.**
> - The `sslmode` select in the monitor editor and the channel, webhook, subscribe and mail-settings
>   forms stay as they are.
> - Probe results and delivery errors carry new messages, and a mail subject is RFC 2047-encoded.
> - The subscribe form, the email channel form and Settings → Mail can now answer 400 for an address
>   the API accepted before (§4.2). They show it the way they show any API error today.
>
> **No migration.** One iteration: report iter-0213, decision D-0280.

## 1. Findings, ranked

| # | Sev | Finding | Where |
| --- | --- | --- | --- |
| T-1a | P1 | The PostgreSQL probe never asks for TLS: `require`, `verify-ca` and `verify-full` connect in plaintext and verify nothing | `pgProber.Probe` in `internal/prober/postgres.go` (before iter-0213) |
| T-1b | P2 | `sslmode` is concatenated into a libpq connection string and the probe does not check it again, so a stored value can add `host=`, `sslrootcert=`, `service=` and other keys. The process environment and the prober user's files also reach every probe | `internal/prober/postgres.go:34-38` (at `7744e95`) |
| T-1c | P1 | On a body-bound job (envelope v2), deleting `sslmode` (postgres) or `tls` (mysql, redis, rabbitmq management) keeps the envelope valid. The digest binds the absent key as its canonical default, but the prober reads absence as "no TLS" (`prefer` for postgres). A carrier writer downgrades the probe and the credential goes out in plaintext | `internal/domain/credsettings.go:573-580`, `internal/dispatch/execbinding.go:63-66`, `internal/prober/mysql.go:79`, `internal/prober/redis.go:39`, `internal/domain/targets.go:165` |
| T-1d | P2 | A partial monitor update that carries no `config` still re-validates and rewrites the stored one. A row with no `sslmode` is silently written `require`; a row with `prefer` or a beta-era value answers 500 | `internal/store/monitors.go:766-775` (at `7744e95`) |
| T-5 | P2 | A partial update of a synthetic monitor is refused with 400 `synthetic: config.scenario is required`: the handler validates the monitor it read without the writer-only scenario. The SPA's Pause/Resume button hits it. A correctness defect, found while checking T-1d | `internal/api/handlers_monitors.go:42`, `:724` |
| T-2 | P2 | Alert delivery follows redirects with the library's policy. On 307/308 it sends the incident body and its HMAC signature to another host. On any redirect it sends the previous URL, token included, in `Referer`. A 301/302/303 counts as delivered although the body was dropped. The pull agent's client does the same with its `Authorization` header | `internal/prober/guard.go:167-181` (at `7744e95`), `internal/notify/notify.go:46-49` (at `7744e95`), `internal/notify/notify.go:255-272`, `webhook.New` (`internal/webhook/webhook.go`), `webhook.Dispatcher.post` (`internal/webhook/webhook.go`), `internal/agent/agent.go:97` (at `7744e95`) |
| T-3 | P2 | A mail subject is written into the message as is. Any name or title that reaches it with CR/LF adds headers or replaces the body: a monitor, service or project name, an incident title (an Alertmanager `summary` among them) or a status-page title. The mail goes from the instance's own sender to channel recipients and public status-page subscribers. Every scheduled SLA report already puts its raw line breaks into the subject | `notify.Dispatcher.sendMail` (before iter-0213), `internal/mailer/mailer.go:249-254` (at `7744e95`) |
| T-4 | P2 | A delivery that fails before a response quotes its full URL in the error text: a Telegram bot token, a Slack webhook path, a tokened webhook URL. The API treats those values as write-only, yet the text is stored in the outbox's `last_error`, written to the log, and served by the dead-letter API | `internal/notify/notify.go:239`, `:244`, `internal/webhook/webhook.go:80`, `:87` (these four at `7744e95`), `internal/outbox/outbox.go:324-345` |

**Severity scale** (`sec-hardening-2`'s):
- **P0** — an authentication or tenant boundary is crossed.
- **P1** — a realistic attacker gains a capability they do not hold.
- **P2** — defence in depth.

**T-1a is P1.**
- An attacker on the path between a prober and a monitored database reads the probe's user name,
  database and query in clear.
- By answering the startup with a cleartext-password request, which pgx honours, the attacker also
  gets the password.
- This holds for a monitor configured `verify-full`, whose whole point is to rule this out.

**T-1c is P1**, the severity of the review remark P1-1 (owner's ruling, 2026-11-15): the defect and
the fix are the same.
- Revision 3 proposed P2, because the attack needs two positions: write access to a job carrier
  (broker publish rights, or the pull channel) *and* a position on the prober → target path.
- It breaks a property the envelope states. `dispatch.SealContext` says the digest keeps a credential
  from being replayed "against a different target, type or transport"
  (`internal/dispatch/credentials.go:120-122`).

The invariants continue `sec-hardening-2`'s numbering (S1–S14), so a citation or test name is unique
across both packages.

## 2. T-1 — the PostgreSQL probe

### 2.1 T-1a: `sslmode` is not applied

**Code.** `pgProber.Probe` (`internal/prober/postgres.go:30-63` at `7744e95`):

```go
cfg, err := pgx.ParseConfig("sslmode=" + sslmode)   // :38
cfg.Host = host                                      // :42
cfg.Port = port
```

`pgx.ParseConfig` (pgx v5.10.0, `pgconn.ParseConfigWithOptions`) computes the TLS configuration **for
the host it parses**:
- The string names no host, so that host is libpq's default, `pgconn.defaultHost()`. It returns the
  first of `/var/run/postgresql`, `/private/tmp` or `/tmp` that exists, and `localhost` only when none
  does.
- A directory is a unix socket. For a unix socket `ParseConfigWithOptions` stores a **nil**
  `TLSConfig` ("Ignore TLS settings if Unix domain socket like libpq").
- The probe then replaces `Host` and `Port` with the target's and leaves `TLSConfig` alone.

So it connects to the target with a TLS configuration that was built for a local socket: none.

**Reproduction** *(run)*. The probe's construction was run inside
`gcr.io/distroless/static-debian12:nonroot`, the runtime stage of `Dockerfile:32`:
- `ParseConfig("sslmode=" + mode)`;
- `Host`/`Port`/`User`/`Database`/`Password` overwritten as the probe does;
- the first message sent to a local listener recorded.

The image has `/tmp`:

```
sslmode=""            default host "/tmp"  TLSConfig=false  first message: StartupMessage IN PLAINTEXT
sslmode="prefer"      default host "/tmp"  TLSConfig=false  first message: StartupMessage IN PLAINTEXT
sslmode="disable"     default host "/tmp"  TLSConfig=false  first message: StartupMessage IN PLAINTEXT
sslmode="require"     default host "/tmp"  TLSConfig=false  first message: StartupMessage IN PLAINTEXT
sslmode="verify-ca"   default host "/tmp"  TLSConfig=false  first message: StartupMessage IN PLAINTEXT
sslmode="verify-full" default host "/tmp"  TLSConfig=false  first message: StartupMessage IN PLAINTEXT
```

The same holds on macOS (`/private/tmp`) and on any Linux host. A correct client sends an `SSLRequest`
(code 80877103) first for every mode but `disable`.

**The suite already depends on the defect.**
- `e2e/tests/probers.spec.ts:49-56` (at `7744e95`) test-connects a postgres monitor to the dev stack's Postgres
  without an `sslmode`.
- The test-connection handler normalizes the missing value to `require`
  (`internal/api/handlers_monitors.go:269`, through `domain.PrepareCredentialSettings` and the schema
  default at `internal/domain/credsettings.go:203`).
- The dev Postgres runs with `ssl = off`.
- The test asserts `up: true` and passes, which is possible only because no TLS is asked for.

**Since when.** The construction has not changed since the first commit. So no released version has
ever applied `sslmode`: not v0.1.0-beta.1 (2026-08-11), not v0.3.9 (2026-11-03), nor anything between
them, and not `main` up to `58b5a32`.
- FR-020 C6 made `require` the default (`a724df8`, 2026-08-14, first tagged in **v0.1.5-beta.1**).
  Since then, every postgres monitor written through the API, the UI or a monitoring-as-code file is
  normalized to `require` unless it says otherwise.
- The spec states that default as "encrypted, server identity NOT verified" (`func-secret-inventory.md`
  §4.2). In practice none of it has been encrypted. §9 lists the living documents corrected
  accordingly.

**Who is exposed.**
- **Passive observer on the prober → database path:** sees the user name, the database, the query
  text and its results.
- **Password:** depends on the server's authentication method. SCRAM does not reveal it to a passive
  observer.
- **Active attacker on that path:** answers with `AuthenticationCleartextPassword` and receives the
  password, because pgx sends it. `verify-full` would have refused the attacker's certificate, but no
  certificate is ever asked for.
- **Shared code:** the worker and the pull agent run the same `pgProber`, so every region is affected.

### 2.2 T-1b: the value is concatenated, and the environment leaks in

**What reaches the probe.** In every role, `m.Config["sslmode"]` comes from a stored monitor row. It
reaches the executor without a second check:
- through the in-process dispatcher, for role `all`;
- through the AMQP job, for `worker`;
- through the pull job, for `agent`.

`dispatch.ValidateAndMaterialize` (`internal/dispatch/credentials.go:442`) checks the credential field
set and the envelope, not the value of `sslmode`.

The values that can be stored:

| Source | Values | Checked by |
| --- | --- | --- |
| API create, test-connection and update with a `config` | `disable`, `require`, `verify-ca`, `verify-full`; absent → `require` | `domain.PrepareCredentialSettings` (`internal/domain/credsettings.go`), from `internal/api/handlers_monitors.go:269`, `:435`, `:701` and the store's own writers `internal/store/monitors.go:474`, `:644` |
| API update **without** a `config` (T-1d) | the stored value, re-validated anyway: absent → `require` is written; `prefer` or a beta value → 500 | `internal/store/monitors.go:768-775` (at `7744e95`) |
| Monitoring-as-code files | the same four | `domain.PrepareTypedSettings` → `PrepareCredentialSettings` (`internal/fileprovider/parse.go:468`) |
| Rows written by v0.1.0-beta.1 … beta.5, before `a724df8` | **any string** (the API took a free-form map). The beta UI offered `disable`, `prefer` (its default), `require` and `verify-full` | nothing. No migration rewrote them. An update that carries a `config` re-validates, so a row keeps its value until someone edits its settings |
| Absent key on such a row | `""` → the probe uses `prefer` (`postgres.go:60-62`) | — |
| A generation-1 AMQP carrier | whatever the message says | the worker trusts the broker. A forged message needs broker publish rights |
| A pull job | whatever core sends | the agent trusts core over HTTPS with its token |

So today a non-enum value needs a beta-era row, a direct database write or a forged carrier. That
reach is why T-1b is P2 and not higher.

**What a value can do.** `ParseConfig` reads keyword/value pairs separated by spaces, so a value with a
space adds keys. Behaviour observed with pgx v5.10.0:

| Value | Effect |
| --- | --- |
| `require host=a,203.0.113.7` | the probe overwrites `Host`, but `Fallbacks` keeps `203.0.113.7:5432`. When the target fails, pgx connects there and sends the monitor's user and password. The dial still goes through the SSRF guard (`DialFunc`) |
| `require port=1,6543 host=a,203.0.113.7` | the same, with a chosen port |
| `require sslrootcert=/path` | reads a file on the prober host. The probe message (`"bad config: " + err`) tells "unable to read CA file: open …: no such file" apart from "unable to add CA to cert pool". That is a file-existence oracle, shown to everyone who can read the monitor |
| `require sslcert=/a sslkey=/b` | reads a key pair from the prober's disk and presents the certificate to the target |
| `require service=x servicefile=/path` | reads connection settings (host, port, sslmode) from a file on the prober host |
| `require options='-c …' application_name=x` | arbitrary run-time parameters in the startup message |
| `require target_session_attrs=read-write`, `connect_timeout=1`, `passfile=/path` | change validation, timeout and password lookup |
| `require sslmode=disable` | the last key wins |

**The environment, with no stored value at all.** `ParseConfigWithOptions` merges `defaultSettings()`
and `parseEnvSettings()` before the string. So these reach every probe:
- `PGHOST`, `PGPORT`, `PGOPTIONS`, `PGAPPNAME`, `PGTZ`, `PGTARGETSESSIONATTRS`, `PGSSLROOTCERT`,
  `PGSSLCERT`, `PGSSLKEY`, `PGSSLNEGOTIATION`, `PGSERVICE` and `PGSERVICEFILE`;
- `~/.pgpass`, `~/.pg_service.conf` and `~/.postgresql/{postgresql.crt,postgresql.key,root.crt}` of the
  prober's user.

If cerbix's own database client certificate lives in `~/.postgresql/` on a host, the probe presents it
to every monitored Postgres. The distroless image has none of these files and sets none of these
variables; a systemd install may have both.

### 2.3 T-1c: an absent defaulted key on a body-bound job

**The two readings of one absent key.**
- **Digest.** Envelope v2 binds the job's execution body (`dispatch.ExecutionBodyDigest`,
  `internal/dispatch/execbinding.go:35`). An absent key enters that digest as its schema default:
  `domain.CanonicalSettingValue` returns `def` when the key is missing (`internal/domain/credsettings.go:573-580`),
  and "absent and explicitly-set-to-default encode identically" is a stated rule
  (`execbinding.go:63-66`, `func-secret-inventory.md` §4.7).
- **Prober.** The prober reads the same absence differently:

| Type | Key | Canonical default | What the prober does when the key is absent |
| --- | --- | --- | --- |
| postgres | `sslmode` | `require` | `prefer` (`internal/prober/postgres.go:60-62`); after the T-1a fix that means plaintext when TLS fails |
| mysql | `tls` | `true` | no TLS (`internal/prober/mysql.go:79`) |
| redis | `tls` | `true` | no TLS (`internal/prober/redis.go:39`) |
| rabbitmq, management | `tls` | `true` | `http://` to a socket target (`domain.RabbitMgmtTLS`, `internal/domain/targets.go:165`) |
| postgres, mysql | `query` | `SELECT 1` | `SELECT 1`: the same |
| rabbitmq, management | `path` | `/api/overview` | `/api/overview`: the same |

**The downgrade** *(run)*. An envelope v2 was sealed over a full body on carrier 3 and opened by
`ValidateAndMaterialize`:

| Body as delivered | postgres (`sslmode`) | mysql (`tls`) | redis (`tls`) |
| --- | --- | --- | --- |
| as sealed (`require` / `true`) | accepted | accepted | accepted |
| key deleted | **accepted**, key absent, password materialized | **accepted**, the same | **accepted**, the same |
| key set to `disable` / `false` | refused: AEAD fails | refused | refused |

Changing the value is caught; deleting it is not.
- **mysql, redis and rabbitmq management:** this already sends the password in plaintext today.
- **postgres:** everything is plaintext today, so the deletion changes nothing yet. After the T-1a fix
  it would turn `require` into `prefer`, and the probe would fall back to plaintext.

**Where an absent key comes from legitimately.** Before the fix, the executor cannot distinguish a
deleted key from a row that never had one. Two kinds of row lack one:

- **A row with `password_ref` and no `sslmode`.** No released write path produces it.
  - `password_ref` arrived in FR-020 C3 (`6a94a89`). The `require` default, materialized on every
    write, arrived in FR-020 C6 (`a724df8`, `1b9324b`). Both first shipped in v0.1.5-beta.1.
  - Every writer that accepts a `password_ref` runs `PrepareCredentialSettings`, which writes
    `sslmode`: API create, update with a `config`, and test-connection, plus monitoring-as-code.
  - The partial update of T-1d writes `require`, not absence.
  - Such a row exists only if someone wrote the database directly.
- **A row with an inline password and no `sslmode`** (or no `tls`): rows from v0.1.0-beta.1 … beta.5.
  - The path *(run)*: `MaterializeExecutionConfigs` decrypts the inline ciphertext
    (`internal/store/materialize.go:329-337`) and seals it into an envelope
    (`internal/store/materialize.go:357`): v1 on carrier 2, v2 on carriers 3 and 4.
  - The body is the stored config as it is. A beta row was observed going out on carrier 3 with
    envelope v2 and no `sslmode` in the body.

So a rule that refuses absence on envelope v2 also refuses legitimate beta-era rows, unless core
states the key before it seals. §2.6 item 5 does both.

### 2.4 T-1d and T-5: a partial update rewrites or refuses the stored config

`PATCH /monitors/{id}` with no `config` member is meant to change other fields only. The SPA's
Pause/Resume button sends exactly that, `{"enabled": …}` (`frontend/src/views/MonitorDetailView.vue:728-740`).

- **Handler.**
  - It reads the monitor through the safe reader: `monitorAccess` → `GetMonitor`
    (`internal/api/handlers_monitors.go:42`). That reader drops the write-only password and the
    writer-only `scenario`.
  - It runs the settings preparation (`PrepareCredentialSettings` / `PrepareTypedSettings`) only
    under `body.Config != nil` (`:701-723`).
  - It **always** runs `Monitor.Validate()` on what it read (`:724`).
- **Store.** `updateMonitor` re-prepares `m.Config` unconditionally for every credentialed type
  (`internal/store/monitors.go:768-775` at `7744e95`).

Observed on the current code *(run)*. The rows went through the real handler and `Store`, in the
application harness of `internal/appmux` on a throwaway database of the `cerbix_test` server:

| Stored `sslmode` | Update without `config` (e.g. `{"enabled": false}`) |
| --- | --- |
| absent | succeeds, and the row now holds `sslmode: require`. After the T-1a fix that row goes DOWN against a server without TLS because someone toggled `enabled` |
| `prefer` (or a beta free-form value) | `Monitor.Validate()` passes, because it does not look at credential settings. The store then refuses with `settings: sslmode must be one of …`, and the handler, which has no branch for it, answers **500** (`h.serverError`, `handlers_monitors.go:744-759`) |

The same happens to a mysql/redis row that predates its `tls` default: the update writes `tls: true`.

**T-5 — a synthetic monitor cannot be paused, resumed or partly edited** *(run)*.
- **What happens.** `PATCH {"enabled": false}` on a synthetic monitor answers **400**
  `synthetic: config.scenario is required`.
  - The handler validates the monitor it read through `GetMonitor`. That reader withholds the
    writer-only `scenario` (`domain.WriterOnlyMonitorConfigKeys`, `internal/domain/monitor.go:516`).
  - `Monitor.Validate()` requires a scenario for this type (`internal/domain/monitor.go:287-297`).
- **Who is affected.** Every synthetic monitor, on every partial update, including the SPA's
  Pause/Resume button. The store would have carried the scenario forward (`carryWriterOnlyConfigTx`,
  FR-028 invariant 6), but the request never reaches it.
- **Severity.** It is a correctness defect, not a security one. It is listed as P2, the lowest rung,
  because it sits on the same path as T-1d and the same fix closes both.

### 2.5 Every other probe, checked the same way

No other probe builds a DSN or URL by concatenating a config value:

| Probe | Construction |
| --- | --- |
| mysql | fields of `mysql.Config` (`internal/prober/mysql.go:52-59`). TLS is the closed set `mysqlTLSConfig` → `""`, `"true"`, `"skip-verify"` (`:78-86`) |
| redis | RESP frames with length prefixes (`internal/prober/redis.go:86-93`); TLS from `tls`/`tls_skip_verify` booleans |
| promql | `url.URL` with `url.Values{"query": …}.Encode()` on a parsed target (`internal/prober/promql.go:85-93`) |
| rabbitmq management | `url.URL` from `domain.ParseRabbitMgmtTarget` and a validated, parsed path (`internal/prober/rabbitmq.go:136-156`, H-2) |
| websocket, http, synthetic, canary | parsed targets (`internal/domain/targets.go`, H-2). Headers go through `net/http`, which refuses CR/LF |
| tls, dns, tcp, icmp, ssh, grpc | host and port only, through the guard |

Outside the probes, `internal/notify/notify.go:198` formats `bot_token` into the Telegram URL path.
The host is the variable `telegramAPI` (`:23`, a constant in production), so the value cannot change
where the request goes. §7 lists it.

### 2.6 Fix

The probe stops passing anything from the monitor through a connection string and builds the
connection from the monitor alone. Core and the dispatch gate stop letting an absent key mean two
things.

1. **A closed list, checked first.** `sslmode` must be one of:
   - `disable`, `require`, `verify-ca`, `verify-full`;
   - the legacy `prefer`;
   - an absent key, **only** on a job that is not body-bound: a generation-1 carrier or envelope v1
     (item 5, §8 decision 3).

   Anything else ends the probe **before any dial** with `Connected: false` and
   `bad config: unsupported sslmode`. That includes `""` written explicitly, `allow`, `REQUIRE`, and a
   correct mode with a second key. The value is not echoed.
2. **TLS built by the probe, not by `ParseConfig`.** The `ServerName` rules are pgx's own
   (`configTLS`, `sslsni` on): a host name is sent as SNI; an IP literal is not, because RFC 6066
   forbids it.

   | `sslmode` | `TLSConfig` | `Fallbacks` |
   | --- | --- | --- |
   | `disable` | nil | none |
   | `require` | `InsecureSkipVerify: true`; `ServerName` = target host when it is a name, empty for an IP literal | none |
   | `verify-ca` | `InsecureSkipVerify: true` + `VerifyPeerCertificate` checking the chain against the system roots, with the peer's intermediates, without the name (pgx's own `verify-ca`); `ServerName` as under `require` | none |
   | `verify-full` | `ServerName` = target host **always**, system roots. For an IP target Go verifies the certificate's IP SANs and sends no SNI | none |
   | `prefer` (legacy, or absent where item 1 allows it) | as `require` | **one**: the same host and port, TLS nil |

   - **IPv6 targets.** `hostPort` currently returns `[::1]` with its brackets when the target has no
     port (`hostPort` in `internal/prober/postgres.go`, before iter-0213). The brackets would reach `ServerName`, the IP-literal
     test and the dial. `hostPort` strips them, so `[::1]` and `[::1]:5432` both give host `::1`.
     `hostPort` is shared with mysql, redis and the socket probes, so the fix reaches them too.
   - **No custom CA.** `sslrootcert` is not a monitor setting today and does not become one (§7).
3. **Nothing from the process** *(run)*. `pgx.ConnectConfig` requires a config made by `ParseConfig`.
   - **The constant string.** The probe calls `ParseConfig` with this string:

     ```
     host=/cerbix-no-socket port=5432 user=cerbix database=cerbix password='' passfile=''
     connect_timeout=0 sslmode=disable target_session_attrs=any min_protocol_version=3.0
     max_protocol_version=3.0 channel_binding=prefer require_auth='' statement_cache_capacity=512
     description_cache_capacity=512 default_query_exec_mode=cache_statement
     ```

     (one line in the code). Every key the environment could set to an unparsable value is named
     here, because the string is merged last. Observed with pgx v5.10.0:
     - **Parses under hostile values.** It parses with `PGPORT=abc`, `PGCONNECT_TIMEOUT=abc`, and
       invalid `PGTARGETSESSIONATTRS`, `PGMIN/MAXPROTOCOLVERSION`, `PGCHANNELBINDING` and
       `PGREQUIREAUTH`. `ParseConfig("sslmode=disable")` fails on the first of these.
     - **Never enters `configTLS`.** The host is a unix-socket path, so pgx skips TLS setup and reads
       no certificate or key. With `PGSSLROOTCERT`/`PGSSLCERT`/`PGSSLKEY` pointing at garbage files it
       still parses; `host=localhost sslmode=require` fails with "unable to add CA to cert pool". No
       socket is opened, because the probe replaces the host before any dial.
     - **Reads no password file.** `passfile=''` makes `pgpassfile.ReadPassfile("")` fail before
       reading anything. With `PGPASSFILE` pointing at `*:*:*:*:leaked`, the constant yields an empty
       password; `ParseConfig("sslmode=disable")` yields `leaked`.
     - **What still gets in.** `RuntimeParams` keeps `application_name` (`PGAPPNAME`), `options`
       (`PGOPTIONS`) and `timezone` (`PGTZ`), and `SSLNegotiation` keeps `PGSSLNEGOTIATION`. Hence the
       explicit reset below.
   - **Every field set after the parse.** This covers every exported field of `pgconn.Config` and
     `pgx.ConnConfig` in v5.10.0:

     | Field | Value | Why |
     | --- | --- | --- |
     | `Host`, `Port`, `User`, `Password`, `Database` | from the monitor | |
     | `TLSConfig`, `Fallbacks` | item 2 | |
     | `ConnectTimeout` | 0 | the probe's context bounds the attempt |
     | `DialFunc` | the guarded, bounded dial, as today | every dial, fallback included, crosses the SSRF guard |
     | `LookupFunc` | kept: the parse's `net.Resolver.LookupHost` | takes nothing from the environment; each address it returns is still dialed through `DialFunc` |
     | `BuildFrontend`, `BuildContextWatcherHandler`, `OnPgError` | kept: the library's | no environment input |
     | `RuntimeParams` | a new empty map | removes `application_name`, `options`, `timezone` and anything a service file adds |
     | `KerberosSrvName`, `KerberosSpn` | `""` | a service file can set them |
     | `SSLNegotiation` | `""` (an `SSLRequest` first) | `PGSSLNEGOTIATION=direct` would skip the `SSLRequest` |
     | `AfterNetConnect`, `ValidateConnect`, `AfterConnect`, `OnNotice`, `OnNotification`, `OAuthTokenProvider` | nil | |
     | `MinProtocolVersion`, `MaxProtocolVersion` | `"3.0"`, `"3.0"` | the values of a clean parse |
     | `ChannelBinding` | `"prefer"` | the value of a clean parse |
     | `RequireAuth` | `""` | |
     | `Tracer` | nil | |
     | `StatementCacheCapacity`, `DescriptionCacheCapacity` | 512, 512 | the values of a clean parse |
     | `DefaultQueryExecMode` | `QueryExecModeCacheStatement` | the value of a clean parse |

     The reset is one function that takes the parsed config and the monitor, so it can be tested on
     a base in which every field is poisoned. A test compares the list with the struct definitions
     by reflection (§2.8), so a pgx upgrade that adds a field fails the proof rather than slipping
     past it. The walk over `pgx.ConnConfig` skips its embedded `Config` field: that field is
     `pgconn.Config`, whose fields the walk covers on their own.
   - **`PGSERVICE` is the one input the probe cannot neutralize.** `ParseConfigWithOptions` reads the
     service file as soon as `service` is present, before the string's keys take precedence
     (pgx v5.10.0, `pgconn.ParseConfigWithOptions`, the `service` branch).
     - **If it is set:** every probe reads `PGSERVICEFILE` (or `~/.pg_service.conf`). The string
       overrides every key it names, and the reset above clears the rest, so the file cannot change
       where, how or as whom the probe connects.
     - **If the file or the service is missing**, the parse fails. The probe then ends with
       `bad config: the PG* environment of the prober breaks the PostgreSQL client` and does not
       echo pgx's text, which quotes the connection string.
     - **Runbook:** do not set `PG*` variables for the cerbix process. Its own database connection
       takes its DSN from the configuration.
4. **Messages that name the cause** (§8, decision 2).
   - **A server that answers `N`.** pgx's own text, "server refused TLS connection", does not say what
     to do. Under `require` and `verify-*` the probe replaces it with:

     `the server does not offer TLS, and sslmode is "<mode>": set sslmode to "disable" if plaintext is intended`

     The mode is one of the closed list, so the message echoes nothing a user wrote.
   - **A certificate failure under `verify-*`.** It keeps the `x509` error, prefixed with
     `TLS certificate not accepted (sslmode "<mode>"): `.
   - Both are connection failures (`Connected: false`). There is no new probe-error code.
5. **A body-bound job states every defaulted key** (T-1c), at both ends:
   - **The registry.** The credential-settings registry gains one attribute per `writeDefault` field:
     `legacy`, the value the executor applied to an absent key before this change. The values are
     `sslmode` → `prefer`, `tls` → `false`, `query` → `SELECT 1`, `path` → `/api/overview`.
   - **Core.** `MaterializeExecutionConfigs` fills every absent `writeDefault` field of the effective
     schema with its `legacy` value, in the job body, before sealing, on every carrier.
     - The stored row is not changed.
     - Both sides then digest the stated value, so a beta row with no `sslmode` goes out as
       `prefer`, which is exactly what it executes today.
     - The test-connection path is already normalized by `PrepareCredentialSettings`, so the fill
       finds nothing to do there.
   - **Gate.** `ValidateAndMaterialize` gains a step before the field-set and AEAD steps, on
     envelope v2 only. If any `writeDefault` field of the effective schema is absent from the body, it
     refuses the job with `dispatch: a body-bound job omits the defaulted setting "<key>"`. The key
     name comes from the registry, not from the payload.
     - The rule is stated over the registry, not over a hand-written list. `query` and `path` are
       covered although their absence is harmless, and a future field whose absence means something
       else is covered by construction.
     - **Envelope v1 and generation-1 carriers keep today's reading of absence.** Neither binds the
       body, so a carrier writer can change any value there anyway. Refusing absence would protect
       nothing and would stop the beta rows.
     - **Only types with a credential schema.** The check runs only for types that have one
       (`domain.CredentialedType`: postgres, mysql, redis, rabbitmq, promql). A synthetic or
       `async_canary` job is body-bound through its scenario or workflow bindings, but it has no
       schema and no defaulted field, so the check has nothing to require of it.
     - **A typed refusal that does not cost the executor its readiness.** The gate returns a sentinel
       error, which `CredentialProbeErrorReason` maps to `unsupported_version`, exactly as it maps
       `ErrLedgerIdentityMissing` (`internal/dispatch/credentials.go:238-244`).
       - `CredentialHealth.Failure` ignores that reason (`internal/dispatch/health.go:35-37`).
       - Without the mapping the refusal would fall into the default arm, `decrypt_auth_failed`
         (`credentials.go:251-256`). Two in a row mark the executor credential-unready (below and
         §5).
       - "The delivery did not honour its carrier's contract" is what `unsupported_version` already
         means, so no reason is minted and the ingest vocabulary does not grow.
   - **Rolling upgrade.** An upgraded `worker` or `agent` **can** receive a job that a pre-upgrade
     core sealed without the key, and does so even when core is upgraded first:
     - **AMQP.** A job stays in its region's queue until consumed or until its per-message TTL
       expires. The TTL is the monitor's `interval_seconds` (`internal/dispatch/amqp.go:577-580`).
     - **Pull.** A row stays in `pull_jobs` until claimed or until `expires_at`, also the monitor's
       interval (`internal/scheduler/scheduler.go:2115`, `internal/store/pulljobs.go:143`).
     - **The bound.** The interval is at most 86 400 s (`maxIntervalSeconds`,
       `internal/domain/monitor.go:438`).
     - **Only beta rows are affected:** monitors whose stored config lacks a defaulted key.

     **What the code does with such a job.**
     - **As revision 4 wrote it, without the mapping above,** the refusal would block the region until
       the executor restarts. Two consecutive refusals set `credential_ready=false`
       (`internal/dispatch/health.go:33-52`, `internal/worker/worker.go:105-112`,
       `internal/agent/agent.go:127-134`). The scheduler then stops handing that region credentialed
       jobs, as `no_capable_executor` (`internal/scheduler/scheduler.go:1937-1951`). With no job
       there is no success to clear the streak. Only a restart sets readiness from the keyring again
       (`agent.go:109-113`, `worker.go:51`).
     - **With the mapping,** each such job produces one `probe_error` `unsupported_version` for its
       monitor, and the region keeps working.

     §5 still prescribes the order, a pause, the signal and the recovery, and
     TestBodyBoundRefusalKeepsTheExecutorReady pins the mapping.
   - **Test fixtures that seal envelope v2 over a body with no defaulted keys.** They become refusals
     and must state the keys (line numbers at `7744e95`):
     - `internal/agent/agent_test.go`: the monitors `m1` (`:54-56`), `m-future` (`:111-112`),
       `m-desync` (`:433-441`) and `m-null` (`:606-608`), all postgres with an empty `Config`;
     - `internal/dispatch/carrierenvelope_test.go:101-107` (postgres, `username`/`database` only);
     - `internal/dispatch/amqptestcarrier_test.go`: the `v3_envelope_v2` case (`:95-108`, `:356`) and
       the well-formed v3 job (`:165-167`, `:239-241`).

     The list comes from reading every `Seal` call in the tests: postgres with a missing key on
     envelope v2. `worker_test.go` seals envelope v1 and is unaffected. The implementation's first
     red run is the authoritative list.
6. **A partial update leaves `config` alone** (T-1d, T-5). The boundary is in the store, with one
   change in the handler.
   - **Why the store.** Without a `config`, the handler already skips the settings preparation
     (`PrepareCredentialSettings` / `PrepareTypedSettings`); the store is what re-prepares
     regardless. The handler still runs `Monitor.Validate()`, and it keeps doing so.
   - **What the handler changes (T-5).** After `monitorAccess` has established
     `ActionProjectWrite`, the handler reads the monitor through `GetMonitorForWriter`
     (`internal/store/monitors.go`) instead of `GetMonitor`. `Monitor.Validate()` then sees the
     scenario the principal is entitled to read anyway.
     - The write-only password stays out, as today.
     - A legacy row with `prefer` or no `sslmode` passes `Validate()`, which does not look at
       credential settings *(run: the `prefer` request got past it to the store)*.
   - **How the store knows.** `UpdateMonitorByPrincipal` gains a `configSubmitted` argument (the
     unexported `updateMonitor` too). The handler sets it to `body.Config != nil`.
   - **When it is false**, the store:
     - does not prepare or marshal `m.Config`;
     - leaves the `config` column out of the `UPDATE`;
     - does not resolve or replace `monitor_secret_refs`.

     The stored config and its references stay byte for byte as they are. Every other field and the
     `execution_revision` rules behave as today.
   - **When it is true**, today's path runs unchanged. A legacy `prefer` submitted back with a `config`
     gets 400 naming the allowed values. That is the moment an editor chooses a value.
   - **Why a flag and not a comparison.** The safe reader gives the handler the config without its
     write-only password. "Unchanged" would then need either decryption or a comparison with a
     special case for every secret key. The handler knows exactly whether a `config` was sent.
   - **The file-apply path** (`updateMonitorTx`) always carries the file's whole config and is
     unchanged.
   - **The interface change** breaks the fake stores in `internal/api/api_test.go` until they follow,
     which is intended.

### 2.7 Invariants

- **S15 — the probe asks for TLS as `sslmode` says.**
  - Under `require`, `verify-ca`, `verify-full` and `prefer`, the first message the target receives is
    an `SSLRequest`. Under `disable`, it is a `StartupMessage`.
  - Under `require` and `verify-*`, a server that answers `N` is down.
  - Under `verify-full`, a certificate not valid for the target host or IP is down. Under
    `verify-ca`, an untrusted chain is down.
  - **`prefer` falls back to plaintext.** Under `prefer`, if the server answers `N` or any step of the
    TLS handshake fails, the probe connects again in plaintext **to the same host and port**.
    - **The resolved IP is not pinned:** the host is resolved again for the second connection.
    - **Every connection** (the TLS attempt and the plaintext one) goes through the SSRF guard.
    - **Limit:** `prefer` protects only against a passive observer, and only when the server offers
      TLS. It does not protect against an active interceptor, who can answer `N`.
- **S16 — the connection is a function of the monitor alone.**
  - A `sslmode` outside the closed list ends the probe with no dial.
  - No `PG*` variable and no file in the prober user's home changes the dialed address, the TLS
    configuration, the fallbacks, the run-time parameters or the startup user.
  - `PGSERVICE` can only make the probe fail with `bad config`; it cannot redirect it.
- **S19 — a body-bound job carries every defaulted setting.**
  - A job on envelope v2 whose body lacks a `writeDefault` field of its effective schema is refused
    before any AEAD work and before any connection.
  - Core never seals such a job: every absent field is stated with its `legacy` value first.
  - The refusal is `unsupported_version` and never marks the executor credential-unready.
- **S20 — an update that carries no `config` does not touch the stored config.**
  - Neither `PrepareCredentialSettings` nor `PrepareTypedSettings` runs.
  - The `config` column and `monitor_secret_refs` are byte-identical before and after.
  - `Monitor.Validate()` still runs, on the monitor as read for a writer, so a synthetic monitor's
    scenario is part of it. A request that `Validate()` accepts is not refused because of what the
    stored settings hold.
  - **The one refusal that stays** (code review, P1-1). With `secrets.enabled: false`, an update of a
    monitor that references the inventory is refused 404 `feature_disabled`, the pause included,
    exactly as before this package. Skipping the reference resolution does not skip the feature switch.
    It is decided on the row locked for the write: a `monitor_secret_refs` entry or a reference in the
    stored config, never the config the caller read before the transaction (second pass, P2-1).

### 2.8 Tests (red on `main` first; names are planned, unbackticked until they exist)

**Probe tests** (`internal/prober`) run against a local listener that speaks just enough of the
protocol:
- it records the first message and the TLS ClientHello's SNI;
- it answers `S` (then TLS with a certificate from a test CA) or `N`;
- it can fail the handshake.

The probe takes its root pool from an unexported field, so tests use the test CA and production uses
the system roots.

| Test | Asserts | Red on `main` because |
| --- | --- | --- |
| TestPostgresProbeAsksForTLSAsSSLModeSays | S15, the first message per mode | `require`, `verify-ca`, `verify-full` and `prefer` send a `StartupMessage` |
| TestPostgresProbeRefusesAServerWithoutTLS | `N` under `require`/`verify-*` → down with exactly the message of §2.6 item 4. The empty cases on `N`: an **absent** key on a job that is not body-bound executes as `prefer` and reconnects in plaintext to the same host and port; an **explicit** `""` → `bad config: unsupported sslmode`, zero dials | the probe never sends `SSLRequest`, so it never sees `N`; an explicit `""` connects in plaintext |
| TestPostgresPreferFallsBackToTheSameHostAndPort | under `prefer`: after `N`, and separately after a failed handshake, a second connection in plaintext to the same host and port; the guard's dial counter is 2; no other address dialed | no `SSLRequest`, so neither `N` nor a second dial ever happens |
| TestPostgresVerifyFullChecksTheCertificate | test-CA certificate for `db.test` at target `db.test` → up under `verify-full`. A certificate from the **trusted** test root for **another** name (`other.test`) at target `db.test` → `verify-ca` passes and sends SNI `db.test`; `verify-full` refuses. The `db.test` certificate at target `127.0.0.1` → down under `verify-full`; a certificate with IP SAN `127.0.0.1` at target `127.0.0.1` → up under `verify-full`. An untrusted chain → down under `verify-ca` and `verify-full`, up to the handshake under `require` | no handshake happens |
| TestPostgresProbeSendsSNIAsPgxDoes | under `require` and `verify-ca`, the ClientHello carries SNI `db.test` for a name target and none for `127.0.0.1` and `::1` | no ClientHello is sent |
| TestHostPortStripsIPv6Brackets | `[::1]` → (`::1`, 5432); `[::1]:6543` → (`::1`, 6543); `db:5432`, `db` unchanged | `[::1]` comes back with its brackets |
| TestPostgresProbeRefusesAnSSLModeOutsideTheList | `allow`, `REQUIRE`, an explicit `""`, `require host=a,127.0.0.2`, `require sslrootcert=/etc/hosts`, `require service=x` → down, zero dials | the second-host value dials `127.0.0.2`. The `sslrootcert` value answers with the file-existence message |
| TestPostgresProbeIgnoresTheEnvironment | the environment is set with `t.Setenv` (full list below the table). The fake server asks for a **cleartext password**. Expected: an `SSLRequest` comes first (not a direct TLS ClientHello); the startup message is protocol 3.0 and carries no `application_name`/`options`/`timezone`; the probe answers the cleartext request; the dialed address is the target's; the other listener gets nothing. `PGSERVICE` naming a missing service → `bad config` with the message of §2.6 item 3 | `PGAPPNAME`/`PGOPTIONS`/`PGTZ` arrive in the startup message; `PGPORT=abc` makes every probe `bad config` |
| TestPostgresConfigResetCoversEveryField | two parts. (1) By reflection, the set of exported fields of `pgconn.Config` and `pgx.ConnConfig` equals the probe's list of §2.6 item 3. (2) The reset function runs over a **poisoned base**: a config in which every field differs from the expected value (`channel_binding=require`, `require_auth=scram-sha-256`, `min_protocol_version=3.2`, `sslnegotiation=direct`, `krbsrvname`, `application_name`, `target_session_attrs=read-write`, cache capacities of 1, non-nil hooks, a foreign `TLSConfig` and `Fallbacks`). Afterwards every field equals the table's value | there is no list and no reset function |

**The environment set by TestPostgresProbeIgnoresTheEnvironment:**
- `PGAPPNAME`, `PGOPTIONS`, `PGTZ`, `PGHOST`;
- `PGPORT=abc`, `PGCONNECT_TIMEOUT=abc`, `PGTARGETSESSIONATTRS=bogus`;
- `PGSSLNEGOTIATION=direct`, `PGCHANNELBINDING=require`, `PGREQUIREAUTH=scram-sha-256`,
  `PGMINPROTOCOLVERSION=3.2`;
- `PGSSLROOTCERT` (a garbage file), `PGPASSFILE` (a match-all file);
- `PGSERVICE` with a service file naming another listener.

**Why removing any one protection makes a test red.** Each protection is covered by exactly one of
the two tests above:
- **A key in the constant string** (such as `channel_binding=prefer`) is covered by
  TestPostgresProbeIgnoresTheEnvironment. Without the key, its variable reaches the wire:
  - `PGCHANNELBINDING=require` and `PGREQUIREAUTH=scram-sha-256` make the probe refuse the cleartext
    request;
  - `PGMINPROTOCOLVERSION=3.2` changes the startup protocol;
  - `PGPORT=abc` and `PGCONNECT_TIMEOUT=abc` make the parse fail.
- **A line of the reset** is covered by the poisoned base of TestPostgresConfigResetCoversEveryField.
  - The env test cannot see these lines, because the constant string already neutralizes most of
    their variables.
  - Without the line, its field keeps the poison.
  - That is why the reset is a function over a parsed base, not inline code in `Probe`.

**Dispatch, store and API tests:**

| Test | Asserts | Red on `main` because |
| --- | --- | --- |
| TestBodyBoundJobWithoutADefaultedKeyIsRefused (`internal/dispatch`) | for every `writeDefault` field of every credentialed schema (postgres `sslmode`, `query`; mysql `tls`, `query`; redis `tls`; rabbitmq management `tls`, `path`): seal envelope v2 over the full body, `delete(Config, key)` → refused on carriers 3 and 4, with the key named. The same deletion is accepted on carrier 2 / envelope v1 and on carrier 1 | accepted, with the password materialized *(run for `sslmode` and both `tls`)* |
| TestBodyBoundRefusalKeepsTheExecutorReady (`internal/dispatch`) | three consecutive refusals for an omitted defaulted key: `CredentialProbeErrorReason` gives `unsupported_version` each time, and `CredentialHealth.Failure` returns false each time. The same three errors without the sentinel give `decrypt_auth_failed`, and the second returns true | the refusal does not exist on `main`. Its unmapped form, `decrypt_auth_failed`, flips readiness on the second job, and the test asserts that contrast |
| TestMaterializeStatesTheLegacyValueOfAnAbsentKey (`internal/store`, DB) | a postgres row without `sslmode` and a mysql row without `tls`, both with an inline password, written by SQL as a beta would have: the job body carries `sslmode: prefer` / `tls: false` on carriers 2, 3 and 4; the envelope opens through `ValidateAndMaterialize`; the row itself is unchanged | the body has no key *(run: carrier 3, envelope v2, `sslmode` absent)* |
| TestUpdateWithoutConfigKeepsTheStoredConfig (`internal/store`, DB) | rows with `sslmode` absent, `prefer`, and `require host=a`, and a synthetic monitor with a scenario, each updated with `configSubmitted=false` and `enabled` flipped: no error, `config` and `monitor_secret_refs` byte-identical | absent → `require` written; `prefer` → `sslmode must be one of …` *(run)* |
| TestPatchWithoutConfigKeepsTheMonitorEditable (`internal/appmux`, DB: the real handler over the real `Store`) | `PATCH {"enabled": false}` on a synthetic monitor → 200, scenario unchanged; on a postgres row with `sslmode: prefer` → 200, config unchanged; on one with no `sslmode` → 200, still no `sslmode`. `PATCH` with a `config` holding `sslmode: prefer` → 400 naming the allowed values | synthetic → 400 `synthetic: config.scenario is required`; `prefer` → 500; absent → 200 with `sslmode: require` written *(run, all three)* |

**E2E.** `probers.spec.ts` "test-connection probes postgres/tcp instantly" changes:
- the existing case sends `sslmode: "disable"`, which is what the dev Postgres supports;
- a new case sends no `sslmode` (so `require`) and expects `up: false` and a message containing
  `does not offer TLS`.

This is the visible proof of S15 on a live stack. The old assertion encoded the defect.

## 3. T-2 and T-4 — redirects on alert delivery and from the pull agent, and the delivery URL in error text

### 3.1 The clients and who uses them

**Delivery.** `Guard.HTTPClient` (`internal/prober/guard.go:167-181` at `7744e95`) is used only for delivery
(`internal/cli/cli.go:560-564`, D-0141).
- It dials every hop through the `notification_egress` guard, which denies private addresses by
  default.
- Its `CheckRedirect` only caps the chain at 10. Every other redirect rule is the library's.
- `notify.New` and `webhook.New` fall back to a plain `&http.Client{Timeout: 10s}` when given nil
  (`internal/notify/notify.go:46-49` at `7744e95`, `webhook.New` (`internal/webhook/webhook.go`)). Production always passes
  the guard's client. The fallback still follows redirects with the library's policy and no cap at
  all.

| Deliverer | Channel | URL | What it sends |
| --- | --- | --- | --- |
| `webhook.Dispatcher.post` (`webhook.Dispatcher.post` (`internal/webhook/webhook.go`)) | org/project webhook | `Webhook.URL`, any http(s) (`internal/domain/webhook.go:25-37`) | POST JSON `IncidentEvent`: incident id, project id, monitor/service ids, title, status, impact, timestamps, acknowledger's id and name, and the update text. Headers `X-Cerbix-Event` and `X-Cerbix-Signature` (HMAC-SHA256 of the body under the hook's secret) |
| `notify.Dispatcher.post` (`internal/notify/notify.go:255-272`) | `webhook` channel | `config.url`, any http(s) (`internal/domain/notification.go:112-119`). The domain says these URLs "embed the auth token in the path" (`:51`) | POST JSON: monitor transition, alert text or SLA report (event, monitor id and name, project id, text) |
| the same | `slack` channel | `config.url` (an incoming-webhook URL whose path is the secret) | POST JSON `{"text": …}` |
| the same | `telegram` channel | `https://api.telegram.org/bot<bot_token>/sendMessage` (`:198`, `:316`, `:341`, `:364`): **the token is in the URL path** | POST JSON `{"chat_id", "text"}` |
| `notify.Dispatcher.sendMail` | `email` channel | SMTP through `EgressDialContext`: no HTTP, no redirect | — |

Neither deliverer sets `Authorization` or `Cookie`. Every credential in this path is either a header
the library does not treat as sensitive (`X-Cerbix-Signature`) or a URL (webhook, Slack, Telegram).

**The pull agent** (`internal/agent/agent.go:97` at `7744e95`) uses a plain `&http.Client{Timeout: 30s}` for
every call to core. Every call carries `Authorization: Bearer <agent token>` (`:857`).

### 3.2 What `net/http` does on a redirect

From the standard library's `net/http` client (Go 1.26/1.27): `redirectBehavior`, `makeHeadersCopier`,
`shouldCopyHeaderOnRedirect`, `refererForURL`.

| | 301 / 302 / 303 | 307 / 308 |
| --- | --- | --- |
| Method | POST becomes **GET** | stays POST |
| Body | **dropped** | **re-sent** from `GetBody` (`bytes.NewReader` bodies always have one) |
| `Content-Type` | dropped with the body | kept |
| `X-Cerbix-Signature`, `X-Cerbix-Event` | **copied to any host** | **copied to any host** |
| `Authorization`, `Cookie`, `Proxy-Authorization` | dropped unless the new host **name** is the same or a subdomain. The rule compares `URL.Hostname()`, so it ignores the port and the scheme | the same |
| `Referer` | set to the **previous URL, path and query included**, unless https → http | the same |
| Final 2xx | delivery counts as **success** | success |

What this means for delivery today:
- **307/308 to another host** delivers the full incident body, with a valid signature for it, to the
  `Location` host. That host can replay it to the real receiver; the signature carries no timestamp.
- **Any redirect** puts the previous URL in `Referer`.
  - For a webhook channel with a token in its path or query, or for a Slack URL, the next host
    receives the secret URL.
  - For Telegram the previous URL holds the bot token, but `api.telegram.org` is a fixed, trusted
    host.
- **301/302/303** turn the alert into a bodiless GET. If the `Location` answers 2xx, the outbox marks
  the event delivered although the receiver got nothing. That silent loss is a correctness defect on
  its own.
- **The pull agent**: a redirect to another port or scheme on the same host keeps the agent's
  `Authorization` header. That includes `https://core` → `http://core:8080`.
- **Not exposed:** every delivery hop is dialed through the delivery guard. A redirect cannot reach a
  private or metadata address unless `notification_egress.allow_private_ips` is on.

**Who can trigger it:** whoever controls the receiving endpoint. That is a misconfigured or
compromised receiver, a moved endpoint, a URL shortener, or a taken-over domain. Project editors set
channel URLs; org admins set webhooks. For the agent, it is whoever controls core's URL or a proxy in
front of it.

### 3.3 Options and decision

| | A. Follow no redirect | B. Follow only same-origin 307/308 | C. Keep the library's policy |
| --- | --- | --- | --- |
| Rule | `CheckRedirect` returns `http.ErrUseLastResponse`. A 3xx is a non-2xx answer: `post` returns `non-2xx status 30x`, and the outbox retries and dead-letters | the predicate `chainLeftOrigin(req, via, false)` from `internal/prober/redirect.go:47` refuses a hop that leaves `via[0]`'s origin. A 301/302/303 is refused too, because it would drop the body. `Referer` is removed | as now |
| Body, signature, `Referer` to another host | never | never | yes (§3.2) |
| Silent loss on 301/302/303 | gone: it becomes a visible failure | gone: refused | stays |
| What stops working on upgrade | every channel or webhook whose URL answers any 3xx. That includes the common ones: `http://` → `https://` and a trailing-slash 308 | a URL whose redirect leaves the origin (`http://` → `https://` counts as leaving) or that uses 301/302/303. Those 301/302/303 integrations already lose every payload today | nothing |
| Code | one line in `HTTPClient` | the predicate, a `Referer` strip and a method rule. A second redirect policy beside the prober's, to keep in step | none |

**Decision: A** (owner, 2026-11-14; §8). The reasons the author gave:
- A delivery URL is written once, by a person, and should be the URL the receiver answers on.
  Following a redirect only hides a stale URL.
- The cases B would keep working (same-origin 307/308) are the cheapest to fix by editing the URL.
- The cases B refuses (301/302/303) are the ones that are already broken today.
- A has the least code and no second policy to keep in step with S13.

**Where A applies** (revision 3, §10 P2-5 and P3-2):
- `Guard.HTTPClient`;
- the nil-client fallbacks of `notify.New` and `webhook.New`. They get the same `CheckRedirect`, so a
  caller that passes nil does not get a laxer client than production;
- the pull agent's client. A 3xx from core is an error: the agent logs it and tries again on its next
  poll or heartbeat. The agent's `--server` URL must be the URL core answers on.

**The cost of A for existing installations:**
- **Delivery.** After the upgrade, an event to a redirecting URL fails with `non-2xx status 30x`. It
  is retried with backoff and dead-letters; nothing is lost silently. The fix is to set the URL to its
  `Location`.
- **Re-delivery to healthy channels.** A delivery fails as a whole when any one of its channels fails
  (`notify.Dispatcher.Deliver`, `internal/notify/notify.go:56-65`), and the outbox retries the whole
  event.
  - So while one channel of a monitor answers a redirect, every other channel of that monitor
    receives the same alert again on each attempt, up to 10 (`maxAttempts`,
    `internal/outbox/outbox.go:24`), until the event dead-letters.
  - The same holds for the webhooks of one incident event, and for that event's status-page
    subscribers. The incident fan-out returns the webhooks' error and only logs the subscribers'
    (`internal/cli/cli.go:1118-1125`), so a redirecting webhook makes every retry mail the
    subscribers again.
  - This is today's behaviour for any failing channel; A adds redirects to the failures. The runbook
    says so, and says to fix the URL or disable the channel. Per-channel delivery state is out of
    scope (§7).
- **Pull agents** whose `--server` URL redirects (for example `http://` behind a proxy that upgrades to
  `https://`) stop polling until the URL is corrected.
- **Documentation.**
  - The CHANGELOG upgrade note names the delivery and agent symptoms and their fix.
  - The runbook says where a dead-letter's `last_error` is seen.
  - Before upgrading, an operator can check each URL with
    `curl -sS -o /dev/null -w '%{http_code}' -X POST <url>`: a 3xx is a URL to fix.

### 3.4 Invariant and tests

- **S17 — delivery and the pull agent follow no redirect.** For every status 301, 302, 303, 307 and
  308, from both deliverers (with the guard's client and with the nil fallback) and from the pull
  agent:
  - the `Location` host receives **no** request;
  - the call returns an error that names the status;
  - the outbox does not mark the event delivered;
  - every call of the pull agent reports the status: a failed claim or result post returns it, a
    heartbeat outside 2xx logs `agent_heartbeat_failed` with it, and a test claim other than 200
    logs `agent_test_claim_failed` with it (code review, P2-1).

In each test, server A answers each status with `Location: B`, and B counts the requests it gets.

| Test | Asserts | Red on `main` because |
| --- | --- | --- |
| TestDeliveryFollowsNoRedirect (`internal/notify`) | every status × the `webhook`, `slack` and `telegram` channels (`telegramAPI` pointed at A) × two clients: `NewGuard(true, false).HTTPClient`, which allows private addresses so only the redirect rule is under test, and the nil fallback of `notify.New`. B gets zero requests; `Deliver` returns `non-2xx status <code>` | 307/308: B receives the POST with its body. 301/302/303: B receives a GET with `Referer` = A's URL (the secret path of a Slack or webhook URL, the bot token for Telegram), and `Deliver` returns nil |
| TestWebhookDeliveryFollowsNoRedirect (`internal/webhook`) | every status × the guard's client and the nil fallback of `webhook.New`, with a signed incident event. B gets zero requests; the error names the status | 307/308: B receives the body and a valid `X-Cerbix-Signature` for it. 301/302/303: a GET with `Referer`, and nil error |
| TestDeliveryClientRefusesRedirects (`internal/prober`) | `Guard.HTTPClient(...).CheckRedirect` returns `http.ErrUseLastResponse` on the first hop | it returns nil under 10 hops |
| TestRedirectedAlertIsNotMarkedDelivered (`internal/outbox`) | the outbox worker with its fake store and a real `notify.Dispatcher`; the channel URL answers 302 to an endpoint that answers 200. The event is failed with `last_error` containing `non-2xx status 302` and is never marked delivered | it is marked delivered: the 302 became a GET that answered 200 |
| TestAgentFollowsNoRedirect (`internal/agent`) | core's poll, result, heartbeat and test-claim endpoints answer 307 to B (`127.0.0.1`, another port). B gets zero requests; the agent reports the status | B receives each request **with `Authorization: Bearer <agent token>`**: same host name, and the library ignores the port |

### 3.5 The other outbound HTTP clients (listed, not changed)

| Client | Where | Redirect policy | Carries |
| --- | --- | --- | --- |
| OIDC discovery, JWKS, token exchange | `internal/auth/auth.go:213` | library's | `client_secret` (Basic header, or in the form body depending on the provider's auth style) and the authorization code. The IdP is operator-trusted, not routed through the guard (D-0141) |
| RabbitMQ management (readiness and queue stats) | `internal/mqadmin/mqadmin.go:80` | library's | Basic credentials from the broker URL, per request |
| `cerbix gate` / `cerbix change` CLI | `internal/cli/gate.go:247-252` | **none** (`ErrUseLastResponse`) | the gate token |
| Probes (http, promql, rabbitmq management, synthetic, canary) | `internal/prober/prober.go`, `canary.go` | S13 (`sec-hardening-2` H-13) | — |
| SMTP (alerts, subscriber mail, password reset) | `internal/mailer` | no HTTP | — |

The pull agent's client moved out of this table into §3.3 (P3-2). The library keeps `Authorization`
on a same-name redirect to another port or scheme. For the two clients with the library's policy, the
endpoint is operator-configured, so this stays out of scope (§7).

### 3.6 T-4: the delivery URL in error text

**Code.** Two calls in each deliverer wrap a library error as it is:

| Call | Wraps | Where |
| --- | --- | --- |
| building the request | `url.Parse`'s error: `build request: %w` | `internal/notify/notify.go:239`, `internal/webhook/webhook.go:80` (both at `7744e95`) |
| sending it | `client.Do`'s error: `post: %w` | `internal/notify/notify.go:244`, `internal/webhook/webhook.go:87` (both at `7744e95`) |

A request that fails before any response returns a `*url.Error`. Its text quotes the request URL
with its path and query; the library masks only a userinfo password. `url.Parse`'s error quotes the
input. That covers a DNS failure, a refused connection, a refusal by the delivery guard, a TLS
failure, a timeout and an invalid URL. A received non-2xx is `non-2xx status N` and carries no URL.

Observed *(run)*, with the URL shapes the deliverers build:

```
post: Post "https://api.telegram.org.invalid/bot123456:SECRET-BOT-TOKEN/sendMessage": dial tcp: lookup api.telegram.org.invalid: no such host
post: Post "https://hooks.slack.invalid/services/T000/B000/SECRETPATH": dial tcp: lookup hooks.slack.invalid: no such host
build request: parse "https://hooks.example/x?token=SECRET\x7f": net/url: invalid control character in URL
```

**Where the text goes:**

| Place | Where | Who reads it |
| --- | --- | --- |
| the deliverers' return value, joined per channel or hook as `channel <id>: …` / `webhook <id>: …` | `internal/notify/notify.go:56-185`, `internal/webhook/webhook.go:70-76` | the outbox worker |
| `outbox_events.last_error` | `FailOutbox` (`internal/outbox/outbox.go:324`, `internal/store/outbox.go:317`). Kept until the event is delivered or replayed (`internal/store/outbox.go:211`, `:280`, `:297`) | anyone who reads the database |
| the log lines `outbox_delivery_retry` and `outbox_dead` | `internal/outbox/outbox.go:345`, `:334` | anyone who reads the process log, outside the database's access control |
| `GET /api/v1/admin/outbox/dead`, shown in Admin → Outbox | `internal/api/handlers_outbox.go:25-41` returns `ListDeadOutbox` rows unchanged | global admins |

**What leaks.** A channel's `url` (Slack, webhook) and `bot_token` (Telegram) are write-only in the
API. They are in `domain.SecretChannelConfigKeys` (`internal/domain/notification.go:50-54`) and
blanked by `Redacted()`: a project viewer never reads them, and an editor re-enters them. Through
`last_error` they reach every global admin; through the log, anyone who can read it. An org webhook
URL with a token in its path or query leaks the same way.

**Why P2.** The readers are operators and global admins, not other tenants.

**The other senders, checked the same way:**
- **Pull agent.** Not affected.
  - Its URLs are core's base URL plus fixed paths and region and agent query parameters; the token
    travels in a header (`internal/agent/agent.go:857`).
  - Its errors are only logged on the agent's host.
  - A password in the `--server` URL's userinfo is masked by the library *(run:
    `https://user:***@core.invalid/…`)*.
- **Mail.** Not affected by URLs.
  - `notify.sendMail` (`smtp: %w`, `internal/notify/notify.go:250`) and `internal/mailer` return
    SMTP errors that name the server's host and port and quote the server's reply.
  - `net/smtp` never puts the password into an error.
  - A reply can quote a recipient address. That is personal data, not a credential (§7).
- **Probes.** They have their own leak invariant (NFR-024, `internal/prober/synthetic.go:22`,
  `internal/prober/canary.go`) and are out of scope.

**Fix.** One helper, `notify.DeliveryFailure(err) error`, turns a failed request into text that holds
no part of the URL. `internal/webhook` imports it from `internal/notify`, so there is one copy. Neither
package imports the other today, and `internal/prober` imports neither, so this adds no cycle.
- **A failed `client.Do`** becomes `post failed: <reason>`. The reason is a closed set, checked in
  this order on the error chain (`*url.Error` and `*net.OpError` both unwrap):

  | Reason | Recognized by |
  | --- | --- |
  | `destination refused by the delivery egress policy` | `prober.IsEgressRefusal(err)`, a new exported predicate in `internal/prober/guard.go`: `errors.As` onto the guard's unexported `*blockedIPError` (`:38-45`). That is what the guard returns for a literal or resolved address its policy refuses (`:134-152`) |
  | `certificate not accepted` | `errors.As` onto `*tls.CertificateVerificationError` |
  | `the server does not speak TLS` | `errors.Is(err, http.ErrSchemeMismatch)`. That is what `net/http` returns when an HTTPS request gets a plain HTTP answer: it replaces the TLS error with this sentinel |
  | `TLS handshake failed` | `errors.As` onto `*net.OpError` whose `Op` is `"remote error"` (the peer sent a TLS alert) or `"local error"` (the client sent one). Or `errors.As` onto the **value** type `tls.RecordHeaderError`: the bytes were neither TLS nor HTTP |
  | `timeout` | `errors.Is(err, context.DeadlineExceeded)`, or a `net.Error` whose `Timeout()` is true |
  | `DNS lookup failed` | `errors.As` onto `*net.DNSError` |
  | `connection refused` | `errors.Is(err, syscall.ECONNREFUSED)` |
  | `connection failed` | anything else |

  - **Every TLS row was checked by running** *(run)*, through `Guard.HTTPClient` with private
    addresses allowed and a planted token in the URL's path and query. Each error text quotes the full
    URL.

    | Server | What `errors.As`/`errors.Is` finds |
    | --- | --- |
    | `httptest` TLS server, untrusted certificate | `*tls.CertificateVerificationError` |
    | plain HTTP server, `https://` URL | `http.ErrSchemeMismatch` (`http: server gave HTTP response to HTTPS client`) |
    | TLS server limited to TLS 1.1 | `*net.OpError` `Op: "remote error"` (`tls: protocol version not supported`) |
    | a listener that answers the ClientHello with a fatal `handshake_failure` alert record | `*net.OpError` `Op: "remote error"` (`tls: handshake failure`) |
    | a listener that answers with a truncated ServerHello | `*net.OpError` `Op: "local error"` (`tls: error decoding message`) |
    | a listener that writes an SSH banner | `tls.RecordHeaderError`, by value (`tls: first record does not look like a TLS handshake`) |

  - **What revision 6 got wrong.** Its row could not match.
    - `tls.RecordHeaderError` is returned by value, so `errors.As` onto `*tls.RecordHeaderError`
      never matches. The run shows the value form matching and the pointer form not.
    - crypto/tls wraps `tls.AlertError` only for QUIC; over TCP a TLS alert arrives as the
      `*net.OpError` above. The run found `tls.AlertError` in no chain.
  - **An out-of-order handshake message** comes back as an untyped crypto/tls error (`tls: received
    unexpected handshake message …`, *run*), so it is classified `connection failed`. That is still a
    reason from the closed set and still free of the URL.
  - The guard row was checked the same way in revision 5 *(run)*. With private addresses refused,
    posting to `127.0.0.1`, `errors.As` resolves the error to `*blockedIPError`.
  - No library text passes through. The property therefore holds by construction, not by scrubbing.
- **A failed request build** becomes `invalid URL`, without quoting it.
- **A received status** stays `non-2xx status N`.
- **What still identifies the failure:** the `channel <id>` / `webhook <id>` prefix tells the operator
  which one failed.

**Text already stored or written.**
- **No migration.** Pending and dead rows keep their text until they are delivered or replayed, and
  replay clears it.
- **The runbook** carries the statement that blanks it (§5, query 3).
- **The upgrade note** says that logs written before the upgrade may hold channel URLs and Telegram
  bot tokens. Rotate those whose logs left the operator's control.

**S21 — no failed delivery's text holds its URL.** No text produced by a failed delivery contains the
path, query or userinfo of the delivery URL, nor a Telegram bot token. That covers the text the
deliverer returns, `last_error`, the log, and the dead-letter API.

| Test | Asserts | Red on `main` because |
| --- | --- | --- |
| TestDeliveryErrorNamesNoURL (`internal/notify`) | distinctive tokens are planted in a `webhook` channel URL's path and query, in a `slack` URL's path, and in a `telegram` channel's `bot_token` (`telegramAPI` pointed at the case's server). Each error names the channel id and **the expected reason**, and contains no planted token. The cases and the reason each expects: a closed port on `127.0.0.1` → `connection refused`; a URL with a control byte → `invalid URL`; a private address under `NewGuard(false, false)` → `destination refused by the delivery egress policy`; an `httptest` TLS server with an untrusted certificate → `certificate not accepted`; an `https://` URL at a plain HTTP server → `the server does not speak TLS`; a listener that answers with a fatal TLS alert record → `TLS handshake failed`; a listener that writes an SSH banner → `TLS handshake failed` | every error quotes the URL with the planted tokens, and none names a reason |
| TestWebhookDeliveryErrorNamesNoURL (`internal/webhook`) | the same for an org webhook with tokens in its path and query | the same |
| TestFailedDeliveryStoresAndLogsNoURL (`internal/outbox`) | the outbox worker with its fake store, a real `notify.Dispatcher` and a `slog` handler writing to a buffer; a channel URL with a planted token at a closed port. Neither the `lastErr` passed to `FailOutbox` nor any log line holds the token, on a retry and on the attempt that dead-letters. The dead-letter API serves `last_error` unchanged, so this covers it | both hold the full URL |

## 4. T-3 — headers in alert mail

### 4.1 Every mail sender, and what reaches its headers

cerbix builds a mail message in exactly two places. Both write `From`, `To` and `Subject` by string
concatenation and send the message through `mailer.SendMailContext`:
- `notify.Dispatcher.sendMail` (`internal/notify/notify.go`, before iter-0213: the message was one `fmt.Sprintf`);
- `mailer.Mailer.SendContext` (`internal/mailer/mailer.go:236-256`, the message, at `7744e95`, `:249-254`).

| Mail | Built by | `From` | `To` | `Subject` | Who controls the `Subject` |
| --- | --- | --- | --- | --- | --- |
| Channel email: every alert text in the table below | `notify.sendMail`; the subject **is** the alert text (`:226` at `7744e95` writes `text` twice) | the channel's `from` | the channel's `to` list | the alert text, whole | see below |
| Status-page subscriber incident mail | `subscribe.render` (`internal/subscribe/subscribe.go:67`) → `mailer.Send` | the instance mail sender (Settings → Mail) | each confirmed subscriber | `[<status>] <Incident.Title>` | project editors (incident title), **and anyone holding a project's Alertmanager token**: an alert's `summary` annotation becomes the incident title (`internal/api/handlers_alertmanager.go:148`) |
| Subscription confirmation | `internal/api/handlers_subscribe.go:67-70`, queued (`TopicSubscriberConfirm`), sent at `internal/outbox/outbox.go:877` | the instance mail sender | the address typed into the public subscribe form | `Confirm your subscription to <StatusPage.Title>` (`:69`) | whoever may edit the status page; `StatusPage.Validate` requires only a non-blank title (`internal/domain/statuspage.go:199`) |
| Password reset | `internal/auth/handlers_reset.go:65-69` | the instance mail sender | the account's email | constant | nobody |

The channel alert texts, each delivered as the whole subject of a channel mail:

| Text | Rendered by | Delivered at | Values from the database | Who writes them, and what is checked |
| --- | --- | --- | --- | --- |
| Monitor transition and reminder: `🔴 <name> is DOWN.` / recovery | `notify.Message` (`internal/notify/notify.go:275`) | `notify.Deliver` | `Monitor.Name` | project editors (API, UI, monitoring-as-code): non-blank only (`internal/domain/monitor.go:231`) |
| Escalation step | `domain.EscalationStepAlert.Message` (`internal/domain/outbox.go:314`) | `internal/outbox/outbox.go:865` | the monitor's or service's name (`Subject()`) | project editors: monitor name as above; no service-name rule refuses a control byte either |
| Service alert (live and burn) | `domain.ServiceAlert.Message` (`internal/domain/servicealert.go:368`) | `internal/outbox/outbox.go:766` | `ServiceName` | project editors (API, monitoring-as-code); no rule refuses a control byte |
| **SLA report**: `📊 Weekly SLA report — <project>` then one line per window | `domain.SLAReport.Message` (`internal/domain/outbox.go:341`) | `internal/outbox/outbox.go:821` | `Project.Name` | org admins: non-blank only (`internal/api/handlers.go:230-233`). **And the text is multi-line by construction**: every scheduled report puts raw line breaks into the subject, with no attacker involved |
| Region worker lost / regained | `domain.RegionWorkerAlert.Message` (`internal/domain/outbox.go:263`) | `internal/outbox/outbox.go:834` | the region name, twice. `%q` escapes the first; the second, in `--region %s`, is written raw | whoever sets a monitor's region. Bounded to `^[a-z0-9-]{1,40}$` (`internal/domain/monitor.go:645`) at the monitor's write (`:409`) and in the config (`internal/config/secrets.go:111`, `:163`), so today it cannot carry a control byte. Listed because the builder must not rely on that |

**The SLA report today** *(run)*. The report text was formatted exactly as `notify.sendMail` formats
it, passed through `net/smtp`'s `DATA` writer, and parsed back with `net/mail`.
- The writer turns each bare LF into CRLF.
- The window lines begin with spaces, so they are read as folded continuations of `Subject`.
- The decoded subject is the whole report, raw emoji included:
  `📊 Weekly SLA report — Payments 7d : 99.912% (10070/10079 checks) 30d: no data`.
- A project name containing a line break followed by a non-space would end the subject and start a
  header of its own.

**`From` and `To` are already safe from injection; `Subject` is not.**
- **`From` with CR or LF.** The send fails at the SMTP envelope before `DATA`: `mailFrom` refuses it
  (`internal/mailer/mailer.go:164-166`).
- **A recipient with CR or LF.** The send fails the same way, because `net/smtp`'s `Client.Rcpt` runs
  `validateLine`. So headers built from these values never reach a server.
- **The subject has no such check.**
  - `Monitor.Validate`, `Incident.Validate` and `StatusPage.Validate` require only a non-blank name or
    title (`internal/domain/monitor.go:231`, `internal/domain/incident.go:167`,
    `internal/domain/statuspage.go:199`).
  - The Alertmanager receiver trims the `summary` and nothing else.

**Addresses are also checked weakly where they are written.**
- **Subscriber address.** It needs only an `@` that is neither first nor last
  (`internal/domain/statuspage.go:298-307` at `7744e95`).
- **Email channel and mail settings.** They need only non-blank `to` and `from`
  (`internal/domain/notification.go:125-128`, `internal/domain/settings.go:214-225`, both at `7744e95`).
- **What gets stored as a result.** A display-name form (`Ops <ops@example.com>`), a list in one
  subscriber field, or a non-ASCII address is stored. Each then fails at the SMTP server on every
  send. `mailFrom` writes `MAIL FROM:<%s>` and `net/smtp` writes `RCPT TO:<%s>`, so a display name
  produces `<Ops <ops@example.com>>`. And `mailFrom` deliberately does not request SMTPUTF8.

**What a CR/LF in a subject does.** Take a value such as
`x\r\nReply-To: a@evil.example\r\nContent-Type: text/html\r\n\r\n<p>…</p>`:
- it adds headers;
- it ends the header block early. cerbix's own `MIME-Version` and `Content-Type` lines become body
  text, so the injected `Content-Type: text/html` is the one the client obeys.

**Who receives it.**
- The mail is sent from the instance's real, authenticated sender.
- It goes to channel recipients and, through an incident title, to every confirmed subscriber of the
  status pages that show the project.
- The envelope recipients do not change, and `net/smtp`'s dot-writer prevents SMTP command smuggling.

**Why P2:** the people who can do it can already write the alert's plain text. What they gain is
headers and an HTML body under the operator's domain.

**Two more defects in today's subjects.**
- **Non-ASCII.** Every alert subject has an emoji, and it is written raw today. RFC 5322 does not
  allow that without SMTPUTF8.
- **Multi-line alerts.** The SLA report is the whole subject today. Its lines are folded into the
  subject only because each begins with spaces (see the run above).
- **Not a header boundary.** None of the rendered texts treats a value as a boundary of a header.
  The region name is safe only because the slug rule happens to bound it.

### 4.2 Fix

One message builder in `internal/mailer` serves both senders, so neither concatenates headers again.
- **Addresses: one rule, applied in two places.** The owner confirmed this rule (P2-2) and the
  subject rule below (P2-3) on 2026-11-15 (§10.2).
  - **The rule.** An address is accepted when all of these hold:
    - `mail.ParseAddress(s)` succeeds;
    - the result has no display name;
    - its `Address` equals `s` byte for byte;
    - `s` is printable ASCII;
    - `s` is at most 254 octets: RFC 5321's path limit less its brackets (code review, P2-2).

    This refuses display names, `<…>`, comments, lists, quoted local parts that Go rewrites, non-ASCII
    addresses, whitespace and every control byte. It accepts `a@b.example` and a domain literal
    `a@[192.0.2.1]` *(run: 13 cases)*.

    Non-ASCII addresses are refused on purpose. cerbix does not request SMTPUTF8 (`mailFrom`), so
    such an address cannot be delivered through a relay that lacks it, and refusing it at write
    tells the person now instead of failing every send.
  - **In the builder:** `From` and every `To` element. A refused address ends the send before any
    connection, with an error that names the field and the element's position, not the value.
  - **Surrounding whitespace.** The rule is applied to the value with surrounding whitespace removed,
    and that value is what is sent. Write and send therefore give one answer:

    | Address | At write | At send today |
    | --- | --- | --- |
    | subscriber | trimmed before `Validate` and stored trimmed (`internal/api/handlers_subscribe.go:50`) | the stored, trimmed value |
    | Settings → Mail `from` | trimmed before `Validate` and stored trimmed (`internal/api/handlers_settings.go:218`) | the stored, trimmed value |
    | email channel `to` elements | split at commas, each element trimmed | each element trimmed (`internal/notify/notify.go:230-234`) |
    | email channel `from` | trimmed for the rule | **sent as stored, untrimmed** (`internal/notify/notify.go:228`) |
    | YAML `mail.from` | checked as written by `Config.Validate()` | sent as written, so a value with spaces never starts |

    - **The one mismatch** is the channel `from`. `notify.sendMail` now trims it exactly as it trims
      the `to` elements. Without that, `" ops@example.com "` would pass at write and be refused at
      send.
    - **Rows already stored** with a padded `from` stop failing at the server, which today receives
      `MAIL FROM:< ops@example.com >`.
    - **Query 2** keeps `btrim`, which is the same trimming.
  - **At write.** The same rule replaces the weak checks above. The handlers already map these
    `Validate` errors to 400:
    - the public subscribe form (`Subscriber.Validate`);
    - the email channel's `from` and each comma-separated element of `to`
      (`NotificationChannel.Validate`);
    - Settings → Mail `from` (`MailSettings.Validate`).

  - **At startup** (owner's decision, 2026-11-15, §8 item 6). `Config.Validate()`
    (`internal/config/config.go`) checks the YAML mail block:
    - **`mail.smtp_host` set and `mail.from` empty** → refused, naming `mail.from`. Today
      `MailConfig.Enabled()` (`internal/config/config.go:374`) treats that block as mail switched
      off, and nothing is said.
    - **`mail.from` set and refused by the rule** → refused, naming `mail.from`, not quoting the
      value.
    - **`mail.smtp_host` empty** → the block is not checked, as today.

    The check sits beside the two the block already has: `smtp_port` and `public_base_url` when mail
    is enabled (`internal/config/config.go:787-794`).
  - **One rule, one copy.** The rule is one function in `internal/domain`. `internal/config` already
    imports that package (`domain.ValidRegion`, `internal/config/secrets.go:111`). The builder, the
    three `Validate` methods and `Config.Validate()` all call it.
  - **When Settings → Mail is already saved in the database**, the startup check can refuse a value
    that no longer applies.
    - **Why the seed is dead then.** The seed applies only while the database's mail group is
      unconfigured (`settings.Service.resolve`, `internal/settings/service.go:91-92`). The first save
      through Settings → Mail marks the group configured (`SaveMail`, `:191-200`), and from then on
      the YAML block is never read.
    - **Why the check cannot see that.** `Config.Validate()` runs inside `config.Parse`
      (`internal/config/config.go:522`), before any database exists for the process. So it cannot
      know whether the seed is still in effect, and it refuses a dead `mail.from` the same way it
      refuses a live one.
    - **This is how the block is already treated.** The `smtp_port` and `public_base_url` checks
      refuse a dead seed today in exactly the same way.
    - **Why the order stays.** The other order would check the seed only while the group is
      unconfigured. It needs the database before validation, a second validation point after the
      configuration has been accepted. The strict-configuration rule of `AGENTS.md` asks for the
      opposite: a validated snapshot before business logic starts.
    - **The cost.** An operator who moved mail to Settings → Mail and left a bad seed in the YAML
      corrects or clears `mail.from` there (or clears the whole block) before the upgraded process
      starts. The upgrade note says so. This does not change the decision.
  - **Rows already stored.** No migration. Each kind of row keeps failing, but earlier and more
    visibly than today:
    - **Email channel** with a refused address: the delivery fails as a whole with the field and
      position, retries and dead-letters. That is what the server's refusal does today, one step
      earlier.
    - **Settings → Mail `from`:** every instance mail fails until it is corrected, as today.
    - **Subscriber** with a refused address: delivery skips that recipient with a log line, and the
      other subscribers receive the mail once. This is how the code already behaves, and no change
      to `internal/subscribe` is needed:
      - `subscribe.Deliver` keeps sending to the remaining recipients and returns only the first
        error (`internal/subscribe/subscribe.go:49-54`);
      - the incident fan-out logs that error as `subscriber_email_failed` and does not fail the event
        (`internal/cli/cli.go:1118-1125`).

      Revision 3 said that the event failed and the outbox re-mailed every valid subscriber. That
      was wrong, and it is corrected here; the behaviour the owner confirmed is unchanged. The
      builder now refuses the address before connecting, so the log line names the field and
      position, not the server's reply. The subscriber's confirmation mail dead-letters.
    - The runbook lists these rows (§5), and the upgrade note has a line on it.
- **Subject.**
  1. The subject text is split at CR, LF and CRLF. The first line that is not blank after trimming
     becomes the subject.
  2. Any other control byte in it is replaced by a space.
  3. A line longer than **200 runes** keeps its first 199 runes and gets `…`.
  4. The result is encoded with `mime.QEncoding.Encode("utf-8", …)`. That leaves printable ASCII as
     it is and writes anything else as RFC 2047 encoded-words.
  5. The header is folded per RFC 5322 §2.2.3: between encoded-words, and at spaces in plain ASCII, so
     no line exceeds 78 octets where whitespace allows it. A 200-rune line without spaces stays one
     line, well under 998 octets.

  The `Subject` header therefore cannot carry a line break of its own.
- **Body.** The full text, unchanged. For channel mail that is the whole alert text, so the lines the
  subject drops are in the body. Line breaks are legitimate there, and `net/smtp` dot-stuffs and
  normalizes them.
- **Header set.** Fixed: `From`, `To`, `Subject`, `MIME-Version: 1.0`,
  `Content-Type: text/plain; charset=UTF-8`, in that order. The same five as today.

No validation is added to monitor names, incident titles or status-page titles. Existing names would
keep their bytes, and three write paths (API, monitoring-as-code, Alertmanager) would each need it.
The boundary is where the bytes become a header.

### 4.3 Invariant

- **S18 — no value a user or an external sender writes adds a header line to a mail cerbix sends.** For
  every message either builder produces:
  - parsed with `net/mail.ReadMessage`, the header set is exactly the five of §4.2;
  - the decoded `Subject` is the first non-blank line of the given subject text, with control bytes
    replaced by spaces and at most 200 runes;
  - no header line exceeds 998 octets;
  - the body is the body given;
  - an address is judged and sent with surrounding whitespace removed, so the write path and the send
    path give one answer for it;
  - a `From` or `To` the address rule refuses is refused before any connection, in both builders.
    That covers the channel's `from` and every element of its comma-separated `to`, and any control
    byte: 0x00, CR, LF and 0x7f among them;
  - the same rule refuses that address at write, with 400;
  - the process does not start with a YAML mail block that sets `smtp_host` and leaves `from` empty or
    gives a `from` the rule refuses. The error names `mail.from`.

### 4.4 Tests (red on `main` first; names are planned)

| Test | Asserts | Red on `main` because |
| --- | --- | --- |
| TestChannelMailCannotAddAHeader (`internal/notify`) | a monitor named `x\r\nBcc: …\r\nContent-Type: text/html\r\n\r\n<p>forged</p>` goes DOWN. The message captured through `sendMailFunc` parses to exactly the five headers, `Subject` decodes to `🔴 x`, and the body is the whole alert text | the captured message has `Bcc` and `Content-Type: text/html` headers, and `<p>forged</p>` as its body |
| TestMailerSubjectCannotAddAHeader (`internal/mailer`) | the same through `Mailer.SendContext`, the path of subscriber, confirmation and reset mail; the subject is an incident title shaped like an Alertmanager `summary` | the same |
| TestMailSubjectIsTheFirstLineAndBounded (`internal/mailer`) | a three-line SLA report → `Subject` is its first non-blank line and the body is all three lines; a 500-rune line → 199 runes and `…`; every header line ≤ 78 octets when the subject has spaces or is encoded | the subject is the whole text, and its line breaks end the header block |
| TestMailSubjectIsEncodedWhenNotASCII (`internal/mailer`) | `🔴 db is DOWN.` → RFC 2047 encoded-words that `mime.WordDecoder` decodes back | the raw UTF-8 bytes are in the header |
| TestMailerRefusesAnAddressItCannotSend (`internal/mailer`) | `From` or `To` = `Ops <ops@example.com>`, `a@b,c@d`, `a b@c`, `üser@example.com`, or a value with 0x00, CR, LF or 0x7f → error naming the field, and the captured sender is **not called**; `a@b.example` and `a@[192.0.2.1]` are sent | the captured sender is called for every one of them. Later the envelope refuses CR and LF; 0x00 and 0x7f it does not refuse at all (`validateLine` and `mailFrom` look only for CR and LF) |
| TestChannelMailRefusesAnAddressItCannotSend (`internal/notify`) | an email channel whose `from`, and separately the second element of whose `to` list (`ops@example.com, <bad>`), carries 0x00, CR, LF or 0x7f, or is one of the other refused forms. The monitor goes DOWN; the delivery error names `from` or `to` and the element's position, not the value; `sendMailFunc` is **not called**; the first, valid `to` element is not mailed either (the channel fails as a whole, §4.2). A `from` of `"  ops@example.com  "` is **sent**, and `sendMailFunc` receives `ops@example.com` as the envelope sender and in the `From` header | `sendMailFunc` is called with every refused value; the padded `from` reaches it untrimmed |
| TestConfigRefusesAMailFromItCannotSend (`internal/config`) | `config.Parse` of a YAML mail block refuses each of these, naming `mail.from` and not quoting the value: `smtp_host` set with `from` empty; `smtp_host` set with `from: "Ops <ops@example.com>"`, `"a b@c"` or `"üser@example.com"`. It accepts `smtp_host` with a valid `from`, and `smtp_host` empty with any `from`, the block then not being checked | each of the four is accepted: `Enabled()` is false for the empty `from`, and nothing checks the syntax of a set one |
| TestAddressesAreSingleAndUnnamedAtWrite (`internal/domain`) | `Subscriber.Validate`, `NotificationChannel.Validate` (email `from` and each `to` element) and `MailSettings.Validate` refuse the same values and accept the same two. A channel `from` with surrounding spaces is accepted, matching the send-side test above | `Ops <ops@example.com>`, `a@b,c@d`, `a b@c` and `üser@example.com` are accepted |
| TestSubscriberMailSkipsARefusedAddress (`internal/subscribe`) | confirmed subscribers `a@b.example`, `a b@c`, `c@d.example` with a sender that refuses the middle one as the builder does: the other two are sent exactly once, and the returned error names the position, not the address | **green on `main`, as a regression guard.** It pins the behaviour the owner confirmed, which the code already has (§4.2). The refusal it relies on is proved red by TestMailerRefusesAnAddressItCannotSend |

## 5. Iteration

One iteration, iter-0213 / D-0280, branch `sec-hardening-3`:
1. T-1: tests red → the probe fix (§2.6 items 1–4) → green; T-1c (item 5) and T-1d with T-5 (item 6)
   the same way; the E2E change.
2. T-2: tests red → option A in the three clients → green.
3. T-3: tests red → the message builder and the address rule → green. Packages touched:
   `internal/mailer`, `internal/notify`, `internal/domain`, `internal/config`.
4. T-4: tests red → the shared failure helper in both deliverers → green.
5. Docs:
   - **Runbook.**
     - The Postgres monitor TLS section, with query 1 below.
     - "Do not set `PG*` variables for the cerbix process".
     - The rolling-upgrade order, the pause from query 4, its signal and recovery (below).
     - Delivery redirects, re-delivery to healthy channels while one channel redirects (§3.3), and the
       agent's `--server` URL.
     - Mail subjects, and query 2 below for stored addresses.
     - Delivery failure text: what the closed set of reasons means, and query 3 below for
       `last_error` written before the upgrade.
   - **`func-secret-inventory.md`.**
     - The corrections of §9 updated to "fixed in iter-0213".
     - In §4.7, "absent and explicitly-set-to-default encode identically" gains its limit: on
       envelope v2 an absent defaulted field is refused, and core states it (S19).
   - **CHANGELOG**, in this order:
     1. the T-1a upgrade note (§8 decision 2), text below;
     2. T-2's note, delivery and agent;
     3. T-3's note, text below;
     4. T-4's line, with the advice on old logs.
   - **The mail block's own documentation** (review of revision 6, P3-4):
     - `docker/config.example.yaml`, the comment above `mail:`. Today it says "leave smtp_host/from
       empty to disable both". It now says that an empty `smtp_host` turns mail off, and that once
       `smtp_host` is set, `from` is required.
     - The doc comment of `MailConfig.Enabled` (`internal/config/config.go:370-373`), to the same
       effect.
     - `README.md` and `INSTALL.md` do not describe the `mail` block, so they are unchanged.
   - **Traceability, D-0280 and iter-0213.**
6. Gates:
   - `go build -buildvcs=false ./...`, `go vet ./...`, `make docs-check`;
   - `make race-both`;
   - `make dev-build && make dev-up && make dev-test`.

**Query 1 — the postgres monitors to look at before upgrading.** Column names were checked against the
schema with `EXPLAIN` on `cerbix_test`, not the dev database *(run)*:
- `monitors.target`, `monitors.config` and `monitors.enabled` (`00003`);
- `monitors.region` (`00033`);
- `managed_monitors.monitor_id` (`00060`);
- `monitors.retired_at` (`00081`).

```sql
SELECT m.id, m.project_id, m.name, m.target, m.region, m.enabled,
       COALESCE(m.config->>'sslmode', '(absent)') AS sslmode,
       (mm.monitor_id IS NOT NULL) AS file_managed
  FROM monitors m
  LEFT JOIN managed_monitors mm ON mm.monitor_id = m.id
 WHERE m.type = 'postgres'
   AND m.retired_at IS NULL
 ORDER BY m.project_id, m.name;
```

`sslmode` is a plaintext key of `monitors.config`; only `password` is encrypted (D-0062). Every
postgres monitor is listed, so that values outside the list show up too. How to read a row:

| `sslmode` | After the upgrade |
| --- | --- |
| `require` | DOWN unless the server offers TLS |
| `verify-ca`, `verify-full` | DOWN unless the server offers TLS **and** its certificate chains to the system roots. Under `verify-full` the certificate must also be valid for `target` (name or IP). A private-CA certificate goes DOWN: there is no custom-CA setting (§7), so use `require` meanwhile |
| `disable` | unchanged |
| `prefer`, `(absent)` | unchanged availability: TLS when offered, plaintext otherwise |
| anything else | DOWN with `bad config: unsupported sslmode`; set one of the four |

- **`region`**: TLS reachability is a property of the path from that region's prober, so check from
  there.
- **`file_managed`**: the monitor is fixed in its monitoring-as-code file, because the API refuses to
  edit it.
- **Disabled monitors** are listed because they go DOWN when re-enabled. **Retired ones** are not:
  they do not run.

**Query 2 — stored addresses the mail rule refuses** (T-3; checked the same way). The regular
expression is an approximation of the rule. It agreed with Go's verdict on all 13 test cases
*(run)*, and the application's check is authoritative:

```sql
SELECT 'subscriber' AS kind, s.id::text, s.status_page_id::text AS owner, s.email AS address
  FROM subscribers s
 WHERE s.email !~ '^[A-Za-z0-9!#$%&''*+/=?^_`{|}~.-]+@([A-Za-z0-9.-]+|\[[0-9A-Fa-f:.]+\])$'
    OR octet_length(s.email) > 254
UNION ALL
SELECT 'channel ' || a.field, c.id::text, c.project_id::text, a.address
  FROM notification_channels c
  CROSS JOIN LATERAL (
        SELECT 'from' AS field, btrim(c.config->>'from') AS address
        UNION ALL
        SELECT 'to', btrim(t) FROM regexp_split_to_table(c.config->>'to', ',') AS t
       ) a
 WHERE c.type = 'email' AND a.address <> ''
   AND (a.address !~ '^[A-Za-z0-9!#$%&''*+/=?^_`{|}~.-]+@([A-Za-z0-9.-]+|\[[0-9A-Fa-f:.]+\])$'
        OR octet_length(a.address) > 254)
UNION ALL
SELECT 'instance mail from', 'instance_settings', '', btrim(i.mail->>'from')
  FROM instance_settings i
 WHERE COALESCE(btrim(i.mail->>'from'), '') <> ''
   AND (btrim(i.mail->>'from') !~ '^[A-Za-z0-9!#$%&''*+/=?^_`{|}~.-]+@([A-Za-z0-9.-]+|\[[0-9A-Fa-f:.]+\])$'
        OR octet_length(btrim(i.mail->>'from')) > 254);
```

- **Settings → Mail `from`** is the `from` key of `instance_settings.mail`
  (`internal/store/migrations/00032_instance_settings_mail.sql`, `domain.MailSettings`).
- **The YAML seed's `mail.from`** is not in the database until Settings → Mail is first saved, so
  the query cannot see it. It needs no query: `Config.Validate()` checks it at startup (§4.2, §8
  item 6).

**Query 3 — delivery failure text that quotes a URL** (T-4). Checked with `EXPLAIN` on
`cerbix_test` *(run)*:
- the `status` values are those of the constraint in `00080`;
- the pattern is case-insensitive and allows space after the quote;
- it matches the `*url.Error` text of §3.6, an upper-case `HTTPS://`, and `url.Parse`'s quote of an
  input with a leading space, and not `non-2xx status 302`.

Run it once after the upgrade:

```sql
UPDATE outbox_events
   SET last_error = 'delivery failed (text removed on upgrade: it quoted the delivery URL)'
 WHERE status IN ('pending', 'pending_fenced', 'dead')
   AND last_error ~* '"\s*https?://';
```

Delivered rows already have an empty `last_error` (`internal/store/outbox.go:211`).

**The T-1a upgrade note**, first in the release's CHANGELOG:

> **PostgreSQL monitors now use TLS as configured — read before upgrading.**
> - **What changed.** Until this release the PostgreSQL probe connected in plaintext whatever `sslmode`
>   said, and `verify-ca`/`verify-full` verified nothing. It now does what the setting says.
> - **What goes DOWN.**
>   - **`require`, `verify-ca`, `verify-full` against a server without TLS.** This includes every
>     postgres monitor saved since v0.1.5-beta.1 without an explicit choice, which was stored as
>     `require`. The monitor's message reads `the server does not offer TLS, and sslmode is
>     "require": set sslmode to "disable" if plaintext is intended`.
>   - **`verify-ca` and `verify-full` against a certificate the system roots do not trust**, such as a
>     private CA. Under `verify-full`, also a certificate not valid for the monitor's target name or
>     IP. There is no custom-CA setting yet; use `require` (encrypted, not verified) meanwhile.
>   - **Any other value**, which only a v0.1.0 beta or a direct database write could store: DOWN with
>     `bad config: unsupported sslmode`.
> - **What does not change.** `prefer` or no value (monitors from the v0.1.0 betas) keeps working:
>   TLS when the server offers it, plaintext otherwise.
> - **Before upgrading.** Run the runbook's query, which lists every postgres monitor with its target,
>   region, `sslmode`, enabled flag and whether a monitoring-as-code file owns it.
>   - Check TLS from each monitor's region.
>   - Fix a file-managed monitor in its file.
>   - Retired monitors are not listed; disabled ones are, because they go DOWN when re-enabled.
> - **Rolling upgrades.** Upgrade the core roles (`api`, `scheduler`, `all`) first. Then wait at
>   least the pause the runbook's query 4 gives (at most one day) before upgrading `worker` and
>   `agent`.
>   - **Why.** A new executor refuses a job that omits `sslmode` or `tls` on a body-bound carrier. Only
>     a new core states those keys for the old monitors that lack them, and jobs the old core already
>     queued live until the pause runs out.
>   - **If you skip the pause,** those monitors show `probe_error` `unsupported_version` until their
>     old jobs expire. Nothing else is affected.

**The T-3 upgrade note**, third in the CHANGELOG:

> **Alert mail: one-line encoded subjects, and mail addresses are checked.**
> - **Subjects.** The subject of every mail is its first line, at most 200 characters, encoded. The
>   full text is in the body.
> - **Startup.** An instance whose YAML has `mail.smtp_host` with an empty `mail.from`, or a
>   `mail.from` such as `Cerbix <alerts@example.com>`, will not start after the upgrade. The error
>   names `mail.from`.
>   - Today the first case silently runs with mail off, and the second fails every send at the
>     server.
>   - **Fix:** set `mail.from` to a bare address (`alerts@example.com`), or remove the `mail` block if
>     mail is configured in Settings → Mail.
>   - This applies even where Settings → Mail is saved and the YAML block is no longer used.
> - **Addresses** must be single bare ASCII addresses. The subscribe form, the email channel form and
>   Settings → Mail answer 400 to any other. Addresses already stored are not changed.
> - **Before upgrading.** Run the runbook's query 2. It lists subscribers, email-channel `from`/`to`
>   elements and the Settings → Mail `from` that will be refused. Fix them where they live:
>   - **Subscriber:** delete it from the status page's subscriber list
>     (`DELETE /api/v1/status-pages/{pageID}/subscribers/{subscriberID}`); the person subscribes
>     again with a valid address.
>   - **Email channel:** edit the channel.
>   - **Instance sender:** Settings → Mail.

**Rolling upgrade: the pause, its signal and recovery** (§2.6 item 5). These go into the runbook.
- **Who is affected.** Only enabled, unretired monitors whose stored config lacks a defaulted key:
  rows from the v0.1.0 betas. Query 4 gives the pause: the longest interval among them. A job they
  produce lives in the AMQP queue or in `pull_jobs` for at most that long. With no such monitor the
  answer is 0, and no pause is needed. Checked with `EXPLAIN` on `cerbix_test` *(run)*:

  ```sql
  SELECT COALESCE(max(m.interval_seconds), 0) AS pause_seconds
    FROM monitors m
   WHERE m.enabled AND m.retired_at IS NULL
     AND (   (m.type = 'postgres' AND NOT (m.config ? 'sslmode' AND m.config ? 'query'))
          OR (m.type = 'mysql'    AND NOT (m.config ? 'tls' AND m.config ? 'query'))
          OR (m.type = 'redis'    AND NOT m.config ? 'tls')
          OR (m.type = 'rabbitmq' AND m.config->>'mode' = 'management'
                                  AND NOT (m.config ? 'tls' AND m.config ? 'path')));
  ```

- **Order.**
  1. Upgrade the core roles.
  2. Wait `pause_seconds`.
  3. Upgrade the executors.
- **Signal if the order is broken.** For those monitors:
  - heartbeats of kind `probe_error` with reason `unsupported_version`;
  - the executor's log line with the same reason: `credential_job_rejected` from a worker
    (`internal/worker/worker.go:104`), `agent_credential_job_rejected` from a pull agent
    (`internal/agent/agent.go:361`).

  Each stops on its own once the old jobs have expired.
- **The region must not go credential-unready.** This change keeps it ready, because the refusal is
  mapped to `unsupported_version` (§2.6 item 5). If a region does show `credential_ready=false`:
  - **Signs:** the agent heartbeat or worker readiness says so, and the scheduler logs
    `credential_materialization_rejected` with `no_capable_executor`.
  - **Cause:** something else is failing to open envelopes, such as a wrong dispatch key.
  - **Recovery:** fix that cause, then restart the region's executors. Readiness is set from the
    keyring at startup, and nothing else clears it while the scheduler withholds work.

## 6. Acceptance

| Invariant | Test |
| --- | --- |
| S15 | TestPostgresProbeAsksForTLSAsSSLModeSays, TestPostgresProbeRefusesAServerWithoutTLS, TestPostgresPreferFallsBackToTheSameHostAndPort, TestPostgresVerifyFullChecksTheCertificate, TestPostgresProbeSendsSNIAsPgxDoes, TestHostPortStripsIPv6Brackets; E2E `probers.spec.ts` (`require` against the dev Postgres → down) |
| S16 | TestPostgresProbeRefusesAnSSLModeOutsideTheList, TestPostgresProbeIgnoresTheEnvironment, TestPostgresConfigResetCoversEveryField |
| S17 | TestDeliveryFollowsNoRedirect, TestWebhookDeliveryFollowsNoRedirect, TestDeliveryClientRefusesRedirects, TestRedirectedAlertIsNotMarkedDelivered, TestAgentFollowsNoRedirect |
| S18 | TestChannelMailCannotAddAHeader, TestMailerSubjectCannotAddAHeader, TestMailSubjectIsTheFirstLineAndBounded, TestMailSubjectIsEncodedWhenNotASCII, TestMailerRefusesAnAddressItCannotSend, TestChannelMailRefusesAnAddressItCannotSend, TestAddressesAreSingleAndUnnamedAtWrite, TestConfigRefusesAMailFromItCannotSend, TestSubscriberMailSkipsARefusedAddress |
| S19 | TestBodyBoundJobWithoutADefaultedKeyIsRefused, TestBodyBoundRefusalKeepsTheExecutorReady, TestMaterializeStatesTheLegacyValueOfAnAbsentKey |
| S20 | TestUpdateWithoutConfigKeepsTheStoredConfig, TestPatchWithoutConfigKeepsTheMonitorEditable |
| S21 | TestDeliveryErrorNamesNoURL, TestWebhookDeliveryErrorNamesNoURL, TestFailedDeliveryStoresAndLogsNoURL |

## 7. Out of scope

- **A custom CA for Postgres monitors** (`sslrootcert` as a setting). `verify-ca` and `verify-full` use
  the system roots, as the settings schema implies. A private CA needs a feature with its own spec.
- **`prefer` against an active interceptor.** `prefer` is opportunistic by definition (S15), so an
  interceptor that answers `N` gets plaintext.
  - Pinning the IP resolved for the TLS attempt to the plaintext attempt would not change that, and
    is not done. The guard checks every dial.
  - Moving a legacy row off `prefer` is an edit of its settings, which the editor makes.
- **`require_auth` and `channel_binding` as monitor settings.** Under `require`, which does not verify
  the server, an interceptor can still ask for the password in cleartext, and pgx answers.
  - libpq behaves the same way, and `verify-full` is the mode that closes it.
  - Refusing cleartext and MD5 authentication per monitor is a feature with its own spec.
- **A TLS-enabled Postgres in the dev stack** for a positive E2E of `verify-full`. The unit tests cover
  the handshake with a Go TLS listener (§8, decision 4).
- **Redirect policy of the OIDC and RabbitMQ management clients** (§3.5). Their endpoints are
  operator-trusted. A same-host redirect to another port or scheme keeps `Authorization` there; it is
  a candidate for its own review.
- **Per-channel delivery state.** One failing channel retries the whole event and re-sends to the
  healthy ones (§3.3). That is today's design; changing it is an outbox change with its own spec.
- **Found on the way, left alone:**
  - **Telegram `bot_token` in the URL path** (`internal/notify/notify.go:198`): formatted, not escaped.
    The host is fixed, so it cannot redirect delivery elsewhere.
  - **Recipient addresses in SMTP error text.** A server's reply can quote a recipient's address.
    It then reaches `last_error` (channel and confirmation mail) or the `subscriber_email_failed` log
    line. That is personal data, not a credential, so it is not T-4. For a subscriber the builder's
    refusal now names a position instead (§4.2); a server's own reply is passed through as today.
  - **Stale index row.** `docs/specs/README.md` still said "iteration 4 not started" for
    `sec-hardening-2` after its merge (`58b5a32`). Corrected in revision 1, together with this spec's
    row.

## 8. Decisions (owner, 2026-11-14)

The four questions of revision 1 were answered on 2026-11-14, following the recommendations of the
author and of the owner's reviewer:

1. **T-2: option A.** Delivery follows no redirect. A 3xx is a delivery failure that is retried and
   dead-lettered. The upgrade note names the symptom (`non-2xx status 30x`) and the fix: set the URL to
   its `Location`.
2. **T-1a: `sslmode` applies as written from this release, with no grace release.** The cost is
   accepted. The domain writes `require` by default, so after the upgrade every postgres monitor
   without an explicit `sslmode` that points at a server without TLS goes DOWN. Three conditions:
   - the monitor's message names the cause (§2.6 item 4);
   - the upgrade note is the **first** entry of the release's CHANGELOG;
   - the runbook carries the query that finds the affected monitors (§5).
3. **T-1a legacy rows.** An empty `sslmode` or `prefer` executes as `prefer`: TLS first, then plaintext
   to the same target if TLS fails. Any other legacy value is refused (S16). Revision 3 narrows
   "empty" to an absent key on a job that is not body-bound. On a body-bound job, core states
   `prefer` explicitly (§2.6 item 5, ruling P1-1).
4. **T-1a proof.** Unit tests at the protocol level: a fake server that records the `SSLRequest` and
   completes a TLS handshake. A TLS-enabled Postgres in the dev stack is a separate change.
5. **T-3 joins this package** as a finding of its own. Its scope covers every mail sender, not only
   channel email (§4).
6. **`mail.from` is checked at startup** (2026-11-15, the question of revision 5). `Config.Validate()`
   refuses:
   - a YAML mail block that sets `mail.smtp_host` and leaves `mail.from` empty;
   - a `mail.from` that the address rule refuses.

   Without `mail.smtp_host` the block is not checked. The refusal applies even when Settings → Mail
   has been saved and the seed no longer takes effect. §4.2 explains why that is the order, and why it
   does not change this decision.

The owner's rulings of 2026-11-15 on the review of revision 2 are recorded per remark in §10.1, and the
answers to revision 3's questions in §10.2.

## 9. Living documents corrected by revision 2

The promise that a postgres monitor with `sslmode: require` is encrypted was untrue in every released
version, v0.1.0-beta.1 through v0.3.9. The promise itself dates from v0.1.5-beta.1; `verify-full` has
been offered since the first beta. Each place that states it now carries a dated correction, and the
text of the decision or design is otherwise unchanged:
- `docs/decisions.md`: D-0062 (the probe's `prefer` default never attempted TLS either) and D-0155
  item (5);
- `docs/runbook.md`: the "PostgreSQL `sslmode=require`" row of the secret-inventory troubleshooting
  table;
- `docs/specs/func-secret-inventory.md`: the `postgres` row of §4.2 and §4.8.

Closed iteration reports are not edited. iter-0210 recorded the concatenation and not the missing TLS,
and it stays as written.

## 10. Reviews and the owner's answers

### 10.1 Dispositions (owner's rulings, 2026-11-15)

Independent review of revision 2 (`2bb6354`): **CHANGES REQUIRED**, 0 P0 / 4 P1 / 5 P2 / 2 P3. The
owner checked each remark against the code and ruled on it. "Run" means the claim was checked by
running code. The scratch programs and throwaway tests were not committed, and none touched the dev
database.

| Remark | Disposition | What changed | Checked by running |
| --- | --- | --- | --- |
| **P1-1** An envelope-v2 postgres job without `sslmode` executes as `prefer` | **Accepted, and widened.** On envelope v2 the executor refuses a job whose body omits the key, before connecting. The canonical default stays `require`. Absence stays executable as `prefer` only for generation-1 carriers and envelope v1, which covers the old rows. Where an old row reaches a v2 envelope, core states the key before sealing. The same defect exists for `tls` on mysql, redis and rabbitmq management, so the rule is stated over the registry (T-1c, raised to P1 by the owner, §10.2) | §1 T-1c; §2.3 (how an old row with `password_ref` or an inline password reaches execution: the former only by a direct database write, the latter through `MaterializeExecutionConfigs`); §2.6 items 1 and 5; S19; TestBodyBoundJobWithoutADefaultedKeyIsRefused (`delete(Config, "sslmode")` among its cases), TestMaterializeStatesTheLegacyValueOfAnAbsentKey | yes. Envelope v2 on carrier 3 sealed over a full body, then the key deleted: accepted for postgres, mysql and redis; changing the value is refused. A beta-style row with no `sslmode` was observed going out on carrier 3 / envelope v2 without the key |
| **P1-2** The constant string for `ParseConfig` still reads the environment, certificates and pgpass | **Accepted.** The new string does not enter `configTLS`, reads no certificate, key or pgpass file, and parses under invalid `PG*` values. `PGAPPNAME`, `PGOPTIONS`, `PGTZ` and `PGSSLNEGOTIATION` still get in, hence an explicit reset of every `pgconn.Config`/`pgx.ConnConfig` field, listed, with a reflection test. `PGSERVICE` is treated separately: it is read before the string applies and can only make the probe fail | §2.6 item 3; S16; TestPostgresProbeIgnoresTheEnvironment, TestPostgresConfigResetCoversEveryField | yes. pgx v5.10.0 under a hostile environment: the constant parses; `sslmode=disable` alone fails on `PGCONNECT_TIMEOUT`; garbage `PGSSLROOTCERT` breaks `host=localhost sslmode=require` and not the constant; a match-all `PGPASSFILE` fills the password for `sslmode=disable` and not for the constant; `RuntimeParams` keep `application_name`/`options`/`timezone`; `PGSERVICE` without a file fails, and with one adds `application_name` and `krbsrvname` |
| **P1-3** `ServerName` and IP targets | **Accepted.** `verify-ca` (and `require`) send the name as SNI as pgx does: a name yes, an IP literal no. `verify-full` with an IP target verifies the IP SAN. `hostPort` strips the brackets of an IPv6 target without a port | §2.6 item 2; S15; TestPostgresVerifyFullChecksTheCertificate (IP cases), TestPostgresProbeSendsSNIAsPgxDoes, TestHostPortStripsIPv6Brackets | the pgx rule was read in `configTLS` (`sslsni`, `net.ParseIP`); the `hostPort` behaviour was read in the code |
| **P1-4** A `PATCH` without `config` rewrites or refuses the stored config | **Accepted.** The boundary is in the store. The handler passes `configSubmitted`; when it is false, the store neither prepares nor writes `config` nor touches the refs | §1 T-1d; §2.2 table; §2.4; §2.6 item 6; S20; TestUpdateWithoutConfigKeepsTheStoredConfig, TestPatchWithoutConfigKeepsTheMonitorEditable | yes, against `cerbix_test`: absent → `require` written; `prefer` → the store's validation error, which the handler turns into 500 (read in `handlers_monitors.go:744-759`) |
| **P2-1** S15 promised "the same address" and implied more protection than `prefer` gives | **Accepted otherwise.** The IP is not pinned. S15 and §7 now say that `prefer` falls back to plaintext on the same host and port after `N` or any TLS-step failure, does not protect against an active interceptor, and every connection crosses the SSRF guard | S15; §2.6 item 2; §7; TestPostgresPreferFallsBackToTheSameHostAndPort | — |
| **P2-2** Addresses are checked only for CR/LF, only in the envelope | **Accepted; confirmed by the owner (§10.2)**, including the behaviour for stored rows. One rule (a single, unnamed, printable-ASCII address) in the message builder and at write: subscriber, email channel, mail settings → 400. Stored rows: no migration; how each kind behaves; a runbook query; a line in the upgrade note | §4.1; §4.2; S18; §5 query 2; TestMailerRefusesAnAddressItCannotSend, TestChannelMailRefusesAnAddressItCannotSend, TestAddressesAreSingleAndUnnamedAtWrite, TestSubscriberMailSkipsARefusedAddress | yes. Go's `net/mail` on 13 inputs, and the SQL approximation on the same 13 in `cerbix_test`: they agree |
| **P2-3** The subject of a multi-line alert | **Accepted; confirmed by the owner (§10.2).** The subject is the first non-blank line, up to 200 runes, folded per RFC 5322; the full text goes in the body | §4.1; §4.2; S18; TestMailSubjectIsTheFirstLineAndBounded | — |
| **P2-4** The runbook query and the upgrade note were too thin | **Accepted.** The query has `target`, `region`, `retired_at`, `managed_monitors` and lists values outside the list. The upgrade note covers `verify-*` with a private CA, the per-region check, file-managed monitors and the rolling-upgrade order | §5 query 1 and the upgrade note | yes. Column names checked with `EXPLAIN` on `cerbix_test` |
| **P2-5** The fallback clients and the test matrix | **Accepted.** The nil-client fallbacks of `notify.New` and `webhook.New` refuse redirects too. The matrix covers every status × channel kind × client, the outbox outcome and the agent, each with why it is red on `main` | §3.1; §3.3; S17; §3.4 | the header rule (`Hostname()`, port ignored) was read in the Go 1.27 standard library |
| **P3-1** The table of `Subject` sources in §4.1 is incomplete: `Project.Name` and the multi-line `SLAReport.Message()` (raw line breaks in every scheduled report, no attacker needed), the region name in `RegionWorkerAlert.Message()`, and `StatusPage.Title` | **Accepted** (the text arrived with revision 4's request). The table now lists every channel alert text with its renderer, delivery site, the database values in it and who writes them; `StatusPage.Title` is named in the confirmation row | §1 T-3; §4.1 (the second table and the SLA run) | yes. The SLA report through notify's format, `net/smtp`'s `DATA` writer and `net/mail`: the decoded subject is the whole report |
| **P3-2** The pull agent's client follows redirects; re-delivery while one channel redirects | **Accepted.** The agent's client refuses redirects (S17). The runbook says that healthy channels receive the alert again on each retry while one channel of the group answers a redirect | §1 T-2; §3.1–§3.5; S17; TestAgentFollowsNoRedirect; §5 runbook items | — |

### 10.2 The owner's answers to revision 3's questions (2026-11-15)

1. **P2-2, the address rule: confirmed**, including the behaviour for stored rows. One line was added
   saying that non-ASCII addresses are refused on purpose (§4.2). Writing it up exposed an error in
   revision 3, now corrected in §4.2: a refused subscriber never failed the incident event, because
   the fan-out only logs subscriber errors. The confirmed outcome is unchanged.
2. **P2-3, the subject rule: confirmed.**
3. **P3-1, the text** (in revision 3's §11, only its number had arrived): applied, §10.1.
4. **T-1c stays in the package at P1**, the severity of P1-1: the defect and the fix are the same
   (§1).
5. **Secret URLs in delivery error text join the package as T-4** (§1, §3.6, S21).
   - Every place the text is stored, logged or served is listed.
   - The pull agent and mail were checked the same way and are not affected: the agent's URLs hold
     no secret, and the library masks a userinfo password; mail errors carry no URL.

### 10.3 The reviewer's test list for P2-5, reconciled

| Reviewer's item | Revision 3 had | Revision 4 |
| --- | --- | --- |
| TestPostgresVerifyFullChecksTheCertificate: a certificate from a trusted root for another name passes `verify-ca`, which sends SNI, and fails `verify-full`; an IP target with an IP SAN under `verify-full` | the IP SAN case; not the other-name case | both cases (§2.8) |
| TestPostgresProbeRefusesAServerWithoutTLS: an empty `sslmode` answered with `N` | missing | an absent key (executes as `prefer`, plaintext reconnect to the same host and port) and an explicit `""` (refused, zero dials) |
| TestPostgresProbeIgnoresTheEnvironment: `PGSSLNEGOTIATION=direct`, `PGCHANNELBINDING=require`, `PGREQUIREAUTH=scram-sha-256`, `PGMINPROTOCOLVERSION=3.2`, `PGPORT=abc`; removing any reset must turn it red | `PGSSLNEGOTIATION=direct` and `PGPORT=abc` only; a reflection test over field names | all five variables, made observable on the wire (cleartext request, protocol 3.0, `SSLRequest` first). The poisoned-base half of TestPostgresConfigResetCoversEveryField covers each line of the reset, which the environment alone cannot reach, because the constant string already neutralizes it (§2.8) |
| S18: refusal of 0x00, CR, LF and 0x7f in both the mailer and notify, for the channel's comma-separated `to` and its `from` | the mailer only, without 0x00 | TestMailerRefusesAnAddressItCannotSend with 0x00; TestChannelMailRefusesAnAddressItCannotSend for the channel's `from` and a later `to` element; S18 states it for both builders |

### 10.4 Review of revision 4: dispositions (owner's rulings, 2026-11-15)

Independent review of revision 4 (`fd61e9f`): **CHANGES REQUIRED**, 0 P0 / 3 P1 / 5 P2 / 1 P3. The
owner compared each remark with the text of revision 4 and with the code. Some were written against
something other than revision 4.

| Remark | Disposition | What changed | Checked by running |
| --- | --- | --- | --- |
| **P1-1** | **Rejected: does not match the text.** The constant string in §2.6 item 3 has `require_auth=''`, not `any` | nothing | — |
| **P1-2** | **Rejected: does not match the text.** `prefer` falls back to plaintext after a failed handshake on purpose: that is the owner's ruling on P2-1 of the first review (S15, §7). The test the remark names is not in the spec | nothing | — |
| **P1-3** PATCH without `config` and `Monitor.Validate()` | **Accepted.** (a) Reproduced through the real handler and `Store`: a synthetic monitor answers 400 on `main`. That is a separate finding, T-5 (P2, a correctness defect), with its fix. (b) The sentence "the handler already skips validation without a config" is corrected: it skips the settings preparation, and always runs `Monitor.Validate()` (`handlers_monitors.go:724`). (c) S20 is narrowed: without a `config`, no settings preparation runs and `config` and `monitor_secret_refs` are not rewritten, while `Monitor.Validate()` stays. A legacy `prefer` row passes it. (d) The S20 test runs a synthetic monitor on the real `Store` | §1 T-5; §2.4; §2.6 item 6; S20; TestUpdateWithoutConfigKeepsTheStoredConfig, TestPatchWithoutConfigKeepsTheMonitorEditable | yes, in the `internal/appmux` harness on a throwaway database of the `cerbix_test` server. `PATCH {"enabled": false}`: synthetic → 400 `synthetic: config.scenario is required`; postgres `prefer` → 500 (`Validate()` passed, the store refused); postgres with no `sslmode` → 200, `require` written |
| **P2-1** (the rest) How the deliverers recognize a guard refusal and a certificate failure | **Accepted.** A new exported predicate, `prober.IsEgressRefusal`, over the guard's `*blockedIPError`; `*tls.CertificateVerificationError` for certificates; the whole classification table | §3.6 Fix | yes. Both classes recognized through `client.Do`'s error chain, with the guard's client |
| **P2-2** Query 2 misses the instance `from`; a startup check of `mail.from` | **Accepted in part.** Query 2 checks `instance_settings.mail->>'from'`. The startup check in `Config.Validate()`: the request for revision 5 carried a placeholder instead of the decision. The owner decided on 2026-11-15, after revision 5, to refuse at startup (§8 item 6, applied in revision 6) | §5 query 2; §4.2; §8 item 6 | yes. `EXPLAIN` on `cerbix_test` |
| **P2-3** Can an upgraded executor receive a job sealed by an old core, and does that block the region? | **Accepted.** Yes, it can receive one, through the AMQP queue or `pull_jobs`, for up to the monitor's interval. As revision 4 wrote it, the refusal would have counted as `decrypt_auth_failed`. Two in a row set `credential_ready=false`, the scheduler then withholds the region's credentialed jobs, and only a restart clears it. The refusal is now a typed `unsupported_version`, which never costs readiness. §5 gives the order, the pause (query 4), the signal and the recovery. Stated too: the check applies only to types with a credential schema. The test fixtures that seal envelope v2 over a body without the keys are named | §2.6 item 5; S19; §5; TestBodyBoundRefusalKeepsTheExecutorReady | the readiness path was read in the code; query 4 checked with `EXPLAIN` on `cerbix_test` |
| **P2-4** The reset table misses fields | **Rejected: does not match the text.** The table already has the three callbacks and `DefaultQueryExecMode`. One sentence was added: the walk over `pgx.ConnConfig` skips its embedded `Config`, whose fields are walked as `pgconn.Config`'s | §2.6 item 3 | — |
| **P2-5** Query 3 | **The table and `regexp_replace` part: rejected.** Query 3 is a different statement. **The pattern part: accepted.** It is now case-insensitive and allows space after the quote: `~* '"\s*https?://'` | §5 query 3 | yes. `EXPLAIN` on `cerbix_test`; the pattern matches `"https://`, `"HTTPS://` and `" https://`, and not `non-2xx status 302` |
| **P3-1** §4.7 of `func-secret-inventory.md` | **Rejected: does not match the text.** The amendment of its §4.7 is already planned, in the iteration's documentation list (§5) | nothing | — |

### 10.5 Review of revision 6: dispositions (owner's rulings, 2026-11-15)

Independent review of revision 6 (`6f82ff6`): **CHANGES REQUIRED**, 0 P0 / 0 P1 / 1 P2 / 3 P3, plus
P3-4, accepted with the approval. The
reviewer confirmed the five rejections of §10.4 and the other sections. The owner checked all four
remarks and accepted them.

| Remark | Disposition | What changed | Checked by running |
| --- | --- | --- | --- |
| **P2-1** The `TLS handshake failed` branch of §3.6 cannot match | **Accepted.** The branch is rewritten. A TLS alert over TCP is a `*net.OpError` with `Op` `"remote error"` or `"local error"`; `tls.RecordHeaderError` matches only as a value. An HTTPS request answered in plain HTTP has its own reason, through `http.ErrSchemeMismatch`. TestDeliveryErrorNamesNoURL gains these cases, each with a token in the URL and the reason it must name: an untrusted certificate, `https://` at an HTTP server, a TLS alert, bytes that are neither TLS nor HTTP | §3.6 Fix (the table and the run); TestDeliveryErrorNamesNoURL | yes. Six servers through `Guard.HTTPClient`: each chain as in §3.6. `tls.AlertError` and `*tls.RecordHeaderError` matched in none of them |
| **P3-1** §5 still sent the YAML `mail.from` to §11 | **Accepted.** The reference now points at the startup check (§4.2, §8 item 6) | §5, after query 2 | — |
| **P3-2** Only the worker's log line was named | **Accepted.** Both are named: `credential_job_rejected` (worker) and `agent_credential_job_rejected` (pull agent) | §5, the rolling-upgrade signal | — |
| **P3-3** A channel `from` with surrounding spaces passes at write and is sent untrimmed | **Accepted, second option.** `notify.sendMail` trims the channel `from` as it trims the `to` elements. A table in §4.2 shows that write and send now give one answer for every address; query 2 keeps `btrim`. S18 states it, and TestChannelMailRefusesAnAddressItCannotSend and TestAddressesAreSingleAndUnnamedAtWrite cover a padded `from` | §4.2; S18; §4.4 | the trimming on each path was read in the code (`handlers_subscribe.go:50`, `handlers_settings.go:218`, `notify.go:228-234`) |
| **P3-4** `docker/config.example.yaml:204` still promises the old behaviour ("leave smtp_host/from empty to disable both") | **Accepted, with the approval of revision 7.** It is added to §5's documentation list, together with the doc comment of `MailConfig.Enabled`. `README.md` and `INSTALL.md` were checked and do not describe the `mail` block | §5 | — |

### 10.6 Code review of the branch (at `6a90e5a`): dispositions (owner's rulings, 2026-11-16)

The implementation had a code review, CHANGES REQUIRED, 0 P0 / 1 P1 / 2 P2 / 1 P3. The owner checked each
finding against the code and accepted all four. One part of P1-1's recommendation was rejected.

| Finding | Disposition | What changed |
| --- | --- | --- |
| **P1-1** (T-1d) With `secrets.enabled: false`, a PATCH without `config` on a monitor that references the inventory passed both feature-switch checks; under an enforced dispatch envelope it could be enabled and re-targeted, and the secret sealed for the new target | **Accepted.** `updateMonitor` refuses it with `ErrSecretsFeatureDisabled` (404 `feature_disabled`) as on main, the pause included, no exception. A monitor without references stays editable; with the switch on, S20 holds | S20; `internal/store/monitors.go` (`updateMonitor`); TestAPartialUpdateHonoursTheSecretsSwitch |
| **P1-1, rejected part** Refusing to read the inventory in `materializeRow` and `MaterializeTestExecutionConfig` while the switch is off | **Rejected.** That is how the code works on main too, and the inventory spec says "nothing else changes" (`func-secret-inventory.md` §4.1). Not touched. The comment in `revisiontimeline_internal_test.go` that said the inventory "refuses even to be read" now says what the switch refuses: writes and reference resolution | the comment only |
| **P2-1** (T-2) `Agent.heartbeat` ignored the response status, and `claimTest` dropped a non-200 without a line | **Accepted.** `heartbeat` logs `agent_heartbeat_failed` with the status outside 2xx; `claimTest` logs `agent_test_claim_failed` for anything but 200 (no pending test is a 200 with `test: null`). The test checks poll, heartbeat, claimTest and postResults each with its own log | S17; `internal/agent/agent.go`; TestAgentFollowsNoRedirect |
| **P2-2** (T-3) `To` was one line (40 recipients: about 1 KB), and the address rule had no length bound | **Accepted.** `To` is folded between addresses; `domain.ValidMailAddress` refuses more than 254 octets, on all five surfaces | §4.2 (the rule); S18; TestToIsFoldedBetweenAddresses, TestAnAddressOver254OctetsIsRefusedOnEverySurface; §5 query 2 and the runbook gain the length condition |
| **P3-1** (T-3) An email channel `to` of only commas and spaces was accepted, and every send failed with "no recipients" | **Accepted.** Refused at write with the message an empty `to` gets | `NotificationChannel.Validate`; TestAnEmailChannelNeedsARecipient |

**Second pass** (at `1723920`): CHANGES REQUIRED, 0 P0 / 0 P1 / 1 P2 / 1 P3. The first pass's P2-1, P2-2 and
P3-1 were confirmed closed; both new findings were accepted.

| Finding | Disposition | What changed |
| --- | --- | --- |
| **P2-1** (P1-1's fix) The secrets-off refusal was decided on the config the handler read before the transaction, while the write keeps the stored config (COALESCE), so the checked and the kept could differ | **Accepted.** The check runs after `SELECT … FOR UPDATE` and refuses when the locked row has a `monitor_secret_refs` entry or its stored config holds a reference; the caller's `m.Config` plays no part. No lock on `project_secrets`; the §4.3 order is unchanged | S20; `internal/store/monitors.go` (`refuseIfLockedRowReferencesTx`); TestAConfigLessUpdateChecksTheLockedRowForReferences |
| **P3-1** The claimTest subtest checked only for "307" | **Accepted.** It requires `agent_test_claim_failed` too; this table's row of TestAgentFollowsNoRedirect names the test-claim endpoint | §3.4; `internal/agent/redirect_test.go` |

**Final pass** (at `7e3bda6`, range `1723920..7e3bda6`): **APPROVED**, 0 P0 / 0 P1 / 0 P2 / 0 P3. Both
findings of the second pass were confirmed closed. The reviewer repeated the three bypasses of P2-1 at the
Store and through `PATCH /api/v1/monitors/{id}` on a throwaway PostgreSQL 16:
- **(c), the race.** The concurrent reference was injected by a hook on the live PATCH.
- **A synthetic scenario binding.** A synthetic monitor whose binding lives only in the plain
  `scenario_secret_<binding>_ref` key of the stored config, with its `monitor_secret_refs` row removed, is
  refused.
- **Locks.** Concurrent secret rotations against config-less updates produced no deadlock.

## 11. Open for the owner

Nothing open.
