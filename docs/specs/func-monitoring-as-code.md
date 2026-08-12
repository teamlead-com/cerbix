# Spec: Monitoring as Code file provider (func-monitoring-as-code)

Status: SPEC (spec-before-code). Requirements: FR-017, NFR-014. Decision: D-0145.
No part of this document is implemented merely by publishing the specification.

## 1. Purpose

Cerbix SHALL be able to reconcile monitor definitions from YAML files without restarting
or reloading the service. The main (static) configuration names one or more file providers
and their directories; changes inside those directories are detected and applied at
runtime.

This is not an in-memory config replacement. Cerbix has persistent monitor IDs, heartbeat
and SLA history, incidents, push tokens, dependencies, tenant boundaries, and concurrent
probe results. Therefore:

- files are the desired-state source for resources owned by a file provider;
- PostgreSQL remains the execution source of truth and the store for runtime/history;
- reconciliation is validated, ownership-aware, idempotent, and transactional;
- the scheduler and workers never parse YAML or read provider directories.

The native user-facing format is a versioned **ProjectBundle**, not a Kubernetes-style
`apiVersion`/`kind`/`metadata`/`spec` envelope and not a serialization of Go/DB structs.

## 2. Non-negotiable invariants

1. **Tenant isolation:** every bundle resolves to one existing organization/project, and
   every reference is checked in that tenant scope. A provider cannot escape its static
   scope through file content.
2. **Typed ownership:** API/UI writes and file-provider writes are distinct application
   entrypoints. A file provider mutates only resources it owns; it never infers ownership
   from a display name.
3. **Hybrid projects:** UI/API-created and file-managed monitors may coexist in one
   project. A bundle is authoritative only for its provider-owned set, not for all project
   rows.
4. **Atomic bundle:** one project bundle is either applied completely in one PostgreSQL
   transaction or not applied. There is no partial monitor/dependency apply.
5. **Last-known-good:** an invalid replacement never changes the last applied bundle and
   never triggers deletion/orphaning from an ambiguous snapshot.
6. **No automatic hard delete:** disappearance from desired state can orphan and disable
   a managed monitor, but never deletes heartbeat, SLA, incident, or audit history.
7. **No-op means no write:** comments, key order, a file rename, or a touch with the same
   normalized semantics do not update the monitor or bump `execution_revision`.
8. **Single binary/control plane:** the provider runs inside `cerbix serve --role all` and
   `--role api`; there is no `controller` role or service.
9. **No inline secrets:** dynamic files do not contain passwords, tokens, cookies, private
   keys, or environment expansion. Secret-bearing fields require an explicit Cerbix
   `secret_ref` contract before their monitor type is supported by this provider.
10. **Bounded work:** file count, file size, bundle size, reconcile duration, concurrency,
    logs, and metric labels are bounded.

## 3. Scope

### 3.1 Version 1

Version 1 manages:

- monitors;
- dependency edges between file-managed monitors in the same bundle;
- provider/bundle generation state and resource provenance;
- read-only source metadata in the API/UI;
- operational diagnostics, audit, and scheduler wake-up.

All existing monitor types MAY be represented only when every required type-specific field
has a strict non-secret schema. A type that needs credentials is unavailable through the
file provider until the referenced secret-inventory contract exists. Unsupported types or
fields reject the bundle; there is no generic `config: map[string]string` escape hatch.

### 3.2 Explicitly outside version 1

- creating/deleting organizations, projects, users, memberships, or API tokens;
- automatic adoption of an API/UI-created monitor;
- transferring/releasing ownership between providers;
- notification channels, escalation/SLA policies, status pages, and maintenance windows;
- templates, includes, deep merge, file precedence, YAML-to-YAML inheritance;
- `${ENV}`, `os.ExpandEnv`, command execution, or remote URL imports;
- Git/HTTP/Kubernetes/Terraform providers;
- a dedicated controller process/role;
- recursive directories;
- automatic physical pruning.

Later bundle formats MAY add new top-level resource maps, for example
`notification_channels`, `escalation_policies`, and `sla_policies`, without changing the
monitor ownership model.

