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
#   ./deploy.sh deploy [--yes]
#   ./deploy.sh drift  [--yes]
#
# Extra args go to alchemy after `--stage prod`. Agents (pi on PATH, CI,
# non-TTY) get no approval prompt, so they pass --yes, and only after a human
# said yes to that exact deploy. --stage and --profile are refused: the stage is
# always prod, and the profile comes from ALCHEMY_PROFILE so the Infisical URL
# check below reads the same profile Alchemy uses.
#
# Secrets come from the self-hosted Infisical (project pg-backup, env prod).
# Auth: an Alchemy profile with an Infisical machine identity
# (`bunx alchemy profile edit --add Infisical`), or INFISICAL_TOKEN.
set -Eeuo pipefail
cd "$(dirname "$0")"

action="${1:-}"
case "$action" in
  plan | deploy | drift) ;;
  *) echo "usage: $0 plan|deploy|drift [alchemy flags, e.g. --yes]" >&2; exit 2 ;;
esac
shift
for arg in "$@"; do
  case "$arg" in
    --stage | --stage=* | --profile | --profile=*)
      echo "refusing: $arg; the stage is always prod and the profile comes from ALCHEMY_PROFILE" >&2
      exit 2
      ;;
  esac
done

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

# Infisical must be the self-hosted instance, never Infisical Cloud.
# INFISICAL_API_URL only applies to env auth (INFISICAL_TOKEN or
# INFISICAL_IDENTITY_ID), which wins over a stored profile. A stored profile
# uses its own apiBaseUrl and defaults to Infisical Cloud when unset
# (@alchemy.run/infisical AuthProvider.ts: readEnvironment vs details/resolve).
infisical_url="https://secrets.kubaszwajka.com"
export INFISICAL_API_URL="${INFISICAL_API_URL:-$infisical_url}"
if [ -n "${INFISICAL_TOKEN:-}" ] || [ -n "${INFISICAL_IDENTITY_ID:-}" ]; then
  if [ "${INFISICAL_API_URL%/}" != "$infisical_url" ]; then
    echo "refusing: env auth would use INFISICAL_API_URL=$INFISICAL_API_URL, expected $infisical_url" >&2
    exit 1
  fi
else
  profile="${ALCHEMY_PROFILE:-default}"
  profile_file="${ALCHEMY_HOME:-$HOME/.alchemy}/profiles/$profile/infisical.json"
  if [ ! -f "$profile_file" ]; then
    echo "refusing: no Infisical login. Run: bunx alchemy profile edit --add Infisical (apiBaseUrl $infisical_url), or set INFISICAL_TOKEN" >&2
    exit 1
  fi
  # Print only the stored apiBaseUrl, never other fields (the file holds credentials).
  profile_url="$(bun -e '
    const find = (v) => {
      if (v && typeof v === "object") {
        if (typeof v.apiBaseUrl === "string") return v.apiBaseUrl;
        for (const x of Object.values(v)) { const r = find(x); if (r) return r; }
      }
      return "";
    };
    console.log(find(JSON.parse(require("fs").readFileSync(process.argv.at(-1), "utf8"))));
  ' "$profile_file")"
  if [ "${profile_url%/}" != "$infisical_url" ]; then
    echo "refusing: Alchemy profile '$profile' points Infisical at '${profile_url:-Infisical Cloud (default)}', expected $infisical_url." >&2
    echo "Fix: bunx alchemy profile edit --add Infisical, and set apiBaseUrl to $infisical_url" >&2
    exit 1
  fi
fi

[ -d node_modules ] || bun install --frozen-lockfile
exec bunx alchemy "$action" --stage prod "$@"
