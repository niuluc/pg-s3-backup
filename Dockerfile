# pg-s3-backup — Postgres backup / restore sidecar.
#
# Self-contained image: Postgres client tools + rclone + supercronic.
# The Postgres client MAJOR version MUST be >= the server it dumps.
# Defaults to 17 (the `latest` image). CI also publishes a PG 18 build
# (`pg18` tag); build one locally with `--build-arg PG_MAJOR=18`.
ARG PG_MAJOR=17
FROM postgres:${PG_MAJOR}-alpine

# https://github.com/aptible/supercronic/releases
ARG SUPERCRONIC_VERSION=v0.2.33
ARG TARGETARCH

RUN set -eux; \
    apk add --no-cache bash rclone ca-certificates tzdata; \
    apk add --no-cache --virtual .dl curl; \
    case "${TARGETARCH:-amd64}" in \
      amd64) sc_arch=amd64 ;; \
      arm64) sc_arch=arm64 ;; \
      *) echo "unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSLo /usr/local/bin/supercronic \
      "https://github.com/aptible/supercronic/releases/download/${SUPERCRONIC_VERSION}/supercronic-linux-${sc_arch}"; \
    chmod +x /usr/local/bin/supercronic; \
    apk del .dl

COPY pg-s3-backup /usr/local/bin/pg-s3-backup
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/pg-s3-backup /usr/local/bin/entrypoint.sh

# supercronic execs cron jobs via $SHELL; the base image leaves it unset, which
# makes the fork/exec fail. Pin it explicitly.
ENV SHELL=/bin/bash

# Override the base postgres entrypoint (which would try to start a server).
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