## 4. Static provider configuration

The static Cerbix config gains an optional strict `providers.file` map. The map key is the
stable provider identity and the only provider label exposed to metrics.

```yaml
providers:
  file:
    platform:
      directory: /etc/cerbix/monitoring.d
      debounce: 2s
      resync_interval: 30s
      orphan_grace_period: 30s

      scope:
        type: instance

      limits:
        max_files: 1000
        max_file_bytes: 1048576
        max_total_bytes: 16777216
        max_monitors_per_bundle: 1000
        max_managed_monitors: 5000
```

An organization-scoped provider:

```yaml
providers:
  file:
    acme:
      directory: /etc/cerbix/monitoring.d/acme
      scope:
        type: organization
        organization: acme
```

A project-scoped provider:

```yaml
providers:
  file:
    payments:
      directory: /etc/cerbix/monitoring.d/payments
      scope:
        type: project
        organization: acme
        project: payments
```

### 4.1 Static validation

- Provider name: `^[a-z][a-z0-9-]{0,39}$`; at most 64 configured providers.
- `directory` is absolute and non-root in every role. An `api`/`all` process additionally
  requires it to exist and be readable at startup. Canonically overlapping provider roots
  are rejected.
- A configured provider requires a database when run by `api`/`all`; other roles do not
  initialize or access it.
- `scope.type` is required and is exactly `instance | organization | project`; there is no
  implicit instance-wide scope.
- `organization` is required only for organization/project scope; `project` is required
  only for project scope. Both are existing immutable Cerbix slugs, not display names.
- `debounce` default `2s`, allowed `[100ms, 30s]`.
- `resync_interval` default `30s`, allowed `[5s, 1h]`.
- `orphan_grace_period` default `30s`, allowed `[0, 24h]`; zero is explicit immediate
  disable after a valid absence, never hard delete.
- Limit defaults are the values shown above; present values must be positive and bounded by
  implementation-wide safety maxima.
- Unknown keys and invalid combinations fail static config loading.

The provider definition itself is static: changing its name, scope, directory, or limits
requires a normal Cerbix process restart. File contents under `directory` are dynamic and
never require a restart/reload signal.

Every role strictly parses the same provider schema so one validated main config may be
reused across a distributed deployment. Only `all` and `api` own the component and perform
role-specific directory/DB startup validation; `scheduler`, `worker`, and `agent` neither
watch nor require the directory. This is explicit role ownership, not runtime fallback.

## 5. Tenant prerequisites and scope resolution

Organizations and projects MUST be provisioned before a bundle references them. They are
created through the authenticated UI/API (and potentially a future CLI/Terraform client),
not as a side effect of watching YAML.

Existing organization/project `slug` values are the file contract's stable keys. A missing
organization/project rejects the affected bundle; Cerbix never creates a similarly named
tenant because of a typo.

Bundle tenant fields depend on provider scope:

| Provider scope | Bundle `organization` | Bundle `project` |
| --- | --- | --- |
| `project` | forbidden | forbidden |
| `organization` | forbidden | required |
| `instance` | required | required |

Resolution MUST constrain the project by its organization in the same query/logical store
operation; a project lookup followed by an unchecked assumption is forbidden. The resolved
pair is the tenant key for validation, locking, ownership, audit, and apply.

`instance` scope is an explicitly privileged operator configuration. Possession of write
access to a provider directory grants management authority only inside that provider's
declared scope.

## 6. ProjectBundle file contract

One regular `.yaml` or `.yml` file represents one project bundle. One provider may contain
many project files, but the same resolved project may be declared by at most one file in a
provider snapshot. A duplicate target rejects both competing candidates and preserves that
project's last-known-good state.

Example under an instance-scoped provider:

```yaml
format: 1

organization: acme
project: payments

monitors:
  api:
    name: Payments API
    type: http
    target: https://payments.internal/health
    method: GET
    interval: 30s
    timeout: 5s
    retries: 2
    failure_threshold: 3
    confirm_interval: 10s
    renotify: 30m
    enabled: true
    auto_incident: true
    region: core
    conditions:
      - status == 200
      - latency < 500ms
    tags:
      - env:production
      - team:payments
    depends_on:
      - database

  database:
    name: Payments PostgreSQL
    type: postgres
    target: postgres.internal:5432
    interval: 30s
    timeout: 5s
    enabled: true
```

