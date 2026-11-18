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

> **New:** [`app/`](app/README.md) holds `pg-backup`, one service with a web
> page that backs up many databases. It is meant to replace one sidecar per
> database.

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

### Restore database grants

Custom-format dumps include schema, table, sequence and default-privilege ACLs. The dump command keeps `--no-owner`, and restore commands use it too. This skips ownership changes but does not remove source-role references from archived default ACLs.

PostgreSQL roles are cluster-wide. `pg_dump` does not create them. Before restore, create every role named by an archived grant, including each source owner after `FOR ROLE`. The restore authority must be a superuser or a member of every referenced source role to apply its default privileges. Owning restored objects or owning the database is not enough. Missing grant roles cause restore and verify to fail.

With `--no-owner`, the effective restore role owns new objects. Default privileges apply only to future objects created by the role named in each entry. Make the role that will create future tables and sequences match the default-privilege entries, or set the required defaults for that creator.

Restore is not transactional and may leave partial objects behind after a failure. Fix missing roles or permissions, then retry into a fresh scratch database. Dumps made with `--no-acl` cannot restore omitted grants. For those dumps, use `pg_restore --no-acl` only as an explicit recovery choice, then reapply app-specific schema, table, sequence and default privileges. Check runtime `SELECT` and `INSERT` access, sequence access, and defaults for future objects.

From the repository root, run `tests/acl-flags.sh` for command regressions and `tests/acl-restore-drill.sh` for a local Docker restore test. The drill uses a throwaway `postgres:17-alpine` container on a Docker-assigned loopback port.

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
  ghcr.io/niuluc/pg-s3-backup:sha-05e1e61
```

The example pins an immutable tag. For production, see
[Pin it in production](#pin-it-in-production).

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
for `linux/amd64` and `linux/arm64`. Pushes to `main` and pull requests build
both images for validation without publishing. A release tag `vX.Y.Z`
publishes the version tags and moves `latest` and `pg18`. A manual
`workflow_dispatch` run builds without publishing unless its `publish`
input is set to `true`. With that opt-in, a dispatch targeting an existing
stable `vX.Y.Z` tag also publishes and moves `latest` and `pg18`. See
[Releasing this image](#releasing-this-image).

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

## Releasing this image

`latest` and `pg18` move when someone pushes a stable release tag, or runs
`workflow_dispatch` with `publish=true` against an existing stable `vX.Y.Z`
tag. The dispatch opt-in can publish the selected tag's images and move both
tags. A merge to `main` validates both images but does not publish them. Manual
workflow runs do not publish unless the `publish` input is explicitly set to
`true`.

Cut a release from an up to date `main`:

```bash
git tag v0.2.0
git push https://github.com/niuluc/pg-s3-backup.git v0.2.0
```

The tag push builds both images and publishes:

| Image | Tags |
| --- | --- |
| Postgres 17 client | `0.2.0`, `0.2`, `sha-<short>`, and moves `latest` |
| Postgres 18 client | `0.2.0-pg18`, `0.2-pg18`, `sha-<short>-pg18`, and moves `pg18` |

Use plain `vX.Y.Z` tags. A tag with a pre-release part, like `v0.2.0-rc.1`,
gets its own tags but does not move `latest` or `pg18`.

### Pin it in production

A production sidecar should run an immutable tag: `X.Y.Z` (or `X.Y.Z-pg18`),
or `sha-<short>` (or `sha-<short>-pg18`). Keep Dokploy autoDeploy off for it.
Do not run `latest` or `pg18` in production. They move with every release, so
a restart can pull new code nobody chose for that app.

Pick the client major from the table in [Image](#image). `pg_dump` must be the
same major as the server or newer. `latest` is the Postgres 17 client and
`pg18` is the Postgres 18 client.

### Upgrade a consumer

1. Cut the release, or pick an existing tag.
2. In Dokploy, change the image tag of one backup app by hand, for example
   `ghcr.io/niuluc/pg-s3-backup:0.2.0`, and redeploy it.
3. Wait for its next nightly run. Check that the dump reached S3 and that
   `pg-s3-backup verify latest` passes in that container. Here `latest`
   means the newest dump, not the image tag.
4. Then move the next app. One app at a time.

To roll back, set the app back to the tag it ran before and redeploy.

## License

MIT — see [LICENSE](LICENSE).
