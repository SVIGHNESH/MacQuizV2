#!/usr/bin/env bash
# Runs every web/e2e/*.e2e.mjs browser suite against a real docker-compose
# stack (docs/09-deployment.md section 4) plus a `vite dev` SPA - the same
# combination each suite's own header comment documents as its prerequisite.
#
# Before this script existed, these suites only ran when someone brought the
# stack up by hand (see .gnhf run notes: "E2E verification gap" - a real
# regression sat undetected across several iterations because nothing ran
# them automatically). This is what CI's e2e job (.github/workflows/ci.yml)
# invokes, and it is safe to run the same way locally.
#
# Order matters: auth.e2e.mjs and admin.e2e.mjs are the only suites that log
# into the shared admin account through the browser UI (not just the API),
# so they run first while the account's login rate-limit budget
# (docs/08 section 4: 5/minute) is still fresh. Every suite's admin-login
# API call retries on 429 using the server's Retry-After header, so the
# remaining suites self-pace even if the budget is briefly exhausted.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

CHROMIUM="${E2E_CHROMIUM:-}"
if [ -z "$CHROMIUM" ]; then
  for candidate in google-chrome-stable google-chrome chromium chromium-browser; do
    if command -v "$candidate" >/dev/null 2>&1; then
      CHROMIUM="$(command -v "$candidate")"
      break
    fi
  done
fi
if [ -z "$CHROMIUM" ]; then
  echo "no Chromium/Chrome binary found; set E2E_CHROMIUM=/path/to/browser" >&2
  exit 1
fi
export E2E_CHROMIUM="$CHROMIUM"

cleanup() {
  status=$?
  if [ -n "${DEV_PID:-}" ]; then
    kill "$DEV_PID" 2>/dev/null || true
    wait "$DEV_PID" 2>/dev/null || true
  fi
  if [ "${E2E_KEEP_STACK:-}" != "1" ]; then
    docker compose down >/dev/null 2>&1 || true
  fi
  exit "$status"
}
trap cleanup EXIT

echo "== bringing up the docker-compose stack =="
docker compose up --build -d

echo "== waiting for the API to report healthy =="
for _ in $(seq 1 60); do
  if curl -fsS http://localhost:8080/healthz >/dev/null 2>&1; then
    break
  fi
  sleep 2
done
if ! curl -fsS http://localhost:8080/healthz >/dev/null 2>&1; then
  echo "API never became healthy" >&2
  docker compose logs --tail 100
  exit 1
fi

echo "== installing web deps and starting the vite dev server =="
(cd web && npm ci)
# --strictPort: without it vite silently moves to 5174 when 5173 is occupied
# (say, by a leftover dev server), the health check below still passes against
# whatever squats on 5173, and the suites then fail with ERR_CONNECTION_REFUSED
# the moment that process dies. Fail loudly at startup instead.
(cd web && npm run dev -- --host 127.0.0.1 --port 5173 --strictPort) &
DEV_PID=$!

for _ in $(seq 1 60); do
  if curl -fsS http://localhost:5173/ >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
if ! curl -fsS http://localhost:5173/ >/dev/null 2>&1; then
  echo "vite dev server never came up" >&2
  exit 1
fi

# auth/admin first: see the ordering note above.
SUITES=(auth admin authoring import livemonitor player publish resultscsv analytics)

failures=()
for suite in "${SUITES[@]}"; do
  echo "== e2e: $suite =="
  if ! (cd web && node "e2e/${suite}.e2e.mjs"); then
    failures+=("$suite")
  fi
done

if [ "${#failures[@]}" -gt 0 ]; then
  echo "FAILED suites: ${failures[*]}" >&2
  exit 1
fi
echo "All e2e suites passed."
