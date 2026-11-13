#!/usr/bin/env bash
# race-both: the full -race suite in BOTH storage modes at once, on throwaway databases.
#
# What it buys, and why nothing is lost:
#   * Two disposable Postgres containers — TimescaleDB (hypertable mode) and plain Postgres
#     (declarative-partition mode) — so the two runs CLAUDE.md requires for storage changes happen
#     side by side instead of one after the other.
#   * Both run with fsync, synchronous_commit and full_page_writes OFF and their data directory on
#     tmpfs. Those settings decide what survives a crash of the database SERVER; no test crashes it,
#     and the containers are deleted at the end, so every assertion sees the same SQL semantics
#     (transactions, locks, constraints, isolation) it sees on a durable server. The store suite
#     truncates before each test, and TRUNCATE is exactly the statement that pays for durability.
#   * The databases are named cerbix_test, which is what store.TruncateAll's safety gate requires,
#     and they live on their own ports — the dev `cerbix` database is never involved.
#
# Usage: scripts/race-both.sh [go test package patterns...]   (default ./...)
# Env:   GO, RACE_TS_IMAGE, RACE_PLAIN_IMAGE, RACE_TS_PORT, RACE_PLAIN_PORT, RACE_LOG_DIR,
#        RACE_KEEP=1 (leave the containers up for a follow-up single-test run)
set -u

GO="${GO:-go}"
TS_IMAGE="${RACE_TS_IMAGE:-timescale/timescaledb:2.17.2-pg16}"
PLAIN_IMAGE="${RACE_PLAIN_IMAGE:-postgres:16-alpine}"
TS_PORT="${RACE_TS_PORT:-55461}"
PLAIN_PORT="${RACE_PLAIN_PORT:-55462}"
LOG_DIR="${RACE_LOG_DIR:-.cache/race}"
TS_NAME="cerbix-race-ts"
PLAIN_NAME="cerbix-race-plain"

if [ "$#" -eq 0 ]; then
  set -- ./...
fi

mkdir -p "$LOG_DIR"

cleanup() {
  if [ "${RACE_KEEP:-}" != "1" ]; then
    docker rm -f "$TS_NAME" "$PLAIN_NAME" >/dev/null 2>&1
  fi
}
trap cleanup EXIT

start_db() { # name image port
  docker rm -f "$1" >/dev/null 2>&1
  docker run -d --name "$1" \
    --tmpfs /var/lib/postgresql/data:rw,size=2g \
    -e POSTGRES_USER=cerbix -e POSTGRES_PASSWORD=cerbix -e POSTGRES_DB=cerbix_test \
    -p "127.0.0.1:$3:5432" "$2" \
    postgres -c fsync=off -c synchronous_commit=off -c full_page_writes=off >/dev/null || {
      echo "race-both: could not start $1 ($2) on port $3" >&2
      exit 1
    }
}

# The image's entrypoint starts a temporary server for initdb scripts and then restarts, so
# pg_isready over the unix socket answers too early. A TCP query against the target database is
# the readiness barrier: the temporary server does not listen on TCP.
wait_db() { # name
  local i
  for i in $(seq 1 60); do
    if docker exec "$1" psql -h 127.0.0.1 -U cerbix -d cerbix_test -Atc 'select 1' >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "race-both: $1 did not become ready in 60 s" >&2
  docker logs --tail 20 "$1" >&2
  exit 1
}

start_db "$TS_NAME" "$TS_IMAGE" "$TS_PORT"
start_db "$PLAIN_NAME" "$PLAIN_IMAGE" "$PLAIN_PORT"
wait_db "$TS_NAME"
wait_db "$PLAIN_NAME"

# Compile once, before the clocks start, so the two runs do not race each other to fill the cache.
"$GO" test -race -count=1 -run '^$' "$@" >/dev/null 2>"$LOG_DIR/build.log" || {
  echo "race-both: the tests do not build — see $LOG_DIR/build.log" >&2
  tail -20 "$LOG_DIR/build.log" >&2
  exit 1
}

run_mode() { # label port packages...
  local label=$1 port=$2 start end rc
  shift 2
  start=$(date +%s)
  CERBIX_TEST_DATABASE_DSN="postgres://cerbix:cerbix@127.0.0.1:$port/cerbix_test?sslmode=disable" \
    "$GO" test -race -count=1 -timeout 40m "$@" >"$LOG_DIR/$label.log" 2>&1
  rc=$?
  end=$(date +%s)
  echo "$rc $((end - start))" >"$LOG_DIR/$label.rc"
}

rm -f "$LOG_DIR/timescale.rc" "$LOG_DIR/plain.rc"
run_mode timescale "$TS_PORT" "$@" &
run_mode plain "$PLAIN_PORT" "$@" &
wait

failed=0
for mode in timescale plain; do
  read -r rc secs <"$LOG_DIR/$mode.rc" 2>/dev/null || { rc=1; secs=0; }
  ok=$(grep -c '^ok ' "$LOG_DIR/$mode.log")
  bad=$(grep -c '^FAIL' "$LOG_DIR/$mode.log")
  echo "race-both: $mode — exit $rc, ${secs}s, $ok packages ok, $bad FAIL lines ($LOG_DIR/$mode.log)"
  if [ "$rc" != "0" ]; then
    failed=1
    grep -E '^(--- FAIL|FAIL|panic:)' "$LOG_DIR/$mode.log" | head -40
  fi
done
exit "$failed"
