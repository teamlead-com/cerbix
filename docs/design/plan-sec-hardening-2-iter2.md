# sec-hardening-2 iteration 2 (H-2, H-13, H-1 fix 8) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:executing-plans (owner: no subagents; the controller implements inline). Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close H-2 (probe input reaches the socket; WebSocket TLS always skipped), H-13 (credentials, bodies and `Referer` follow redirects to another origin, including the two-hop canary defect live on `main`), and H-1 fix 8 (a token mints a token) exactly as `docs/specs/sec-hardening-2.md` revision 7 (as amended 2026-11-14) specifies.

**Architecture:**
- H-2: each target is parsed by its protocol's parser, in `domain` (every write surface goes through `Monitor.Validate`) and again in the prober before it dials. The WebSocket handshake is built with `net/http`.
- H-13: one redirect predicate, "has the chain left the origin of `via[0]`?", shared by the Runner's clients and the canary, with the per-client behaviour from the spec's table.
- Fix 8: API token create/delete refuse `ViaToken` principals.

**Tech Stack:** Go 1.26 (`net/http`, `net/url`, `crypto/tls`), goose migrations, Vue 3 + TS, Playwright E2E.

**Spec:** `docs/specs/sec-hardening-2.md` §2 H-2, §3 H-13, §2 H-1 fix 8, §5 row 2. Amends D-0218 / `docs/specs/func-async-canary.md` D3c1 (three points, H-13).

**Branch / worktree:** `sec-hardening-2-iter2` from `main` (`181afde`), worktree `.claude/worktrees/sec-hardening-2-iter2`. Commit only there; push happens after the whole spec (owner, 2026-11-14).

## Global Constraints

- The spec is the source of truth. If code or a test contradicts it, stop and report; do not pick a side.
- No change outside H-2, H-13, H-1 fix 8 (and the docs they name).
- Tests come first: RED on the current code, then GREEN.
- Store tests use `cerbix_test` only, on both storage modes.
- Next free numbers: migration `00113`, iteration `iter-0210`, decision `D-0277`.
- The setting name is `tls_skip_verify`, default `false`. It is a **plain typed setting** for `websocket`, not a credential schema.
- WebSocket target schemes: `ws`, `wss`, `http`, `https`.
- A refusal reason starts with `redirect refused:` and contains no `://`.
- `canaryMaxRedirects = 3` is unchanged. The Runner's hop limit is the library's 10.
- The tooltip and runbook copy, verbatim: "global administration is for people; automation gets organization or project rights".
- Commit messages follow repo style and end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Owner rulings (2026-11-14)

1. **Socket targets** (`ssh`, `redis`, `rabbitmq` AMQP mode): `host` or `host:port`. The port is optional because the UI documents "port defaults to …", but when present it must be numeric and in 1–65535. The host must be non-empty with no whitespace or control bytes.
2. **WebSocket targets without a scheme:** migration 00113 prefixes `ws://`, which is the meaning the prober gave them. New writes require a scheme.

## Decisions this plan makes

- **Rabbit management `path`** must start with `/` and must not start with `//`. It may contain no `@`, no `\`, no whitespace and no control byte. The URL is assembled as `url.URL{Scheme, Host, Path, RawQuery}`; the path's own `?query` is split off into `RawQuery`.
- **The WebSocket TLS config** is `ServerName: host`, `MinVersion: tls.VersionTLS12`, `InsecureSkipVerify: config["tls_skip_verify"] == "true"`.
- **The plain typed schema** accepts only `tls_skip_verify` with the values `true`/`false` (other values are an error). The key is emitted only when given, and absent means false.
- **The API** calls `PrepareTypedSettings` for `HasTypedSettings(type)` (credentialed **or** plain-typed). Other types (composite, synthetic, canary, …) keep their current path.
- **Migration 00113** changes config and target in place. It does **not** bump `execution_revision`, because probe behaviour is preserved: those monitors already skipped verification and already used `ws://`.
- **Runner origin:** the lower-cased host plus an explicit port (the default port by scheme).

## Review Focus

