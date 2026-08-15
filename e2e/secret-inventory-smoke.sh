#!/usr/bin/env bash
# FR-020 live control-plane + pull-agent smoke. Uses one isolated throwaway database;
# role=all owns the master/materializer and a separate DB-less role=agent owns only its
# regional dispatch key. No source/dev database is touched.
set -euo pipefail

# The smoke provisions its OWN throwaway Postgres when the named container is absent, so
# `make secret-smoke` works on a clean checkout. Previously it depended on a container that
# nothing in the repository created, which made the only live proof of FR-020 unrunnable
# for anyone but the machine it was written on — an unrunnable check is not a check.
PG_CONTAINER="${PG_CONTAINER:-cerbix-secret-smoke-pg}"
PG_PORT="${PG_PORT:-55452}"
PG_USER="${PG_USER:-postgres}"
PG_PASSWORD="${PG_PASSWORD:-pass}"
OWN_CONTAINER=""
# A REAL auth-less Redis. It is the control for the fail-open pin: an anonymous PING
# against it answers +PONG, so if a stripped credential ever reached a prober the monitor
# would report UP. Nothing else in the smoke can demonstrate that lie end to end.
REDIS_CONTAINER="cerbix-secret-smoke-redis"
REDIS_PORT="${REDIS_PORT:-56379}"

if ! docker inspect "$PG_CONTAINER" >/dev/null 2>&1; then
  OWN_CONTAINER="$PG_CONTAINER"
  docker run -d --name "$PG_CONTAINER" \
    -e POSTGRES_USER="$PG_USER" -e POSTGRES_PASSWORD="$PG_PASSWORD" \
    -p "$PG_PORT":5432 postgres:16-alpine >/dev/null
  for _ in $(seq 1 60); do
    if docker exec "$PG_CONTAINER" pg_isready -U "$PG_USER" >/dev/null 2>&1; then break; fi
    sleep 1
  done
fi
DB_NAME="cerbix_secret_inventory_smoke"
BASE_URL="http://127.0.0.1:18082"
TOKEN="cbx_secret_inventory_smoke"
VALUE="secret-smoke-value"
TARGET_USER="cerbix_secret_smoke_user"
TMP="$(mktemp -d)"
mkdir -p "$TMP/monitoring.d"

cleanup() {
  if [ -n "${AGENT_PID:-}" ]; then kill "$AGENT_PID" 2>/dev/null || true; wait "$AGENT_PID" 2>/dev/null || true; fi
  if [ -n "${CERBIX_PID:-}" ]; then kill "$CERBIX_PID" 2>/dev/null || true; wait "$CERBIX_PID" 2>/dev/null || true; fi
  docker exec "$PG_CONTAINER" psql -U "$PG_USER" -d postgres -c "DROP DATABASE IF EXISTS $DB_NAME WITH (FORCE)" >/dev/null 2>&1 || true
  docker exec "$PG_CONTAINER" psql -U "$PG_USER" -d postgres -c "DROP ROLE IF EXISTS $TARGET_USER" >/dev/null 2>&1 || true
  # Only remove a container this run created: a pre-existing one may belong to someone else.
  if [ -n "$OWN_CONTAINER" ]; then docker rm -f "$OWN_CONTAINER" >/dev/null 2>&1 || true; fi
  docker rm -f "$REDIS_CONTAINER" >/dev/null 2>&1 || true
}
trap cleanup EXIT

docker rm -f "$REDIS_CONTAINER" >/dev/null 2>&1 || true
docker run -d --name "$REDIS_CONTAINER" -p "$REDIS_PORT":6379 redis:7-alpine >/dev/null
for _ in $(seq 1 60); do docker exec "$REDIS_CONTAINER" redis-cli ping 2>/dev/null | grep -q PONG && break; sleep 0.5; done
docker exec "$REDIS_CONTAINER" redis-cli ping 2>/dev/null | grep -q PONG || { echo "FAIL: auth-less redis control did not start"; exit 1; }

