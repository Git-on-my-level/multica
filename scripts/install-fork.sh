#!/usr/bin/env bash
# Fork install entry point — sets fork defaults and delegates to install.sh.
export MULTICA_GITHUB_REPO="${MULTICA_GITHUB_REPO:-Git-on-my-level/multica}"
export MULTICA_SKIP_BREW=1
export MULTICA_CLI_REF="${MULTICA_CLI_REF:-main}"
export MULTICA_GITHUB_BRANCH="${MULTICA_GITHUB_BRANCH:-main}"

repo="${MULTICA_GITHUB_REPO}"
branch="${MULTICA_GITHUB_BRANCH}"

# Local checkout: install.sh sits beside this wrapper.
if [ -n "${BASH_SOURCE[0]:-}" ]; then
  _dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  if [ -f "${_dir}/install.sh" ]; then
    exec bash "${_dir}/install.sh" "$@"
  fi
fi

# curl .../install-fork.sh | bash — fetch install.sh from the fork repo.
exec bash <(curl -fsSL "https://raw.githubusercontent.com/${repo}/${branch}/scripts/install.sh") "$@"
