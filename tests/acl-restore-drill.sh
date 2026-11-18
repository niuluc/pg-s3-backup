#!/usr/bin/env bash
set -euo pipefail
name="pg-s3-acl-drill-$$-${RANDOM}"
container_id=
work=$(mktemp -d)
cleanup() {
  if [ -n "$container_id" ]; then
    docker rm -fv "$container_id" >/dev/null 2>&1 || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT

container_id=$(docker run -d --name "$name" -e POSTGRES_HOST_AUTH_METHOD=trust \
  -p '127.0.0.1::5432' postgres:17-alpine)
port=$(docker port "$container_id" 5432/tcp)
case "$port" in 127.0.0.1:*) ;; *) echo "Docker did not assign a loopback port: $port" >&2; exit 1 ;; esac
printf 'Postgres container %s listening on %s\n' "$container_id" "$port"
readiness_deadline=$((SECONDS + 60))
ready=false
while (( SECONDS < readiness_deadline )); do
  if timeout 2s docker exec "$container_id" env PGCONNECT_TIMEOUT=1 \
    psql -h 127.0.0.1 -U postgres -d postgres -X -v ON_ERROR_STOP=1 -c 'select 1' \
    >/dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 1
done
if [[ "$ready" != true ]]; then
  echo 'Timed out waiting for PostgreSQL TCP query readiness after 60 seconds.' >&2
  echo 'PostgreSQL container state:' >&2
  docker inspect --format '{{.State.Status}} (running={{.State.Running}})' "$container_id" >&2 || true
  echo 'Recent PostgreSQL container logs:' >&2
  docker logs --tail 20 "$container_id" >&2 || true
  exit 1
fi
psql() { docker exec -i "$container_id" psql -X -v ON_ERROR_STOP=1 -U postgres "$@"; }

psql <<'SQL'
CREATE ROLE grant_owner;
CREATE ROLE grant_reader;
CREATE DATABASE acldb OWNER grant_owner;
SQL
psql -d acldb <<'SQL'
CREATE SCHEMA app AUTHORIZATION grant_owner;
CREATE TABLE app.items (id serial PRIMARY KEY, value text);
ALTER TABLE app.items OWNER TO grant_owner;
GRANT USAGE ON SCHEMA app TO grant_reader;
GRANT SELECT, INSERT ON app.items TO grant_reader;
GRANT USAGE, SELECT ON SEQUENCE app.items_id_seq TO grant_reader;
ALTER DEFAULT PRIVILEGES FOR ROLE grant_owner IN SCHEMA app GRANT SELECT, INSERT ON TABLES TO grant_reader;
ALTER DEFAULT PRIVILEGES FOR ROLE grant_owner IN SCHEMA app GRANT USAGE, SELECT ON SEQUENCES TO grant_reader;
SQL
# Keep --no-owner on the legacy fixture, as the original dump command did.
docker exec "$container_id" pg_dump -U postgres -d acldb -Fc --no-owner --no-acl -f /tmp/legacy.dump
docker exec "$container_id" pg_dump -U postgres -d acldb -Fc --no-owner -f /tmp/fixed.dump
docker exec "$container_id" createdb -U postgres legacydb
docker exec "$container_id" pg_restore -U postgres -d legacydb --no-owner --exit-on-error /tmp/legacy.dump
if psql -d legacydb -tAc "SELECT has_table_privilege('grant_reader','app.items','SELECT')" | grep -q t; then
  echo 'legacy dump unexpectedly preserved grants' >&2; exit 1
fi
# Remove archived grant dependencies to test restoration when the grantee is absent.
psql -d acldb <<'SQL'
REVOKE ALL ON SCHEMA app FROM grant_reader;
REVOKE ALL ON ALL TABLES IN SCHEMA app FROM grant_reader;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA app FROM grant_reader;
ALTER DEFAULT PRIVILEGES FOR ROLE grant_owner IN SCHEMA app REVOKE SELECT, INSERT ON TABLES FROM grant_reader;
ALTER DEFAULT PRIVILEGES FOR ROLE grant_owner IN SCHEMA app REVOKE USAGE, SELECT ON SEQUENCES FROM grant_reader;
SQL
docker exec "$container_id" psql -U postgres -c 'DROP ROLE grant_reader' >/dev/null

