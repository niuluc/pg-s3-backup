#!/usr/bin/env bash
# End-to-end test against real Postgres 16/17/18 and an S3 stand-in.
# Drives the app only through its HTTP form endpoints, like a browser.
#
#   app/test/e2e.sh            run, then tear the stack down
#   KEEP=1 app/test/e2e.sh     leave the stack running afterwards
set -Eeuo pipefail
cd "$(dirname "$0")/.."

export COMPOSE_PROJECT_NAME=pgbackup-e2e PORT="${PORT:-8092}"
B="http://127.0.0.1:${PORT}"
pass=0
fail() { echo "FAIL: $*" >&2; docker compose logs app | tail -30 >&2; exit 1; }
ok() { echo "ok   $*"; pass=$((pass + 1)); }
cleanup() { [ "${KEEP:-}" = 1 ] || docker compose down -v --remove-orphans >/dev/null 2>&1 || true; }
trap cleanup EXIT

docker compose down -v --remove-orphans >/dev/null 2>&1 || true
docker compose up -d --build --quiet-pull >/dev/null 2>&1 || { docker compose up -d --build; exit 1; }
for _ in $(seq 60); do curl -fs "$B/healthz" >/dev/null && break; sleep 1; done
curl -fs "$B/healthz" >/dev/null || fail "app did not come up"
# Postgres needs the seed to finish before the first connection test.
for svc in pg16:shop pg17:blog pg18:stats; do
  for _ in $(seq 60); do
    docker compose exec -T "${svc%%:*}" psql -U "${svc##*:}" -d "${svc##*:}" -Atc "select count(*) from orders" 2>/dev/null | grep -q 40000 && break
    sleep 1
  done
done

state() { docker compose exec -T app cat /data/state.json; }
# post PATH [curl args...] -> prints the redirect URL
post() { local p=$1; shift; curl -s -o /dev/null -w '%{redirect_url}' -X POST "$B$p" "$@"; }
run_id() { sed -n 's|.*/runs/\([0-9a-f]*\).*|\1|p'; }
wait_run() { # wait_run ID -> prints "status|client|message"
  for _ in $(seq 120); do
    out="$(state | python3 -c "
import json,sys
r=[r for r in json.load(sys.stdin)['runs'] if r['id']=='$1'][0]
print(r['status']+'|'+r.get('client','')+'|'+r.get('message',''))")"
    case "$out" in running*) sleep 1 ;; *) echo "$out"; return ;; esac
  done
  fail "run $1 did not finish"
}
add() { # add NAME HOST DB PASSWORD -> prints target id
  post /targets --data-urlencode "name=$1" --data-urlencode "host=$2" -d port=5432 \
    --data-urlencode "database=$3" --data-urlencode "user=$3" --data-urlencode "password=$4" \
    --data-urlencode "schedule=0 3 * * *" -d retention=14 -d enabled=on | sed -n 's|.*/targets/\([0-9a-f]*\).*|\1|p'
}
psql_in() { docker compose exec -T "$1" psql -U "$2" -d "$3" -Atc "$4"; }

# 1. Add targets; version is detected on save.
declare -A ID
ID[shop]=$(add shop pg16 shop shop-pw)
ID[blog]=$(add blog pg17 blog blog-pw)
ID[stats]=$(add stats pg18 stats stats-pw)
ID[gone]=$(add gone no-such-host gone x)
for n in shop blog stats gone; do [ -n "${ID[$n]}" ] || fail "add $n returned no id"; done
vers="$(state | python3 -c "import json,sys; print(' '.join(t['name']+'='+t['serverVersion'].split('.')[0] for t in sorted(json.load(sys.stdin)['targets'], key=lambda t:t['name']) if t['serverVersion']))")"
[ "$vers" = "blog=17 shop=16 stats=18" ] || fail "detected versions: $vers"
ok "add 4 targets, detected $vers"