### 6.1 Root fields

| Field | Contract |
| --- | --- |
| `format` | Required integer; version 1 accepts only `1`. |
| `organization` | Scope-dependent immutable organization slug (§5). |
| `project` | Scope-dependent immutable project slug (§5). |
| `monitors` | Required map; key is the source UID, value is a strict monitor spec. Empty map is valid and orphans the provider-owned set after grace. |

Unknown root fields reject the bundle. YAML aliases may be accepted only insofar as the
strict decoder resolves them into the same bounded value tree; custom tags are forbidden.
Duplicate mapping keys are errors.

### 6.2 Monitor UID

The key under `monitors` is immutable provider-local identity:

```text
provider_id + organization_id + project_id + kind("monitor") + source_uid
```

UID syntax: `^[a-z][a-z0-9-]{0,62}$`. A file path and display `name` are provenance and
presentation, not identity. Therefore:

- changing `name` updates the existing DB monitor;
- renaming/touching the file preserves the existing DB monitor;
- changing a UID is old-resource orphaning plus a new-resource create;
- changing `type` for an existing UID is rejected because monitor type and push-token
  semantics are immutable; use a new UID intentionally.

### 6.3 Common monitor fields

The version-1 strict schema maps explicit bundle fields to the existing domain model:

- required: `name`, `type` and all type-required target/settings fields;
- optional common fields: `target`, `method`, `interval`, `timeout`, `retries`,
  `failure_threshold`, `confirm_interval`, `renotify`, `grace`, `conditions`, `tags`,
  `region`, `enabled`, `auto_incident`, `depends_on`, and a type-specific `settings`
  object;
- durations are duration strings and MUST normalize to whole seconds accepted by the
  domain model; fractional-second values reject;
- `settings` is decoded by monitor type with known fields; arbitrary string maps are
  forbidden;
- server-owned fields (`id`, `project_id`, `status`, counters, timestamps,
  `execution_revision`, `last_result_ts`, push token) are forbidden.

Format-1 defaults are deterministic contract constants, not mutable instance/UI monitor
defaults: method `GET`, interval `60s`, timeout `10s`, retries `0`, failure threshold `1`,
region `core`, enabled `true`, auto-incident `true`; existing domain normalization still
applies (for example push threshold rules and confirm-interval bounds). A later format is
required to change the meaning of an existing default.

`depends_on` contains source UIDs from the same bundle. Self, missing, duplicate, cyclic,
cross-project, UI-managed, and other-provider dependency targets reject the whole bundle
in version 1. Dependency order is non-semantic.

## 7. Canonicalization and semantic hash

The parser produces a normalized `DesiredProject` application value. A canonical hash is
calculated only after defaults, duration conversion, set normalization, and domain
validation. It excludes:

- YAML comments, whitespace, key order, source path, and mtime;
- server-owned runtime fields;
- push token/ciphertext;
- provider generation timestamps.

Set-like values (`tags`, dependency UIDs and any type-defined sets) are sorted and
deduplicated for hashing; order-sensitive conditions/scenario steps retain order. The
canonical form, not raw YAML bytes, determines create/update/no-op.

If the semantic monitor hash is unchanged, reconciliation MUST NOT call the semantic
monitor update path. Moving a file may update provenance separately but cannot change
`updated_at`, reset result watermarks, wake the scheduler, or bump `execution_revision`.

## 8. Ownership and coexistence with UI/API

File ownership is per resource, not per project. A project may contain:

```text
api              managed by file/platform
database         managed by file/platform
temporary-check  managed by UI/API
```

- API/UI-created monitors have no provider ownership row and remain fully editable through
  existing RBAC.
- File reconciliation never compares desired files against all project monitors; it
  compares only against monitors owned by the same provider and tenant.
