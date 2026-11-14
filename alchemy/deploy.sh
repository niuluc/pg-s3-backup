#!/usr/bin/env bash
# Plan or deploy the pg-backup stack. Run from omarchy only (for now): it
# reaches hetzner-manager and hetzner-worker-prod over ssh, and keeps the
# Alchemy state in ./.alchemy (local state).
#
#   ./deploy.sh plan
#   ./deploy.sh deploy
#
# Secrets come from the self-hosted Infisical (project pg-backup, env prod).
# Auth: an Alchemy profile with an Infisical machine identity
# (`bunx alchemy profile edit --add Infisical`), or INFISICAL_TOKEN.
set -Eeuo pipefail
cd "$(dirname "$0")"

action="${1:-}"
case "$action" in
  plan | deploy) ;;
  *) echo "usage: $0 plan|deploy" >&2; exit 2 ;;
esac

export INFISICAL_API_URL="${INFISICAL_API_URL:-https://secrets.kubaszwajka.com}"

[ -d node_modules ] || bun install --frozen-lockfile
exec bunx alchemy "$action" --stage prod
