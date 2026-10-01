# Release design for pg-s3-backup

Status: decided 2026-10-01: option d.

The fleet row listed pg-s3-backup as `release: gated` (ADR 0001 in the release
skill), not onboarded yet. This page checked whether the gated "Create release"
workflow fits this image, listed the options, and recommended one. The operator
picked option d. The sections from "How the image ships today" to
"Recommendation" are kept as the record of that choice. They describe the
workflow as it was before step 4.

## Decision

Option d. Releases are plain `v*` tags pushed by hand, and every production
consumer pins an immutable tag with autoDeploy off. A merge to `main` must not
reach a production backup.

| Step | What | State |
|---|---|---|
| 1 | Pin `offerlink-dev-db-backup` to `sha-05e1e61` and `nonoiseletter-db-backup` to `sha-05e1e61-pg18`, autoDeploy off | Being done in Dokploy on 2026-10-01, checked by digest against what they ran before |
| 2 | Paperwork: ADR 0001 amendment and the fleet row | Done in the operator's fleet repo |
| 3 | Release process: tag `vX.Y.Z` by hand, consumers upgrade one app at a time | Written down in README.md, "Releasing this image" |
| 4 | `publish.yml`: `main` pushes only sha tags, `latest` and `pg18` move only on a release tag | This change (PR #2) |

### Tags per event after step 4

`latest` and `pg18` come from one explicit rule per matrix leg:
`type=raw,value=latest` (pg17) and `type=raw,value=pg18` (pg18), each with
`enable=${{ startsWith(github.ref, 'refs/tags/v') && !contains(github.ref, '-') }}`.
Both legs set `flavor: latest=false`, so metadata-action never adds `latest`
by itself. Before, the pg17 leg had `latest=auto`, which adds `latest` on any
stable semver tag. That is fine for pg17 but would put `latest` on the pg18
image too, so the pg18 leg always had `latest=false`. Now both legs do the
same thing.

Example: commit `6e56e2e`, release tag `v0.2.0`. Registry prefix
`ghcr.io/niuluc/pg-s3-backup:` left out.

| Event | pg17 leg | pg18 leg | Pushed? |
|---|---|---|---|
| push to `main` | `sha-6e56e2e` | `sha-6e56e2e-pg18` | yes |
| push of tag `v0.2.0` | `0.2.0` `0.2` `latest` `sha-6e56e2e` | `0.2.0-pg18` `0.2-pg18` `pg18` `sha-6e56e2e-pg18` | yes |
| push of tag `v0.2.0-rc.1` | `0.2.0-rc.1` `sha-6e56e2e` | `0.2.0-rc.1-pg18` `sha-6e56e2e-pg18` | yes |
| pull request to `main` | `sha-6e56e2e` | `sha-6e56e2e-pg18` | no, build only |
| `workflow_dispatch` on `main` | `sha-6e56e2e` | `sha-6e56e2e-pg18` | yes |
| `workflow_dispatch` on tag `v0.2.0` | same as the tag push | same as the tag push | yes |

The same table for the old rules differs in three rows. A push to `main` and a
`workflow_dispatch` on `main` also pushed `latest` and `pg18`. A tag push moved
`latest` but not `pg18`.

How this was checked: the rows come from running metadata-action's own
`dist/index.js` (v5.10.0, `c299e40`, what `@v5` points to) locally with
`GITHUB_*` set for each event. The `enable=` expression was evaluated by hand
with the same logic, since only GitHub evaluates `${{ }}`. Nothing was pushed.
The real proof comes after the merge: the digests of `latest` and `pg18` must
not change when the merge commit lands on `main`.

## How the image ships today

`.github/workflows/publish.yml` is the only workflow.

| Event | What it does |
|---|---|
| push to `main` | builds pg17 and pg18, amd64 and arm64, and pushes `latest`, `pg18`, `sha-<7>` and `sha-<7>-pg18` |
| push of a `v*` tag | pushes `X.Y.Z`, `X.Y`, `X.Y.Z-pg18`, `X.Y-pg18` and the sha tags. The pg17 build also moves `latest` (`flavor: latest=auto`, see the comment in the file). `pg18` does not move, it only follows `main`. |
| pull request to `main` | builds both, pushes nothing |
| `workflow_dispatch` | same as the ref it runs on |

So every merge to `main` is a release of `latest` and `pg18`. A push of a
feature branch runs nothing, since `push` only lists `main` and `v*` tags.
The repo has no `v*` tags yet.

## Who pulls it

From `skills/infra/data/fleet.yaml`:

| Consumer | Image | Floats? |
|---|---|---|
| `offerlink-dev-db-backup` (OfferLink prod, hetzner-worker-prod) | `latest`, Dokploy autoDeploy on | yes |
| `nonoiseletter-db-backup` (priv-newsletter prod) | `pg18` | yes |
| `board-app-db-backup` | `sha-05e1e61` | no, pinned |
| `trippy-db-backup` | `sha-05e1e61` | no, pinned |

Two prod backup jobs follow `main`. A bad merge here can break the backups of
two products, and nobody sees it until the next 03:00 run fails, or until a
restore is needed.

## Does the reference workflow fit?

The reference is `.github/workflows/release-create.yml` in
`JakubSzwajka/drunk-cat-stack` (read at `1dce4aa`). Devops step a says to copy
it as is and change only the `env:` block. Here that breaks in five places:

```
reference "Create release"            pg-s3-backup
--------------------------            ------------
build ONE image, runner arch only  -> needs pg17 + pg18, amd64 + arm64
push :prod-sha-<12> :vX.Y.Z :latest -> :latest is what OfferLink prod pulls
deploy job: ONE Dokploy app id      -> 4 consumer apps in 4 projects
poll APP_URL + /api/health          -> no HTTP server, PID 1 is supercronic
  for .commit == sha12
fx release verify, checks 2-5       -> health, one Dokploy app, one
                                        prod-sha image on one machine,
                                        one #deploys line: none fit
```

1. The build step would overwrite the multi-arch pg17 `latest` with an amd64
   pg17 image and build no pg18 at all. Fixing that means a matrix and QEMU, so
   the copy drifts from the reference far past the `env:` block.
2. The deploy job deploys one app. This image has four, owned by other
   projects with their own release cadence.
3. The health wait needs a public URL that reports the commit. A backup sidecar
   has no route and should not get one.
4. `fx release verify` has no opt-out for checks 2 to 5. `fx validate` warns
   about the missing `prod.health_url`.
5. The reference tags with `GITHUB_TOKEN`. GitHub does not start other
   workflows from events made with that token, so that tag push would not run
   `publish.yml` either. Any design that cuts a tag from a workflow must build
   in that same workflow.

The commit-in-health idea (devops step b) also has no place to live: there is
no health endpoint to return `.commit`.

## Options

| | Option | What it costs | Risk |
|---|---|---|---|
| a | Copy "Create release", drop the deploy job and the health wait. A release only builds, tags and cuts a GitHub Release. Add an opt-out to fx for checks 2 to 5. | A matrix build that differs from the reference. fx code and fleet schema changes in `~/.agents`. The gated words (deploy, safe rollback) stop meaning anything for this row. | Low for the image. The workflow still pushes `latest` unless that line is removed, which is another drift. |
| b | Add a small HTTP health listener to the image, give it a route, keep the reference as is. | A second process next to supercronic, so a supervisor or a backgrounded shell job. A Traefik route and a domain for a backup container. Still one app per workflow, so three consumers stay out. | High. It changes the backup image's behaviour. A 200 says nothing about whether last night's dump reached S3. It opens a backup container to the internet. |
| c | Do not gate. Version with plain `v*` tags pushed by hand; `publish.yml` already builds every tag it needs. Reclassify the fleet row. | An ADR 0001 amendment, since the ADR names pg-s3-backup as gated. No code change. | `latest` and `pg18` keep following `main`, so two prod consumers still float. |
| d | c, plus: treat the image as a library. Pin every consumer to an immutable tag (`X.Y.Z`, `X.Y.Z-pg18` or `sha-...`) and turn off autoDeploy on them. A consumer moves to a new version through its own change, and proves it with `pg-s3-backup verify latest`. Later, stop moving `latest` and `pg18` on `main`. | Two Dokploy edits on prod apps (needs a yes each). The ADR amendment from c. A later `publish.yml` change, which must wait until no consumer floats. | Lowest. A merge here cannot reach a prod backup at all. |

## Recommendation

Option d. The gated profile assumes one app with one health URL and a
rollback by redeploy. This repo is closer to a base image used by several
apps, so the useful gate is on the consumer side: each app pins a tag and
moves it on purpose.

Suggested order, each step its own yes:

This list is the original proposal. The "Decision" section has the state.

1. [ ] Pin `offerlink-dev-db-backup` and `nonoiseletter-db-backup` to the
   immutable tags that match what they run now, and turn autoDeploy off.
   The last `main` build was `05e1e61`, so that should be `sha-05e1e61` and
   `sha-05e1e61-pg18`. Compare digests in GHCR before the switch.
   After that, merging to this repo touches no prod backup.
2. [ ] Amend ADR 0001 and the fleet row: pg-s3-backup leaves `gated`, as
   `infra` or as a new library class. That is the operator's call.
3. [ ] Cut `v0.1.0` by pushing the tag by hand. `publish.yml` builds it today.
   Note that this also moves `latest`, which is harmless once step 1 is done.
4. [ ] Optional: change `publish.yml` so `main` pushes only sha tags, and
   `latest` and `pg18` move only on a `v*` tag, or never.

The proof that a backup image works is a dump in S3 that restores. If fx ever
needs a check for this row, `pg-s3-backup verify latest` run in the consumer
container is that check, not an HTTP probe.

## Not verified

- Whether Dokploy autoDeploy on a Docker-image app redeploys when GHCR gets a
  new `latest`. Nothing in `publish.yml` calls a Dokploy webhook. Treat it as
  "it will redeploy at some point", since any restart or reschedule can pull
  the new `latest` anyway.
- Which tags exist in GHCR. The package is private and the local `gh` token
  has no `read:packages` scope. `sha-05e1e61` exists, per the board-app and
  trippy fleet notes.
- Whether any consumer runs on arm64.
