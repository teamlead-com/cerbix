# Monitoring as Code — bundle directory

**Mental model:** a YAML file here is the *desired state* of one project's monitors. cerbix
continuously reconciles reality to match the files — like GitOps for uptime checks. You never
click "New monitor" for these; you edit a file, and the monitors appear, change, or get disabled
to match. Monitors owned by a file are **read-only in the UI/API** (a 409 on edit/delete), so the
file stays the single source of truth.

This directory is mounted read-only into the dev `cerbix` container at `/etc/cerbix/monitoring.d`
and watched by the `platform` file provider declared in `docker/config.dev.yaml`.

---

## How the flow works

```
  docker/monitoring.d/*.yaml        (you edit these — the desired state)
            │  poll + debounce (~1–2s), plus a periodic full resync
            ▼
  file provider (runs only in --role api / all; one leader across replicas)
            │  1. read *.yaml / *.yml  →  2. decode + validate (strict; reject on any error)
            │  3. resolve tenant from SCOPE (see table)  →  4. diff vs current owned monitors
            ▼
  ONE transaction per project:  create / update / no-op / orphan / restore
            ▼
  monitors in the DB, tagged "managed by file"  →  scheduler probes them as usual
```

Key properties:

- **Only `.yaml` / `.yml` are read.** `example.yaml.example` is deliberately ignored, so the
  provider is idle out of the box (no monitors created until you add a real file).
- **One file = one project.** Every monitor in a file belongs to the file's `organization`/`project`.
  Manage a second project with a second file. Two files claiming the same project → both frozen
  (kept as-is), never silently merged.
- **Strict & fail-safe.** A malformed/invalid file is rejected and the last-known-good is kept —
  a broken edit never wipes live monitors.
- **Tenants are never created.** The `organization`/`project` slugs must already exist in the DB.

---

## Scope → what the file header must contain

The provider's `scope` (in `config.dev.yaml`) decides which tenant a file targets and therefore
which header fields are allowed:

| Provider scope | Header fields the file must set | Meaning |
| --- | --- | --- |
| `instance` (dev default) | `organization:` **and** `project:` | file may target any org/project |
| `organization` | `project:` only (org is fixed by config) | file targets one project in the pinned org |
| `project` | neither (both fixed by config) | file targets exactly the pinned project |

Setting a field the scope forbids (e.g. `organization:` under a project-scoped provider) is a
rejection. The dev stack uses **instance** scope, so the example sets both.

---

## Try it in the dev stack

```bash
# 1. bring up the dev stack (file provider is already configured, idle)
docker compose -f docker/docker-compose.yml --profile single up -d --build

# 2. create the tenant the example targets (slugs must exist first) — via the UI, or SQL:
#    INSERT INTO organizations (slug,name) VALUES ('acme','Acme');
#    INSERT INTO projects (org_id,slug,name) SELECT id,'payments','Payments' FROM organizations WHERE slug='acme';

# 3. drop in a real bundle (drop the .example suffix so it is matched)
cp docker/monitoring.d/example.yaml.example docker/monitoring.d/payments.yaml
#    edit organization:/project: if you used different slugs

# 4. within ~1–2s the monitors appear — no restart. Verify:
#    • UI: Monitors list shows them with a read-only "Managed by file" badge
#    • API (global admin): GET /api/v1/admin/file-providers  → {bundles, providers}
```

---

## Lifecycle (what each change does)

| You do… | cerbix does… |
| --- | --- |
| Add a monitor (new UID) | **create** |
| Change a monitor's fields | **update** (bumps the execution revision) |
| Re-save with no real change / rename the file | **no-op** — no DB write, no revision bump |
| Change only `depends_on` | dependency update — graph changes, **no** revision bump |
| Remove a monitor / delete the file | **orphan** → after the `orphan_grace_period`, the monitor is **disabled** (never hard-deleted; history/incidents/push-token preserved) |
| Re-add the same UID later | **restore** — same DB id, same push token, re-enabled |
| Set `enabled: false` on a monitor | declaratively **pause** it (kept, not deleted) |

There is no automatic hard delete: removing a file disables its monitors after grace, it does not
erase their history. To truly remove them, disable via the file first, then delete through an
explicit admin action.

---

## Rules & gotchas

- **UID = the map key** under `monitors:` (`^[a-z][a-z0-9-]{0,62}$`). It is the stable identity —
  renaming a UID is treated as delete-old + create-new. Keep it stable.
- **`type` is immutable** for a UID (push-token/identity semantics). To change type, use a new UID.
- **No inline secrets** — any password/token/key field rejects the whole bundle. Create the
  secret in the project's Secrets inventory, then use `password_ref` in typed `settings`.
- **Supported types:** `http`, `tcp`, `icmp`, `dns`, `tls`, `grpc`, `websocket`, `ssh`, `push`,
  `postgres`, `mysql`, `redis`, `rabbitmq`. Credentialed settings and encrypted-by-default TLS
  rules are defined in `docs/specs/func-secret-inventory.md`; composite/synthetic/promql remain
  unavailable through files.
- **Durations are whole seconds:** `30s`, `1m`, `2m`, `1h` — not `500ms`, not `1.5s`.
- **`region:`** routes a monitor to a prober pool (default `core`). A non-core region needs a
  **live** worker/agent or its jobs TTL-expire (nothing probes them). Composite monitors are
  core-only.
- **Where it runs:** only the `api` / `all` roles watch this directory; a single leader reconciles
  across replicas (`scheduler`/`worker`/`agent` never touch it).

See the full contract in [`docs/specs/func-monitoring-as-code.md`](../../docs/specs/func-monitoring-as-code.md)
and the config reference in [`docker/config.example.yaml`](../config.example.yaml) (`providers.file`).
