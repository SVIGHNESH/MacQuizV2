# MacQuiz v2 - Operations: Backups, Monitoring, Runbook

Source: DEP-001 v1.0 section 8; SDD-001 v2.0 section 15.
Status: implementation baseline.

## 1. Backups (the non-negotiable part of self-hosted Postgres)

- Nightly `pg_dump` (custom format, compressed) from a cron container, uploaded to a versioned R2 bucket.
  A full academic year of attempt data fits in single-digit GB, well under the free 10 GB.
  Implemented as `scripts/backup` (Dockerfile + `backup.sh` + crontab), run as the `backup` service in `docker-compose.prod.yml`; configured via the `BACKUP_R2_*`/`AWS_*` vars in `.env.production.example`.
- Retention: 7 daily + 8 weekly dumps, pruned by the same job.
  R2 object versioning protects against a bad prune (enabled once on the bucket at provisioning time, not by the script).
- Restore drill once per term: pull the latest dump into a scratch container and run the smoke tests against it.
  An untested backup is a hope, not a backup.
- Exam-day belt: a pre-quiz-window dump is triggered automatically by the scheduler when any quiz enters `scheduled` for the same day.
  Implemented via a `backup_triggers` table: `quiz.Service.Publish` upserts a same-UTC-day row when a quiz's `starts_at` is today, and the `backup` container's tighter `*/5 * * * *` cron (`check-trigger.sh`) polls it, runs the same dump/upload/prune as the nightly job, and marks the row fulfilled so later polls that day are no-ops.
- Current RPO: 24 h (nightly) improving to near-zero on exam days via the pre-window dump.
  When 24 h stops being acceptable, add WAL archiving to R2 with pgBackRest for point-in-time recovery (effort, not money).

## 2. Monitoring on $0

- UptimeRobot pings `/healthz` every 5 min from outside, alerting by email/Telegram.
  `/healthz` checks DB connectivity, Redis connectivity, and queue depth.
  External probing catches the "VM died" failure class that self-hosted monitoring cannot see.
- Grafana Cloud free tier receives OpenTelemetry metrics from the app.
  Key series: autosave latency (`macquiz.attempt.autosave.duration`), WebSocket connection count (`macquiz.realtime.ws_connections`), queue lag (`macquiz.queue.lag_seconds`, the same query `/healthz` uses), and violation/kick event rates (`macquiz.attempt.violations`, `macquiz.attempt.kicks`).
  Implemented as `server/internal/telemetry` (OTLP/HTTP export via `go.opentelemetry.io/otel`), wired into both `serve()` (service name `macquiz-api`) and `worker()` (service name `macquiz-worker`).
  The worker exports the same `macquiz.queue.lag_seconds` gauge (it queries Postgres directly, independent of whether the API process is up) plus `macquiz.worker.due_transitions` (a counter labeled by `kind` - `quizzes_opened`, `quizzes_closed`, `attempts_auto_submitted`, `attempts_force_submitted`, `attempts_graded`, `results_released`, `quizzes_rolled_up` - one per sweep pass), matching the fields already in its "due transitions applied" log line.
  Configured via `MACQUIZ_OTEL_EXPORTER_ENDPOINT`/`MACQUIZ_OTEL_EXPORTER_HEADERS`; blank (the default) disables telemetry, so every instrument is a no-op until a Grafana Cloud OTLP endpoint is set.
  14-day retention on the free tier is accepted.
  OpenTelemetry log export is not implemented; the app logs structured JSON to stdout only (Compose/journald captured), not shipped to Grafana Cloud.
- Watchtower is deliberately absent: images update only via the deploy pipeline, never automatically under a live quiz.

## 3. Alert thresholds (initial)

| Signal | Warn | Page |
|--------|------|------|
| /healthz failures | 1 miss | 2 consecutive misses |
| Autosave p95 | > 200 ms sustained 5 min | > 300 ms sustained 5 min during a live window |
| Queue lag (delayed jobs overdue) | > 10 s | > 60 s (deadline timers at risk) |
| Disk usage on pg volume | > 70% | > 85% |
| Backup job | - | Nightly dump missing or failed |

## 4. Deploy policy

- Migrations run in the app entrypoint before traffic is accepted.
- Deploys are refused while any quiz is `live` (pre-deploy check).
  This is the cheapest possible prevention of the worst incident this platform could have.
- Rollback: redeploy the previous image tag; migrations must therefore be backward-compatible one version (expand-then-contract).

## 5. Incident quick-reference

| Symptom | Likely cause | First move |
|---------|-------------|-----------|
| Students cannot start at go-live | Scheduler job missed (worker down or job stuck in River) | Lazy validation should already treat the quiz as live on read; verify worker container, then re-run the boot re-scan |
| Autosaves slow or failing | Postgres pressure or disk | Check pg volume, active connections, autosave p95 dashboard |
| Teacher dashboard frozen but students fine | Gateway or pub/sub issue | Dashboard falls back to 10 s polling automatically; restart app container after the window if needed |
| Kick not reflected on student screen | Socket lost | By design the next autosave returns 409 ATTEMPT_KICKED; no action needed, verify the attempt row status |
| VM unreachable | Host outage | Restore path: new VM, `docker compose up`, restore latest R2 dump; DNS via Cloudflare |

## 6. Boot recovery invariants

- Worker re-scans Postgres at boot for due-but-unfired quiz transitions and overdue attempt deadlines, and fires them immediately.
- Delayed jobs live in Postgres (River), so they survive restarts and outright Redis loss; the boot re-scan remains as a second belt.
- The app refuses to start if migrations fail; Compose `restart: unless-stopped` handles crash loops visibly rather than silently.