go build -buildvcs=false -o "$TMP/cerbix" ./cmd/cerbix
docker exec "$PG_CONTAINER" psql -U "$PG_USER" -d postgres -c "DROP DATABASE IF EXISTS $DB_NAME WITH (FORCE)" >/dev/null
docker exec "$PG_CONTAINER" psql -U "$PG_USER" -d postgres -c "DROP ROLE IF EXISTS $TARGET_USER" >/dev/null
docker exec "$PG_CONTAINER" psql -U "$PG_USER" -d postgres -c "CREATE DATABASE $DB_NAME" >/dev/null
docker exec "$PG_CONTAINER" psql -U "$PG_USER" -d postgres -c "CREATE ROLE $TARGET_USER LOGIN PASSWORD '$VALUE'; GRANT CONNECT ON DATABASE $DB_NAME TO $TARGET_USER" >/dev/null

cat >"$TMP/config.yaml" <<YAML
server: {listen: "127.0.0.1:18082"}
database: {dsn: "postgres://$PG_USER:$PG_PASSWORD@127.0.0.1:$PG_PORT/$DB_NAME?sslmode=disable"}
log: {level: info}
prober: {allow_private_ips: true, allow_metadata_ips: false}
security:
  encryption_key: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
  dispatch:
    regions:
      edge:
        primary: {id: "smoke-2026a", key: "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="}
        previous: []
secrets: {enabled: true, dispatch_envelope: "enforced"}
pull:
  regions: [edge]
  token: "$TOKEN"
providers:
  file:
    platform:
      directory: "$TMP/monitoring.d"
      debounce: 200ms
      resync_interval: 5s
      orphan_grace_period: 30s
      scope: {type: instance}
YAML

cat >"$TMP/agent-bad.yaml" <<YAML
server: {listen: "127.0.0.1:18083"}
log: {level: info}
prober: {allow_private_ips: true, allow_metadata_ips: false}
security:
  dispatch:
    regions:
      edge:
        primary: {id: "smoke-2026a", key: "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI="}
        previous: []
secrets: {enabled: true, dispatch_envelope: "enforced"}
pull: {server_url: "$BASE_URL", token: "$TOKEN"}
YAML

cat >"$TMP/agent-good.yaml" <<YAML
server: {listen: "127.0.0.1:18084"}
log: {level: info}
prober: {allow_private_ips: true, allow_metadata_ips: false}
security:
  dispatch:
    regions:
      edge:
        primary: {id: "smoke-2026a", key: "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="}
        previous: []
secrets: {enabled: true, dispatch_envelope: "enforced"}
pull: {server_url: "$BASE_URL", token: "$TOKEN"}
YAML

"$TMP/cerbix" migrate --config "$TMP/config.yaml" >/dev/null
TOKEN_HASH="$(printf %s "$TOKEN" | sha256sum | awk '{print $1}')"
docker exec "$PG_CONTAINER" psql -U "$PG_USER" -d "$DB_NAME" -v ON_ERROR_STOP=1 -c \
  "INSERT INTO organizations(slug,name) VALUES('acme','Acme');
   INSERT INTO projects(org_id,slug,name) SELECT id,'payments','Payments' FROM organizations WHERE slug='acme';
   INSERT INTO api_tokens(org_id,project_id,name,role,token_hash)
     SELECT o.id,p.id,'smoke','editor','$TOKEN_HASH' FROM organizations o JOIN projects p ON p.org_id=o.id;" >/dev/null
PSQL() { docker exec "$PG_CONTAINER" psql -U "$PG_USER" -d "$DB_NAME" -tAc "$1" | tr -d '[:space:]'; }
PROJECT_ID="$(PSQL "SELECT id FROM projects WHERE slug='payments'")"

