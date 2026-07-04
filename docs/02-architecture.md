# MacQuiz v2 - Architecture

Source: SDD-001 v2.0 sections 4, 15; DEP-001 sections 1-3.
Status: implementation baseline.

## 1. Shape: modular monolith plus workers

One well-factored deployable, not microservices.
At this scale (thousands of concurrent students, not millions), a single deployable is simpler, more robust, and easier to reason about.
The modules are separate packages with clean interfaces so any of them can be split into a service later without a rewrite.

Two pieces are designed for independence from day one because their scaling profiles differ:

- The realtime gateway: many long-lived WebSocket connections, little CPU.
- The import/grading workers: bursty, queue-driven.

All cross-module communication that could later cross a process boundary goes through Redis pub/sub and the job queue.
Extraction later is a deployment change, not a design change.

## 2. Component diagram

```
Clients
  Admin console | Teacher app | Student app        (one React SPA, role-based routing)
        |  HTTPS / REST + WebSocket
Edge
  API gateway (TLS, auth check, rate limiting, routing)
  Realtime gateway (WebSocket fan-out, channel authorization)
        |  internal RPC / pub-sub
Application modules (one Node process in the default deployment)
  auth-users   : login, RBAC policy, admin provisioning
  quiz         : authoring, versioning, scheduling, assignment
  attempt      : session state, timing, autosave, grading triggers, guardrails, kick
  analytics    : aggregation jobs, reporting queries
  import-worker: bulk-upload validation and commit (runs in the worker process)
        |  SQL / cache / queue
Data layer
  PostgreSQL 16   : source of truth (users, quizzes, attempts, answers)
  Redis 7         : sessions, live presence, pub/sub, snapshot cache, queue backend
  Object storage  : bulk-upload files, question images, exports (S3-compatible, R2)
  Job queue       : BullMQ on Redis (imports, grading, scheduled open/close, rollups)
```

## 3. Module boundaries and responsibilities

| Module | Owns | Never does |
|--------|------|-----------|
| auth-users | Accounts, sessions, JWT issuance, the central `can(actor, action, resource)` policy, groups | Quiz or attempt logic |
| quiz | Draft CRUD, question CRUD, imports, publish/snapshot, scheduling, assignments | Grading, attempt state |
| attempt | Attempt start/resume, autosave, deadline enforcement, submit funnel, violations, kick/readmit, attempt_events | Editing quiz content |
| analytics | quiz_stats and student_stats rollups, reporting endpoints, exports | Writes to transactional tables |
| realtime gateway | Socket lifecycle, channel subscribe authorization, fan-out of Redis pub/sub events | Business logic; it only relays |
| worker | BullMQ consumers: scheduler transitions, deadline timers, grading, imports, rollups, backup trigger | Serving HTTP |

## 4. Key architectural decisions

1. Server-authoritative timing.
   The quiz opens, closes, and auto-submits based on server time; client clocks are never trusted.
   Client countdowns are cosmetic, driven by a server-provided deadline plus a clock-offset estimate.
2. Snapshot on publish.
   Publishing copies the question set into an immutable version; attempts pin the version they ran against.
   Mid-quiz edits can never corrupt an in-flight attempt.
3. Single policy function.
   Permissions are enforced at the API layer with `can(actor, action, resource)` evaluated on every request and at every WebSocket channel subscribe.
   The UI merely hides what the API would reject.
4. One submit funnel.
   Manual submit, deadline auto-submit, force-close, and kick all go through the same idempotent `submit(attempt_id, kind)` routine.
   The deadline check, grading trigger, event emission, and idempotency guarantees are shared; races resolve to whichever committed first.
5. Client signals are evidence, server actions are enforcement.
   Nothing the browser reports blocks anything by itself, and nothing the browser suppresses can keep the server from terminating an attempt.
6. Lazy state validation.
   The scheduler fires open/close jobs at exact timestamps, but the API also validates quiz state on read.
   If a request arrives after starts_at but before the job ran, the quiz is treated as live; a delayed job can never block students.
7. Events are persisted, then fanned out.
   Every realtime event is first an appended `attempt_events` row, then a Redis publish.
   The dashboard is snapshot + delta, so late joins and reconnects are consistent and the timeline is replayable for audit.

## 5. Load-shape driven design

The defining pattern is the synchronized start: hundreds of students hit "start" in the same minute, then a steady drizzle of autosaves, then a submit spike at the deadline.

- Attempt start is one small transaction plus a Redis-cached question snapshot (cached at go-live).
- Autosaves are single-row upserts on a primary key; they scale linearly and never contend.
- Grading and rollups are queue jobs; the close-time spike is absorbed by the queue, not the request path.
- API tier and realtime gateway are stateless (state in Postgres/Redis) and scale horizontally behind a load balancer when needed; Redis pub/sub bridges gateway nodes.

## 6. Tech stack

| Layer | Choice | Rationale |
|-------|--------|-----------|
| Frontend | React + TypeScript, single app, role-based routing | One codebase for three roles; shared component library |
| Backend | Node.js (NestJS) modular monolith | First-class WebSocket support, strong typing, easy module extraction |
| Database | PostgreSQL 16 | Transactions for imports/attempts, JSONB for question bodies, read replica for reporting when needed |
| Cache / bus | Redis 7 | Sessions, snapshot cache, pub/sub between API and gateway |
| Queue | BullMQ on Redis | Imports, grading, scheduled open/close, rollups; no extra infrastructure |
| Object storage | S3-compatible (Cloudflare R2) | Bulk-upload files, question images, exports; zero egress fees |
| Observability | OpenTelemetry + Grafana stack | The live-quiz minute is when traces and dashboards matter most |

NestJS is chosen over FastAPI so the app and worker share one TypeScript codebase and one container image (see 09-deployment.md).

## 7. Scaling path (in order, each triggered by observed load)

1. Day one: everything on one VM (see 09-deployment.md).
2. Split the realtime gateway into its own container/VM when sustained concurrency exceeds ~3-4k sockets or autosave p95 trends up during windows.
3. Add a Postgres read replica for analytics and exports when reporting load contends with the transactional path.
4. Multiple stateless app nodes behind a load balancer, Redis pub/sub bridging gateways.
5. Managed Postgres when ops time exceeds hosting bills.

Sharding is deliberately absent; nothing in the capacity model (01-requirements.md section 5) approaches the need.