1. A WebSocket target with an IPv6 literal (`ws://[::1]:8080/x`) parses, dials `[::1]:8080` and sends `Host: [::1]:8080`. Task 3.
2. A Rabbit management target given as a URL with its own path and query (`https://rabbit:15671/base`) plus `path=/api/overview?columns=x` yields `https://rabbit:15671/api/overview?columns=x`. Task 3.
3. Redirect `http://A:80 → https://A:443 → https://B` keeps credentials on the upgrade hop and strips them at `B`. Task 6.
4. A synthetic step with an **empty** body and a 307 to `B`: no body means no refusal, so it is delivered with its headers stripped. Task 6.
5. A canary redirect A → B → A, back to the first origin, is still "left", so the binding headers stay stripped. Task 7.

---

### Task 1: SPA mock delta (owner gate)

**Files:** Create `docs/design/mock-websocket-tls.html`, in the style of `docs/design/mock-machine-clients.html`.

Two states:
- (a) **The WebSocket monitor editor in New Monitor.** A "Skip TLS certificate verification" checkbox uses the existing redis/mysql/rabbitmq TLS control pattern (`NewMonitorView.vue`, `tlsSettings.skipVerify`). It is shown only when the target starts `wss://` or `https://`, with this help text: "Accept self-signed or otherwise unverifiable certificates. Off by default; existing wss monitors were switched on at upgrade so none went DOWN."
- (b) **The Users list.** The machine row's disabled "Grant admin" carries the tooltip "A machine identity cannot be a global admin — global administration is for people; automation gets organization or project rights".

- [ ] Write the mock, commit `docs: mock for the WebSocket TLS control and the machine tooltip (sec-hardening-2 iteration 2)`.
- [ ] Show it to the owner. **Task 9 waits for approval**, while the backend continues.

### Task 2: Target rules in `domain` (S2 at the three write surfaces)

**Files:** Create `internal/domain/targets.go` and `internal/domain/targets_test.go`. Modify `internal/domain/monitor.go` (`Validate`).

**Interfaces (Produces):**
- `func ParseWebSocketTarget(target string) (*url.URL, error)`. Trims, refuses any control byte or whitespace inside, requires `url.Parse` success, scheme ∈ {ws,wss,http,https} (lower-cased), a non-empty `Hostname()`, no `User` and no `Fragment`. A port, when present, must be numeric 1–65535.
- `func ValidateSocketTarget(target string) error`. Applies the `host[:port]` rule of owner ruling 1.
- `func ValidateRabbitMgmtPath(path string) error`. Applies the rule from the decisions above. Empty is allowed and means `/api/overview`.
- `Monitor.Validate`:
  - `websocket` → `ParseWebSocketTarget`;
  - `ssh`, `redis`, and `rabbitmq` with `config["mode"] != "management"` → `ValidateSocketTarget`;
  - `rabbitmq` management → the target is either a URL whose scheme is `http`/`https`, with a host and no userinfo, or a socket target; plus `ValidateRabbitMgmtPath(config["path"])`.
  - Messages never echo the target.

- [ ] **Step 1: Failing tests.**
  - `TestParseWebSocketTarget` table:
    - accepted: `ws://h/x`, `wss://h:8443/a?b=1`, `http://h`, `https://[::1]:9/x`;
    - refused: `h:80/x` (no scheme), `ftp://h`, `ws://h/a\r\nX: y`, `ws:// h`, `ws://`, `ws://u:p@h`, `ws://h:99999`, `ws://h/#f`.
  - `TestValidateSocketTarget` table:
    - accepted: `redis.internal`, `redis.internal:6379`, `[::1]:22`;
    - refused: `""`, `":22"`, `h:abc`, `h:0`, `h:70000`, `h\r\n`, `h x`, `h\x00`.
  - `TestValidateRabbitMgmtPath`:
    - accepted: `""`, `/api/overview`, `/api/x?y=1`;
    - refused: `api`, `//evil/x`, `/@10.0.0.9:8080/x`, `@10.0.0.9:8080/x`, `/a\r\n`, `/a b`, `/a\\b`.
  - `TestMonitorValidateRefusesHostileTargets`: one hostile case per type through `Monitor.Validate`, and the error text never contains the target.