# 2. Backup each with the matching client; the broken one fails.
for n in shop:16 blog:17 stats:18; do
  name=${n%%:*} major=${n##*:}
  res="$(wait_run "$(post "/targets/${ID[$name]}/backup" | run_id)")"
  [ "${res%%|*}" = ok ] || fail "backup $name: $res"
  [ "$(cut -d'|' -f2 <<<"$res")" = "postgresql$major" ] || fail "backup $name used $res"
done
ok "backup shop/blog/stats with postgresql16/17/18"
res="$(wait_run "$(post "/targets/${ID[gone]}/backup" | run_id)")"
[ "${res%%|*}" = failed ] || fail "backup of unreachable host: $res"
for _ in 1 2 3 4 5; do curl -fs "$B/" | grep -q '1 failing' && break; sleep 1; done
curl -fs "$B/" | grep -q '1 failing' || fail "dashboard does not show the failure"
ok "unreachable target fails and shows on the dashboard"

# 3. Verify each.
for n in shop blog stats; do
  res="$(wait_run "$(post "/targets/${ID[$n]}/verify" | run_id)")"
  [ "${res%%|*}" = ok ] || fail "verify $n: $res"
done
[ -z "$(psql_in pg16 shop shop "select datname from pg_database where datname like 'shop_verify_%'")" ] || fail "verify left a scratch db"
ok "verify shop/blog/stats, scratch databases dropped"

# 4. Restore as copy: live untouched, copy has the data.
file="$(state | python3 -c "import json,sys; print([r['file'] for r in json.load(sys.stdin)['runs'] if r['kind']=='backup' and r['status']=='ok' and r['file'].startswith('shop-')][0])")"
psql_in pg16 shop shop "delete from orders where id > 30000" >/dev/null
res="$(wait_run "$(post "/targets/${ID[shop]}/restore" -d mode=copy --data-urlencode "file=$file" | run_id)")"
[ "${res%%|*}" = ok ] || fail "restore copy: $res"
copy="${res##*restored into }"
[ "$(psql_in pg16 shop "$copy" "select count(*) from orders")" = 40000 ] || fail "copy $copy does not have 40000 orders"
[ "$(psql_in pg16 shop shop "select count(*) from orders")" = 30000 ] || fail "restore copy touched live"
ok "restore as copy into $copy, live untouched"

# 5. Replace live: needs the typed name; takes a safety backup first.
redir="$(post "/targets/${ID[shop]}/restore" -d mode=replace --data-urlencode "file=$file" -d confirm=wrong)"
grep -q 'err=' <<<"$redir" || fail "replace without confirm was not refused: $redir"
before="$(state | python3 -c "import json,sys; print(sum(1 for r in json.load(sys.stdin)['runs'] if r['kind']=='restore-replace'))")"
[ "$before" = 0 ] || fail "refused replace still started a run"
ok "replace refused without typed confirmation"
psql_in pg16 shop shop "create table added_after_backup(x int)" >/dev/null
res="$(wait_run "$(post "/targets/${ID[shop]}/restore" -d mode=replace --data-urlencode "file=$file" -d confirm=shop | run_id)")"
[ "${res%%|*}" = ok ] || fail "restore replace: $res"
[ "$(psql_in pg16 shop shop "select count(*) from orders")" = 40000 ] || fail "live not restored"
[ "$(psql_in pg16 shop shop "select to_regclass('public.added_after_backup') is null")" = t ] || fail "table created after the backup survived replace"
safety="${res##*undo with }"
docker compose exec -T s3 test -f "/data/backups/shop/$safety" || fail "safety backup $safety not in S3"
ok "replace live: 30000 -> 40000 orders, newer table dropped, safety backup $safety stored"

# 6. Undo with the safety backup.
res="$(wait_run "$(post "/targets/${ID[shop]}/restore" -d mode=replace --data-urlencode "file=$safety" -d confirm=shop | run_id)")"
[ "${res%%|*}" = ok ] || fail "undo: $res"
[ "$(psql_in pg16 shop shop "select count(*) from orders")" = 30000 ] || fail "undo did not bring back 30000 orders"
ok "undo with the safety backup: back to 30000 orders"

# 6b. Replace on Postgres 18 too (newest client and server).
sfile="$(state | python3 -c "import json,sys; print([r['file'] for r in json.load(sys.stdin)['runs'] if r['kind']=='backup' and r['status']=='ok' and r['file'].startswith('stats-')][0])")"
psql_in pg18 stats stats "truncate orders" >/dev/null
res="$(wait_run "$(post "/targets/${ID[stats]}/restore" -d mode=replace --data-urlencode "file=$sfile" -d confirm=stats | run_id)")"
[ "${res%%|*}" = ok ] || fail "pg18 replace: $res"
[ "$(psql_in pg18 stats stats "select count(*) from orders")" = 40000 ] || fail "pg18 live not restored"
ok "replace live on Postgres 18"

# 6c. A second target writing the same folder and database is refused.
redir="$(post /targets -d name=dupe -d host=pg16 -d port=5432 -d database=shop -d user=shop -d password=shop-pw --data-urlencode "schedule=0 3 * * *" -d retention=14 -d prefix=shop)"
grep -q 'err=' <<<"$redir" || fail "duplicate folder accepted: $redir"
ok "duplicate S3 folder + database refused"

# 7. Restore of a file that is not this target's backup is refused.
res="$(wait_run "$(post "/targets/${ID[shop]}/restore" -d mode=copy --data-urlencode "file=../blog/x.dump" | run_id)")"
[ "${res%%|*}" = failed ] || fail "foreign file restore: $res"
ok "restore of a foreign file refused"

# 8. Pages render.
for p in / "/targets/${ID[shop]}" "/targets/${ID[shop]}/edit" /targets/new "/targets/${ID[shop]}/restore?file=$file"; do
  code="$(curl -s -o /dev/null -w '%{http_code}' "$B$p")"
  [ "$code" = 200 ] || fail "GET $p -> $code"
done
ok "all pages return 200"

echo "PASS: $pass checks"
