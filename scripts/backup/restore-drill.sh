#!/usr/bin/env bash
# Restore drill (docs/10-operations.md section 1): pull the latest backup
# dump into a scratch Postgres container, restore it, then prove the
# restored database is actually usable - an untested backup is a hope, not
# a backup. Meant to be run by hand once per term (or on demand before/
# after a provisioning change); it never runs on a schedule like backup.sh.
#
# By default fetches the latest daily dump from the same R2 bucket
# backup.sh writes to. Pass --dump-file <path> to drill against a dump
# already on disk instead (e.g. one fetched by hand, or for testing this
# script itself without R2 credentials).
set -euo pipefail

dump_file=""
dump_key=""

while [ $# -gt 0 ]; do
  case "$1" in
    --dump-file)
      dump_file="$2"
      shift 2
      ;;
    --dump-key)
      dump_key="$2"
      shift 2
      ;;
    *)
      echo "usage: $0 [--dump-file <path> | --dump-key daily/macquiz-YYYY-MM-DD.dump]" >&2
      exit 1
      ;;
  esac
done

: "${MACQUIZ_IMAGE:?MACQUIZ_IMAGE is required (the app image to run 'migrate' against the restored dump)}"
: "${TAG:=latest}"

container="macquiz-restore-drill"
network="macquiz-restore-drill-net"
downloaded=""

cleanup() {
  docker rm -f "$container" >/dev/null 2>&1 || true
  docker network rm "$network" >/dev/null 2>&1 || true
  if [ -n "$downloaded" ]; then
    rm -f "$downloaded"
  fi
}
trap cleanup EXIT

# Idempotent: clear out anything left behind by a prior run that crashed
# before its own cleanup ran.
cleanup

if [ -z "$dump_file" ]; then
  : "${BACKUP_R2_BUCKET:?BACKUP_R2_BUCKET is required when --dump-file is not given}"
  : "${BACKUP_R2_ENDPOINT:?BACKUP_R2_ENDPOINT is required when --dump-file is not given}"
  : "${AWS_ACCESS_KEY_ID:?AWS_ACCESS_KEY_ID is required when --dump-file is not given}"
  : "${AWS_SECRET_ACCESS_KEY:?AWS_SECRET_ACCESS_KEY is required when --dump-file is not given}"

  s3() { aws --endpoint-url "$BACKUP_R2_ENDPOINT" s3 "$@"; }
  s3api() { aws --endpoint-url "$BACKUP_R2_ENDPOINT" s3api "$@"; }

  if [ -z "$dump_key" ]; then
    echo "[restore-drill] locating latest daily dump in s3://${BACKUP_R2_BUCKET}/daily/"
    dump_key=$(s3api list-objects-v2 --bucket "$BACKUP_R2_BUCKET" --prefix "daily/" \
      --query 'sort_by(Contents, &Key)[-1].Key' --output text 2>/dev/null || true)
    if [ -z "$dump_key" ] || [ "$dump_key" = "None" ]; then
      echo "[restore-drill] no daily dump found in s3://${BACKUP_R2_BUCKET}/daily/" >&2
      exit 1
    fi
  fi

  downloaded="/tmp/macquiz-restore-drill-$$.dump"
  echo "[restore-drill] downloading s3://${BACKUP_R2_BUCKET}/${dump_key}"
  s3 cp "s3://${BACKUP_R2_BUCKET}/${dump_key}" "$downloaded"
  dump_file="$downloaded"
fi

if [ ! -f "$dump_file" ]; then
  echo "[restore-drill] dump file not found: ${dump_file}" >&2
  exit 1
fi

drill_password="restore-drill-password"

echo "[restore-drill] starting scratch Postgres container"
docker network create "$network" >/dev/null
docker run -d --name "$container" --network "$network" \
  -e POSTGRES_USER=macquiz -e POSTGRES_PASSWORD="$drill_password" -e POSTGRES_DB=macquiz \
  postgres:16-alpine >/dev/null

echo -n "[restore-drill] waiting for scratch Postgres to accept connections"
# The official postgres image starts twice on a fresh volume: once on a
# unix socket to run init scripts, then a restart onto the real listener.
# pg_isready can report ready during that first, transient instance, so
# wait for its log line to appear a second time rather than trusting the
# first successful pg_isready.
ready=""
for _ in $(seq 1 60); do
  starts=$(docker logs "$container" 2>&1 | grep -c "database system is ready to accept connections" || true)
  if [ "$starts" -ge 2 ] && docker exec "$container" pg_isready -U macquiz -d macquiz >/dev/null 2>&1; then
    ready=1
    break
  fi
  echo -n "."
  sleep 1
done
echo
if [ -z "$ready" ]; then
  echo "[restore-drill] scratch Postgres never became ready" >&2
  exit 1
fi

echo "[restore-drill] restoring ${dump_file} into the scratch database"
docker cp "$dump_file" "${container}:/tmp/restore.dump"
docker exec -e PGPASSWORD="$drill_password" "$container" \
  pg_restore -U macquiz -d macquiz --no-owner -j 4 /tmp/restore.dump

scratch_url="postgres://macquiz:${drill_password}@${container}:5432/macquiz?sslmode=disable"

echo "[restore-drill] running 'migrate' against the restored database (schema-compatibility check)"
docker run --rm --network "$network" -e MACQUIZ_DATABASE_URL="$scratch_url" \
  "${MACQUIZ_IMAGE}:${TAG}" migrate

echo "[restore-drill] running data smoke checks against the restored database"
smoke_query="
select 'users' as table_name, count(*) as row_count from users
union all select 'quizzes', count(*) from quizzes
union all select 'quiz_versions', count(*) from quiz_versions
union all select 'attempts', count(*) from attempts
union all select 'attempt_events', count(*) from attempt_events
union all select 'audit_log', count(*) from audit_log
order by table_name;
"
docker exec -e PGPASSWORD="$drill_password" "$container" \
  psql -U macquiz -d macquiz -v ON_ERROR_STOP=1 -c "$smoke_query"

echo "[restore-drill] PASS - dump restored, migrations applied cleanly, core tables queryable"