- Absence from a bundle never changes an unmanaged/UI-created monitor or a monitor owned
  by another provider.
- A file declaration never auto-adopts an unmanaged row by name, target, or other mutable
  fields. Display-name equality is not identity.
- A DB monitor is owned by at most one provider.
- File-managed declarative fields are read-only through normal CRUD. The API returns
  `409 managed_by_file` with provider, source UID, and tenant-safe relative source path.
- UI/API users may create additional unmanaged monitors in the same project after file
  reconciliation.

The store/application boundary must enforce ownership; a check only in an HTTP handler is
insufficient. API and provider writes use typed application paths (conceptually
`UpdateUserManagedMonitor` and `ApplyFileManagedBundle`) rather than a caller-controlled
boolean/source string.

Current pause toggles change declarative `enabled`; therefore version 1 disables that
control for a file-managed monitor. A future server-owned operational-pause overlay may be
allowed without giving UI/API ownership of the file spec, but is outside this version.

Automatic adoption/release is outside version 1. A future explicit transfer must preserve
the DB UUID, history, incidents, dependencies, and push token and must be audited; deleting
and recreating is not an acceptable transfer mechanism.

## 9. Reconciliation pipeline and atomicity

Filesystem events are hints to rescan; they are never translated directly into imperative
create/update/delete operations.

For each triggered scan:

1. Coalesce events for the configured debounce interval.
2. Acquire/confirm provider leadership (§12).
3. Enumerate eligible files deterministically and enforce all resource bounds.
4. Read each candidate and strict-decode its header and full bundle.
5. Resolve scope and tenant; group candidates by resolved project.
6. Reject duplicate project candidates.
7. Normalize and domain-validate monitors, references, and the dependency DAG.
8. Under a bounded context, read provider-owned current state and build a deterministic
   plan: `create | update | dependency_update | noop | orphan | restore`.
9. Apply one valid project plan in one PostgreSQL transaction, locking affected resources
   in deterministic ID/UID order.
10. In the same transaction persist bundle generation/provenance, append tenant audit
    records, and issue the scheduler notification when execution config changed.
11. After commit publish provider metrics/status; never publish success before commit.

The application service owns orchestration. It reuses the same domain monitor validation
as API CRUD but never invokes HTTP handlers. Monitor row, dependency edges, ownership rows,
bundle state, audit, and notification must not be split into independently committed
steps.

### 9.1 Per-project fault isolation

One project bundle is the atomic and failure-isolation unit:

- a valid payments bundle may apply while an independently resolved infrastructure bundle
  is invalid;
- an invalid bundle retains its own last-known-good generation;
- no valid subset within an invalid bundle applies;
- duplicate files targeting one project freeze that project only.

If a syntactically invalid file cannot be associated with a tenant/bundle, it is an
`unbound_error`: no state is applied from that file and provider-wide orphan processing is
suspended for that scan. Independently valid bundles may still apply non-destructive
create/update/no-op plans. Suspending orphaning prevents a broken/half-written replacement
from making a previously managed project appear intentionally absent.

An invalid file at a previously known relative path freezes the last-known-good bundle
recorded for that path. Invalid generations never advance orphan timers.

### 9.2 Concurrent API and ingest writes

- Ownership/provenance rows and monitor rows are locked/checked inside the apply
  transaction; a concurrent API write cannot pass a stale ownership check.
- A semantic file update uses the same config-write contract as `UpdateMonitor`, including
  P0b `execution_revision` bump and watermark reset. Monitoring as Code MUST NOT be enabled
  in production before the P0b contract in `func-result-protocol.md` is implemented.
- A dependency-only change updates the dependency graph without bumping
  `execution_revision`, matching D-0142.
- An in-flight old-revision result is rejected by result ingest after the file config
  commit; the file provider never manipulates live status/counters itself.

## 10. Disappearance, orphaning, and restoration

Only a fully valid bundle generation may declare a previously owned UID absent. The first
valid absence records `orphaned_at`; it does not delete history. Once the configured grace
period has elapsed and a subsequent valid scan still omits the UID, Cerbix disables the
monitor through the ownership-aware config-write path and records an audit event.

