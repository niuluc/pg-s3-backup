# Prototype: one backup service for many databases

Question: can one service with a small web page replace the six
`pg-s3-backup` sidecars, one per database, that run today?

Status: throwaway prototype. The real version gets rebuilt properly. It
does not copy this code.

## What it is

```
browser ──(tailnet only)──► pg-backup ──► postgres 16 / 17 / 18 (by service name)
                               │
                               └──rclone──► s3://bucket/<folder>/<db>-<time>.dump
```

- One Go binary with server-rendered HTML. No JavaScript framework.
- Targets live in `/data/state.json` (mode 0600): name, host, port, database,
  user, password, cron schedule (UTC), retention and S3 folder. You manage
  them in the web page.
- Version detection: on save and before each job, the app reads
  `server_version_num`. It then picks `/usr/libexec/postgresql<major>/pg_dump`.
  The image ships the 16, 17 and 18 clients (`alpine:3.23`).
- Backup: `pg_dump -Fc`, then `pg_restore --list` as an integrity gate, then
  upload. Pruning deletes only that target's own `<db>-*.dump` files.
- Verify: restores the newest dump into a throwaway `<db>_verify_<ts>`
  database, counts its tables and drops it.
- One job per target at a time. At most `MAX_PARALLEL` (default 2) jobs run
  at once.
- Restore is not in the web page, on purpose. It would stay a command.

## Run it locally

```bash
docker compose up -d --build     # pg16, pg17, pg18, an S3 stand-in, the app
open http://127.0.0.1:8090
```

Local mode has no login. Bind it to your machine only.

## Login through Tailscale

Set `TS_AUTHKEY` and the app joins the tailnet itself (tsnet). It does
not listen on any public port:

| Env | Meaning |
|---|---|
| `TS_AUTHKEY` | Auth key from the Tailscale admin console. Use it once, then node state persists in `/data/tailscale`. |
| `TS_HOSTNAME` | Node name, default `pg-backup`. URL: `https://pg-backup.<tailnet>.ts.net`. |
| `TS_HTTPS` | `true` (default) serves HTTPS with a Tailscale cert. This needs HTTPS turned on for the tailnet. |
| `ALLOWED_USERS` | Comma list of Tailscale logins. Empty lets in everyone on the tailnet. |

The app asks the Tailscale daemon who each request comes from (`WhoIs`).
That login is the user. There are no passwords, sessions or cookies.
You need no subdomain, DNS record, Traefik route or Cloudflare change.

## What was proved (2026-11-14, local)

| Check | Result |
|---|---|
| Add 4 targets via the form | 3 connected (PG 16.16, 17.11, 18.7); 1 wrong host saved with a visible error |
| Back up now, all 4 | 3 ok with clients `postgresql16`/`17`/`18`; 1 failed and shows red on the dashboard |
| Scheduled run (`*/2 * * * *`) | fired, trigger `schedule` |
| Verify latest, 3 targets | ok, 2 tables restored each, scratch databases dropped |
| Files in S3 | `backups/{shop,blog,stats}/<db>-<ts>.dump` |
| Restart | targets and history kept; jobs running at restart are marked failed |
| Tailscale mode | compiles; **not run**, it needs an auth key |

## Verdict

Yes, one service works and is simpler to run than six sidecars. Adding a
database is one form. The client version is always correct, which would
fix the Umami PG18-dump-on-PG16 restore problem.

Open before a real build:

1. Secrets: database passwords sit in one file on one volume. Today each
   sidecar holds one in Dokploy env. With tailnet-only access, this is
   acceptable at our scale.
2. Network: the service must reach every database. Today they are all on
   `dokploy-network`, so one attachment covers them.
3. Alerts: no failure alert yet. Add a Grafana/Loki alert on failed runs,
   or a `/metrics` endpoint.
4. Migration: import the six sidecars' env once, run both in parallel for a
   few nights, then remove the sidecars one at a time.
