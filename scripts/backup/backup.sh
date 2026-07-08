#!/usr/bin/env bash
# Nightly backup job (docs/10-operations.md section 1): pg_dump the
# production database (custom format, already compressed), upload it to a
# Cloudflare R2 bucket (S3-compatible), and prune to 7 daily + 8 weekly
# dumps. R2 object versioning (a bucket-level setting, not this script) is
# the belt against a bad prune.
#
# Run nightly by crond inside the backup container (docker-compose.prod.yml);
# can also be run manually (e.g. before a live exam window) with the same
# env vars.
set -euo pipefail

: "${MACQUIZ_DATABASE_URL:?MACQUIZ_DATABASE_URL is required}"
: "${BACKUP_R2_BUCKET:?BACKUP_R2_BUCKET is required}"
: "${BACKUP_R2_ENDPOINT:?BACKUP_R2_ENDPOINT is required}"
: "${AWS_ACCESS_KEY_ID:?AWS_ACCESS_KEY_ID is required}"
: "${AWS_SECRET_ACCESS_KEY:?AWS_SECRET_ACCESS_KEY is required}"

daily_keep="${BACKUP_DAILY_KEEP:-7}"
weekly_keep="${BACKUP_WEEKLY_KEEP:-8}"

s3() { aws --endpoint-url "$BACKUP_R2_ENDPOINT" s3 "$@"; }
s3api() { aws --endpoint-url "$BACKUP_R2_ENDPOINT" s3api "$@"; }

today=$(date -u +%F) # YYYY-MM-DD
dow=$(date -u +%u)   # ISO day of week, 1=Monday .. 7=Sunday
dump_file="/tmp/macquiz-${today}.dump"

cleanup() { rm -f "$dump_file"; }
trap cleanup EXIT

echo "[backup] dumping database to ${dump_file}"
pg_dump -Fc -f "$dump_file" "$MACQUIZ_DATABASE_URL"

daily_key="daily/macquiz-${today}.dump"
echo "[backup] uploading s3://${BACKUP_R2_BUCKET}/${daily_key}"
s3 cp "$dump_file" "s3://${BACKUP_R2_BUCKET}/${daily_key}"

# Sunday's daily dump doubles as that week's weekly dump - avoids a second
# pg_dump run and keeps the daily/weekly copies byte-identical.
if [ "$dow" = "7" ]; then
  weekly_key="weekly/macquiz-${today}.dump"
  echo "[backup] Sunday - also uploading s3://${BACKUP_R2_BUCKET}/${weekly_key}"
  s3 cp "$dump_file" "s3://${BACKUP_R2_BUCKET}/${weekly_key}"
fi

# Prunes a prefix down to the $keep most recent keys (lexicographic sort on
# an ISO-date-suffixed key is also chronological order).
prune() {
  local prefix="$1" keep="$2"
  local keys
  keys=$(s3api list-objects-v2 --bucket "$BACKUP_R2_BUCKET" --prefix "$prefix" \
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
    echo "[backup] pruning s3://${BACKUP_R2_BUCKET}/${key}"
    s3 rm "s3://${BACKUP_R2_BUCKET}/${key}"
  done
}

prune "daily/" "$daily_keep"
prune "weekly/" "$weekly_keep"

echo "[backup] done"