An entire bundle file disappearing follows the same rule for that provider/project. A
provider directory becoming unreadable, an invalid/ambiguous snapshot, or a lost watcher
never counts as desired absence.

Reintroducing the same tenant/source UID:

- reuses the same monitor DB UUID and push token;
- clears `orphaned_at`;
- reapplies changed semantics normally (and bumps revision only when needed);
- preserves heartbeat/SLA/incident/audit history.

The file provider never calls physical monitor delete. A later administrative prune is a
separate explicit, confirmed retention feature and is not implied by `orphan_grace_period`.

Removing/renaming a provider from the static config also performs no data mutation. It
leaves managed objects and their last-known-good provenance in PostgreSQL; provider
decommission/release requires an explicit future operation.

**Scope change is a quarantine, not adoption or deletion (D-0153).** Absence is trusted only
within the provider's current static scope: a project OUTSIDE the current scope is never
orphaned, even when absent from the snapshot. So a provider that restarts under the same name
with a narrower/different scope value leaves its prior-scope rows running, provider-owned,
read-only, and counted in diagnostics — skipped before the absence check with a throttled
`file_provider_owned_out_of_scope` warning. The provider cannot see the old scope's directory
intent, so it neither adopts nor destroys those rows. To move authority, give the new scope a
new provider name; to retire the old scope, revert to it and reconcile, or run an explicit
release/migration. Widening scope is safe: rows that fall back inside the widened scope resume
normal valid-absence semantics.

## 11. Filesystem/watch contract

- The provider watches the configured directory itself, not individual file inodes.
- Eligible files are regular immediate children ending in `.yaml` or `.yml`; dotfiles,
  editor temporaries, sockets, devices, FIFOs, and subdirectories are ignored/rejected as
  defined by strict tests. Version 1 is non-recursive.
- **Change detection is poll-based (normative; D-0147).** The provider samples a bounded
  directory signature (eligible file names + sizes + mtimes, no content read) on a short
  interval and triggers a debounced full rescan when it changes. A create, write, remove,
  rename, or replacement therefore triggers a debounced rescan within one poll interval; an
  event-driven (inotify/fsnotify) hint layer MAY be added in front of the same rescan path
  but is not required. Because the signature is name+size+mtime, a change that alters none of
  those — notably a pure `chmod` — is guaranteed to be observed no later than the next
  periodic resync (below), not necessarily on the next poll.
- A periodic full resync is mandatory (it is the primary correctness path, not a fallback):
  poll signals may miss a same-size+mtime change, and directory inodes may be replaced
  wholesale by ConfigMap/git-sync style updates.
- The provider re-establishes its watch after a directory replacement. While unavailable,
  last-known-good remains active and no orphaning occurs.
- Internal symlinks may be followed only when their canonical regular-file target remains
  inside the configured canonical root. Symlink escape rejects the file.
- Operators SHOULD publish files by write-to-temp + fsync + atomic rename, but correctness
  cannot depend solely on that convention.

## 12. Roles, HA, and scheduler propagation

The file provider is an API control-plane component:

- started by `--role all` and `--role api` only;
- never started by scheduler, worker, or agent;
- no current or future `controller` role is part of this contract.

Every `api`/`all` replica may run a candidate loop. Exactly one candidate per provider may
apply at a time, elected with a provider-specific PostgreSQL session advisory lock and a
same-session liveness check. Loss/error of the held lock stops apply before re-contention;
the pattern follows scheduler anti-split-brain leadership but uses distinct keys.

All candidates for one provider MUST see equivalent directory content. Advisory locking
prevents concurrent apply but cannot distinguish a stale local directory from a desired
new one; deployments therefore use a shared read-only volume, identical ConfigMap, or
identical git-sync checkout. This is a deployment invariant and must be documented and
alerted. A single-node `--role all` deployment needs no additional component.

After a transaction changes execution config it emits a same-transaction PostgreSQL
notification (channel contract: `monitor_config_changed`; payload is bounded generation/
project identity, never YAML). The scheduler refreshes affected state promptly. Its normal
periodic DB snapshot refresh remains the lost-notification recovery path. Workers continue
to receive ordinary jobs and know nothing about providers.

