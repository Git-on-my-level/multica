#!/usr/bin/env bash
# Smoke tests for install-fork.sh identity resolution (env / git remote / default).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

mkdir -p "$tmpdir/repo/scripts"
cp "$ROOT/scripts/install-fork.sh" "$tmpdir/repo/scripts/install-fork.sh"
printf '%s\n' '#!/usr/bin/env bash' 'echo "install.sh called repo=${MULTICA_GITHUB_REPO}"' \
  >"$tmpdir/repo/scripts/install.sh"
chmod +x "$tmpdir/repo/scripts/install.sh" "$tmpdir/repo/scripts/install-fork.sh"

git -C "$tmpdir/repo" init -q
git -C "$tmpdir/repo" remote add origin git@github.com:ExampleOrg/my-multica.git

# Env wins over git remote and FORK_DEFAULT.
out="$(MULTICA_GITHUB_REPO=env/override bash "$tmpdir/repo/scripts/install-fork.sh" 2>/dev/null)"
expected="install.sh called repo=env/override"
if [ "$out" != "$expected" ]; then
  echo "env override failed: want '$expected' got '$out'" >&2
  exit 1
fi

# Git remote when env unset (and FORK_DEFAULT would otherwise apply — remote wins first).
out="$(unset MULTICA_GITHUB_REPO; bash "$tmpdir/repo/scripts/install-fork.sh" 2>/dev/null)"
expected="install.sh called repo=ExampleOrg/my-multica"
if [ "$out" != "$expected" ]; then
  echo "git remote derive failed: want '$expected' got '$out'" >&2
  exit 1
fi

# Positional owner/repo.
out="$(unset MULTICA_GITHUB_REPO; bash "$tmpdir/repo/scripts/install-fork.sh" acme/from-arg 2>/dev/null)"
expected="install.sh called repo=acme/from-arg"
if [ "$out" != "$expected" ]; then
  echo "positional arg failed: want '$expected' got '$out'" >&2
  exit 1
fi

# FORK_DEFAULT when no env, no usable remote (empty remotes).
git -C "$tmpdir/repo" remote remove origin
out="$(unset MULTICA_GITHUB_REPO; bash "$tmpdir/repo/scripts/install-fork.sh" 2>/dev/null)"
# stderr note is discarded; stdout should still show the default.
if [[ "$out" != "install.sh called repo=Git-on-my-level/multica" ]]; then
  echo "FORK_DEFAULT failed: got '$out'" >&2
  exit 1
fi

echo "install-fork identity ok"