"$TMP/cerbix" serve --config "$TMP/config.yaml" >"$TMP/serve.log" 2>&1 & CERBIX_PID=$!
for _ in $(seq 1 40); do curl -sf "$BASE_URL/readyz" >/dev/null 2>&1 && break; sleep 0.25; done
curl -sf "$BASE_URL/readyz" >/dev/null

CREATE_BODY="$(curl -sf -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d "{\"name\":\"db-password\",\"value\":\"$VALUE\"}" \
  "$BASE_URL/api/v1/projects/$PROJECT_ID/secrets")"
if printf %s "$CREATE_BODY" | grep -q "$VALUE"; then echo "FAIL: create API echoed secret value"; exit 1; fi

cat >"$TMP/monitoring.d/payments.yaml" <<YAML
format: 1
organization: acme
project: payments
monitors:
  database:
    name: Database
    type: postgres
    target: 127.0.0.1:$PG_PORT
    region: edge
    interval: 10s
    timeout: 5s
    settings:
      username: $TARGET_USER
      database: $DB_NAME
      password_ref: db-password
      sslmode: disable
      query: SELECT 1
YAML

for _ in $(seq 1 60); do [ "$(PSQL "SELECT count(*) FROM monitors WHERE name='Database'")" = 1 ] && break; sleep 0.25; done
MONITOR_ID="$(PSQL "SELECT id FROM monitors WHERE name='Database'")"
[ -n "$MONITOR_ID" ] || { echo "FAIL: file-managed postgres monitor was not created"; cat "$TMP/serve.log"; exit 1; }

# A v2-capable agent with the right key id but wrong key bytes remains process-live,
# reports decrypt_auth_failed as a diagnostic (never a heartbeat/DOWN), and degrades
# both local readiness and its central credential_ready capability.
"$TMP/cerbix" serve --config "$TMP/agent-bad.yaml" --role agent --region edge >"$TMP/agent-bad.log" 2>&1 & AGENT_PID=$!
for _ in $(seq 1 120); do [ "$(PSQL "SELECT COALESCE(last_probe_error_reason,'') FROM monitors WHERE id='$MONITOR_ID'")" = decrypt_auth_failed ] && break; sleep 0.25; done
[ "$(PSQL "SELECT COALESCE(last_probe_error_reason,'') FROM monitors WHERE id='$MONITOR_ID'")" = decrypt_auth_failed ] || { echo "FAIL: wrong-key agent did not report decrypt_auth_failed"; cat "$TMP/agent-bad.log" "$TMP/serve.log"; exit 1; }
[ "$(PSQL "SELECT count(*) FROM heartbeats WHERE monitor_id='$MONITOR_ID'")" = 0 ] || { echo "FAIL: probe_error was stored as a heartbeat"; exit 1; }
[ "$(curl -sS -o /dev/null -w '%{http_code}' http://127.0.0.1:18083/readyz)" = 503 ] || { echo "FAIL: wrong-key agent readiness did not degrade"; exit 1; }
for _ in $(seq 1 80); do [ "$(PSQL "SELECT count(*) FROM agent_heartbeats WHERE region='edge' AND NOT credential_ready")" -gt 0 ] && break; sleep 0.25; done
[ "$(PSQL "SELECT count(*) FROM agent_heartbeats WHERE region='edge' AND NOT credential_ready")" -gt 0 ] || { echo "FAIL: central capability did not record degraded agent"; exit 1; }
kill "$AGENT_PID"; wait "$AGENT_PID" 2>/dev/null || true; unset AGENT_PID

