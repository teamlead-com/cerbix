# sec-hardening-2 iteration 1 (H-1, H-5, H-6) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close H-1 (a realm JWT for any client authenticates as a human), H-5 (a password reset keeps sessions alive) and H-6 (unbounded bodies/passwords, a limiter-free `reset/confirm`) exactly as `docs/specs/sec-hardening-2.md` revision 7 specifies.

**Architecture:** H-1 splits OIDC provisioning into a machine path (bearer, refuses and rolls back on a `human` row) and a human path (callback, `FOR UPDATE`, strips a machine's grants on the flip), gated by a new `machine_clients` OIDC setting and a `users.kind` column. H-5 moves session revocation into the password-writing transactions. H-6 caps password bytes in the hash primitives, caps bodies at the router edge, and splits the login limiter into three.

**Tech Stack:** Go 1.26, pgx/v5, goose migrations, go-oidc/v3, Vue 3 + TS (openapi-typescript), Playwright E2E.

**Spec:** `docs/specs/sec-hardening-2.md` §2 H-1, §3 H-5, §3 H-6, §5 row 1, §8. Decisions amended: D-0034, D-0139 item 3 (context: D-0043).

**Branch / worktree:** branch `sec-hardening-2-iter1` (spec commit `d440731`); all work in a worktree of that branch. Commit only there; never push, merge or open a PR.

## Global Constraints

- The spec is the source of truth. If code or an existing test contradicts it, STOP and report to the controller; do not pick a side.
- No change outside H-1, H-5, H-6. Anything else found goes into the iteration report, not the code.
- Tests first: each new test must FAIL before the task's code and PASS after.
- Store tests are DB-gated: `export CERBIX_TEST_DATABASE_DSN="postgres://cerbix:cerbix@localhost:5432/cerbix_test?sslmode=disable"`. Never the dev `cerbix` DB.
- Next free numbers: migration `00112`, iteration `iter-0209`, decision `D-0276`.
- `machine_clients` default is empty, and empty means the JWT bearer path is off. The setting may not contain the effective interactive `client_id`.
- Client claim set, in this order: `azp`, `client_id`, `cid`, `appid`. All non-empty members of the set must be equal.
- Delegated markers refused: non-empty `nonce`, non-empty `sid`, non-empty `cid` that differs from `sub`. Do NOT refuse on `scp`.
- Backfill predicate, verbatim: `oidc_sub IS NOT NULL AND last_seen_at IS NULL AND password_hash IS NULL AND NOT is_global_admin AND email LIKE '%@clients'`.
- Log event `oidc_bearer_refused` (fields `reason`, `client_claim`, `client`, `issuer`, `sub`); metric `cerbix_oidc_bearer_refused_total{reason}`.
- Password cap 1024 **bytes** (`len(s)`, not runes) in `auth.HashPassword` and `auth.VerifyPassword`.
- Body caps: 4 KiB (`4 << 10`) on `/auth/local/*`; 1 MiB (`1 << 20`) default on the session/bearer API router; smaller per-handler caps stay.
- API tokens are never revoked by a password change or reset.
- Commit messages: repo style (`feat:`/`fix:`/`test:`/`docs:`), ending with the line `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Decisions this plan makes (the spec leaves them open)

- **Refusal reasons** (bounded set, used by both the log and the metric): `invalid_token`, `invalid_claims`, `machine_clients_empty`, `client_claim_missing`, `client_claims_disagree`, `client_not_listed`, `delegated_nonce`, `delegated_sid`, `delegated_cid_sub`, `subject_is_human`. `invalid_token` (signature/issuer/expiry failure) is counted but logged at Debug, because any unknown bearer string lands there. Every other reason is logged at Warn.
- **Store sentinel:** `store.ErrSubjectIsHuman = errors.New("store: oidc subject belongs to a human user")`.
- **Audit on machine → human:** one instance-level row (`org_id` NULL, action `user.kind_human`) and one row per affected org (action `member.revoke_machine_grants`) listing the removed memberships, all written in the flip transaction with actor = the user.
- **Limiter thresholds**, from `local.login_rate_limit_per_minute` = L (default 10; L ≤ 0 disables all three): per-IP L/min (separate buckets `login|<ip>` and `reset|<ip>`, where request and confirm share `reset|`); per-(IP, normalized email) `max(1, (L+1)/2)`/min; per-email failures-only `5*L`/min. Normalized email is `strings.ToLower(strings.TrimSpace(s))`, used only as a limiter key, never as the lookup.
- **E2E dedicated user (owner ruling 2026-11-13):** seeded into the dev stack's DB by `make dev-ready-single`, with no product surface.
- **The `kind` badge** is shown for machine identities only, unless the owner's mock review says otherwise.

## Review Focus

1. A token whose `azp`/`client_id`/`cid`/`appid`/`nonce`/`sid` claim is not a string (for example an array): the claims decode fails, the request gets 401 `invalid_claims`, never 500 and never acceptance. Test in Task 4.
2. A `machine_clients` entry with surrounding whitespace, duplicates, empty strings, or the own `client_id` padded with spaces: the entries are trimmed, deduplicated and emptied before validation, so a padded own `client_id` is still refused. Test in Task 3.
3. A password of exactly 1024 bytes is accepted. A password of 1025 bytes, or 342 three-byte runes (1026 bytes, only 342 characters), is refused, and the refusal is 400, not 500, on reset/confirm and `/me/password`. Test in Task 7.
4. Mixed-case or space-padded email (`" Admin@X.com"`) at login: it shares limiter buckets with `admin@x.com`. Test in Task 8.
5. The first-insert race: callback and bearer for a never-seen `sub` at the same instant must end with exactly one row. If the bearer won, the row is flipped and stripped. The callback never gets a 500. Test in Task 2.

---

### Task 1: Mock delta for the SPA (owner gate)

**Files:**
- Create: `docs/design/mock-machine-clients.html`

Self-contained HTML in the style of `docs/design/mock-project-members.html` (same tokens, light and dark). Show:
(a) the Settings → Authentication OIDC form with a new **Machine clients** list field below "Bootstrap admins". It uses the same chip/list control as bootstrap admins, with this help text: "Client IDs whose client-credentials tokens may call the API. Empty turns JWT bearer access off. Each client must be service-account-only at your identity provider. This app's own Client ID is not allowed." Include an inline error state for "contains this app's Client ID".
(b) a `machine` badge next to the name in Settings → Users, the org Members list and the project Members list (direct and inherited rows).
(c) the Users row action "Make global admin" disabled for a machine row, with the tooltip "A machine identity cannot be a global admin — use an API token".

- [ ] **Step 1:** Write the mock.
- [ ] **Step 2:** Commit: `docs: mock for machine clients and the kind badge (sec-hardening-2 H-1)`.
- [ ] **Step 3:** The controller shows the mock to the owner. **Frontend Task 11 waits for approval; backend tasks continue.**

### Task 2: `users.kind`, backfill, `oidc_settings.machine_clients` column, and the two store upserts

**Files:**
- Create: `internal/store/migrations/00112_users_kind_machine_clients.sql`
- Modify: `internal/domain/domain.go` (`User`, `Member`, `InheritedMember`), `internal/store/users.go`, `internal/store/memberships.go:79-116`, `internal/store/projectmembers.go:24-96`, `internal/store/users_admin.go` (no SQL change needed if `AdminUser` embeds `User` and the scan adds `kind`)
- Remove: `Store.UpsertUserByOIDCIdentity`; switch every test fixture that called it (`internal/store/users_admin_test.go`, `internal/store/store_integration_test.go`, others found by `grep -rn UpsertUserByOIDCIdentity internal/store`) to `UpsertHumanUserByOIDCIdentity`
- Test: `internal/store/userkind_internal_test.go` (new)

**Interfaces:**
- Produces:
  - `domain.UserKindHuman = "human"`, `domain.UserKindMachine = "machine"`. `User.Kind string \`json:"kind"\``, `Member.Kind string \`json:"kind"\``, `InheritedMember.Kind string \`json:"kind"\``
  - `userColumns` gains `kind` at the end, and `scanUser` scans it.
  - `func (s *Store) UpsertMachineUserByOIDCIdentity(ctx context.Context, issuer, sub, email, displayName string) (domain.User, error)`. It returns `ErrSubjectIsHuman` when the identity is a human.
  - `type HumanFlip struct { Flipped bool; WasGlobalAdmin bool; Revoked []domain.Membership }`
  - `func (s *Store) UpsertHumanUserByOIDCIdentity(ctx context.Context, issuer, sub, email, displayName string) (domain.User, HumanFlip, error)`
  - `var ErrSubjectIsHuman`

Migration (`-- +goose Up`, idempotent so the backfill test can re-run the Up section):
`ALTER TABLE users ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'human'`, plus a CHECK `kind IN ('human','machine')` added via a guarded DO-block (named `users_kind_check`); `UPDATE users SET kind = 'machine' WHERE kind = 'human' AND <backfill predicate verbatim>`; `ALTER TABLE oidc_settings ADD COLUMN IF NOT EXISTS machine_clients text[] NOT NULL DEFAULT '{}'`. The Down section drops both columns. Do NOT add a CHECK tying `kind` to `is_global_admin`: the spec's fixture sets that combination directly.

Machine upsert, one tx: run the legacy claim `UPDATE` exactly as in today's `UpsertUserByOIDCIdentity`. Then `INSERT … (…, kind) VALUES (…, 'machine') ON CONFLICT (oidc_issuer, oidc_sub) WHERE oidc_sub IS NOT NULL DO UPDATE SET email=…, display_name=…, updated_at=now() WHERE users.kind = 'machine' RETURNING userColumns`. When no row comes back, return `ErrSubjectIsHuman`; the deferred rollback undoes the legacy claim. This path never touches `is_global_admin`.

Human upsert, one tx. Run the legacy claim. Then loop at most twice:
1. `SELECT id, kind FROM users WHERE oidc_issuer=$1 AND oidc_sub=$2 FOR UPDATE`.
2. If a row is found and `kind='machine'`:
   - `DELETE FROM memberships WHERE user_id=$1 RETURNING id, user_id, org_id, COALESCE(project_id::text,''), role, created_at` collects `Revoked`.
   - Read `is_global_admin` into `WasGlobalAdmin`.
   - Insert the audit rows (see the decisions above).
   - `UPDATE users SET kind='human', is_global_admin=false, email, display_name, updated_at RETURNING userColumns`. `Flipped=true`.
3. If a row is found and `kind='human'`: a plain update of email, display_name and updated_at.
4. If no row is found: `INSERT … 'human' ON CONFLICT (oidc_issuer, oidc_sub) WHERE oidc_sub IS NOT NULL DO NOTHING RETURNING userColumns`. On an empty result, `continue` the loop. Never use `DO UPDATE SET kind='human'` here.

Member list SQL selects `u.kind` into `Member.Kind`/`InheritedMember.Kind`.

- [ ] **Step 1: Write failing tests** in `internal/store/userkind_internal_test.go` (package `store`, `serviceSchemaStore(t)` like `lastseen_internal_test.go`):
  - `TestUsersKindBackfill`: set `kind='human'` everywhere and insert rows with raw SQL. Re-run the 00112 Up section (pattern of `TestLastSeenMigrationBackfillsFromTheNewestSession`). Assert:
    - `sa@clients` (oidc_sub set, last_seen NULL, no password, not admin) becomes `machine`.
    - A dormant OIDC human admin (`ada@x.com`, last_seen NULL, `is_global_admin`) stays `human`.
    - A dormant OIDC non-admin with a real email stays `human`.
    - `x@clients` with `last_seen_at` set stays `human`.
    - A local user stays `human`.
  - `TestMachineUpsertRefusesHumanRowUnchanged`: create a human via `UpsertHumanUserByOIDCIdentity`, then snapshot `SELECT row_to_json(u) FROM users u WHERE id=$1`. `UpsertMachineUserByOIDCIdentity` with the same (issuer, sub) and a different email gives `errors.Is(err, ErrSubjectIsHuman)`, and the row JSON is byte-identical.
  - `TestMachineUpsertLegacyNullIssuerHumanUntouched`: insert a legacy row with raw SQL (`oidc_issuer NULL`, `oidc_sub='s1'`, kind human) and snapshot it. The machine upsert for (`https://idp`, `s1`) gives `ErrSubjectIsHuman`, `count(*) WHERE oidc_sub='s1'` is 1, and the row is byte-identical (issuer still NULL).
  - `TestMachineUpsertCreatesAndUpdatesMachine`: the first call creates `kind='machine'` and `is_global_admin=false`. The second call updates the email.
  - `TestHumanUpsertFlipsMachineAndStripsGrants`: create a machine row and an org membership plus a project membership, then set `is_global_admin=true` with raw SQL. `UpsertHumanUserByOIDCIdentity` gives `Flipped=true`, `WasGlobalAdmin=true`, `len(Revoked)=2`. The row has `kind='human'` and `is_global_admin=false`, and there are 0 memberships. `audit_logs` holds one `user.kind_human` row with `org_id IS NULL` and one `member.revoke_machine_grants` row for the org.
  - `TestHumanUpsertKeepsHumanGrants`: an existing human with a membership keeps it (`Flipped=false`).
  - `TestHumanUpsertLegacyClaim`: a legacy NULL-issuer human row gets the issuer, the same id, and kind human.
  - `TestBearerAndCallbackRace`: for 50 iterations each, in two variants: (i) the machine row with a membership exists beforehand; (ii) no row exists. Start `UpsertMachineUserByOIDCIdentity` and `UpsertHumanUserByOIDCIdentity` concurrently behind a shared start barrier. Assert:
    - The human call returns no error.
    - Exactly one row for (issuer, sub).
    - Final `kind='human'`, 0 memberships, `is_global_admin=false`.
    - If the machine call succeeded, the user id it returned equals the final row's id.
  - `TestMemberListsCarryKind`: `ListOrgMembers`, `ListProjectMembers`, `ListProjectInheritedMembers` and `ListAllUsers` report `machine` for a machine member and `human` for a human member.
- [ ] **Step 2:** Run `go test ./internal/store/ -run 'TestUsersKind|TestMachineUpsert|TestHumanUpsert|TestBearerAndCallbackRace|TestMemberListsCarryKind' -count=1 -v`. Expected: compile FAIL (undefined methods).
- [ ] **Step 3:** Implement the migration, the domain fields, the scan changes, both upserts, the member-list `kind`, and the fixture migration off the removed method.
- [ ] **Step 4:** Run the same command, then `go test ./internal/store/ -count=1 -timeout 40m` (whole package, because the fixtures changed). Expected: PASS. `go build -buildvcs=false ./...` will fail in `internal/auth`. That is expected and fixed in Task 4. Until then, keep the build green by making the minimal compile fix in `internal/auth` (the callback calls `UpsertHumanUserByOIDCIdentity` and ignores the flip; the bearer path calls `UpsertMachineUserByOIDCIdentity`), plus the matching `fakeStore` methods. No behaviour beyond that.
- [ ] **Step 5:** Commit `fix: users.kind and split OIDC provisioning into machine and human upserts (sec-hardening-2 H-1)`.

### Task 3: The `machine_clients` setting (YAML, DB, API, runtime)

**Files:**
- Modify: `internal/domain/oidc.go`, `internal/store/oidc.go`, `internal/config/config.go` (+ its `Validate`), `internal/api/handlers_oidc.go`, `internal/auth/auth.go` (`EffectiveOIDC`, `resolveEffective`, `buildRuntime`, `oidcRuntime`), `docker/config.example.yaml` (commented `machine_clients: []` under `oidc:`)
- Test: `internal/config/config_test.go` (or the file holding OIDC config tests), `internal/api/oidc_settings_test.go` (or the existing OIDC handler test file), `internal/store/userkind_internal_test.go`, `internal/auth/machine_test.go` (new)

**Interfaces:**
- Produces:
  - `domain.OIDCSettings.MachineClients []string \`json:"machine_clients"\``
  - `config.OIDCConfig.MachineClients []string \`yaml:"machine_clients"\``
  - `EffectiveOIDC.MachineClients []string`
  - `oidcRuntime.machineClients map[string]bool`
  - `func NormalizeMachineClients(in []string, ownClientID string) ([]string, error)` in `internal/domain/oidc.go`. It trims, drops empties, dedupes and keeps the first-seen order. It returns `ErrOwnClientInMachineClients` when an entry equals `strings.TrimSpace(ownClientID)`.
  - `oidcSettingsView.MachineClients []string \`json:"machine_clients"\``, never null.

Every path normalizes: the API PUT answers 400 `machine_clients must not contain this application's client_id`, `config.Validate` errors, and `buildRuntime` normalizes and also drops the own id defensively.

- [ ] **Step 1: Failing tests:**
  - `TestNormalizeMachineClients`: `[" svc ", "svc", "", "ci"]` with own `"cerbix"` gives `["svc","ci"]`. `["  cerbix "]` with own `"cerbix"` gives `ErrOwnClientInMachineClients`.
  - `TestOIDCSettingsMachineClientsRoundTrip` (store, DB): upsert with `["svc"]`, then get gives `["svc"]`. A row saved before the field existed reads `[]`, not nil.
  - API: `TestSetOIDCSettingsRefusesOwnClientID` gives 400. `TestOIDCSettingsMachineClientsInView`: GET returns `machine_clients` as `[]` when there is no row, and the saved list after a PUT.
  - Config: `TestConfigRefusesOwnClientInMachineClients`.
  - `TestResolveEffectiveMachineClients` (auth, fake store): YAML `["yaml-svc"]` with no DB row means the runtime lists `yaml-svc`. A DB row with `["db-svc"]` means the runtime lists only `db-svc` (the DB replaces the YAML).
- [ ] **Step 2:** Run the tests (`go test ./internal/domain/ ./internal/config/ ./internal/api/ ./internal/auth/ -run '<names>' -count=1`, plus the store one with the DSN). Expected: FAIL (undefined).
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Rerun. Expected: PASS, and `go build -buildvcs=false ./... && go vet ./...` is clean.
- [ ] **Step 5:** Commit `feat: machine_clients OIDC setting — the explicit list of client-credentials clients (sec-hardening-2 H-1)`.

### Task 4: The bearer boundary (S1): client claim, delegated markers, machine-only subjects, refusal log and metric

**Files:**
- Create: `internal/auth/machine.go` (claim extraction plus the new `principalFromJWT`; remove the old one from `middleware.go`)
- Modify: `internal/auth/middleware.go`, `internal/auth/handlers.go` (callback), `internal/auth/context.go` (Store interface: remove `UpsertUserByOIDCIdentity`, add both new methods), `internal/auth/fakestore_test.go`, `internal/auth/auth_test.go` (`mockOIDC` gains `extra map[string]any` merged into the claims, and `nn==""` omits the `nonce` claim entirely), `internal/auth/clientcreds_test.go` (now lists `azp` in `machine_clients`), `internal/metrics/metrics.go`, `internal/cli/cli.go` (wire the registry into the authenticator)
- Test: `internal/auth/machine_test.go`, `internal/metrics/metrics_test.go`

**Interfaces:**
- Consumes: Task 2's upserts and `ErrSubjectIsHuman`, Task 3's `oidcRuntime.machineClients`.
- Produces:
  - `type BearerRefusalRecorder interface { RecordOIDCBearerRefused(reason string) }`
  - `func (a *Authenticator) WithMetrics(m BearerRefusalRecorder) *Authenticator`
  - `func (r *Registry) RecordOIDCBearerRefused(reason string)`, exposed as `cerbix_oidc_bearer_refused_total{reason}` (counter, help: "OIDC bearer JWTs refused on the client-credentials path, by reason.")
  - `func clientClaim(c jwtClientClaims) (name, value string, reason string)`, with `type jwtClientClaims struct { Azp, ClientID, Cid, Appid, Nonce, Sid string }` decoded from `azp`, `client_id`, `cid`, `appid`, `nonce`, `sid`.

`principalFromJWT` order:
1. A nil runtime gives `(false, nil)` with no log.
2. A verify failure is `invalid_token`.
3. A claims decode error is `invalid_claims`.
4. `clientClaim` gives `client_claim_missing` or `client_claims_disagree`.
5. An empty list is `machine_clients_empty`.
6. A client not in the list is `client_not_listed`.
7. `nonce` is `delegated_nonce`, then `sid` is `delegated_sid`, then a `cid` other than `sub` is `delegated_cid_sub`.
8. `UpsertMachineUserByOIDCIdentity`: `ErrSubjectIsHuman` is `subject_is_human`, and any other error is returned (500).
9. `loadPrincipal`.
10. **Then** `GetUser` again: a `Kind` other than `machine` is `subject_is_human`.
11. Set `ViaToken=true`, and log `oidc_bearer_accepted` at Debug with `client_claim`.

Every refusal returns `(false, nil)`, which `RequireAuth` turns into a 401, increments the metric, and logs `oidc_bearer_refused`. The email allowlist is not applied. The callback calls `UpsertHumanUserByOIDCIdentity`. When `Flipped`, it logs at Warn: `oidc_machine_became_human`, with `user_id`, `revoked_memberships` (count) and `was_global_admin`. The bootstrap-admin rule then runs unchanged on the returned user.

- [ ] **Step 1: Failing tests** in `internal/auth/machine_test.go` (hermetic mock OIDC, fake store). Each 401 case also asserts the fake store saw no write and the recorded reason:
  - `TestBearerForeignClientWithHumanSubIs401`: a human `sub` exists from an SSO login, a token with `azp:"other-app"` and machine clients `["svc"]` gives 401 `client_not_listed`, and the human row is unchanged.
  - `TestBearerOwnInteractiveTokenIs401`: a token with `azp` equal to cerbix's client id and a nonce gives 401 `client_not_listed`.
  - `TestBearerListedClientAuthenticates`: `azp:"svc"` with the list `["svc"]` gives 204, `ViaToken`, and the fake user has kind `machine`.
  - `TestBearerEmptyListIs401`: gives `machine_clients_empty`.
  - `TestBearerDelegatedMarkersRefused`: table tests for `nonce` → `delegated_nonce`, `sid` → `delegated_sid`, and `cid:"svc"` with `sub:"user-1"` → `delegated_cid_sub`.
  - `TestBearerOktaAndEntraShapes`: `{cid:"svc", sub:"svc"}` and `{appid:"svc"}` are each accepted when `svc` is listed.
  - `TestBearerClientClaimsDisagree`: `{azp:"svc", client_id:"other"}` gives 401 `client_claims_disagree`. `{azp:"svc", client_id:"svc"}` is accepted.
  - `TestBearerNonStringClaimIs401`: `{azp:["svc"]}` gives 401 `invalid_claims` (Review Focus 1).
  - `TestBearerIgnoresEmailAllowlist`: an enforced email allowlist (policy) does not refuse a listed machine token that has no email (rule 6).
  - `TestBearerAgainstHumanRowIs401`: the fake upsert returns `ErrSubjectIsHuman`, which gives 401 `subject_is_human`.
  - `TestBearerKindReadAfterMemberships`: the fake store hook flips the user to `human` inside `ListMembershipsForUser`, which gives 401 `subject_is_human` (not a principal with memberships).
  - `TestCallbackFlipLogsAndRegrantsBootstrapOnlyIfVerified`: the fake returns `Flipped`. With a verified bootstrap email, `SetGlobalAdmin(true)` is called. With an unverified one, it is not.
  - `TestRegistryExposesOIDCBearerRefused` (metrics): after `RecordOIDCBearerRefused("client_not_listed")` the exposition contains `cerbix_oidc_bearer_refused_total{reason="client_not_listed"} 1`.
  - Update `TestClientCredentialsBearer` so it lists the minted `azp` in `machine_clients`. Its assertions stay the same.
- [ ] **Step 2:** Run `go test ./internal/auth/ ./internal/metrics/ -count=1 -run 'Bearer|Callback|ClientCredentials|OIDCBearer' -v`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test ./internal/auth/ ./internal/metrics/ ./internal/cli/ -count=1`, plus `go build -buildvcs=false ./... && go vet ./...`. Expected: PASS.
- [ ] **Step 5:** Commit `fix: the OIDC bearer path accepts only listed machine clients and machine subjects (sec-hardening-2 H-1, amends D-0034)`.

### Task 5: No machine user is a global admin

**Files:** Modify `internal/api/handlers_users.go`. Test: the existing admin-users API test file (`grep -ln updateAdminUser internal/api/*_test.go`).

- [ ] **Step 1: Failing test** `TestUpdateAdminUserRefusesMachineGlobalAdmin`: PATCH `{"is_global_admin":true}` on a fake user with `Kind:"machine"` gives 400 `{"error":"a machine identity cannot be a global admin"}`, and `SetGlobalAdmin` is not called. PATCH `false` on that user is still allowed (needed to clean up a pre-existing flag).
- [ ] **Step 2:** Run it. Expected: FAIL (200).
- [ ] **Step 3:** Add the check after `GetUser`, when `*body.IsGlobalAdmin && u.Kind == domain.UserKindMachine`.
- [ ] **Step 4:** Run `go test ./internal/api/ -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit `fix: refuse the global-admin flag for machine identities (sec-hardening-2 H-1)`.

### Task 6: Session revocation (S5)

**Files:** Modify `internal/store/users_reset.go`, `internal/store/users_local.go` (add `ChangePassword`), `internal/api/api.go` (Store interface: replace `SetPassword`/`DeleteSessionsByUser` with `ChangePassword` if nothing else in `api` uses them; check with grep), `internal/api/handlers.go:458-508`, the api fakes (`internal/api/api_test.go` fakeStore and any other `api.Store` fake that fails to compile). Test: `internal/store/sessionrevoke_internal_test.go` (new) and an api handler test.

**Interfaces:**
- Produces: `func (s *Store) ChangePassword(ctx context.Context, userID, passwordHash, keepSessionToken string) (int64, error)`. In one tx it updates `password_hash` `WHERE id=$1 AND password_hash IS NOT NULL` (0 rows gives `ErrNotFound`), then deletes `sessions WHERE user_id=$1 AND token_hash <> HashToken(keep)`. It returns the number of deleted sessions.
- `ResetPasswordWithToken` keeps its signature. After the password update it runs `DELETE FROM sessions WHERE user_id=$1` inside the same tx.

- [ ] **Step 1: Failing tests** (store, DB). Forced failure uses a trigger: a function named `test_fail_session_delete_<random>` plus a `BEFORE DELETE ON sessions FOR EACH ROW WHEN (OLD.user_id = '<id>') EXECUTE FUNCTION …` that raises, dropped in `t.Cleanup`.
  - `TestResetRevokesEverySession`: a local user with two sessions, then a token reset. Both `SessionByToken` calls give `ErrNotFound`, and an api token of the same user's org (if trivially creatable, else skip that assertion) still resolves.
  - `TestResetRollsBackWhenSessionDeleteFails`: install the trigger. The reset returns an error, the token is not consumed (a second reset after dropping the trigger succeeds), and the hash is unchanged.
  - `TestChangePasswordKeepsOnlyCallerSession`: three sessions, `ChangePassword(keep=s1)` returns 2, `s1` is alive and the others are `ErrNotFound`.
  - `TestChangePasswordAtomicWhenSessionDeleteFails`: with the trigger installed, an error comes back and `PasswordHashByID` is unchanged.
  - `TestChangePasswordNotLocalAccount`: an OIDC user gives `ErrNotFound`.
  - API: `TestChangePasswordStoreFailureIs500`: the fake `ChangePassword` returns an error, which gives 500, not 204.
- [ ] **Step 2:** Run them. Expected: FAIL.
- [ ] **Step 3:** Implement. `changePassword` calls `h.store.ChangePassword(ctx, p.UserID, newHash, auth.SessionTokenFrom(ctx))`. `ErrNotFound` gives 400 "not a local account", any other error gives `serverError`, and success logs `password_changed` with `sessions_invalidated`. `ResetConfirmHandler` logs `password_reset_completed` unchanged.
- [ ] **Step 4:** Run `go test ./internal/store/ -run 'TestReset|TestChangePassword' -count=1 -v` and `go test ./internal/api/ ./internal/auth/ -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit `fix: a password reset ends every session; a change is one transaction (sec-hardening-2 H-5, amends D-0139 item 3)`.

### Task 7: Password-byte cap and body caps

**Files:** Modify `internal/auth/password.go`, `internal/auth/handlers.go` (`Routes`), `internal/auth/handlers_reset.go`, `internal/api/handlers.go` (`changePassword`, new constant `apiMaxBody`), `internal/api/api.go` (new `APIRouter`), `internal/cli/cli.go:660`. Test: `internal/auth/password_test.go`, `internal/auth/bodycap_test.go` (new), `internal/api/bodycap_test.go` (new).

**Interfaces:**
- Produces:
  - `const MaxPasswordBytes = 1024`
  - `var ErrPasswordTooLong = errors.New("auth: password longer than 1024 bytes")`, returned by both `HashPassword` and `VerifyPassword` before any Argon2 work
  - `const localAuthMaxBody = 4 << 10` (auth)
  - `const apiMaxBody = 1 << 20` (api)
  - `func (h *Handler) APIRouter() http.Handler { return maxBytes(h.Router(), apiMaxBody) }`. cli mounts `authn.RequireAuth(apiHandler.APIRouter())`.

`Routes` wraps the three `/auth/local/*` handlers in a body cap. The cap applies at read time, so the limiter in Task 8 still runs before any read. Reset/confirm and `/me/password` answer 400 `new password too long` when `len(new) > MaxPasswordBytes`, before hashing.

- [ ] **Step 1: Failing tests:**
  - `TestPasswordByteCap`: 1024 bytes hashes and verifies. 1025 bytes gives `ErrPasswordTooLong` from both functions. `strings.Repeat("€", 342)` (1026 bytes) is refused (Review Focus 3).
  - `TestLocalAuthBodyCap`: through `a.Routes(mux)`, a 4097-byte JSON body to each of `/auth/local/login`, `/reset/request` and `/reset/confirm` gives 400. A 4000-byte valid body is not 400 for being oversized.
  - `TestResetConfirmTooLongIs400`: a 1025-byte `new_password` gives 400 `new password too long`, and the token is not consumed (fake).
  - `TestChangePasswordTooLongIs400` (api).
  - `TestAPIRouterCapsBodiesAtOneMiB` (api): through `APIRouter()`, with a global-admin principal in the context, a PUT `/api/v1/settings/oidc` body of 1 MiB + 1 gives 400 `invalid JSON body`.
- [ ] **Step 2:** Run them. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test ./internal/auth/ ./internal/api/ ./internal/cli/ -count=1` and `go build -buildvcs=false ./...`. Expected: PASS.
- [ ] **Step 5:** Commit `fix: cap password bytes and request bodies before Argon2id (sec-hardening-2 H-6)`.

### Task 8: Split limiters (S6)

**Files:** Modify `internal/auth/ratelimit.go`, `internal/auth/auth.go` (fields and `New`), `internal/auth/local.go`, `internal/auth/handlers_reset.go`, the comment on `LoginRateLimitPerMinute` in `internal/config/config.go`. Test: `internal/auth/ratelimit_test.go`, `internal/auth/limiters_test.go` (new).

**Interfaces:**
- Produces: on `loginLimiter`, `func (l *loginLimiter) blocked(key string, now time.Time) bool` (true when the in-window count is at or above the limit, records nothing) and `func (l *loginLimiter) record(key string, now time.Time)`. `allow` is unchanged. The `Authenticator` fields `ipLimiter`, `ipEmailLimiter` and `emailFailLimiter` replace `loginLimiter`. `func normalizeEmail(s string) string`.

Order in `LocalLoginHandler`:
1. `ipLimiter.allow("login|"+ip)` → 429 before the body is read.
2. Decode.
3. `ipEmailLimiter.allow(ip+"|"+norm)` → 429.
4. `emailFailLimiter.blocked(norm)` → 429.
5. Verify. An unknown user, a wrong password or a failed second factor runs `emailFailLimiter.record(norm)` and then answers as today.

`ResetRequestHandler`: `ipLimiter.allow("reset|"+ip)` before the body, then `ipEmailLimiter.allow(ip+"|"+norm)` after decoding. `ResetConfirmHandler`: `ipLimiter.allow("reset|"+ip)` before the body. All three answer 429 with the existing JSON error shape.

- [ ] **Step 1: Failing tests** (fake store, L=3, `verifyPasswordFn` kept real or stubbed for speed):
  - `TestEmailFailuresFromOneAddressDoNotLockOutAnother`: address A sends 9 wrong-password attempts for `u@x.com` (429s appear after 3). Then address B with the correct password gets 200 (the spec's S6 test).
  - `TestEmailFailureLimiterTripsAcrossAddresses`: 15 (=5L) failures from 15 distinct IPs, then a 16th fresh IP with the correct password gets 429.
  - `TestIPEmailLimiter`: 2 (= `(3+1)/2`) attempts from one IP for one email, then the third for that email gets 429, while a different email from the same IP is still allowed.
  - `TestResetConfirmLimitedBeforeBodyRead`: L=1. The second confirm from one IP gets 429, and its body is an `io.Reader` that calls `t.Error` on `Read`.
  - `TestResetRequestPerIPAndPerIPEmail`.
  - `TestLimiterEmailNormalization`: `" U@X.com"` and `"u@x.com"` share the per-(IP, email) and per-email buckets (Review Focus 4).
  - `TestLimitersDisabledWhenZero`.
  - `TestLimiterBlockedRecordsNothing` (unit, `ratelimit_test.go`).
- [ ] **Step 2:** Run `go test ./internal/auth/ -run 'Limiter|EmailFailure|IPEmail|ResetConfirmLimited|ResetRequestPerIP' -count=1 -v`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test -race ./internal/auth/ -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit `fix: split the login limiter — per IP before the body, per IP and email, per email on failures (sec-hardening-2 H-6)`.

### Task 9: OpenAPI and generated types

**Files:** Modify `openapi.yaml`, `frontend/src/api/schema.d.ts` (generated; find the exact path from `frontend/package.json`'s `gen:api`).

Add:
- `kind` (enum `human|machine`, required) on the User, Member, InheritedMember and AdminUser schemas.
- `machine_clients` (array of strings) on the OIDC settings view and the PUT body, with the 400 for the own `client_id`.
- The 400 on `PATCH /api/v1/admin/users/{userID}` for a machine identity.
- 429 on `/auth/local/reset/confirm`.
- 400 for an oversized body on `/auth/local/*`, if those paths are in the file.

- [ ] **Step 1:** Edit `openapi.yaml`, then run from `frontend/`: `docker run --rm --user "$(id -u):$(id -g)" -e npm_config_cache=/tmp/.npm -v "$PWD":/app -v "$PWD/../openapi.yaml":/openapi.yaml -w /app node:22-alpine npm run gen:api`.
- [ ] **Step 2:** Run `go test ./internal/api/ -count=1`. If the repo has an openapi/route parity test, it must pass. Then run `docker run --rm --user "$(id -u):$(id -g)" -e npm_config_cache=/tmp/.npm -v "$PWD":/app -w /app node:22-alpine sh -c "npm run build"`. Expected: success.
- [ ] **Step 3:** Commit `docs: OpenAPI for users.kind, machine_clients and the new refusals (sec-hardening-2 H-1, H-6)`.

### Task 10: E2E: the dedicated reset user

**Files:** Create `e2e/seed/e2e-reset-user.sql`. Modify `Makefile` (`dev-ready-single`) and `e2e/tests/mail.spec.ts`.

The seed runs `INSERT INTO users (email, display_name, password_hash) VALUES ('e2e-reset@cerbix.local', 'E2E reset', '<argon2id hash>') ON CONFLICT (email) WHERE password_hash IS NOT NULL DO NOTHING`. The hash is a real `auth.HashPassword` output of a random secret that is thrown away, so nobody can sign in until a reset. Generate it once with a throwaway Go snippet in the scratchpad. `dev-ready-single` appends, after `wait_ready`: `$(DEV_DC) exec -T postgres psql -U cerbix -d cerbix -v ON_ERROR_STOP=1 < e2e/seed/e2e-reset-user.sql`. Check the exact `DEV_DC` service name in `docker/docker-compose.yml`.

The new password-reset test, with no new login and no logout:
1. Request a reset for `e2e-reset@cerbix.local` and follow the letter to confirm `tempPass1`, which gives 204.
2. The existing single anonymous-context login with `tempPass1` succeeds. `GET /api/v1/me` in that context gives 200.
3. Request and confirm a second reset to `tempPass2`. The same anonymous context's `GET /api/v1/me` now gives 401 (S5 on the live stack).
4. The shared admin session's `GET /api/v1/me` still gives 200.

The "restore the admin password" block is removed.

- [ ] **Step 1:** Write the seed, the Make step and the spec.
- [ ] **Step 2:** Run it against the live stack in Task 12. A dry check here: `make -n dev-ready-single` shows the seed line.
- [ ] **Step 3:** Commit `test: mail.spec resets a dedicated seeded user, never the shared admin (sec-hardening-2 H-5)`.

### Task 11: SPA: the `machine_clients` field and the `kind` badge (after the owner approves the mock)

**Files:** Modify `frontend/src/views/SettingsView.vue` (OIDC form state lines ~145-210 plus template), `frontend/src/components/settings/UsersPanel.vue`, `frontend/src/components/settings/MembersPanel.vue`, `frontend/src/components/settings/ProjectMembersPanel.vue`; regenerate `internal/web/dist` via `make spa-snapshot`.

Implement exactly the approved mock. The form loads `machine_clients` (default `[]`) and saves it. A client-side check mirrors the server's own-client-id refusal, and the server's 400 message is shown as `oidc.error`.

- [ ] **Step 1:** Implement.
- [ ] **Step 2:** Run `npm run build` (docker command above). Expected: vue-tsc and vite succeed.
- [ ] **Step 3:** From the repo root, run `make spa-snapshot`. `internal/web/dist/assets/placeholder.txt` must still exist.
- [ ] **Step 4:** Commit `feat: machine clients in the OIDC form and the machine badge in member lists (sec-hardening-2 H-1)`.

### Task 12: Verification

- [ ] Run `go build -buildvcs=false ./... && go vet ./...`.
- [ ] Hypertable mode: `CERBIX_TEST_DATABASE_DSN=…5432/cerbix_test go test -race -count=1 -timeout 40m ./...`. Drop and recreate `cerbix_test` first if migration 00112 was edited after its first apply.
- [ ] Declarative-partition mode: start a throwaway `postgres:16-alpine` on port 55432 with a `cerbix_test` DB, run the same command with that DSN, then remove the container.
- [ ] Run `make dev-up && make dev-test`. Expected: the iter-0206 baseline of 89 pass and 1 skip, with `mail.spec.ts` green.
- [ ] Run `python3 scripts/check-docs-references.py`.
- [ ] Record the pass/fail/skip counts of all three runs for the report. Fix any failure through superpowers:systematic-debugging, not by weakening a test.

### Task 13: Close the iteration (one `docs:` commit)

- `docs/iterations/iter-0209.md`: scope, the deviation (the seeded E2E user, owner ruling 2026-11-13), the client-credentials token shape per provider from their docs (Keycloak, Okta, Entra ID v1/v2, Google): which of `azp`/`client_id`/`cid`/`appid`/`sub`/`scp`/`nonce`/`sid` appear. Also the findings seen on the way and left alone, and the test counts.
- `docs/decisions.md` **D-0276**: the bearer boundary. It explicitly amends D-0034 (the audience relaxation now stands only for listed machine clients, and membership-gating alone is no longer the safety argument) and D-0139 item 3 (a reset revokes every session). It also records the limiter thresholds and the refusal reasons.
- `docs/traceability.md` row. `docs/overview.md` if it describes the client-credentials path or sessions. `docs/runbook.md`: machine clients, service-account-only clients at the IdP, `oidc_bearer_refused`, and the GCP-SA re-creation step. The spec status line: iteration 1 implemented.
- `CHANGELOG.md` under `## [vX.Y.Z] - Unreleased`: with `machine_clients` empty, client-credentials tokens get 401 (list the clients; watch `oidc_bearer_refused`). A service account whose token carries a real email (GCP SA) is refused with `subject_is_human`: delete that user and let the next token re-create it, then grant its memberships again. A password reset ends every session.
- Commit `docs: sec-hardening-2 iteration 1 — report, D-0276, traceability, changelog (iter-0209)`.