docker exec "$container_id" createdb -U postgres fixeddb
if docker exec "$container_id" pg_restore -U postgres -d fixeddb --no-owner --exit-on-error /tmp/fixed.dump >"$work/missing-role.log" 2>&1; then
  echo 'restore without grant roles unexpectedly succeeded' >&2; exit 1
fi
grep -Eq 'grant_reader|role.*does not exist' "$work/missing-role.log"
docker exec "$container_id" dropdb -U postgres fixeddb
docker exec "$container_id" createdb -U postgres fixeddb
docker exec "$container_id" psql -U postgres -c 'CREATE ROLE grant_reader' >/dev/null
docker exec "$container_id" pg_restore -U postgres -d fixeddb --no-owner --exit-on-error /tmp/fixed.dump
psql -d fixeddb -tAc "SELECT has_table_privilege('grant_reader','app.items','SELECT') AND has_table_privilege('grant_reader','app.items','INSERT') AND has_sequence_privilege('grant_reader','app.items_id_seq','USAGE')" | grep -qx t
psql -d fixeddb -c "SET ROLE grant_reader; INSERT INTO app.items(value) VALUES ('fixed-items') RETURNING id; SELECT value FROM app.items" | grep -q fixed-items
psql -d fixeddb <<'SQL'
ALTER SCHEMA app OWNER TO grant_owner;
SET ROLE grant_owner;
CREATE TABLE app.future_after_restore (id serial PRIMARY KEY, value text);
RESET ROLE;
SQL
psql -d fixeddb -tAc "SELECT has_table_privilege('grant_reader','app.future_after_restore','SELECT') AND has_table_privilege('grant_reader','app.future_after_restore','INSERT') AND has_sequence_privilege('grant_reader','app.future_after_restore_id_seq','USAGE')" | grep -qx t
psql -d fixeddb -c "SET ROLE grant_reader; INSERT INTO app.future_after_restore(value) VALUES ('fixed') RETURNING id; SELECT value FROM app.future_after_restore" | grep -q fixed
# Explicit ACL-free recovery, then reapply object and future-object grants.
docker exec "$container_id" createdb -U postgres recoverdb
docker exec "$container_id" pg_restore -U postgres -d recoverdb --no-owner --no-acl --exit-on-error /tmp/fixed.dump
psql -d recoverdb <<'SQL'
GRANT USAGE ON SCHEMA app TO grant_reader;
GRANT SELECT, INSERT ON app.items TO grant_reader;
GRANT USAGE, SELECT ON SEQUENCE app.items_id_seq TO grant_reader;
ALTER DEFAULT PRIVILEGES FOR ROLE grant_owner IN SCHEMA app GRANT SELECT, INSERT ON TABLES TO grant_reader;
ALTER DEFAULT PRIVILEGES FOR ROLE grant_owner IN SCHEMA app GRANT USAGE, SELECT ON SEQUENCES TO grant_reader;
ALTER SCHEMA app OWNER TO grant_owner;
SQL
psql -d recoverdb -tAc "SELECT has_table_privilege('grant_reader','app.items','SELECT') AND has_table_privilege('grant_reader','app.items','INSERT') AND has_sequence_privilege('grant_reader','app.items_id_seq','USAGE')" | grep -qx t
psql -d recoverdb -c "SET ROLE grant_reader; INSERT INTO app.items(value) VALUES ('recovery-items') RETURNING id; SELECT value FROM app.items" | grep -q recovery-items
psql -d recoverdb <<'SQL'
SET ROLE grant_owner;
CREATE TABLE app.future_after_recovery (id serial PRIMARY KEY, value text);
RESET ROLE;
SQL
psql -d recoverdb -tAc "SELECT has_table_privilege('grant_reader','app.future_after_recovery','SELECT') AND has_table_privilege('grant_reader','app.future_after_recovery','INSERT') AND has_sequence_privilege('grant_reader','app.future_after_recovery_id_seq','USAGE')" | grep -qx t
psql -d recoverdb -c "SET ROLE grant_reader; INSERT INTO app.future_after_recovery(value) VALUES ('recovery') RETURNING id; SELECT value FROM app.future_after_recovery" | grep -q recovery

echo 'Docker ACL restore drill passed'