# A correctly keyed replacement agent recovers capability/readiness and executes the
# next authoritative v2 job against a real PostgreSQL target.
"$TMP/cerbix" serve --config "$TMP/agent-good.yaml" --role agent --region edge >"$TMP/agent-good.log" 2>&1 & AGENT_PID=$!
for _ in $(seq 1 160); do [ "$(PSQL "SELECT count(*) FROM heartbeats WHERE monitor_id='$MONITOR_ID' AND up")" -gt 0 ] 2>/dev/null && break; sleep 0.25; done
[ "$(PSQL "SELECT count(*) FROM heartbeats WHERE monitor_id='$MONITOR_ID' AND up")" -gt 0 ] || { echo "FAIL: credentialed postgres probe did not become UP: $(PSQL "SELECT msg FROM heartbeats WHERE monitor_id='$MONITOR_ID' ORDER BY ts DESC LIMIT 1")"; cat "$TMP/serve.log"; exit 1; }
[ "$(curl -sS -o /dev/null -w '%{http_code}' http://127.0.0.1:18084/readyz)" = 200 ] || { echo "FAIL: correctly keyed agent is not ready"; exit 1; }
[ "$(PSQL "SELECT count(*) FROM agent_heartbeats WHERE region='edge' AND credential_ready")" -gt 0 ] || { echo "FAIL: central capability did not recover"; exit 1; }

[ "$(PSQL "SELECT config->>'password_ref' FROM monitors WHERE id='$MONITOR_ID'")" = db-password ] || { echo "FAIL: monitor reference was not persisted"; exit 1; }
[ "$(PSQL "SELECT count(*) FROM project_secrets WHERE project_id='$PROJECT_ID' AND value_encrypted LIKE 'enc:v2a:%'")" = 1 ] || { echo "FAIL: inventory value is not AAD ciphertext"; exit 1; }
[ "$(PSQL "SELECT count(*) FROM monitors WHERE id='$MONITOR_ID' AND (config ? 'password')")" = 0 ] || { echo "FAIL: MaC monitor persisted an inline password"; exit 1; }
if grep -q "$VALUE" "$TMP/serve.log" "$TMP/agent-bad.log" "$TMP/agent-good.log"; then echo "FAIL: secret value appeared in service logs"; exit 1; fi

REV_BEFORE="$(PSQL "SELECT execution_revision FROM monitors WHERE id='$MONITOR_ID'")"
GEN_BEFORE="$(PSQL "SELECT generation FROM file_provider_bundles WHERE provider_id='platform'")"
HB_BEFORE="$(PSQL "SELECT count(*) FROM heartbeats WHERE monitor_id='$MONITOR_ID'")"
ROTATE_CODE="$(curl -sS -o "$TMP/rotate.json" -w '%{http_code}' -X PATCH -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"value":"rotated-smoke-value"}' "$BASE_URL/api/v1/projects/$PROJECT_ID/secrets/db-password")"
[ "$ROTATE_CODE" = 204 ] || { echo "FAIL: rotate returned $ROTATE_CODE"; cat "$TMP/rotate.json"; exit 1; }
for _ in $(seq 1 40); do [ "$(PSQL "SELECT execution_revision FROM monitors WHERE id='$MONITOR_ID'")" -gt "$REV_BEFORE" ] && break; sleep 0.25; done
[ "$(PSQL "SELECT execution_revision FROM monitors WHERE id='$MONITOR_ID'")" -gt "$REV_BEFORE" ] || { echo "FAIL: rotate did not fence monitor revision"; exit 1; }
[ "$(PSQL "SELECT generation FROM file_provider_bundles WHERE provider_id='platform'")" = "$GEN_BEFORE" ] || { echo "FAIL: secret rotation changed MaC generation"; exit 1; }
for _ in $(seq 1 80); do [ "$(PSQL "SELECT count(*) FROM heartbeats WHERE monitor_id='$MONITOR_ID'")" -gt "$HB_BEFORE" ] && break; sleep 0.25; done
[ "$(PSQL "SELECT count(*) FROM heartbeats WHERE monitor_id='$MONITOR_ID'")" -gt "$HB_BEFORE" ] || { echo "FAIL: no post-rotation credentialed probe"; exit 1; }

