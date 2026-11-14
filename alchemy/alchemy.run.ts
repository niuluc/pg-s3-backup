// pg-backup on the hetzner Swarm.
//
// One replicated service (1 task) on the tier==prod worker. It joins the
// existing `dokploy-network` overlay, where every Postgres service lives, and
// keeps state.json plus its Tailscale node identity on a named volume.
//
// Not managed here, on purpose: the Swarm itself and `dokploy-network`.
// Both belong to Dokploy; this stack only references them by name.
import * as Infisical from "@alchemy.run/infisical";
import * as Docker from "@alchemy.run/docker";
import * as Alchemy from "alchemy";
import * as Config from "effect/Config";
import * as Effect from "effect/Effect";

export default Alchemy.Stack(
  "pg-backup",
  {
    providers: Docker.providers(),
    state: Alchemy.localState(),
    // INFISICAL_API_URL points at the self-hosted instance (set by deploy.sh).
    secrets: [Infisical.Secrets({ project: "pg-backup", environment: "prod" })],
  },
  Effect.gen(function* () {
    // Swarm manager: services are created here and scheduled by the Swarm.
    const manager = yield* Docker.Context("hetzner-manager", {
      name: "pg-backup-hetzner-manager",
      docker: "host=ssh://hetzner-manager",
      description: "hetzner Swarm manager (pg-backup stack)",
    });

    // The tier==prod worker. A `local` volume lives on one node, so the
    // volume is created on the node that runs the task, not on the manager.
    // The service mounts it by name; with a single tier==prod node the task
    // always lands next to it.
    const workerProd = yield* Docker.Context("hetzner-worker-prod", {
      name: "pg-backup-hetzner-worker-prod",
      docker: "host=ssh://hetzner-worker-prod",
      description: "hetzner tier==prod worker (pg-backup data volume)",
    });

    // DATA_DIR: state.json (databases, passwords, run history) and
    // tailscale/ (tsnet node key). Losing it means re-adding databases and a
    // new Tailscale node, so it can be neither replaced nor destroyed.
    const data = yield* Docker.Volume("data", {
      context: workerProd,
      name: "pg-backup-data",
    }).pipe(Alchemy.RemovalPolicy.protect());

    const version = yield* Config.String("PG_BACKUP_VERSION");

    const service = yield* Docker.Service("pg-backup", {
      context: manager,
      name: "pg-backup",
      image: `ghcr.io/niuluc/pg-backup:${version}`,
      replicas: 1,
      placement: { constraints: ["node.labels.tier==prod"] },
      networks: ["dokploy-network"],
      volumes: [{ hostPath: data.name, containerPath: "/data" }],
      environment: {
        DATA_DIR: "/data",
        TS_HOSTNAME: "pg-backup",
        ALLOWED_USERS: yield* Config.String("ALLOWED_USERS"),
        TS_AUTHKEY: yield* Config.Redacted("TS_AUTHKEY"),
        S3_ENDPOINT: yield* Config.String("S3_ENDPOINT"),
        S3_BUCKET: yield* Config.String("S3_BUCKET"),
        S3_ACCESS_KEY_ID: yield* Config.Redacted("S3_ACCESS_KEY_ID"),
        S3_SECRET_ACCESS_KEY: yield* Config.Redacted("S3_SECRET_ACCESS_KEY"),
        DISCORD_WEBHOOK_URL: yield* Config.Redacted("DISCORD_WEBHOOK_URL"),
      },
      // Never two tasks at once: they would share one tsnet identity and one
      // state.json. Stop the old task first, roll back if the new one fails.
      updateConfig: { parallelism: 1, order: "stop-first", failureAction: "rollback" },
      restartPolicy: { condition: "any", delay: "10s" },
      // A running pg_dump or restore gets time to finish or fail cleanly.
      stopGracePeriod: "120s",
      labels: { "managed-by": "alchemy", stack: "pg-backup" },
    });

    return { service: service.name, volume: data.name, image: `ghcr.io/niuluc/pg-backup:${version}` };
  }),
);