## 13. Logical persistence model

Exact table names may differ, but the schema MUST enforce equivalent constraints.

`file_provider_bundles` stores:

- provider ID, organization ID, project ID (unique bundle identity);
- tenant-safe relative source path and canonical content/spec hash;
- monotonically increasing applied generation;
- last applied/attempt timestamps, status, bounded structured error;
- last-known-good and orphan-scan state.

`managed_monitors` stores:

- monitor ID (unique FK to monitors);
- provider ID, organization ID, project ID, source UID;
- canonical monitor spec hash, source path, applied generation;
- applied/orphaned timestamps;
- unique `(provider, organization, project, source_uid)`.

Tenant FKs/queries must prove that `monitor.project_id` equals the provenance project. The
schema must prevent two providers from owning one monitor. Provider status errors must not
contain raw YAML or secret values.

Push monitor creation generates its token server-side through the existing secure path.
No reconcile may rotate/replace a token on update, no-op, file rename, orphan, or restore.

## 14. Security

- Provider directories are operator-trusted management inputs bounded by static scope;
  filesystem write permissions are therefore security-sensitive.
- YAML cannot create tenants, memberships, users, tokens, or elevate roles.
- Every project/reference lookup includes organization/project constraints; cross-project
  dependencies reject even when both projects are in one provider scope.
- Unknown fields, duplicate keys, invalid tags/types, alias expansion beyond size bounds,
  and custom YAML tags reject.
- No environment interpolation, shell execution, network includes, or implicit fallback.
- Inline secret-bearing monitor fields reject with `inline_secret_forbidden`. Logs, status
  records, plan diffs, and metrics never expose the submitted value.
  - A monitor `target` that carries credentials in its URL **userinfo**
    (`https://user:pass@host`, `postgres://user:pass@host/db`, password-only
    `https://:pass@host`) rejects.
  - A monitor `target` whose **query string** carries a known secret-bearing key
    (`?token=…`, `?api_key=…`, `?password=…`, …) rejects — the same finite secret-key set
    that classifies inline settings secrets applies to the target query, so a cleartext
    credential in the URL is caught wherever it sits. A query that cannot be decoded rejects
    conservatively; a query with only non-secret keys (`?x=1`) is accepted.
  - A **URL-shaped** target (one bearing a `://` scheme separator) that fails to parse
    (e.g. an invalid percent-escape or a control character) also rejects, because the target
    cannot then be proven free of embedded credentials and domain validation only checks the
    target is non-empty.
  - Rejection reasons never echo the raw target or any query value (D-0152).
- Absolute provider paths are visible only to global operators/logs. Tenant UI/API returns
  provider name and a sanitized relative path only.
- Parser nesting/alias expansion and all file/resource sizes are bounded to prevent CPU/
  memory denial of service.

## 15. API and UI contract

Normal project list/detail endpoints include bounded management metadata for each monitor:

```json
{
  "management": {
    "source": "file",
    "provider": "platform",
    "uid": "api",
    "path": "acme-payments.yaml",
    "read_only": true
  }
}
```

Unmanaged monitors report `source: "ui"` (or omit the extension consistently). Existing
tenant visibility applies before management metadata is returned.

The UI SHALL:

- show `Managed by file` and provider/source badges;
- leave New Monitor available so UI-managed monitors can coexist;
- disable declarative edit/delete/pause controls for file-managed monitors with an
  explanation;
- continue showing live status, heartbeats, SLA, incidents, and authorized push details;
- filter by source (`All`, `UI`, named provider) when the monitor list supports filters.

Global-admin provider diagnostics expose configured providers, leadership, last scan,
last successful generation(s), bounded errors, counts, and relative paths. Organization
admins may see only diagnostics wholly inside their organization; project members see
only per-monitor provenance already visible through normal project RBAC. No tenant can
observe another tenant's bundle/error/path.

Version 1 has no endpoint that edits YAML, forces adoption, or performs hard prune.

## 16. Observability and readiness