- [ ] **Step 2:** `go test ./internal/domain/ -run 'WebSocketTarget|SocketTarget|RabbitMgmtPath|HostileTargets' -count=1`. Expected: FAIL (undefined).
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test ./internal/domain/ ./internal/api/ ./internal/fileprovider/ -count=1`. Expected: PASS. Fix existing fixtures that used a scheme-less websocket target only if the spec requires it; otherwise stop and report.
- [ ] **Step 5:** Add a surface test: `TestMonitorWriteSurfacesRefuseHostileTargets` (internal/api). For create, update and test-connection, a websocket target with `\r\n` and a rabbit path `@host` each give 400. Add one MaC decode case in the `internal/fileprovider` tests, giving `domain_invalid`.
- [ ] **Step 6:** Commit `fix: every write surface parses probe targets with their protocol's parser (sec-hardening-2 H-2)`.

### Task 3: Probers refuse before dialing; the WebSocket handshake through `net/http`

**Files:** Modify `internal/prober/websocket.go`, `ssh.go`, `redis.go`, `rabbitmq.go` (`rabbitMgmtURL`, AMQP). Test: `internal/prober/targets_test.go` (new).

**Interfaces (Consumes):** Task 2's three functions.
- `websocketProber.Probe`:
  - `u, err := domain.ParseWebSocketTarget(m.Target)`; on error, return `Msg: "bad target"` **before** `p.dial`.
  - Dial `u.Host` with the default port by scheme.
  - Use TLS as decided above.
  - Build `req, _ := http.NewRequest("GET", u.String(), nil)` with the scheme mapped to http/https for the request line. Set `req.Host = u.Host` and the four upgrade headers, then `req.Write(conn)`. `wsTarget` is deleted.
- `rabbitMgmtURL(target, path string, tlsEnabled bool) (string, error)` assembles a `url.URL`. It returns an error for an invalid path or target.

- [ ] **Step 1: Failing tests.**
  - `TestProbersRefuseHostileTargetsBeforeDialing`: for each of websocket, ssh, redis, rabbitmq AMQP and rabbitmq management, a monitor that bypassed `Validate` is probed with a `dial` (or http client) that records calls. Expect `Connected=false` and zero dial calls.
  - `TestWebSocketSelfSignedIsDownByDefault`: an `httptest.NewTLSServer` WebSocket fixture (it answers 101 with the right Accept). Without `tls_skip_verify` → DOWN with a `tls:` message; with `tls_skip_verify=true` → UP.
  - `TestWebSocketHandshakeWire`: a raw TCP fixture captures the request bytes. Check the request line `GET /chat?x=1 HTTP/1.1` and `Host: 127.0.0.1:<port>`. Also check the IPv6 case (Review Focus 1, skipped when `[::1]` cannot listen).
  - `TestRabbitMgmtURLAssembled`: Review Focus 2, plus `path="@10.0.0.9:8080/x"` → error.
