# pg-backup

One service that backs up many Postgres databases to one S3 bucket. You
manage it in a small web page that only your Tailscale login can open.
It replaces one `pg-s3-backup` sidecar per database.

```
browser ──(tailnet only)──► pg-backup ──► Postgres 16 / 17 / 18, by service name
                               │
                               └─rclone─► s3://<bucket>/<folder>/<database>-<time>.dump
```

## What it does

- **Databases.** Each database has a name, a host, port, database, user and
  password, a cron schedule (UTC), retention in days and an S3 folder. You
  add and edit them in the web page.
- **Version.** Before each job the service reads `server_version_num` and
  uses the client of the same major from `/usr/libexec/postgresql<major>/`.
  The image has the 16, 17 and 18 clients. If the same major is missing, it
  uses the nearest newer one.
- **Backup.** `pg_dump -Fc`, then `pg_restore --list` as an integrity
  check, then upload. Pruning deletes only that database's own
  `<database>-*.dump` files older than its retention. Two databases cannot
  share the same S3 folder and database name.
- **Verify.** Restores the newest dump into `<database>_verify_<time>`,
  counts its tables, then drops it.
- **Restore as copy.** Restores a chosen dump into a new database
  `<database>_restore_<time>`. The live database stays as it is. The
  copy stays until you drop it.
- **Replace live.** You must type the database name. The service takes a
  fresh backup first. Then, in one transaction, it drops every user schema
  and replays the dump. Objects created after the dump go too. The
  transaction commits only if `pg_restore` and every statement succeed, so
  a failure changes nothing. The run page names the safety backup to undo
  with.
- **Jobs.** One job per database at a time and at most `MAX_PARALLEL` jobs
  in total. Each run keeps its log. Each database keeps its last 30 runs.
- **Failures.** Each failure is written to the container log as
  `job failed: target=<name> kind=<kind>`. It also goes to Slack when
  `SLACK_BOT_TOKEN` and `SLACK_CHANNEL` are set. Successful jobs do not send
  notifications, including the first success after a failure.

## Where data lives

| What | Where |
|---|---|
| Databases, passwords, run history | `$DATA_DIR/state.json`, mode 0600 |
| Tailscale node identity | `$DATA_DIR/tailscale/` |
| Dumps | S3 only. Local temp files go to `$WORKDIR` and are deleted after each job. |

`DATA_DIR` must be a persistent volume. If you lose it, you must add the
databases again. The dumps in S3 are not affected.

## Login

The service joins the tailnet itself with
[tsnet](https://tailscale.com/kb/1244/tsnet). It has no public port,
no DNS record and no Traefik route. For each request it asks Tailscale
who the sender is (`WhoIs`). That Tailscale login is the user. Logins that
are not in `ALLOWED_USERS` get 403. The service refuses cross-site POSTs.

On first start, set `TS_AUTHKEY`. If you do not, the log prints a
login URL. Open it once. The node state then persists in
`$DATA_DIR/tailscale`.

## Configuration

| Env | Default | Meaning |
|---|---|---|
| `S3_ENDPOINT`, `S3_BUCKET`, `S3_ACCESS_KEY_ID`, `S3_SECRET_ACCESS_KEY` | required | One bucket for every database |
| `S3_REGION` / `S3_PROVIDER` / `S3_FORCE_PATH_STYLE` | `us-east-1` / `Other` / `true` | rclone S3 settings |
| `ALLOWED_USERS` | required | Comma list of Tailscale logins |
| `TS_AUTHKEY` | — | Needed on first start only |
| `TS_HOSTNAME` | `pg-backup` | URL is `https://<name>.<tailnet>.ts.net` |
| `TS_HTTPS` | `true` | Needs HTTPS turned on for the tailnet. `false` serves HTTP on the tailnet. |
| `SLACK_BOT_TOKEN` | optional | Slack bot token with `chat:write`; requires `SLACK_CHANNEL` |
| `SLACK_CHANNEL` | optional | Slack channel ID, not a channel name; requires `SLACK_BOT_TOKEN` |
| `MAX_PARALLEL` | `2` | Jobs that may run at the same time |
| `DATA_DIR` / `WORKDIR` | `/data` / `/tmp/pg-backup` | State and temp dumps |
| `DEV_NO_AUTH` | — | `1` serves plain HTTP on `LISTEN_ADDR` with no login. Local dev only. `LISTEN_ADDR` must be loopback unless `DEV_NO_AUTH_ANY_ADDR=1` (for a container whose port is published on 127.0.0.1). |

The Postgres role needs `CREATEDB` for verify and restore as copy.

For Slack alerts, install a bot with `chat:write` and invite it to the target
channel. Set `SLACK_CHANNEL` to the channel ID for `#infra-alerts`, not the name.
Alerts are disabled when either Slack setting is empty. Each failed job sends
one `chat.postMessage` request, with a 10-second timeout and no retries.
HTTP errors, invalid responses and Slack responses with `ok: false` are logged
without the bot token or response body. Notification failure does not change
the job result.

## Restore database grants

Dumps include schema, table, sequence and default-privilege ACLs. Restore paths keep `--no-owner`. This skips ownership changes but does not remove source-role references from archived `ALTER DEFAULT PRIVILEGES FOR ROLE` entries.

PostgreSQL roles are cluster-wide, and `pg_dump` does not create them. Create every role named by an archived grant, including each referenced source owner, before restore. The restore authority must be a superuser or a member of each referenced source role. Owning the database or restored objects alone does not grant this authority. Restore and verify fail when a grant names a missing role.

With `--no-owner`, the effective restore role owns newly created objects. Default privileges affect only future objects created by the role named in each entry. Ensure the role that will create future tables and sequences has the required defaults.

A failed restore-as-copy or manual `pg_restore` can leave partial objects because those restores are not transactional. Fix missing roles or permissions, then retry restore-as-copy into a fresh scratch database. Replace live runs in a transaction. Grant or SQL errors roll back the transaction, leaving no partial replacement. Legacy dumps made with `--no-acl` cannot regain omitted grants. For explicit recovery, restore with `pg_restore --no-acl`, then apply schema, table, sequence and default-privilege grants. Check runtime `SELECT`, `INSERT` and sequence access, plus defaults for future tables and sequences.

From the repository root, run `tests/acl-flags.sh` and `tests/acl-restore-drill.sh` to check command flags and a local Docker restore.

## Container image

The app image is `ghcr.io/<owner>/pg-backup`. Pull requests and pushes to
`main` run checks without publishing. A push of an `app-vX.Y.Z` tag publishes
the versioned image. A manual workflow run does not publish unless its
`publish` input is explicitly set to `true`.

## Develop

```bash
cd app
docker compose up -d --build   # Postgres 16/17/18, an S3 stand-in, the app on :8091
test/e2e.sh                    # full end-to-end run in its own stack, then teardown
go test ./...                  # unit tests
```

The human acceptance flow is in [MANUAL_TESTING.md](MANUAL_TESTING.md).
