#!/usr/bin/env sh
# Runs the E2E suite in the official Playwright container against a live stack.
#   ./e2e/run.sh                          # http://localhost:8080
#   CERBIX_URL=http://host:8080 ./e2e/run.sh tests/monitors.spec.ts
# The suite creates e2e-prefixed entities and cleans them up — dev stacks only.
set -e
cd "$(dirname "$0")"
docker run --rm --network host \
  -e CERBIX_URL="${CERBIX_URL:-http://localhost:8080}" \
  -e CERBIX_ADMIN_EMAIL -e CERBIX_ADMIN_PASSWORD \
  -v "$PWD":/e2e -w /e2e mcr.microsoft.com/playwright:v1.49.0-noble \
  sh -c "npm i --no-fund --no-audit --loglevel=error >/dev/null && npx playwright test $*"