Required low-cardinality metrics (`provider` is bounded by the static max; no file/project/
monitor IDs in labels):

- `cerbix_file_provider_leader{provider}` gauge;
- `cerbix_file_provider_reconcile_total{provider,outcome}` with bounded outcomes
  `applied|noop|rejected|error`;
- `cerbix_file_provider_reconcile_duration_seconds{provider}`;
- `cerbix_file_provider_last_success_timestamp_seconds{provider}`;
- `cerbix_file_provider_managed_monitors{provider}`;
- `cerbix_file_provider_orphaned_monitors{provider}`;
- `cerbix_file_provider_bundle_errors{provider}` gauge.

Structured logs carry provider, relative path, tenant IDs/slugs, generation, plan counts,
duration, and a bounded error code/message. Repeated parse/watcher errors are rate-limited.
No monitor/file identifiers become Prometheus labels.

Readiness rules:

- invalid static provider config, an unreadable root at startup, DB absence, or inability
  to initialize the provider before serving is fail-fast for an `api`/`all` process;
- a follower not holding leadership remains ready;
- after startup, an invalid dynamic bundle or temporary watcher/root failure rejects the
  desired generation and marks provider status degraded but does not invalidate the
  already valid DB runtime or restart the process;
- last-known-good continues running; the degraded condition is explicit in status,
  metrics, logs, and an operational alert;
- no successful reconcile within an operator-defined alert window is alertable at fleet
  level, because local follower readiness cannot prove an active provider leader.

This is strict validation, not warn-and-continue: invalid desired state is never applied;
the previously committed valid state remains the runtime contract.

## 17. Backpressure and failure handling

- At most one reconcile runs per provider process; an event received while running sets a
  single `dirty` bit rather than enqueueing unbounded work. Completion immediately rescans
  once when dirty.
- Providers may reconcile concurrently only under a small global bound; DB transactions
  have lock/statement deadlines and deterministic lock order.
- A timed-out/failed transaction rolls back the entire bundle and records `error`; it never
  advances generation/provenance or scheduler notification.
- A provider leadership loss cancels parsing/planning/apply context; commit is authoritative
  only if the transaction completed while leadership was still confirmed by the
  implementation's fencing/lock discipline.
- Resource-limit violation rejects the affected file/bundle with a bounded diagnostic and
  never truncates or partially accepts it.

Failure summary:

| Condition | State mutation | Orphan clock | Runtime |
| --- | --- | --- | --- |
| Valid semantic change | Atomic apply | Advance as applicable | New DB state |
| Valid semantic no-op | None (except diagnostic attempt state) | Advance valid absence checks | Existing state |
| Invalid known bundle | None for bundle | Frozen | Last-known-good |
| Unbound invalid file | Valid non-destructive bundles may apply | Provider-wide orphaning suspended | Last-known-good for ambiguous scope |
| Directory/watcher unavailable after startup | None | Frozen | Last-known-good |
| Apply transaction failure | Rollback | Frozen | Last-known-good |
| File/UID restored | Atomic restore/update | Cleared | Same DB identity/history |

## 18. Required tests

### 18.1 Parser/domain

- strict root/monitor/type-specific known fields; duplicate keys/custom tags rejected;
- format/scope matrix, unknown org/project, cross-tenant references;
- UID/provider-name syntax, deterministic duration/default normalization;
- canonical hash ignores comments/key order/file rename and preserves ordered semantics;
- dependency missing/self/cycle/cross-project/UI-managed target rejection;
- inline secret and server-owned field rejection; parser size/depth/alias bounds.

### 18.2 Reconcile/store integration (real opt-in PostgreSQL)

- create/update/no-op in one transaction; rollback on any monitor/dependency error;
- two bundles in different projects: one invalid does not block the valid one;
- duplicate bundle target freezes last-known-good;
- API/UI monitor survives file apply/removal; other-provider monitor survives;
- API update of file-managed monitor is atomically rejected under a concurrency race;
- no-op/file rename does not change `updated_at` or `execution_revision`;
- semantic update bumps revision/reset watermark exactly per D-0142;
- dependency-only update does not bump revision;
- orphan grace, invalid-snapshot freeze, disable, restore same ID/history/token;
- transaction failure emits no scheduler notification and advances no generation;
- audit/provenance rows tenant-correct and secrets absent.

