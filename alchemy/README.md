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

The image tag is the constant `PG_BACKUP_VERSION` in `alchemy.run.ts`. A
version bump is a commit. `app.yml` publishes that tag when `app-v<version>`
is pushed.

Every other value comes from Infisical (project `pg-backup`, environment
`prod`). No value has a default.

| Name | Kind |
|---|---|
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

- **Registry credentials.** `Docker.Service` in beta.87 has no registry
  credentials and never passes `--with-registry-auth`
  (`node_modules/@alchemy.run/docker/src/Docker.ts`, `service.create` and
  `service.update`). A private GHCR image fails to pull on the worker unless
  that node can already pull it. Open decision: make the package public, or
  add a read-only login on the worker.
- **Secrets in the service spec.** Environment values, redacted ones
  included, are stored in the service spec. `docker service inspect` on a
  manager shows them. Dokploy apps have the same exposure today.
- **Secrets in argv on the deploy machine.** `Docker.Service` passes each
  value as `--env K=V` on `docker service create` and `--env-add K=V` on
  `docker service update`, so it is visible in the process list on the deploy machine
  while the command runs. The Container docs say only names are passed; Service
  differs. Source: `node_modules/@alchemy.run/docker/src/Service.ts`
  (`env: environment` in the create call, `"env-add": environment` in the
  update call; values unwrapped with `Redacted.value`) and
  `node_modules/@alchemy.run/docker/src/Docker.ts` (`formatArgs`, which turns
  them into `--env` and `--env-add` flags).
- **Takeover by name.** If a service named `pg-backup` already exists,
  `Docker.Service` create takes it over without `--adopt`
  (`node_modules/@alchemy.run/docker/src/Service.ts`, the `existingByName`
  branch). No such service exists today.
- **No healthcheck.** The app listens only on the tailnet (tsnet), so a
  container healthcheck cannot reach `/healthz`.

## Follow-ups

1. Read secrets from Swarm secrets: add `*_FILE` variants (for example
   `S3_SECRET_ACCESS_KEY_FILE`) to the app. Then mount Swarm secrets
   instead of `environment`, which removes the two secret exposures above.
2. Serve `/healthz` on `127.0.0.1` inside the container as well. Then add a
   Swarm healthcheck such as `wget -qO- http://127.0.0.1:<port>/healthz`.
