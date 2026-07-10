# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

MacQuiz v2: a quiz/exam platform.
Go modular-monolith backend (`server/`), React + TypeScript SPA (`web/`), OpenAPI contract (`api/openapi.yaml`), and a distilled design/spec reference in `docs/` (start with `docs/README.md`; the root `*.html` files are the raw source documents it was distilled from).

## Commands

Dev loop (Docker Compose is the intended path):

```sh
make up           # Postgres (host :5433), Redis (host :6380), migrate + bootstrap one-shots, app (:8080), worker
make down
docker compose logs -f app worker
make web-dev      # Vite SPA on :5173, proxies /api, /healthz, /ws to :8080
```

Bootstrap admin (from docker-compose.yml): `admin@macquiz.local` / `admin-dev-password`.
The admin UI is a placeholder; provision a teacher via the API to exercise the real workspace (see HowToRun.txt).

Server (all from `server/`, or via the root Makefile):

```sh
make build        # binary -> server/bin/macquiz
make test         # go test ./... ; DB-backed tests need the Compose stack up
make vet
make fmt
cd server && go test ./internal/attempt -run TestName   # single test
```

Integration tests read `MACQUIZ_TEST_DATABASE_URL` (CI: `postgres://macquiz:macquiz@localhost:5432/macquiz?sslmode=disable`; local Compose stack exposes 5433) and `MACQUIZ_TEST_REDIS_URL`.
Each DB-backed test creates its own throwaway database via `itest.FreshDatabase`, so the connecting role needs CREATEDB.
CI also runs golangci-lint (config: `server/.golangci.yml`) and fails if `gofmt -l` is non-empty.

Web (all from `web/`):

```sh
npm run dev
npm run build     # tsc -b then vite build
npm run lint      # oxlint
npm run typecheck
```

API contract codegen; CI fails if either generated file drifts from `api/openapi.yaml`:

```sh
make generate-api          # web/src/api/schema.d.ts
make generate-server-api   # server/internal/apischema/types.gen.go
```

Browser E2E (puppeteer-core; needs Docker plus a local Chrome/Chromium, override with `E2E_CHROMIUM=`):

```sh
./scripts/e2e/run.sh                 # full run: brings up the stack, runs every suite
node web/e2e/publish.e2e.mjs         # single suite, needs stack + vite dev already running
```

Suite order matters in the full run: `auth` and `admin` go first because they consume the shared admin account's login rate-limit budget (5/min).

## Architecture

One Go binary (`server/cmd/macquiz`), multiple modes: `serve` (HTTP API + realtime gateway), `worker` (River job consumers: scheduler open/close, per-attempt deadline timers, grading, imports, analytics rollups), `migrate`, `bootstrap`, `loadtest-seed`.
Dev and prod Compose stacks run `serve` and `worker` as separate containers of the same image.

Module layout under `server/internal/`: `authusers`, `quiz`, `attempt`, `analytics`, `realtime`, `audit` are the business modules.
Each exposes `Routes() http.Handler`; `httpserver` mounts them under `/api/v1` (and the realtime gateway at `/ws`, outside the REST timeout middleware) and contains no business logic itself.
Cross-module calls are wired explicitly in `cmd/macquiz/main.go` (e.g. quiz's route tree hosts attempt's start handler; the gateway gets owner-lookup callbacks).

Key mechanics:

- **Contract-first API**: `api/openapi.yaml` is the source of truth; both the Go types and the TS client schema are generated from it and drift-checked in CI.
- **Migrations**: plain goose SQL files embedded in the binary (`server/internal/db/migrations/`); `serve` applies pending migrations at boot before accepting traffic, serialized by a Postgres session lock.
- **Jobs**: River queue lives in Postgres (no separate broker). The worker re-scans Postgres at boot for due-but-unfired transitions, so a queue outage can never leave a quiz stuck.
- **Redis is best-effort only**: realtime pub/sub fan-out, snapshot cache, and sessions. A bad URL fails boot, but an unreachable Redis degrades (publishes drop time-bounded, cache reads become misses) and must never stall the REST write path or fail boot.
- Same degrade-not-fail posture for optional integrations: email (Resend), telemetry (OTel -> Grafana Cloud), R2 import storage - each disabled when its env vars are unset (see `server/internal/config/config.go`, every knob has a dev default).
- **Realtime**: events are persisted to `attempt_events` first, then published to Redis (`persist first, publish second`). The event envelope has no sequence number, so clients must never accumulate counters from deltas; compute in SQL and re-read.
- **Frontend is strictly same-origin**: `web/src/api/client.ts` uses `baseUrl: '/'` and WebSocket URLs derive from `location.host`; httpOnly cookies, no CORS. The Vite proxy provides this in dev, Caddy in prod. Always go through the typed `api` client, never fetch directly.
- **UI tokens**: all styling flows from `web/src/styles/tokens.css`; components never use raw hex (docs/11-frontend-design-system.md).

## Non-negotiable invariants (docs/README.md)

1. Server time is the only clock; `deadline_at` is the single timing authority.
2. `questions.correct` never reaches a student client.
3. All attempt terminations (manual, auto, forced, kicked) go through the one idempotent `submit(attempt_id, kind)` funnel.
4. Every permission decision goes through `can(actor, action, resource)`; unassigned resources return 404.
5. Events and audit rows are append-only; persist first, publish second.
6. Client guardrail signals are evidence; only server-side status flips enforce anything.
7. No deploys while any quiz is live (`/deploy-check` endpoint gates the deploy workflow).
