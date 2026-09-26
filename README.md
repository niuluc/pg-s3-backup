# pg-s3-backup — Postgres backup / restore sidecar

Self-contained image that dumps a Postgres database **over the network** and
stores the dump in any S3-compatible bucket via [rclone](https://rclone.org/).
On start it runs a [supercronic](https://github.com/aptible/supercronic)
schedule; you can also `docker exec` in to list, restore, or verify backups on
demand.

```
┌─────────────────┐   pg_dump (network)   ┌──────────┐   rclone   ┌────────┐
│  pg-s3-backup   │ ────────────────────► │ postgres │            │   S3   │
│  (supercronic)  │                       └──────────┘ ─────────► │ bucket │
└─────────────────┘   any node                any node            └────────┘
```

## Why it exists

Most Postgres backup sidecars shell out to `docker exec` against a local
container. On a multi-node Docker Swarm — and specifically on
[Dokploy](https://dokploy.com/) — the scheduled backup job runs `docker ps` on
the **manager** while the database task sits on a **worker**, so it finds
nothing and silently backs up an empty set.

`pg-s3-backup` connects to Postgres over the overlay network by service DNS. No
docker socket, no node co-location, no placement constraint. Any Swarm +
Dokploy setup hits that bug; this sidesteps it.

## Commands

| Command | Description |
| --- | --- |
| `pg-s3-backup backup` | `pg_dump -Fc` → integrity-check → upload to S3 → prune old remote copies. This is what the cron runs. |
| `pg-s3-backup list` | List backups in the S3 prefix (size / modtime / name). |
| `pg-s3-backup restore <name\|latest> [target-db]` | Download a backup and `pg_restore --clean --if-exists` it. **Destructive** — see safety note. |
| `pg-s3-backup verify [<name\|latest>]` | Restore into a throwaway `<db>_verify_<ts>` database, assert it has tables, then drop it. Non-destructive. |
| `pg-s3-backup check-env` | Validate required env and exit. |

`latest` resolves to the newest backup by name (timestamps sort lexically).

### Restore safety

`restore` overwrites the target database. It requires confirmation:

- **Interactive** (`docker exec -it …`): prompts `y/N`.
- **Non-interactive** (no TTY): refuses unless `BACKUP_YES=1` is set.

`verify` is the safe way to prove a backup is restorable — it never touches the
live database. A backup you have never restored is not a backup.

## Configuration (environment)

**Required**

| Var | Meaning |
| --- | --- |
| `PGHOST` | Postgres host — the service name on the shared network. |
| `PGUSER` | Postgres role. |
| `PGPASSWORD` | Password for that role. |
| `PGDATABASE` | Database to back up. |
| `S3_ENDPOINT` | S3-compatible endpoint URL. |
| `S3_BUCKET` | Bucket name. |
| `S3_ACCESS_KEY_ID` | Access key. |
| `S3_SECRET_ACCESS_KEY` | Secret key. |

**Optional (defaults)**

| Var | Default | Meaning |
| --- | --- | --- |
| `PGPORT` | `5432` | Postgres port. |
| `S3_PREFIX` | `db` | Key prefix within the bucket. |
| `S3_REGION` | `us-east-1` | Region. |
| `S3_PROVIDER` | `Other` | rclone S3 provider. Hetzner Object Storage: `Other`. MinIO: `Minio`. |
| `S3_FORCE_PATH_STYLE` | `true` | Path-style addressing. Set `false` for virtual-hosted providers if needed. |
| `RETENTION_DAYS` | `14` | Delete remote backups older than this after each run. |
| `BACKUP_CRON` | `0 2 * * *` | supercronic schedule for the scheduled backup. |
| `BACKUP_ON_START` | `false` | Run one backup immediately on container start. |
| `WORKDIR` | `/tmp/pg-s3-backup` | Scratch directory for dumps. |
| `BACKUP_YES` | — | Set `1` to skip the destructive-restore confirmation. |

Secrets are passed through the environment only, never on the command line, so
they don't leak via `ps`. Prefer Docker/Swarm secrets in production.

## Deploy (Docker Swarm)

Run it as a Swarm service on the same overlay network as the database:

```bash
docker service create \
  --name my-db-backup \
  --network my-internal-net \
  --env PGHOST=postgres \
  --env PGUSER=myapp \
  --env PGDATABASE=myapp \
  --env PGPASSWORD=**** \
  --env S3_ENDPOINT=https://<region>.your-objectstorage.com \
  --env S3_BUCKET=my-backups \
  --env S3_PREFIX=db \
  --env S3_ACCESS_KEY_ID=**** \
  --env S3_SECRET_ACCESS_KEY=**** \
  --env BACKUP_CRON='0 2 * * *' \
  --env RETENTION_DAYS=14 \
  ghcr.io/niuluc/pg-s3-backup:latest
```

Or deploy it as a Dokploy application pointing at the same image, with the env
above and the database's network attached.

> **If you alert on this container, the service name is load-bearing.** Metric
> queries that find the container by name (e.g. Grafana
> `{container=~".*db-?backup.*"}`) match nothing if you rename the service. When
> such a rule uses `noDataState: Alerting`, that becomes a permanent false
> critical rather than a silent gap. Pick a name and keep the alert query in
> sync with it.

### Operate

```bash
c=$(docker ps -q -f name=my-db-backup)
docker exec -it "$c" pg-s3-backup list
docker exec -it "$c" pg-s3-backup verify latest          # safe restore proof
docker exec -it "$c" pg-s3-backup restore latest         # DESTRUCTIVE (prompts)
```

## Image

Published by `.github/workflows/publish.yml` to `ghcr.io/niuluc/pg-s3-backup`
for `linux/amd64` and `linux/arm64` on push to `main`. Pull requests build for
validation only.

The `pg_dump`/`pg_restore` major version must be `>=` the server it dumps. Pick
the tag that matches your server:

| Server | Tag | Also published |
| --- | --- | --- |
| Postgres ≤ 17 | `latest` | `sha-<short>`, and `X.Y.Z` / `X.Y` on `v*` tags |
| Postgres 18 | `pg18` | `sha-<short>-pg18`, and `X.Y.Z-pg18` / `X.Y-pg18` on `v*` tags |

`latest` stays on the 17 client, so existing sidecars do not change. Point a
sidecar at `pg18` only when its server runs Postgres 18.

The client major is the `PG_MAJOR` build argument (default `17`):

```bash
docker build --build-arg PG_MAJOR=18 -t pg-s3-backup:pg18 .
```

## License

MIT — see [LICENSE](LICENSE).
