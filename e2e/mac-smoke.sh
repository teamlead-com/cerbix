#!/usr/bin/env bash
# Monitoring-as-Code no-restart real-binary smoke (spec func-monitoring-as-code §18.4).
# Proves the live file provider inside `cerbix serve --role all` reconciles the FULL lifecycle
# WITHOUT restarting the process: create → scheduler executes the file-managed monitor →
# semantic update (generation bump, same DB id) → last-known-good on invalid input →
# orphan-disable (no hard delete) → restore (same DB id AND same push token). Uses an isolated
# throwaway DB; never touches dev data. Requires a reachable Postgres (default the dev compose
# on :5432) and `go`.
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
# wait_for <expected> <sql> <label> [tries]
wait_for(){ local want="$1" sql="$2" label="$3" tries="${4:-40}"; for _ in $(seq 1 "$tries"); do [ "$(PSQL "$sql")" = "$want" ] && return 0; sleep 0.5; done; echo "FAIL $label: got '$(PSQL "$sql")', want '$want'"; exit 1; }

# The good bundle: an http monitor (scheduler will probe it) + a push monitor (server-minted
# token; used to prove token preservation across orphan→restore).
write_good(){ local apiname="$1"; printf 'format: 1\norganization: acme\nproject: payments\nmonitors:\n  api: {name: %s, type: http, target: https://x, interval: 30s, timeout: 5s}\n  ping: {name: Ping, type: push, interval: 60s}\n' "$apiname" > "$D/mon.d/p.yaml"; }

"$D/cerbix" serve --config "$D/config.yaml" >"$D/serve.log" 2>&1 & PID=$!
trap 'kill $PID 2>/dev/null || true' EXIT
for i in $(seq 1 20); do curl -sf http://127.0.0.1:18080/readyz >/dev/null 2>&1 && break; sleep 0.5; done; sleep 2

# 1) CREATE — both monitors appear.
write_good API
wait_for 2 "SELECT count(*) FROM monitors" "create"
MID_HTTP="$(PSQL "SELECT id FROM monitors WHERE name='API'")"
MID_PUSH="$(PSQL "SELECT id FROM monitors WHERE type='push'")"
PUSH_HASH1="$(PSQL "SELECT push_token_hash FROM monitors WHERE id='$MID_PUSH'")"
GEN1="$(PSQL "SELECT generation FROM file_provider_bundles WHERE provider_id='platform'")"
[ -n "$MID_HTTP" ] && [ -n "$MID_PUSH" ] && [ -n "$PUSH_HASH1" ] || { echo "FAIL create: missing ids/token"; exit 1; }

# 2) SCHEDULER EXEC — the inproc scheduler picks up the file-managed http monitor (via the
#    monitor_config_changed NOTIFY) and probes it; a heartbeat row is written.
for _ in $(seq 1 50); do [ "$(PSQL "SELECT count(*) FROM heartbeats WHERE monitor_id='$MID_HTTP'")" -gt 0 ] 2>/dev/null && break; sleep 0.5; done
[ "$(PSQL "SELECT count(*) FROM heartbeats WHERE monitor_id='$MID_HTTP'")" -gt 0 ] || { echo "FAIL scheduler-exec: no heartbeat for file-managed monitor"; exit 1; }

# 3) UPDATE — a semantic change (rename) applies IN PLACE: same DB id, new name, generation bumps.
write_good API2
wait_for API2 "SELECT name FROM monitors WHERE id='$MID_HTTP'" "update-name"
[ "$(PSQL "SELECT count(*) FROM monitors")" = "2" ] || { echo "FAIL update: monitor was recreated, not updated in place"; exit 1; }
GEN2="$(PSQL "SELECT generation FROM file_provider_bundles WHERE provider_id='platform'")"
[ "$GEN2" -gt "$GEN1" ] || { echo "FAIL update: generation did not advance ($GEN1 -> $GEN2)"; exit 1; }

# 4) LAST-KNOWN-GOOD — invalid input must NOT orphan/disable/mutate the live monitors.
printf 'format: 1\n  this: [is not: valid yaml\n' > "$D/mon.d/p.yaml"
sleep 6   # > resync_interval + debounce: the bad bundle is scanned and rejected
[ "$(PSQL "SELECT name FROM monitors WHERE id='$MID_HTTP'")" = "API2" ] || { echo "FAIL LKG: last-known-good name was lost on invalid input"; exit 1; }
[ "$(PSQL "SELECT count(*) FROM monitors WHERE enabled")" = "2" ] || { echo "FAIL LKG: invalid input disabled live monitors"; exit 1; }
[ "$(PSQL "SELECT generation FROM file_provider_bundles WHERE provider_id='platform'")" = "$GEN2" ] || { echo "FAIL LKG: rejected bundle advanced the generation"; exit 1; }

# restore a valid bundle (identical to last good) → clean no-op re-apply, monitors intact.
write_good API2
wait_for applied "SELECT status FROM file_provider_bundles WHERE provider_id='platform'" "recover-applied"

# 5) ORPHAN-DISABLE — removing the file disables (never hard-deletes) both owned monitors.
rm -f "$D/mon.d/p.yaml"
wait_for 0 "SELECT count(*) FROM monitors WHERE enabled" "orphan-disable"
[ "$(PSQL "SELECT count(*) FROM monitors")" = "2" ] || { echo "FAIL orphan: monitors were hard-deleted"; exit 1; }

# 6) RESTORE — re-adding the same source restores in place: SAME DB ids AND SAME push token.
write_good API2
wait_for 2 "SELECT count(*) FROM monitors WHERE enabled" "restore-enable"
[ "$(PSQL "SELECT id FROM monitors WHERE type='push'")" = "$MID_PUSH" ] || { echo "FAIL restore: push monitor got a new DB id"; exit 1; }
[ "$(PSQL "SELECT id FROM monitors WHERE type='http'")" = "$MID_HTTP" ] || { echo "FAIL restore: http monitor got a new DB id"; exit 1; }
[ "$(PSQL "SELECT push_token_hash FROM monitors WHERE id='$MID_PUSH'")" = "$PUSH_HASH1" ] || { echo "FAIL restore: push token was NOT preserved across orphan→restore"; exit 1; }

# 7) NO RESTART — the whole lifecycle ran in one live process.
kill -0 $PID || { echo "FAIL: process restarted during the lifecycle"; exit 1; }
echo "PASS: create + scheduler-exec + in-place update (gen bump) + last-known-good + orphan-disable + restore (same id & push token), NO process restart"