- [ ] **Step 2:** Run `go test ./internal/prober/ -run '<names>' -count=1`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test -race ./internal/prober/ -count=1`. Expected: PASS. Existing websocket tests that relied on skip-verify against a self-signed fixture must set `tls_skip_verify`; that is a spec-mandated change, so list it in the report.
- [ ] **Step 5:** Commit `fix: probers parse their target before dialing; WebSocket TLS verifies unless tls_skip_verify (sec-hardening-2 H-2)`.

### Task 4: `tls_skip_verify` as a plain typed setting (API and MaC)

**Files:** Modify `internal/domain/typedsettings.go` and `internal/api/handlers_monitors.go` (create ~:265, test ~:412, update ~:656). Test `internal/domain/typedsettings_test.go`, `internal/api/` (monitor create/update), and `internal/fileprovider/` (round-trip).

**Interfaces (Produces):**
- `func HasTypedSettings(typ MonitorType) bool`
- A plain schema `plainSettingsSchemas = map[MonitorType]map[string]plainSetting{MonitorWebSocket: {"tls_skip_verify": {boolean: true}}}`, consulted by `PrepareTypedSettings` for non-credentialed types.

- [ ] **Step 1: Failing tests.**
  - `TestPrepareTypedSettingsWebSocket`: `{"tls_skip_verify":"true"}` → same; `{"tls_skip_verify":"yes"}` → error; `{"other":"x"}` → `ErrUnknownSetting`; `{}` → empty.
  - The API create of a websocket monitor with `config.tls_skip_verify="true"` persists it, and `"maybe"` → 400. The test-connection path does the same.
  - `TestFileProviderWebSocketTLSSkipVerifyRoundTrip`: a bundle with `settings: {tls_skip_verify: "true"}` applies, and the stored config carries it.
  - A composite monitor's `children`/`quorum` config is still accepted by the API (regression guard for `HasTypedSettings`).
- [ ] **Step 2:** Run them. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test ./internal/domain/ ./internal/api/ ./internal/fileprovider/ -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit `feat: tls_skip_verify for WebSocket monitors, validated on the API and in Monitoring-as-Code (sec-hardening-2 H-2)`.

### Task 5: Migration 00113 (existing WebSocket monitors keep their behaviour)

**Files:** Create `internal/store/migrations/00113_websocket_tls_skip_verify.sql` and test `internal/store/websockettls_internal_test.go`.

The Up section is idempotent:
- `UPDATE monitors SET target = 'ws://' || btrim(target) WHERE type='websocket' AND btrim(target) !~* '^(ws|wss|http|https)://'`.
- `UPDATE monitors SET config = coalesce(config,'{}'::jsonb) || '{"tls_skip_verify":"true"}' WHERE type='websocket' AND btrim(target) ~* '^(wss|https)://' AND NOT (coalesce(config,'{}'::jsonb) ? 'tls_skip_verify')`. Check the real column type of `config` first and adapt.
- Down: no-op, with a comment.

- [ ] **Step 1: Failing test** `TestWebSocketTLSMigration`. Insert websocket rows `wss://a`, `https://b`, `ws://c`, `d.internal:80/x`, and a `wss://e` that already has `tls_skip_verify=false`, plus one redis row with `tls_skip_verify` absent. Re-run the Up section, then assert:
  - `a` and `b` get `true`;
  - `c` is untouched;
  - `d` becomes `ws://d.internal:80/x` with no key;
  - `e` keeps `false`;
  - the redis row is untouched;
  - `execution_revision` is unchanged everywhere.
- [ ] **Step 2:** Expected: FAIL (file missing).
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the test on both modes.
- [ ] **Step 5:** Commit `fix: migration 00113 keeps existing WebSocket monitors working under verified TLS (sec-hardening-2 H-2)`.

### Task 6: The Runner's redirect policy (S13, Runner column)

**Files:** Create `internal/prober/redirect.go` and `internal/prober/redirect_test.go`. Modify `internal/prober/prober.go` (`NewRunnerWithGuard`: the http, promql, rabbitmq and synthetic clients get `CheckRedirect: runnerCheckRedirect`) and `internal/prober/rabbitmq.go` (the skip-verify branch copies `CheckRedirect`).

**Interfaces (Produces):**
- `func requestOrigin(u *url.URL) string`: the lower-cased `scheme://host:port`, with the port defaulted by scheme.
- `func chainLeftOrigin(req *http.Request, via []*http.Request, allowUpgrade bool) bool`. It is true if `req` or any of `via[1:]` has an origin other than `via[0]`'s. With `allowUpgrade`, a hop `http://h:80 → https://h:443` where `h` is `via[0]`'s host does not count. Both clients use it.
- `func runnerCheckRedirect(req *http.Request, via []*http.Request) error`:
  - The limit is 10 hops (`len(via) >= 10` → error).
  - Refuse (return an error starting `redirect refused:`) when:
    - the scheme is not http/https;
    - `req.URL.User != nil`;
    - the hop is a downgrade (`https` in any earlier hop, `http` now);
    - the chain has left the origin and `req.Body != nil && req.Body != http.NoBody` (307/308).
  - Otherwise, when the chain has left: delete from `req.Header` every name present in `via[0].Header`.
  - Always: `req.Header.Del("Referer")`.

