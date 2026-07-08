#!/usr/bin/env bash
# Exam-day backup belt (docs/10-operations.md section 1): run on a much
# tighter cron than the nightly job and poll Postgres for a same-day trigger
# row written by quiz.Service.Publish (server/internal/quiz/scheduler.go)
# when a quiz's starts_at lands on today (UTC). When one is pending, run the
# same dump/upload/prune as the nightly job so the exam gets a fresh backup
# minutes before the window instead of up to 24h stale, then mark the
# trigger fulfilled so later polls the same day are no-ops.
set -euo pipefail

: "${MACQUIZ_DATABASE_URL:?MACQUIZ_DATABASE_URL is required}"

pending=$(psql "$MACQUIZ_DATABASE_URL" -Atqc \
  "SELECT trigger_date FROM backup_triggers WHERE trigger_date = (now() AT TIME ZONE 'UTC')::date AND fulfilled_at IS NULL")

if [ -z "$pending" ]; then
  exit 0
fi

echo "[backup] exam-day trigger found for ${pending} - running pre-window dump"
/backup.sh
psql "$MACQUIZ_DATABASE_URL" -c \
  "UPDATE backup_triggers SET fulfilled_at = now() WHERE trigger_date = '${pending}'"
echo "[backup] exam-day trigger for ${pending} fulfilled"
