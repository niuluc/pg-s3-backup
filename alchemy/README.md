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
| `ALLOWED_USERS`, `S3_ENDPOINT`, `S3_BUCKET`, `SLACK_CHANNEL` | string |
| `S3_ACCESS_KEY_ID`, `S3_SECRET_ACCESS_KEY`, `SLACK_BOT_TOKEN` | redacted |
| `TS_AUTHKEY` | redacted, optional. Needed only for the first start. Remove it from Infisical afterwards; the next deploy drops the env var. |

For the Slack rollout, add `SLACK_BOT_TOKEN` and `SLACK_CHANNEL` to this
Infisical project and remove `DISCORD_WEBHOOK_URL`. Use the channel ID for
`#infra-alerts`. Install a bot with `chat:write` and invite it to the channel.

Do not apply this environment change to the current image. The app image
release waits for Kuba under INFRA-217, together with the database-grants fix.
The rollout needs that approved image and both Slack settings in one update.
This source change does not release an image or change the running service.

## Prerequisites

1. **Tailscale.** Turn on MagicDNS and HTTPS Certificates for the tailnet in
   the admin console. Without them tsnet exits with "you must enable HTTPS in
   the admin panel".
2. **First start only: `TS_AUTHKEY`.** Create a non-reusable,
   non-ephemeral auth key and put it in Infisical as `TS_AUTHKEY`. Deploy.
   After the node `pg-backup` joins:
   1. In the Tailscale admin console, disable key expiry on the machine.
   2. Delete `TS_AUTHKEY` from Infisical and deploy again. The next deploy
      drops the env var.

   The node identity lives in `/data/tailscale` on the `pg-backup-data`
   volume, so later restarts and deploys need no key.
3. **Infisical login** that points at the self-hosted instance (see below).

## Run

From omarchy only:

```bash
cd alchemy
./deploy.sh plan           # preview
./deploy.sh deploy         # apply (asks for approval in a terminal)
./deploy.sh drift          # compare live resources with state
```

Extra args go to Alchemy after `--stage prod`. `--stage` and `--profile`
are refused; set `ALCHEMY_PROFILE` to pick a profile.

**Agents and `--yes`.** Alchemy shows no approval prompt in agent
environments: `pi` on `PATH`, CI, or no TTY. There, an agent runs
`./deploy.sh deploy --yes`, but only after a human said yes to that exact
deploy, with the plan in front of them.

Infisical must be the self-hosted instance, `https://secrets.kubaszwajka.com`.
How the URL is chosen depends on the login:

- Env auth (`INFISICAL_TOKEN` or `INFISICAL_IDENTITY_ID`) wins over a stored
  profile and uses `INFISICAL_API_URL`. `deploy.sh` sets it to the
  self-hosted instance when unset.
- A stored profile (`bunx alchemy profile edit --add Infisical`, machine
  identity) uses its own `apiBaseUrl` and ignores `INFISICAL_API_URL`. Without
  `apiBaseUrl` it uses Infisical Cloud. Store
  `https://secrets.kubaszwajka.com` as `apiBaseUrl` in the profile.
  Source: `node_modules/@alchemy.run/infisical/src/AuthProvider.ts`,
  `readEnvironment` versus the stored-profile path.

`deploy.sh` checks this before it runs Alchemy and refuses on a mismatch. For
env auth it checks `INFISICAL_API_URL`. Otherwise it reads only `apiBaseUrl` from
`~/.alchemy/profiles/<ALCHEMY_PROFILE or default>/infisical.json`. If a
future Alchemy changes that file's layout, the check fails closed.

State lives outside the repo, in `~/.local/share/alchemy/state` (mode 0700) on
the deploy machine. `localState()` takes no path in beta.87: it always uses
`<cwd>/.alchemy/state` (`node_modules/alchemy/src/State/LocalState.ts`,
`makeLocalState`). So `deploy.sh` makes `.alchemy/state` a symlink to that root,
and refuses to run if `.alchemy/state` exists and is anything else. The root is
shared with other stacks. Alchemy files state under `<stack>/<stage>/`, so every
stack needs its own name. `.alchemy/` is gitignored. Always run through
`deploy.sh`: a bare `bunx alchemy` on a fresh checkout writes state into the
worktree.

A changed secret plans as an update of `pg-backup`, not a no-op or replace.
`Service.ts` `diff` compares the environment after unwrapping `Redacted`
values. The environment is updated in place.

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
- **Secrets in plain text in state.** Local state stores `Redacted` values
  unwrapped (`node_modules/alchemy/src/State/StateEncoding.ts`,
  `encodeState`). `~/.local/share/alchemy/state` on the deploy machine holds
  every secret of this stack.
- **No healthcheck.** The app listens only on the tailnet (tsnet), so a
  container healthcheck cannot reach `/healthz`.

## Follow-ups

1. Read secrets from Swarm secrets: add `*_FILE` variants (for example
   `S3_SECRET_ACCESS_KEY_FILE`) to the app. Then mount Swarm secrets
   instead of `environment`, which removes the two secret exposures above.
2. Use an encrypted remote state store instead of local state.
3. Serve `/healthz` on `127.0.0.1` inside the container as well. Then add a
   Swarm healthcheck such as `wget -qO- http://127.0.0.1:<port>/healthz`.
