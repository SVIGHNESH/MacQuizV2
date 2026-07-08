#!/usr/bin/env bash
# Runs the go-live herd load test end to end against the local dev Compose
# stack (docs/12-implementation-plan.md, Milestone 9's "load test the
# go-live herd" requirement):
#
#   1. Seeds fixtures via `macquiz loadtest-seed` (a teacher, 2,000 students,
#      and a live-window quiz assigned to all of them) inside the app image,
#      reusing its real service layer instead of hand-rolled SQL.
#   2. Runs scripts/loadtest/herd.js with grafana/k6, sharing the app
#      container's network namespace so it can reach it on localhost
#      regardless of the Compose project/network name.
#
# Requires `docker compose up -d` already running (see docker-compose.yml).
# Everything after `--` is forwarded to `k6 run` (e.g. `-e HERD_STUDENTS=500`
# for a smaller smoke run, or `--out json=report.json`).
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."

if ! docker compose ps app --format '{{.State}}' 2>/dev/null | grep -q running; then
  echo "error: the app service isn't running - start it first: docker compose up -d" >&2
  exit 1
fi

echo "==> seeding load-test fixtures"
seed_json="$(docker compose run --rm \
  -e MACQUIZ_LOADTEST_STUDENTS="${MACQUIZ_LOADTEST_STUDENTS:-2000}" \
  app loadtest-seed)"
echo "$seed_json"

# loadtest-seed's stdout is a JSON log line followed by the pretty-printed
# fixture object; both contain the same quiz_id, so grabbing the first
# "quiz_id": "..." match is enough regardless of which one it lands on.
quiz_id="$(echo "$seed_json" | grep -o '"quiz_id":[[:space:]]*"[^"]*"' | head -1 | sed -E 's/.*"([^"]+)"$/\1/')"

if [ -z "$quiz_id" ]; then
  echo "error: could not parse quiz_id out of loadtest-seed output" >&2
  exit 1
fi
echo "==> quiz_id=$quiz_id"

app_cid="$(docker compose ps -q app)"
if [ -z "$app_cid" ]; then
  echo "error: could not resolve the running app container" >&2
  exit 1
fi

echo "==> running k6 herd.js against the app container's network namespace"
docker run --rm \
  --network "container:${app_cid}" \
  -v "$(pwd)/scripts/loadtest:/scripts:ro" \
  -e BASE_URL="http://localhost:8080" \
  -e WS_BASE_URL="ws://localhost:8080" \
  -e QUIZ_ID="$quiz_id" \
  -e STUDENT_PASSWORD="${MACQUIZ_LOADTEST_PASSWORD:-LoadTest!Pass123}" \
  grafana/k6:latest run "$@" /scripts/herd.js