### 18.3 Watcher/HA

- create/write/atomic rename/remove coalesces to the expected rescan;
- file replacement and missed-event periodic resync recover;
- directory loss never appears as desired deletion and watch reattaches;
- dirty-bit coalescing is bounded under an event storm;
- two API replicas: advisory mutual exclusion, leadership loss/stepdown/failover;
- different providers may progress independently;
- race suite covers watcher/reconcile/status readers.

### 18.4 API/UI/E2E

- tenant cannot see another tenant's provider metadata/errors;
- file-managed CRUD returns `409 managed_by_file`; unmanaged CRUD still works;
- UI can create an additional monitor in a file-managed project;
- badges/read-only controls/filtering are correct for viewer/editor/admin;
- real binary: create org/project, place bundle, observe monitor scheduled without process
  restart; modify target/config, observe revision-safe update; break YAML, observe LKG;
  remove/restore and prove history/ID/token preservation;
- `--role all` works without extra services; distributed API candidates need no
  controller role.

## 19. Requirements and acceptance

### Functional requirements (FR-017)

- Configure named, tenant-scoped file providers in static config.
- Hot-detect and reconcile format-1 ProjectBundles without service restart/reload.
- Persist ownership/generation and apply bundle changes transactionally.
- Preserve last-known-good on invalid input and orphan/disable safely on valid absence.
- Allow unmanaged UI/API monitors to coexist in the same project.
- Surface management provenance and provider diagnostics under tenant-aware authorization.

### Non-functional requirements (NFR-014)

- Strict/bounded YAML and config parsing; no partial bundle apply or silent fallback.
- Tenant-scoped provider authority and references; no automatic tenant creation/adoption.
- HA-safe single active reconciler per provider inside `api`/`all`.
- Idempotent canonical no-op and D-0142 revision-safe semantic updates.
- No inline secrets, hard deletion, unbounded queues/labels, or history loss.

### Acceptance criteria

1. A format-1 bundle creates and updates monitors in an existing tenant without process
   restart, and scheduler execution observes the committed change promptly.
2. Invalid YAML/domain/reference input changes no bundle-owned DB state and retains LKG.
3. One invalid project bundle does not prevent an independently valid project bundle from
   applying; ambiguous invalid input cannot orphan anything.
4. UI/API-created monitors coexist and are never mutated by provider absence/diff.
5. File-managed CRUD is rejected atomically; source metadata is tenant-safe and visible.
6. No-op/touch/rename causes no semantic DB write or revision bump.
7. Removal disables only after a valid absence plus grace; restore reuses ID/history/token.
8. Multi-replica leadership/failover and watcher resync are race-tested and bounded.
9. Required metrics/logs/status and degraded/LKG alerts are documented and tested.
10. `go test -race ./...` plus the real-binary no-restart/LKG E2E are green.

## 20. Delivery order

Implementation SHALL be split into reviewable iterations; this specification does not
authorize combining all concerns in one change:

1. **Contract foundation:** strict static config + ProjectBundle parser/canonicalizer and
   pure validate/plan tests; no runtime apply.
2. **Persistence/ownership:** provenance/generation schema, typed application write paths,
   atomic one-shot reconciliation, API read-only enforcement; depends on P0b.
3. **Hot/HA:** directory watcher, debounce/resync, provider advisory leadership,
   scheduler notification, LKG/failure handling.
4. **Ops/UI:** metrics, alerts, audit, diagnostics API, badges/read-only states/filter,
   runbook and real-binary E2E.

Each implementation iteration updates `docs/status.md`, `docs/traceability.md`,
`docs/decisions.md`, config reference/runbook, OpenAPI/frontend types where applicable, and
its immutable iteration report. FR-017/NFR-014 remain `TODO` until every acceptance
criterion is delivered; partial phases may be `IN_PROGRESS` but never reported `DONE`.
