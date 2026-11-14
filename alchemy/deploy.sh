#!/usr/bin/env bash
# Plan or deploy the pg-backup stack. Run from omarchy only (for now): it
# reaches hetzner-manager and hetzner-worker-prod over ssh.
#
# Alchemy keeps local state in <cwd>/.alchemy/state and stores Redacted values
# there in plain text. localState() takes no path in beta.87, so
# ./.alchemy/state is a symlink to ~/.local/share/alchemy/state (mode 0700),
# outside the repo. That root is shared with other stacks; Alchemy files
# state under <stack>/<stage>/, so stack names must differ.
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

state_root="${HOME}/.local/share/alchemy/state"
install -d -m 0700 "$state_root"
chmod 0700 "$state_root"
mkdir -p .alchemy
if [ -L .alchemy/state ]; then
  target="$(readlink .alchemy/state)"
  if [ "$target" != "$state_root" ]; then
    echo "refusing: .alchemy/state points to $target, expected $state_root" >&2
    exit 1
  fi
elif [ -e .alchemy/state ]; then
  echo "refusing: .alchemy/state is not a symlink; move its contents to $state_root and remove it" >&2
  exit 1
else
  ln -s "$state_root" .alchemy/state
fi

export INFISICAL_API_URL="${INFISICAL_API_URL:-https://secrets.kubaszwajka.com}"

[ -d node_modules ] || bun install --frozen-lockfile
exec bunx alchemy "$action" --stage prod