RENAME_CODE="$(curl -sS -o "$TMP/rename.json" -w '%{http_code}' -X PATCH -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"renamed-password"}' "$BASE_URL/api/v1/projects/$PROJECT_ID/secrets/db-password")"
[ "$RENAME_CODE" = 409 ] && grep -q secret_renamed_in_use "$TMP/rename.json" || { echo "FAIL: file-managed rename guard returned $RENAME_CODE"; cat "$TMP/rename.json"; exit 1; }
DELETE_CODE="$(curl -sS -o "$TMP/delete.json" -w '%{http_code}' -X DELETE -H "Authorization: Bearer $TOKEN" \
  "$BASE_URL/api/v1/projects/$PROJECT_ID/secrets/db-password")"
[ "$DELETE_CODE" = 409 ] && grep -q secret_in_use "$TMP/delete.json" || { echo "FAIL: delete guard returned $DELETE_CODE"; cat "$TMP/delete.json"; exit 1; }

# ---------------------------------------------------------------------------
# Live fail-open pin (§9). The unit tests prove the structural gate refuses a stripped
# credential BEFORE dialling; only a real auth-less target can demonstrate what that
# refusal prevents. This Redis answers PING to anyone, so an anonymous probe would report
# UP — a monitor that looks healthy while its credential has been removed is monitoring
# that lies. A legitimately credentialed probe against it can only be DOWN (Redis rejects
# AUTH when no password is set), so UP is unreachable by any honest path.
cat >>"$TMP/monitoring.d/payments.yaml" <<YAML
  cache:
    name: Cache
    type: redis
    target: 127.0.0.1:$REDIS_PORT
    region: edge
    interval: 60s
    timeout: 5s
    settings:
      password_ref: db-password
      tls: "false"
YAML

for _ in $(seq 1 80); do [ "$(PSQL "SELECT count(*) FROM monitors WHERE name='Cache'")" = 1 ] && break; sleep 0.25; done
REDIS_MONITOR_ID="$(PSQL "SELECT id FROM monitors WHERE name='Cache'")"
[ -n "$REDIS_MONITOR_ID" ] || { echo "FAIL: redis monitor was not created"; cat "$TMP/serve.log"; exit 1; }

# Stop the agent so the enqueued job is still pending when the credential is stripped.
kill "$AGENT_PID" 2>/dev/null || true; wait "$AGENT_PID" 2>/dev/null || true; unset AGENT_PID
for _ in $(seq 1 120); do
  [ "$(PSQL "SELECT count(*) FROM pull_jobs WHERE payload->'monitor'->>'id'='$REDIS_MONITOR_ID'")" -gt 0 ] && break
  sleep 0.25
done
[ "$(PSQL "SELECT count(*) FROM pull_jobs WHERE payload->'monitor'->>'id'='$REDIS_MONITOR_ID'")" -gt 0 ] || { echo "FAIL: no pending redis job to tamper with"; exit 1; }
[ "$(PSQL "SELECT count(*) FROM pull_jobs WHERE payload->'monitor'->>'id'='$REDIS_MONITOR_ID' AND payload ? 'credential_envelope'")" -gt 0 ] || { echo "FAIL: redis job carried no credential envelope to strip"; exit 1; }

# The attack: delete one JSON member. No key, no forgery.
PSQL "UPDATE pull_jobs SET payload = payload - 'credential_envelope' WHERE payload->'monitor'->>'id'='$REDIS_MONITOR_ID'" >/dev/null

"$TMP/cerbix" serve --config "$TMP/agent-good.yaml" --role agent --region edge >"$TMP/agent-strip.log" 2>&1 & AGENT_PID=$!
# Wait for EITHER outcome, then assert. The order matters: if the gate ever regresses the
# run must fail saying the monitor reported UP, because that is the defect — a probe_error
# that failed to appear is only its symptom.
for _ in $(seq 1 160); do
  STRIP_REASON="$(PSQL "SELECT COALESCE(last_probe_error_reason,'') FROM monitors WHERE id='$REDIS_MONITOR_ID'")"
  STRIP_UP="$(PSQL "SELECT count(*) FROM heartbeats WHERE monitor_id='$REDIS_MONITOR_ID' AND up")"
  { [ "$STRIP_REASON" = decrypt_auth_failed ] || [ "$STRIP_UP" -gt 0 ]; } && break
  sleep 0.25
