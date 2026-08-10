# Installing cerbix

Two supported single-node paths — pick one:

- **[Option A — Docker Compose](#option-a--docker-compose)** (recommended): one command, PostgreSQL included.
- **[Option B — bare binary + systemd](#option-b--bare-binary--systemd)**: one static binary against your own PostgreSQL.

Either way the app side is a single process: the binary embeds the web UI, the REST API,
and the database migrations (they apply themselves on startup). The only hard external
dependency is **PostgreSQL 16+** (the `timescale/timescaledb` image is preferred — cerbix
then stores heartbeats as compressed hypertables automatically; plain PostgreSQL works too).

---

## Option A — Docker Compose

### 1. Get the repo and prepare secrets

```bash
git clone https://github.com/teamlead-com/cerbix.git
cd cerbix
cp docker/.env.prod.example docker/.env
```

Edit `docker/.env`:

```bash
POSTGRES_PASSWORD=<strong password>
RABBITMQ_PASSWORD=<strong password>
CERBIX_ENCRYPTION_KEY=$(openssl rand -base64 32)   # secrets-at-rest key — keep it safe
CERBIX_ADMIN_EMAIL=admin@example.com               # global admin, created on first start
CERBIX_ADMIN_PASSWORD=<strong password>
# CERBIX_HTTP_BIND=127.0.0.1:8080                  # default: localhost-only, see step 3
```

> Losing `CERBIX_ENCRYPTION_KEY` makes stored channel credentials unreadable — back it up.

### 2. Start

```bash
docker compose --env-file docker/.env -f docker/docker-compose.prod.yml up -d
```

This pulls `ghcr.io/teamlead-com/cerbix` (SPA + binary in one distroless image; pin a
version via `CERBIX_IMAGE` in `docker/.env`) and starts `postgres`, `rabbitmq`, and
`cerbix` (`--role all`). Migrations apply automatically; the admin account is created on
the first start against an empty database.

> While the repository is private, pulling needs `docker login ghcr.io` with a PAT
> (`read:packages`). Alternatively build the image from source:
> `docker build -t ghcr.io/teamlead-com/cerbix:latest .` at the repo root.

Verify:

```bash
curl -s http://127.0.0.1:8080/healthz     # {"status":"ok"}
docker compose --env-file docker/.env -f docker/docker-compose.prod.yml logs -f cerbix
```

### 3. Put TLS in front

The API/UI binds to `127.0.0.1:8080` by design. Terminate TLS with your reverse proxy
(Traefik, nginx, Caddy) and proxy to it. Only set `CERBIX_HTTP_BIND=0.0.0.0:8080` if you
deliberately expose the port directly.

### 4. Log in

Open `https://your-host/`, sign in with `CERBIX_ADMIN_EMAIL` / `CERBIX_ADMIN_PASSWORD`,
and rotate the password in the UI. Configure SMTP/OIDC/branding under **Settings** —
these live in the database and override the config file from then on.

### Upgrading

```bash
# bump CERBIX_IMAGE in docker/.env to the new tag, then:
docker compose --env-file docker/.env -f docker/docker-compose.prod.yml pull cerbix
docker compose --env-file docker/.env -f docker/docker-compose.prod.yml up -d
```

New migrations apply on startup. Data lives in the named volumes `pgdata` and
`rabbit_volume` and survives rebuilds. Back up with
`docker exec <postgres-container> pg_dump -U cerbix cerbix > cerbix.sql`.

---

## Option B — bare binary + systemd

### 1. Get the binary

From a GitHub release (assets are static, no libc required):

```bash
curl -fL -o cerbix \
  https://github.com/teamlead-com/cerbix/releases/download/vX.Y.Z/cerbix_vX.Y.Z_linux_amd64
# arm64 hosts: ..._linux_arm64. Verify against SHA256SUMS from the same release.
sudo install -m 0755 cerbix /usr/local/bin/cerbix
cerbix version
```

> While the repository is private, release assets need authentication — use
> `gh release download vX.Y.Z -R teamlead-com/cerbix` instead of curl.
> Or build from source at the repo root: `make build` (binary lands in `bin/cerbix`;
> requires Go 1.25+ and an already-built SPA in `internal/web/dist`).

### 2. Prepare PostgreSQL

On your PostgreSQL server:

```sql
CREATE USER cerbix WITH PASSWORD '<strong password>';
CREATE DATABASE cerbix OWNER cerbix;
-- Optional but recommended: the timescaledb extension (chunked, compressed
-- heartbeat storage). Requires the TimescaleDB package on the server:
\c cerbix
CREATE EXTENSION IF NOT EXISTS timescaledb;
```

Without the extension cerbix falls back to plain daily partitions automatically.

### 3. System user, directories, config

```bash
sudo useradd --system --home /nonexistent --shell /usr/sbin/nologin cerbix
sudo mkdir -p /etc/cerbix
```

`/etc/cerbix/config.yaml` (full reference: [`docker/config.example.yaml`](docker/config.example.yaml)):

```yaml
server:
  listen: "127.0.0.1:8080"     # behind your TLS reverse proxy

database:
  dsn: "postgres://cerbix:${CERBIX_DB_PASSWORD}@127.0.0.1:5432/cerbix?sslmode=disable"

local:
  enabled: true

security:
  admin_email: "admin@example.com"
  admin_password: "${CERBIX_ADMIN_PASSWORD}"   # creates the global admin on an empty system
  encryption_key: "${CERBIX_ENCRYPTION_KEY}"   # openssl rand -base64 32

session:
  secure: true                 # requires HTTPS in front
```

`${VARS}` are expanded from the process environment at startup, so secrets stay out of
the config file. Put them in `/etc/cerbix/cerbix.env`:

```bash
sudo tee /etc/cerbix/cerbix.env >/dev/null <<'EOF'
CERBIX_DB_PASSWORD=<strong password>
CERBIX_ADMIN_PASSWORD=<strong password>
CERBIX_ENCRYPTION_KEY=<openssl rand -base64 32>
EOF
sudo chown root:cerbix /etc/cerbix/cerbix.env /etc/cerbix/config.yaml
sudo chmod 0640 /etc/cerbix/cerbix.env /etc/cerbix/config.yaml
```

### 4. systemd unit

`/etc/systemd/system/cerbix.service`:

```ini
[Unit]
Description=cerbix — uptime & SLA monitoring
Wants=network-online.target
After=network-online.target postgresql.service

[Service]
Type=simple
User=cerbix
Group=cerbix
EnvironmentFile=/etc/cerbix/cerbix.env
ExecStart=/usr/local/bin/cerbix serve --config /etc/cerbix/config.yaml --role all
Restart=on-failure
RestartSec=3
LimitNOFILE=65536

# Hardening — cerbix writes nothing to disk (all state is in PostgreSQL).
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ProtectKernelTunables=true
ProtectControlGroups=true
RestrictSUIDSGID=true

# Only needed if you use ICMP (ping) monitors; delete otherwise.
# AmbientCapabilities=CAP_NET_RAW

[Install]
WantedBy=multi-user.target
```

> ICMP monitors alternative to CAP_NET_RAW: allow unprivileged ping for the service
> group via `sysctl -w net.ipv4.ping_group_range="0 2147483647"`.

### 5. Start and verify

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now cerbix
curl -s http://127.0.0.1:8080/healthz     # {"status":"ok"}
journalctl -u cerbix -f                   # JSON logs; look for bootstrap_admin_created
```

Put your TLS reverse proxy in front of `127.0.0.1:8080` and log in with the admin
credentials from step 3.

### Upgrading

```bash
sudo systemctl stop cerbix
sudo install -m 0755 cerbix-new /usr/local/bin/cerbix
sudo systemctl start cerbix     # new migrations apply on startup
```

Migrations are forward-only: take a `pg_dump` before upgrading if you want a rollback
path. Ops endpoints for your monitoring: `/healthz`, `/readyz`, and Prometheus metrics
at `/metrics`.

---

## Scaling beyond one node

The same binary runs as separate `api` / `scheduler` / `worker` / `agent` processes over
RabbitMQ (per-region worker pools, HTTP-pull agents for segments with no broker access).
See the profiles in [`docker/docker-compose.yml`](docker/docker-compose.yml), the
multi-geo stack in [`docker/docker-compose.geo.yml`](docker/docker-compose.geo.yml), and
the deployment map in [`docs/overview.md`](docs/overview.md). One caveat when running
multiple roles: apply migrations once (`cerbix migrate --config …`) before starting
several roles on a schema that has new migrations.