- [ ] **Step 1: Failing tests** (`httptest` servers A and B on different ports; a client with `CheckRedirect: runnerCheckRedirect`; A's handler redirects per case; B records the headers, body and `Referer` it received). The first request carries `Authorization: Basic x` and `X-Api-Key: k`.
  - `TestRunnerRedirectCrossOriginStripsHeaders`: A → B, nothing reaches B.
  - `TestRunnerRedirectTwoHopStaysStripped`: A → B/1 → B/2. This is RED on main.
  - `TestRunnerRedirectBackToOriginStaysStripped`: A → B → A; the second visit to A sees no credential.
  - `TestRunnerRedirectOtherPortIsCrossOrigin`.
  - `TestRunnerRedirectHTTPSUpgradeKeepsCredentials`: `http://A` → `https://A` on the same host keeps them (TLS fixture via the `httptest` TLS server on a mapped port; if two listeners cannot share a host name, model it with `chainLeftOrigin` unit cases instead), plus Review Focus 3.
  - `TestRunnerRedirectRefusesDowngradeUserinfoAndScheme`: `https→http`, a `Location` with `u:p@`, `ftp://`. All are refused, and the message contains no `://`.
  - `TestRunnerRedirectNeverSendsReferer`: a target with `?token=t` and a 302 to B leaves no `Referer` at B.
  - `TestRunnerRedirectRefusesCrossOriginBody`: POST with a body + 307 to B → refused, nothing at B. A 307 within A is delivered with its body.
  - `TestSyntheticSecretBodyNeverCrossesOrigin` (in `internal/prober`, modelled on `canaryenvelopeexec_test.go`). It drives a synthetic job through `dispatch.ValidateAndMaterialize`, covering three cases:
    - a step body with `{{secret:b}}`;
    - a step whose body is a `{{var}}` extracted from step 1;
    - a static body.
    For each case, a 307 to B is refused and a 307 within A is delivered. Review Focus 4: an empty body and a 307 to B → delivered.
  - `TestRabbitSkipVerifyClientKeepsTheRedirectRule`: a management probe with `tls_skip_verify` followed across origins → Basic auth does not reach B.
- [ ] **Step 2:** Run them. Expected: FAIL (undefined, or headers seen at B).
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test -race ./internal/prober/ ./internal/dispatch/ -count=1`. Expected: PASS. A refusal message must survive `failureClass` (assert the classified reason is not empty or unclassified).
- [ ] **Step 5:** Commit `fix: the Runner's HTTP clients keep credentials, bodies and Referer inside the first request's origin (sec-hardening-2 H-13)`.

### Task 7: The canary's redirect policy (S13, canary column; amends D-0218/D3c1)

**Files:** Modify `internal/prober/canary.go`:
- `CheckRedirect` compares with `via[0]` using `chainLeftOrigin(req, via, false)`.
- `Referer` is deleted on every hop.
- The secret-bearing marker travels on the context: `canaryWithSecretBody(ctx)` and `canarySecretBody(ctx) bool`.
- `canaryRenderValue` returns `(any, bool, error)`, where the bool means a secret was resolved, recursively.
- `canaryBuildBody` returns `(body []byte, contentType string, secretBearing bool, err error)`.
- The new refusal: left && body present && secret-bearing → `redirect refused: secret-bearing body to another origin`.

Also modify `internal/prober/canary_test.go`, `docs/decisions.md` (D-0218 amendment note) and `docs/specs/func-async-canary.md` (D3c1 amended by points (a), (b) and (c), verbatim from the spec).

- [ ] **Step 1: Tests.**
  - Change `TestCanaryDropsEveryBindingHeaderOnACrossHostRedirect` and `TestCanaryProvenanceNeverReachesEitherTargetOnTheWire`: each `mutate` drops the `token` field from the body, as the spec says. Their assertions stay.
  - New tests:
    - `TestCanaryTwoHopRedirectStaysStripped`: A → B/1 → B/2. RED on main.
    - `TestCanaryRedirectBackToOriginStaysStripped`: Review Focus 5.
    - `TestCanaryRefusesSecretBearingBodyAcrossOrigin`: three subtests for a top-level secret, a secret nested in a list, and a multipart field. A 307 to B → stage failure, nothing at B, a reason without `://`.
    - `TestCanaryDeliversBodyWithoutSecretAcrossOrigin`: the 307 upload flow still works.
    - `TestCanaryNeverSendsReferer`.
  - The same-origin, downgrade and userinfo tests stay unmodified and green.
- [ ] **Step 2:** Run `go test ./internal/prober/ -run Canary -count=1`. Expected: the new tests FAIL.
- [ ] **Step 3:** Implement and amend the two documents.
- [ ] **Step 4:** Run `go test -race ./internal/prober/ -count=1` and `python3 scripts/check-docs-references.py`. Expected: PASS.
- [ ] **Step 5:** Commit `fix: the canary compares every hop with the first request's origin and never forwards a secret-bearing body (sec-hardening-2 H-13, amends D-0218)`.

### Task 8: A token never mints a token (H-1 fix 8)

**Files:** Modify `internal/api/handlers_apitokens.go` (create, delete). Test in `internal/api/api_apitokens_test.go`.

- [ ] **Step 1: Failing test** `TestApiTokenWritesRefuseTokenPrincipals`.
  - For a `cbx_` token principal (`ViaToken`, role org_admin) and an OIDC machine principal (`ViaToken`), POST and DELETE `/api/v1/organizations/o1/api-tokens…` give 403 `{"error":"api tokens are managed by people, not tokens"}`. The store is not written.
  - GET (list) still gives 200.
  - A session org admin can still create and delete.
- [ ] **Step 2:** Expected: FAIL (201/204).
- [ ] **Step 3:** Put the check first in both handlers, the same shape as `projectMembersAccess`.
- [ ] **Step 4:** Run `go test ./internal/api/ -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit `fix: API tokens are created and deleted by people, never by a token (sec-hardening-2 H-1 fix 8)`.

### Task 9: SPA (after the owner approves the mock)

**Files:** Modify `frontend/src/views/NewMonitorView.vue` (WebSocket TLS checkbox: load and save `tls_skip_verify`, shown for `wss://`/`https://`), `frontend/src/components/settings/UsersPanel.vue` and `UsersPanel.spec.ts` (tooltip copy). Add a vitest spec for the WebSocket control (`frontend/src/views/NewMonitorWebSocketTLS.spec.ts`), then run `make spa-snapshot`.

- [ ] RED vitest first: the control appears for `wss://`, sends `config.tls_skip_verify`, and is absent for `ws://`; the new tooltip text. Then implement, run `npm test` with the repo root mounted, run `npm run build`, run `gmake spa-snapshot`, and commit `feat: WebSocket TLS verification control; the machine tooltip names the right tool (sec-hardening-2 iteration 2)`.

### Task 10: OpenAPI and generated types

- [ ] `openapi.yaml`: 403 on `POST/DELETE` of API tokens for token principals; the WebSocket `tls_skip_verify` setting in the monitor config description. Run `gen:api`, then run `go test ./internal/api/` (the request-contract tests). Commit `docs: OpenAPI for iteration 2 (token-issued tokens refused, WebSocket tls_skip_verify)`.

### Task 11: Verification

- [ ] Run `go build -buildvcs=false ./... && go vet ./...`.
- [ ] Run `-race` on both storage modes (TimescaleDB :55433, plain :55432, `cerbix_test`), with GNU Make 4 on `PATH`.
- [ ] Run vitest with the repo root mounted.
- [ ] Run `gmake dev-up && gmake dev-test`, then `python3 scripts/check-docs-references.py`.

### Task 12: Close the iteration (docs commit)

- `docs/iterations/iter-0210.md`: scope, owner rulings, gates, the tests the spec changed, and findings left alone.
- `docs/decisions.md` D-0277: the H-2 parsing rules and owner rulings, the redirect predicate and its table, fix 8. It also records the D-0218 amendment.
- `docs/traceability.md` row; `docs/runbook.md` (WebSocket `tls_skip_verify`, redirect rule, the fix 8 wording replacing "use an API token"); `docs/overview.md` if affected.
- `CHANGELOG.md` `[v0.4.0] - Unreleased`:
  - WebSocket TLS is verified;
  - migration 00113;
  - **MaC users add `tls_skip_verify: "true"` to their bundle, or the next apply resets it**;
  - redirects no longer carry credentials, bodies or `Referer` across origins;
  - a token can no longer create or delete API tokens.
- `docs/specs/sec-hardening-2.md` status line: iteration 2 implemented.
