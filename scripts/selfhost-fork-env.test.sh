#!/usr/bin/env bash
# Smoke tests for scripts/selfhost-fork-env.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

cp "$ROOT/scripts/selfhost-fork-env.sh" "$tmpdir/selfhost-fork-env.sh"
chmod +x "$tmpdir/selfhost-fork-env.sh"

# Upstream remote → no-op (no MULTICA_GITHUB_REPO export required).
git -C "$tmpdir" init -q
git -C "$tmpdir" remote add origin https://github.com/multica-ai/multica.git
cp "$ROOT/.env.example" "$tmpdir/.env"
(
  cd "$tmpdir"
  out="$(MULTICA_ENV_FILE=.env ./selfhost-fork-env.sh)"
  if grep -qE '^MULTICA_GITHUB_REPO=Git-on-my-level' .env 2>/dev/null; then
    echo "upstream clone should not write fork repo" >&2
    exit 1
  fi
  if [ -n "$out" ] && ! echo "$out" | grep -q 'MULTICA_GITHUB_REPO'; then
    : # ok — may be empty
  fi
)

# Fork remote → writes repo + images.
git -C "$tmpdir" remote set-url origin git@github.com:AcmeOrg/multica.git
(
  cd "$tmpdir"
  # Reset images to upstream defaults
  if grep -qE '^MULTICA_BACKEND_IMAGE=' .env; then
    sed -i.bak 's|^MULTICA_BACKEND_IMAGE=.*|MULTICA_BACKEND_IMAGE=ghcr.io/multica-ai/multica-backend|' .env
    sed -i.bak 's|^MULTICA_WEB_IMAGE=.*|MULTICA_WEB_IMAGE=ghcr.io/multica-ai/multica-web|' .env
  fi
  eval "$(MULTICA_ENV_FILE=.env ./selfhost-fork-env.sh)"
  grep -qE '^MULTICA_GITHUB_REPO=AcmeOrg/multica$' .env
  grep -qE '^MULTICA_BACKEND_IMAGE=ghcr.io/acmeorg/multica-backend$' .env
  grep -qE '^MULTICA_WEB_IMAGE=ghcr.io/acmeorg/multica-web$' .env
  [ "$MULTICA_GITHUB_REPO" = "AcmeOrg/multica" ]
)

echo "selfhost-fork-env ok"
