#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
name="pg-s3-acl-drill-$$-${RANDOM}"
port=$(python3 - <<'PY'
import socket
s=socket.socket(); s.bind(('127.0.0.1', 0)); print(s.getsockname()[1]); s.close()
PY
)
work=$(mktemp -d)
cleanup() {
  docker rm -f "$name" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

docker run -d --name "$name" -e POSTGRES_HOST_AUTH_METHOD=trust -p "127.0.0.1:${port}:5432" postgres:17-alpine >/dev/null
for _ in $(seq 1 60); do
  docker exec "$name" pg_isready -U postgres >/dev/null 2>&1 && break
  sleep 1
done
docker exec "$name" pg_isready -U postgres >/dev/null
psql() { docker exec -i "$name" psql -X -v ON_ERROR_STOP=1 -U postgres "$@"; }

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
ALTER DEFAULT PRIVILEGES FOR ROLE grant_owner IN SCHEMA app GRANT SELECT ON TABLES TO grant_reader;
ALTER DEFAULT PRIVILEGES FOR ROLE grant_owner IN SCHEMA app GRANT USAGE, SELECT ON SEQUENCES TO grant_reader;
SET ROLE grant_owner;
CREATE TABLE app.future (id serial PRIMARY KEY, value text);
RESET ROLE;
SQL
# A legacy archive made with --no-acl cannot restore grants omitted at dump time.
docker exec "$name" pg_dump -U postgres -d acldb -Fc --no-acl -f /tmp/legacy.dump
docker exec "$name" pg_dump -U postgres -d acldb -Fc --no-owner -f /tmp/fixed.dump
docker exec "$name" createdb -U postgres legacydb
docker exec "$name" pg_restore -U postgres -d legacydb --no-owner --exit-on-error /tmp/legacy.dump
if psql -d legacydb -tAc "SELECT has_table_privilege('grant_reader','app.items','SELECT')" | grep -q t; then
  echo 'legacy dump unexpectedly preserved grants' >&2; exit 1
fi
# Roles are cluster-wide, so remove the source ACL dependencies before testing
# restoration into a cluster where the grantee role is absent.
psql -d acldb <<'SQL'
REVOKE ALL ON SCHEMA app FROM grant_reader;
REVOKE ALL ON ALL TABLES IN SCHEMA app FROM grant_reader;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA app FROM grant_reader;
ALTER DEFAULT PRIVILEGES FOR ROLE grant_owner IN SCHEMA app REVOKE SELECT ON TABLES FROM grant_reader;
ALTER DEFAULT PRIVILEGES FOR ROLE grant_owner IN SCHEMA app REVOKE USAGE, SELECT ON SEQUENCES FROM grant_reader;
SQL
docker exec "$name" psql -U postgres -c 'DROP ROLE grant_reader' >/dev/null

docker exec "$name" createdb -U postgres fixeddb
if docker exec "$name" pg_restore -U postgres -d fixeddb --no-owner --exit-on-error /tmp/fixed.dump >"$work/missing-role.log" 2>&1; then
  echo 'restore without grant roles unexpectedly succeeded' >&2; exit 1
fi
grep -Eq 'grant_reader|role.*does not exist' "$work/missing-role.log"
docker exec "$name" dropdb -U postgres fixeddb
docker exec "$name" createdb -U postgres fixeddb
docker exec "$name" psql -U postgres -c 'CREATE ROLE grant_reader' >/dev/null
docker exec "$name" pg_restore -U postgres -d fixeddb --no-owner --exit-on-error /tmp/fixed.dump
psql -d fixeddb -tAc "SELECT has_table_privilege('grant_reader','app.items','SELECT') AND has_sequence_privilege('grant_reader','app.items_id_seq','USAGE')" | grep -qx t
psql -d fixeddb -tAc "SELECT has_table_privilege('grant_reader','app.future','SELECT')" | grep -qx t
psql -d fixeddb -c "SET ROLE grant_reader; INSERT INTO app.items(value) VALUES ('ok')" >/dev/null
# Explicit ACL-free recovery, followed by deliberate grant reapplication.
docker exec "$name" createdb -U postgres recoverdb
docker exec "$name" pg_restore -U postgres -d recoverdb --no-owner --no-acl --exit-on-error /tmp/fixed.dump
psql -d recoverdb <<'SQL'
GRANT USAGE ON SCHEMA app TO grant_reader;
GRANT SELECT, INSERT ON app.items TO grant_reader;
GRANT USAGE, SELECT ON SEQUENCE app.items_id_seq TO grant_reader;
SQL
psql -d recoverdb -tAc "SELECT has_table_privilege('grant_reader','app.items','SELECT')" | grep -qx t
echo 'Docker ACL restore drill passed'
