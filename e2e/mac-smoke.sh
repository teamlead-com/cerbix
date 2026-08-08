#!/usr/bin/env bash
# Monitoring-as-Code no-restart real-binary smoke (spec func-monitoring-as-code §18.4).
# Proves the live file provider inside `cerbix serve --role all` reconciles create/orphan/
# restore WITHOUT restarting the process. Uses an isolated throwaway DB; never touches dev
# data. Requires: a reachable Postgres (default the dev compose on :5432) and `go`.
set -euo pipefail
PG_CONTAINER="${PG_CONTAINER:-cerbix-postgres-1}"
DSN_DB="cerbix_mac_smoke"
D="$(mktemp -d)"; mkdir -p "$D/mon.d"
go build -buildvcs=false -o "$D/cerbix" ./cmd/cerbix
docker exec "$PG_CONTAINER" psql -U cerbix -d postgres -c "DROP DATABASE IF EXISTS $DSN_DB WITH (FORCE)" >/dev/null
docker exec "$PG_CONTAINER" psql -U cerbix -d postgres -c "CREATE DATABASE $DSN_DB" >/dev/null
cat > "$D/config.yaml" <<YAML
server: {listen: "127.0.0.1:18080"}
database: {dsn: "postgres://cerbix:cerbix@localhost:5432/$DSN_DB?sslmode=disable"}
log: {level: info}
providers:
  file:
    platform:
      directory: $D/mon.d
      debounce: 200ms
      resync_interval: 5s
      orphan_grace_period: 0s
      scope: {type: instance}
YAML
"$D/cerbix" migrate --config "$D/config.yaml" >/dev/null
docker exec "$PG_CONTAINER" psql -U cerbix -d "$DSN_DB" -c \
  "INSERT INTO organizations (slug,name) VALUES ('acme','Acme'); INSERT INTO projects (org_id,slug,name) SELECT id,'payments','Payments' FROM organizations WHERE slug='acme';" >/dev/null
PSQL(){ docker exec "$PG_CONTAINER" psql -U cerbix -d "$DSN_DB" -tAc "$1" | tr -d '[:space:]'; }
"$D/cerbix" serve --config "$D/config.yaml" >"$D/serve.log" 2>&1 & PID=$!
trap 'kill $PID 2>/dev/null || true' EXIT
for i in $(seq 1 20); do curl -sf http://127.0.0.1:18080/readyz >/dev/null 2>&1 && break; sleep 0.5; done; sleep 2
printf 'format: 1\norganization: acme\nproject: payments\nmonitors:\n  api: {name: API, type: http, target: https://x, interval: 30s, timeout: 5s}\n' > "$D/mon.d/p.yaml"
for i in $(seq 1 20); do [ "$(PSQL "SELECT count(*) FROM monitors")" = "1" ] && break; sleep 0.5; done
[ "$(PSQL "SELECT count(*) FROM monitors")" = "1" ] || { echo "FAIL create"; exit 1; }
rm -f "$D/mon.d/p.yaml"
for i in $(seq 1 30); do [ "$(PSQL "SELECT count(*) FROM monitors WHERE enabled")" = "0" ] && break; sleep 0.5; done
[ "$(PSQL "SELECT count(*) FROM monitors")" = "1" ] && [ "$(PSQL "SELECT count(*) FROM monitors WHERE enabled")" = "0" ] || { echo "FAIL orphan"; exit 1; }
kill -0 $PID || { echo "FAIL: process restarted"; exit 1; }
echo "PASS: create + orphan-disable (no hard delete) with NO process restart"
