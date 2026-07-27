#!/usr/bin/env bash
#
# Container entrypoint.
#
#   docker run IMAGE                 -> start supercronic on BACKUP_CRON
#   docker run IMAGE pg-s3-backup list  -> run an explicit command and exit
#   docker exec  C  pg-s3-backup list   -> (bypasses this entrypoint entirely)
#
set -Eeuo pipefail

# Explicit-command mode: anything passed as args is exec'd directly.
if [ "$#" -gt 0 ]; then
  exec "$@"
fi

: "${BACKUP_CRON:=0 2 * * *}"

# Fail fast with a clear message if required config is missing.
pg-s3-backup check-env

cron_file="/tmp/pg-s3-backup.cron"
printf '%s /usr/local/bin/pg-s3-backup backup\n' "${BACKUP_CRON}" >"${cron_file}"

echo "[entrypoint] pg-s3-backup scheduled: '${BACKUP_CRON}'"
echo "[entrypoint] target: ${PGUSER}@${PGHOST}:${PGPORT:-5432}/${PGDATABASE} -> s3:${S3_BUCKET}/${S3_PREFIX:-db}"

if [ "${BACKUP_ON_START:-false}" = "true" ]; then
  pg-s3-backup backup || echo "[entrypoint] initial backup failed (continuing to schedule)"
fi

# -no-reap: supercronic's bundled PID-1 reaper mis-execs in this base image, and
# jobs are run synchronously (waited on) so there are no zombies to reap. Use
# swarm `init: true` / `docker run --init` if you ever need a real init reaper.
exec supercronic -no-reap -passthrough-logs "${cron_file}"
