#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)

# Protect the CLI command lines and every Go pg_dump/pg_restore command.
if grep -n -- '--no-acl' "$root/pg-s3-backup" "$root/app/jobs.go"; then
  echo 'normal shell or app commands must not discard ACLs' >&2
  exit 1
fi
grep -q -- '-Fc --no-owner -f' "$root/pg-s3-backup"
grep -q -- '--no-owner --exit-on-error --clean --if-exists' "$root/pg-s3-backup"
grep -q -- '--no-owner --exit-on-error' "$root/pg-s3-backup"
grep -q -- '"-Fc", "--no-owner", "-f"' "$root/app/jobs.go"
count=$(grep -c -- '"--no-owner", "--exit-on-error"' "$root/app/jobs.go")
[ "$count" -ge 2 ]
grep -q -- '"--no-owner", "--exit-on-error", "-f", "-", tmp' "$root/app/jobs.go"
echo 'ACL command regression checks passed'
