#!/usr/bin/env bash
# Print post-selfhost CLI install hints (fork vs upstream).
set -euo pipefail

UPSTREAM_REPO="multica-ai/multica"
repo="${MULTICA_GITHUB_REPO:-}"
branch="${MULTICA_GITHUB_BRANCH:-main}"

if [ -z "$repo" ] && [ -f .env ]; then
  repo="$(grep -E '^MULTICA_GITHUB_REPO=' .env 2>/dev/null | head -1 | cut -d= -f2- || true)"
  branch="$(grep -E '^MULTICA_GITHUB_BRANCH=' .env 2>/dev/null | head -1 | cut -d= -f2- || true)"
  branch="${branch:-main}"
fi

if [ -z "$repo" ] && command -v git >/dev/null 2>&1; then
  url="$(git remote get-url origin 2>/dev/null || true)"
  if [[ "$url" =~ github\.com[:/]([^/]+)/([^/.]+)(\.git)?$ ]]; then
    repo="${BASH_REMATCH[1]}/${BASH_REMATCH[2]}"
  fi
fi

echo "Next — install the CLI and connect your machine:"
if [ -n "$repo" ] && [ "$repo" != "$UPSTREAM_REPO" ]; then
  echo "  MULTICA_GITHUB_REPO=$repo curl -fsSL https://raw.githubusercontent.com/$repo/$branch/scripts/install-fork.sh | bash"
  echo "  multica setup self-host"
  echo "(see FORK.md)"
else
  echo "  brew install multica-ai/tap/multica"
  echo "  multica setup self-host"
fi
