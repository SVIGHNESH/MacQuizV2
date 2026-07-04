# MacQuiz v2 - Implementation Plan

Derived from SDD-001 v2.0 and DEP-001 v1.0.
Status: proposed build order; each milestone is independently demoable and testable.

## Guiding rules

- Build in vertical slices: each milestone ends with a working end-to-end flow, not a finished layer.
- The submit funnel, policy function, and serializer isolation are written once, early, and everything else reuses them.
- Every milestone lands with its tests; the E2E suite grows with each slice.

## Milestone 0 - Repo and skeleton (foundation)

- Monorepo: `apps/web` (React + TypeScript), `apps/server` (NestJS), shared `packages/contracts` (API types, event types, error codes).
- NestJS modules scaffolded: auth-users, quiz, attempt, analytics, realtime, worker entrypoint.
- Postgres migrations tooling; initial schema from 03-data-model.md.
- Docker Compose dev stack (postgres, redis, app, worker); CI running lint, typecheck, tests on PR.

Exit: `docker compose up` serves a health check; CI green.

## Milestone 1 - Identity and provisioning (FR-1)

- Admin login, JWT access + rotating refresh in httpOnly cookies, Argon2id, forced first-login password reset.
- `POST /users`, `PATCH /users/:id`, groups and membership.
- The `can(actor, action, resource)` policy function and guard wiring on every route.
- `audit_log` writes for every mutation.

Exit: admin provisions a teacher and a student; both log in; audit rows exist.

## Milestone 2 - Authoring (FR-2)

- Quiz draft CRUD, question CRUD with validation, reorder, editor autosave.
- Serializer layer with the student-role stripping of `correct` (tested from day one even though students cannot see quizzes yet).

Exit: teacher creates a draft quiz with questions of all four types.

## Milestone 3 - Scheduling, assignment, lifecycle (FR-4, FR-5)

- Publish: snapshot + version, window validation, guardrail config storage.
- Assignments (individual + group expansion), 404-not-403 scoping, student quiz list.
- Scheduler jobs `open_quiz` / `close_quiz` via BullMQ delayed jobs, plus lazy state validation on read and worker boot re-scan.

Exit: quiz goes live at starts_at with no manual action; assigned student sees it, unassigned student gets 404.

## Milestone 4 - Attempt player (FR-6)

- Attempt start transaction (assignment + window + max_attempts), deadline precompute, Redis snapshot cache.
- Autosave upserts with deadline + status checks; resume endpoint; per-attempt deadline timer job; the `submit(attempt_id, kind)` funnel with manual/auto/forced.
- Grading job (deterministic, idempotent) and results per release policy.

Exit: E2E: student takes a quiz, closes the laptop, is auto-submitted at deadline, gets graded.

## Milestone 5 - Realtime (FR-7)

- WebSocket gateway with channel authorization (`quiz:{id}:monitor`, `attempt:{id}`, `user:{id}:notify`).
- attempt_events append + Redis publish on every attempt action; snapshot endpoint `GET /quizzes/:id/live`; snapshot + delta dashboard.
- Heartbeat, disconnected/reconnected flags, progress coalescing (1 event / 2 s), polling fallback.

Exit: teacher watches 3 simulated students progress live under 1 s latency; reconnect shows a consistent roster.

## Milestone 6 - Guardrails and kick (FR-10, FR-11)

- Player guardrails: fullscreen lock + blocker overlay, focus tracking with durations, clipboard/context-menu block, violation toasts.
- Violation pipeline (WS + REST fallback), violation_count, ladder actions (flag / auto_submit / notify).
- Kick endpoint (reason required, rate-limited), kick transaction via the submit funnel, socket lockout delivery, 409 ATTEMPT_KICKED fallback, readmit flow.

Exit: E2E: student leaves fullscreen 3 times and is flagged; teacher kicks with a reason; student sees the lockout screen; readmit grants a fresh attempt.

## Milestone 7 - Bulk import (FR-3)

- Pre-signed upload to R2, import worker validation with per-row/column errors, review UI, transactional commit.

Exit: 500-row CSV imports cleanly; a bad file produces a row-level error report and writes nothing.

## Milestone 8 - Analytics (FR-8, FR-9)

- Rollup-on-close job: quiz_stats (distribution, mean/median, item analysis, integrity summary) and student_stats.
- Teacher and admin dashboards, org-wide views, CSV exports.

Exit: closing a quiz produces dashboards without heavy live queries.

## Milestone 9 - Production hardening and launch

- Deploy per 09-deployment.md: VM, Compose, Caddy, Cloudflare (DNS/Pages/R2), GitHub Actions deploy, live-quiz deploy freeze.
- Backups + restore drill, UptimeRobot, Grafana Cloud dashboards and alert thresholds per 10-operations.md.
- Load test the go-live herd: 1,000 simulated starts in 60 s, 2,000 sockets, autosave p95 < 300 ms verified.

Exit: production URL, monitored, backed up, load-tested.

## Test strategy summary

- Unit: policy function, deadline math, grading, ladder logic.
- Integration: submit-funnel races (kick vs auto-submit), import transactionality, scheduler recovery after Redis loss.
- E2E (Playwright): the exit criteria of milestones 4, 6, 7 as scripted user journeys, run in CI.
- Load: k6 or artillery scenario for the synchronized start, run before launch and before each exam season.

## Design-review scorecard (system-design framework)

Diagnostic check of the design as documented:

| Diagnostic | Status |
|-----------|--------|
| Functional + non-functional requirements listed | Pass (01-requirements.md) |
| QPS and storage estimates | Pass (01 section 5) |
| Every component redundant | Fail by choice at Tier 0: single VM, single region; mitigated by backups + restore path; Tier 2 adds warm standby |
| DB scaling strategy defined | Pass (vertical first, read replica, split later; no premature sharding) |
| Cache for read-heavy paths | Pass (Redis snapshot cache, rollup tables) |
| Async via queues | Pass (BullMQ: imports, grading, scheduling, rollups) |
| Monitoring and alerting plan | Pass (10-operations.md) |
| Deployment strategy defined | Pass (09-deployment.md; deploy freeze in live windows instead of zero-downtime) |

Score: 9/10.
The single conscious gap is redundancy, traded for $0 hosting with defined paying triggers; revisit when the "real exams with consequences" trigger fires.