done
[ "$STRIP_UP" = 0 ] || { echo "FAIL: a credential-stripped redis probe reported UP — the gate let an anonymous probe through and the monitor now lies"; cat "$TMP/agent-strip.log"; exit 1; }
[ "$STRIP_REASON" = decrypt_auth_failed ] || { echo "FAIL: stripped credential did not produce a typed probe_error (got '$STRIP_REASON')"; cat "$TMP/agent-strip.log"; exit 1; }
docker exec "$REDIS_CONTAINER" redis-cli ping 2>/dev/null | grep -q PONG || { echo "FAIL: the auth-less control target stopped answering, so the pin proves nothing"; exit 1; }

# ---------------------------------------------------------------------------
# Live mixed-capability barrier (§9). Core emits generation 3 into this region because a
# capability-2 agent proved it can open envelope v2. A capability-1 client is a legitimate
# mid-rollout state, not an attack, and it must never be handed a generation-3 row.
kill "$AGENT_PID" 2>/dev/null || true; wait "$AGENT_PID" 2>/dev/null || true; unset AGENT_PID
for _ in $(seq 1 160); do
  [ "$(PSQL "SELECT COALESCE(max(protocol_version),0) FROM pull_jobs WHERE region='edge'")" = 3 ] && break
  sleep 0.25
done
[ "$(PSQL "SELECT COALESCE(max(protocol_version),0) FROM pull_jobs WHERE region='edge'")" = 3 ] || { echo "FAIL: core never emitted carrier generation 3 into a capability-2 region"; exit 1; }

CAP1_CLAIM="$(curl -sS -H "Authorization: Bearer $TOKEN" -H 'X-Cerbix-Credential-Envelope: 1'   "$BASE_URL/api/v1/agent/v2/jobs?region=edge&max=16")"
if printf %s "$CAP1_CLAIM" | grep -qE '"protocol_versions":\[[^]]*3'; then
  echo "FAIL: a capability-1 claim was handed a generation-3 row"; printf %s "$CAP1_CLAIM" | head -c 400; exit 1
fi
CAP2_CLAIM="$(curl -sS -H "Authorization: Bearer $TOKEN" -H 'X-Cerbix-Credential-Envelope: 2'   "$BASE_URL/api/v1/agent/v3/jobs?region=edge&max=16")"
printf %s "$CAP2_CLAIM" | grep -qE '"protocol_versions":\[[^]]*3' || { echo "FAIL: a capability-2 claim did not receive the generation-3 row"; printf %s "$CAP2_CLAIM" | head -c 400; exit 1; }
UNDERSTATED="$(curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN"   -H 'X-Cerbix-Credential-Envelope: 1' "$BASE_URL/api/v1/agent/v3/jobs?region=edge")"
[ "$UNDERSTATED" = 400 ] || { echo "FAIL: an understated capability was accepted on the generation-3 endpoint ($UNDERSTATED)"; exit 1; }

if grep -q "$VALUE" "$TMP/agent-strip.log"; then echo "FAIL: secret value appeared in agent logs"; exit 1; fi
kill -0 "$CERBIX_PID" || { echo "FAIL: cerbix process exited"; exit 1; }
echo "PASS: secret API → MaC ref → AAD at rest → pull-v2 wrong-key degradation → keyed recovery/JIT decrypt → postgres UP → rotate fence/no generation drift → rename/delete guards → live fail-open pin (auth-less redis never reports UP) → live generation-3 emission + mixed-capability barrier"
