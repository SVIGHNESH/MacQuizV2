#!/usr/bin/env bash
# Nightly backup job (docs/10-operations.md section 1): pg_dump the
# production database (custom format, already compressed), upload it to an
# S3 bucket, and prune to 7 daily + 8 weekly dumps. Bucket versioning (a
# bucket-level setting, not this script) is the belt against a bad prune.
#
# Run nightly by crond inside the backup container (docker-compose.prod.yml);
# can also be run manually (e.g. before a live exam window) with the same
# env vars.
#
# Optional dead-man's-switch: if BACKUP_HEALTHCHECK_URL is set (e.g. a
# Healthchecks.io check URL), ping it on start/success/failure so a missing
# or failed nightly dump pages someone - docs/10-operations.md section 3's
# "Backup job: nightly dump missing or failed" threshold, which otherwise
# has no app-emitted metric behind it since this is a cron script, not a
# request path server/internal/telemetry instruments.
set -euo pipefail

: "${MACQUIZ_DATABASE_URL:?MACQUIZ_DATABASE_URL is required}"
: "${BACKUP_S3_BUCKET:?BACKUP_S3_BUCKET is required}"
: "${AWS_REGION:?AWS_REGION is required}"

# Credentials are deliberately not required here. On the EC2 deployment the
# instance role supplies them through IMDS, so no long-lived key pair sits
# in .env.production; AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY still work if
# set, which is what running this against a non-AWS bucket needs.
#
# BACKUP_S3_ENDPOINT is likewise optional: unset means the regional S3
# endpoint, set means an S3-compatible service (R2, MinIO).
daily_keep="${BACKUP_DAILY_KEEP:-7}"
weekly_keep="${BACKUP_WEEKLY_KEEP:-8}"

aws_args=(--region "$AWS_REGION")
if [ -n "${BACKUP_S3_ENDPOINT:-}" ]; then
  aws_args+=(--endpoint-url "$BACKUP_S3_ENDPOINT")
fi

s3() { aws "${aws_args[@]}" s3 "$@"; }
s3api() { aws "${aws_args[@]}" s3api "$@"; }

# Best-effort: a monitoring ping must never fail the backup job itself, so
# every call is curl -fsS with a short timeout and a swallowed error.
ping_healthcheck() {
  [ -n "${BACKUP_HEALTHCHECK_URL:-}" ] || return 0
  curl -fsS -m 10 -o /dev/null "${BACKUP_HEALTHCHECK_URL}${1:-}" || true
}

today=$(date -u +%F) # YYYY-MM-DD
dow=$(date -u +%u)   # ISO day of week, 1=Monday .. 7=Sunday
dump_file="/tmp/macquiz-${today}.dump"

cleanup() {
  local status=$?
  rm -f "$dump_file"
  if [ "$status" -eq 0 ]; then
    ping_healthcheck ""
  else
    ping_healthcheck "/fail"
  fi
}
trap cleanup EXIT

ping_healthcheck "/start"

echo "[backup] dumping database to ${dump_file}"
pg_dump -Fc -f "$dump_file" "$MACQUIZ_DATABASE_URL"

daily_key="daily/macquiz-${today}.dump"
echo "[backup] uploading s3://${BACKUP_S3_BUCKET}/${daily_key}"
s3 cp "$dump_file" "s3://${BACKUP_S3_BUCKET}/${daily_key}"

# Sunday's daily dump doubles as that week's weekly dump - avoids a second
# pg_dump run and keeps the daily/weekly copies byte-identical.
if [ "$dow" = "7" ]; then
  weekly_key="weekly/macquiz-${today}.dump"
  echo "[backup] Sunday - also uploading s3://${BACKUP_S3_BUCKET}/${weekly_key}"
  s3 cp "$dump_file" "s3://${BACKUP_S3_BUCKET}/${weekly_key}"
fi

# Prunes a prefix down to the $keep most recent keys (lexicographic sort on
# an ISO-date-suffixed key is also chronological order).
prune() {
  local prefix="$1" keep="$2"
  local keys
  keys=$(s3api list-objects-v2 --bucket "$BACKUP_S3_BUCKET" --prefix "$prefix" \
    --query 'sort_by(Contents, &Key)[].Key' --output text 2>/dev/null || true)
  if [ -z "$keys" ] || [ "$keys" = "None" ]; then
    return 0
  fi
  local count excess i=0
  count=$(wc -w <<<"$keys")
  excess=$((count - keep))
  if [ "$excess" -le 0 ]; then
    return 0
  fi
  for key in $keys; do
    i=$((i + 1))
    if [ "$i" -gt "$excess" ]; then
      break
    fi
    echo "[backup] pruning s3://${BACKUP_S3_BUCKET}/${key}"
    s3 rm "s3://${BACKUP_S3_BUCKET}/${key}"
  done
}

prune "daily/" "$daily_keep"
prune "weekly/" "$weekly_keep"

echo "[backup] done"
