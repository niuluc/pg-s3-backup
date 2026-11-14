# pg-backup deploy (Alchemy)

This Alchemy stack runs the `pg-backup` service (see `app/`) on the hetzner
Swarm. It does not use Dokploy. Alchemy and every `@alchemy.run/*` package are
pinned to `2.0.0-beta.87`. Bun is pinned in `mise.toml`.

```
omarchy ──ssh──► hetzner-manager      Docker.Service pg-backup (1 replica)
        └─ssh──► hetzner-worker-prod  Docker.Volume pg-backup-data (protected)

pg-backup task on tier==prod ── dokploy-network (overlay, not managed here) ──► every Postgres
```

## What the stack manages

| Resource | Where | Notes |
|---|---|---|
| `Docker.Context` `pg-backup-hetzner-manager` | `ssh://hetzner-manager` | Local Docker context on the deploying machine |
| `Docker.Context` `pg-backup-hetzner-worker-prod` | `ssh://hetzner-worker-prod` | Used only to create the volume |
| `Docker.Volume` `pg-backup-data` | worker-prod | `/data`: `state.json` and `tailscale/` (tsnet node state). `RemovalPolicy.protect()`: never replaced or destroyed. |
| `Docker.Service` `pg-backup` | Swarm, `node.labels.tier==prod` | `stop-first` updates, so two tasks never share one tsnet identity |

The stack does not manage the Swarm or `dokploy-network`. Both belong to Dokploy.

## Config

All values come from Infisical (project `pg-backup`, environment `prod`). No value has a default.

| Name | Kind |
|---|---|
| `PG_BACKUP_VERSION` | string, image tag of `ghcr.io/niuluc/pg-backup` |
| `ALLOWED_USERS`, `S3_ENDPOINT`, `S3_BUCKET` | string |
| `TS_AUTHKEY`, `S3_ACCESS_KEY_ID`, `S3_SECRET_ACCESS_KEY`, `DISCORD_WEBHOOK_URL` | redacted |

## Run

From omarchy only:

```bash
cd alchemy
./deploy.sh plan     # preview
./deploy.sh deploy   # apply
```

`deploy.sh` sets `INFISICAL_API_URL` to the self-hosted instance. The Infisical
login is an Alchemy profile (`bunx alchemy profile edit --add Infisical`) or
`INFISICAL_TOKEN`. State lives in `./.alchemy/` on omarchy and is gitignored.

## Known limits

- `Docker.Service` in beta.87 has no registry credentials and does not pass
  `--with-registry-auth`. A private GHCR image fails to pull on the worker
  unless that node can already pull it.
- Environment values, redacted ones included, go to `docker service create
  --env` and are stored in the service spec. `docker service inspect` on a
  manager shows them.
