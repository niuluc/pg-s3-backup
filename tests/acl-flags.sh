#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# No normal command may silently disable archived ACLs.
if grep -n -- '--no-acl' "$root/pg-s3-backup" "$root/app/jobs.go"; then
  echo 'normal shell or app commands must not discard ACLs' >&2
  exit 1
fi

shell_function() {
  awk -v target="$1" '
    $0 ~ "^" target "\\(\\) \\{" { in_function=1 }
    in_function && /^cmd_[[:alnum:]_]+\(\) \{/ && $0 !~ "^" target "\\(\\) \\{" { exit }
    in_function { print }
  ' "$root/pg-s3-backup"
}
go_function() {
  awk -v target="$1" '
    $0 ~ "^func \\(j \\*job\\) " target "\\(" { in_function=1 }
    in_function && /^func / && $0 !~ "^func \\(j \\*job\\) " target "\\(" { exit }
    in_function { print }
  ' "$root/app/jobs.go"
}
assert_contains() {
  local label=$1 expected=$2 file=$3
  if ! grep -Fq -- "$expected" "$file"; then
    echo "$label is missing required arguments: $expected" >&2
    exit 1
  fi
}

shell_function cmd_backup >"$tmp/shell-backup"
shell_function cmd_restore >"$tmp/shell-restore"
shell_function cmd_verify >"$tmp/shell-verify"
assert_contains 'shell backup' '-Fc --no-owner -f' "$tmp/shell-backup"
assert_contains 'shell restore' '--no-owner --exit-on-error --clean --if-exists' "$tmp/shell-restore"
assert_contains 'shell verify' '--no-owner --exit-on-error' "$tmp/shell-verify"

go_function dumpAndUpload >"$tmp/go-backup"
go_function restoreCopy >"$tmp/go-restore-copy"
go_function restoreReplace >"$tmp/go-restore-replace"
go_function verify >"$tmp/go-verify"
assert_contains 'Go backup' '[]string{"-Fc", "--no-owner", "-f", tmp}' "$tmp/go-backup"
assert_contains 'Go restore-copy' '[]string{"--no-owner", "--exit-on-error"}' "$tmp/go-restore-copy"
assert_contains 'Go restore-replace' '[]string{pgRestore, "--no-owner", "--exit-on-error", "-f", "-", tmp}' "$tmp/go-restore-replace"
assert_contains 'Go verify' '[]string{"--no-owner", "--exit-on-error"}' "$tmp/go-verify"

echo 'ACL command regression checks passed'
