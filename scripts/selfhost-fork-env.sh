#!/usr/bin/env bash
# Patch .env for fork self-host and emit export lines for the current shell.
#
# Used by `make selfhost` / `make selfhost-build` so fork detection stays out of
# the shared Makefile target (easier upstream merges). Safe to run on upstream
# clones — no-ops when the resolved repo is multica-ai/multica.
#
# Usage (from repo root):
#   eval "$(./scripts/selfhost-fork-env.sh)"
#
set -euo pipefail

UPSTREAM_REPO="multica-ai/multica"
ENV_FILE="${MULTICA_ENV_FILE:-.env}"

derive_repo_from_git_remote() {
  command -v git >/dev/null 2>&1 || return 1
  local url
  url="$(git remote get-url origin 2>/dev/null || true)"
  [ -n "$url" ] || return 1
  if [[ "$url" =~ github\.com[:/]([^/]+)/([^/.]+)(\.git)?$ ]]; then
    printf '%s/%s' "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}"
    return 0
  fi
  return 1
}

set_env_file_value() {
  local file="$1"
  local key="$2"
  local value="$3"
  local line="${key}=${value}"

  if [ ! -f "$file" ]; then
    printf '%s\n' "$line" >"$file"
    return 0
  fi

  if grep -qE "^${key}=" "$file" 2>/dev/null; then
    if [ "$(uname -s)" = "Darwin" ]; then
      sed -i '' "s|^${key}=.*|${line}|" "$file"
    else
      sed -i "s|^${key}=.*|${line}|" "$file"
    fi
    return 0
  fi

  if grep -qE "^# ${key}=" "$file" 2>/dev/null; then
    if [ "$(uname -s)" = "Darwin" ]; then
      sed -i '' "s|^# ${key}=.*|${line}|" "$file"
    else
      sed -i "s|^# ${key}=.*|${line}|" "$file"
    fi
    return 0
  fi

  printf '%s\n' "$line" >>"$file"
}

repo="${MULTICA_GITHUB_REPO:-}"
if [ -z "$repo" ]; then
  repo="$(derive_repo_from_git_remote || true)"
fi

if [ -z "$repo" ] || [ "$repo" = "$UPSTREAM_REPO" ]; then
  # Still emit current values so callers can source safely.
  if [ -n "${MULTICA_GITHUB_REPO:-}" ]; then
    printf "export MULTICA_GITHUB_REPO=%q\n" "$MULTICA_GITHUB_REPO"
  fi
  exit 0
fi

owner_lower="$(printf '%s' "${repo%%/*}" | tr '[:upper:]' '[:lower:]')"
backend_default="ghcr.io/${owner_lower}/multica-backend"
web_default="ghcr.io/${owner_lower}/multica-web"
cur_backend="${MULTICA_BACKEND_IMAGE:-ghcr.io/multica-ai/multica-backend}"
cur_web="${MULTICA_WEB_IMAGE:-ghcr.io/multica-ai/multica-web}"

set_env_file_value "$ENV_FILE" "MULTICA_GITHUB_REPO" "$repo"
printf "export MULTICA_GITHUB_REPO=%q\n" "$repo"
echo "==> Fork detected ($repo); wrote MULTICA_GITHUB_REPO to $ENV_FILE" >&2

if [ "$cur_backend" = "ghcr.io/multica-ai/multica-backend" ]; then
  set_env_file_value "$ENV_FILE" "MULTICA_BACKEND_IMAGE" "$backend_default"
  printf "export MULTICA_BACKEND_IMAGE=%q\n" "$backend_default"
  echo "==> Using $backend_default" >&2
fi

if [ "$cur_web" = "ghcr.io/multica-ai/multica-web" ]; then
  set_env_file_value "$ENV_FILE" "MULTICA_WEB_IMAGE" "$web_default"
  printf "export MULTICA_WEB_IMAGE=%q\n" "$web_default"
  echo "==> Using $web_default" >&2
fi
