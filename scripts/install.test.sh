#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Build a self-contained sandbox with stub `curl` and a tarball that the
# release-binary fallback path will download. Each test supplies its own
# `brew` stub to model a specific Homebrew failure mode.
_setup_sandbox() {
  local tmp="$1"
  local stub_bin="$tmp/stub-bin"
  local install_bin="$tmp/install-bin"
  local payload_dir="$tmp/payload"
  mkdir -p "$stub_bin" "$install_bin" "$payload_dir"

  cat >"$payload_dir/multica" <<'STUB'
#!/usr/bin/env bash
echo "multica v0.3.2 (commit: test)"
STUB
  chmod +x "$payload_dir/multica"
  tar -czf "$tmp/multica.tar.gz" -C "$payload_dir" multica

  cat >"$stub_bin/curl" <<'STUB'
#!/usr/bin/env bash
if [[ "$*" == *"-sI"* ]]; then
  printf 'HTTP/2 302\r\nlocation: https://github.com/multica-ai/multica/releases/tag/v0.3.2\r\n'
  exit 0
fi

out=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    -o)
      out="$2"
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done

if [[ -z "$out" ]]; then
  echo "stub curl expected -o" >&2
  exit 2
fi
cp "$MULTICA_TEST_ARCHIVE" "$out"
STUB
  chmod +x "$stub_bin/curl"
}

_run_installer() {
  local tmp="$1"
  shift
  local out="$tmp/install.out"
  local err="$tmp/install.err"
  if ! PATH="$tmp/stub-bin:$tmp/install-bin:/usr/bin:/bin" \
    MULTICA_BIN_DIR="$tmp/install-bin" \
    MULTICA_TEST_ARCHIVE="$tmp/multica.tar.gz" \
    "$@" \
    bash "$ROOT_DIR/scripts/install.sh" >"$out" 2>"$err"; then
    echo "install.sh exited non-zero" >&2
    cat "$out" >&2 || true
    cat "$err" >&2 || true
    return 1
  fi

  if [[ ! -x "$tmp/install-bin/multica" ]]; then
    echo "expected fallback binary at $tmp/install-bin/multica" >&2
    cat "$out" >&2 || true
    cat "$err" >&2 || true
    return 1
  fi
}

_run_installer_expect_brew_diag() {
  local tmp="$1"
  shift
  local err="$tmp/install.err"
  _run_installer "$tmp" "$@"
  if ! grep -q "Homebrew output (last 80 lines):" "$err"; then
    echo "expected diagnostic tail in stderr" >&2
    cat "$err" >&2 || true
    return 1
  fi
}

test_brew_install_failure_falls_back_to_release_binary() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  _setup_sandbox "$tmp"
  cat >"$tmp/stub-bin/brew" <<'STUB'
#!/usr/bin/env bash
case "${1:-}" in
  tap)
    exit 0
    ;;
  install)
    echo "simulated brew install failure" >&2
    exit 42
    ;;
  list)
    exit 1
    ;;
  *)
    exit 0
    ;;
esac
STUB
  chmod +x "$tmp/stub-bin/brew"

  _run_installer_expect_brew_diag "$tmp"
}

test_brew_tap_failure_falls_back_to_release_binary() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  _setup_sandbox "$tmp"
  cat >"$tmp/stub-bin/brew" <<'STUB'
#!/usr/bin/env bash
case "${1:-}" in
  tap)
    echo "simulated brew tap failure" >&2
    exit 17
    ;;
  *)
    echo "brew $* should not be reached after tap failure" >&2
    exit 99
    ;;
esac
STUB
  chmod +x "$tmp/stub-bin/brew"

  _run_installer_expect_brew_diag "$tmp"
}

_run_installer_skip_brew() {
  local tmp="$1"
  local out="$tmp/install.out"
  local err="$tmp/install.err"
  if ! PATH="$tmp/stub-bin:$tmp/install-bin:/usr/bin:/bin" \
    MULTICA_BIN_DIR="$tmp/install-bin" \
    MULTICA_TEST_ARCHIVE="$tmp/multica.tar.gz" \
    MULTICA_SKIP_BREW=1 \
    bash "$ROOT_DIR/scripts/install.sh" >"$out" 2>"$err"; then
    echo "install.sh exited non-zero" >&2
    cat "$out" >&2 || true
    cat "$err" >&2 || true
    return 1
  fi

  if [[ ! -x "$tmp/install-bin/multica" ]]; then
    echo "expected binary at $tmp/install-bin/multica" >&2
    cat "$out" >&2 || true
    cat "$err" >&2 || true
    return 1
  fi
}

test_skip_brew_uses_release_binary() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  _setup_sandbox "$tmp"
  # No brew stub — skip-brew should go straight to release binary.
  _run_installer_skip_brew "$tmp"
}

test_fork_repo_skips_homebrew() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  _setup_sandbox "$tmp"
  cat >"$tmp/stub-bin/curl" <<'STUB'
#!/usr/bin/env bash
if [[ "$*" == *"-sI"* ]]; then
  printf 'HTTP/2 302\r\nlocation: https://github.com/Git-on-my-level/multica/releases/tag/v0.3.2\r\n'
  exit 0
fi
out=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    *) shift ;;
  esac
done
cp "$MULTICA_TEST_ARCHIVE" "$out"
STUB
  chmod +x "$tmp/stub-bin/curl"

  cat >"$tmp/stub-bin/brew" <<'STUB'
#!/usr/bin/env bash
echo "brew should not be called for fork repos" >&2
exit 99
STUB
  chmod +x "$tmp/stub-bin/brew"

  _run_installer "$tmp" env MULTICA_GITHUB_REPO=Git-on-my-level/multica
  if grep -q "brew should not be called" "$tmp/install.err"; then
    echo "brew was invoked for fork repo" >&2
    return 1
  fi
}

_setup_source_build_sandbox() {
  local tmp="$1"
  local stub_bin="$tmp/stub-bin"
  local install_bin="$tmp/install-bin"
  local repo_dir="$tmp/fake-repo"
  mkdir -p "$stub_bin" "$install_bin" "$repo_dir/server/cmd/multica"

  cat >"$repo_dir/server/cmd/multica/main.go" <<'GO'
package main
import "fmt"
func main() { fmt.Println("multica v0.3.2 (commit: test)") }
GO

  cat >"$stub_bin/curl" <<'STUB'
#!/usr/bin/env bash
if [[ "$*" == *"-sI"* ]]; then
  printf 'HTTP/2 302\r\nlocation: https://github.com/Git-on-my-level/multica/releases/tag/v0.3.2\r\n'
  exit 0
fi
exit 22
STUB
  chmod +x "$stub_bin/curl"

  cat >"$stub_bin/git" <<STUB
#!/usr/bin/env bash
case "\${1:-}" in
  clone)
    dest="\${@: -1}"
    mkdir -p "\$dest"
    cp -R "$tmp/fake-repo/." "\$dest/"
    exit 0
    ;;
  fetch|checkout)
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
STUB
  chmod +x "$stub_bin/git"

  cat >"$stub_bin/go" <<'STUB'
#!/usr/bin/env bash
if [[ "$1" == "build" ]]; then
  out=""
  shift
  while [[ $# -gt 0 ]]; do
    case "$1" in
      -o) out="$2"; shift 2 ;;
      *) shift ;;
    esac
  done
  cat >"$out" <<'BIN'
#!/usr/bin/env bash
echo "multica v0.3.2 (commit: test)"
BIN
  chmod +x "$out"
  exit 0
fi
exit 1
STUB
  chmod +x "$stub_bin/go"
}

test_source_build_fallback_when_release_missing() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  _setup_source_build_sandbox "$tmp"

  local out="$tmp/install.out"
  local err="$tmp/install.err"
  if ! PATH="$tmp/stub-bin:$tmp/install-bin:/usr/bin:/bin" \
    MULTICA_BIN_DIR="$tmp/install-bin" \
    MULTICA_SKIP_BREW=1 \
    MULTICA_GITHUB_REPO="Git-on-my-level/multica" \
    MULTICA_CLI_REF="main" \
    bash "$ROOT_DIR/scripts/install.sh" >"$out" 2>"$err"; then
    echo "install.sh exited non-zero" >&2
    cat "$out" >&2 || true
    cat "$err" >&2 || true
    return 1
  fi

  if [[ ! -x "$tmp/install-bin/multica" ]]; then
    echo "expected source-built binary at $tmp/install-bin/multica" >&2
    cat "$out" >&2 || true
    cat "$err" >&2 || true
    return 1
  fi

  if ! grep -q "Building Multica CLI from source" "$out"; then
    echo "expected source build message in stdout" >&2
    cat "$out" >&2 || true
    return 1
  fi
}

test_tar_extract_failure_falls_back_to_source_build() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  _setup_source_build_sandbox "$tmp"

  cat >"$tmp/stub-bin/curl" <<'STUB'
#!/usr/bin/env bash
if [[ "$*" == *"-sI"* ]]; then
  printf 'HTTP/2 302\r\nlocation: https://github.com/Git-on-my-level/multica/releases/tag/v0.3.2\r\n'
  exit 0
fi
out=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    *) shift ;;
  esac
done
echo "not a valid tarball" > "$out"
STUB
  chmod +x "$tmp/stub-bin/curl"

  local out="$tmp/install.out"
  local err="$tmp/install.err"
  if ! PATH="$tmp/stub-bin:$tmp/install-bin:/usr/bin:/bin" \
    MULTICA_BIN_DIR="$tmp/install-bin" \
    MULTICA_SKIP_BREW=1 \
    MULTICA_GITHUB_REPO="Git-on-my-level/multica" \
    MULTICA_CLI_REF="main" \
    bash "$ROOT_DIR/scripts/install.sh" >"$out" 2>"$err"; then
    echo "install.sh exited non-zero" >&2
    cat "$out" >&2 || true
    cat "$err" >&2 || true
    return 1
  fi

  if [[ ! -x "$tmp/install-bin/multica" ]]; then
    echo "expected source-built binary after tar failure" >&2
    cat "$out" >&2 || true
    cat "$err" >&2 || true
    return 1
  fi

  if ! grep -q "Building Multica CLI from source" "$out"; then
    echo "expected source build fallback after tar failure" >&2
    cat "$out" >&2 || true
    return 1
  fi
}

test_install_creates_missing_bin_dir() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  _setup_sandbox "$tmp"
  rm -rf "$tmp/install-bin"

  local out="$tmp/install.out"
  local err="$tmp/install.err"
  if ! PATH="$tmp/stub-bin:$tmp/install-bin:/usr/bin:/bin" \
    MULTICA_BIN_DIR="$tmp/install-bin" \
    MULTICA_TEST_ARCHIVE="$tmp/multica.tar.gz" \
    MULTICA_SKIP_BREW=1 \
    bash "$ROOT_DIR/scripts/install.sh" >"$out" 2>"$err"; then
    echo "install.sh exited non-zero" >&2
    cat "$out" >&2 || true
    cat "$err" >&2 || true
    return 1
  fi

  if [[ ! -d "$tmp/install-bin" ]]; then
    echo "expected installer to create $tmp/install-bin" >&2
    return 1
  fi

  if [[ ! -x "$tmp/install-bin/multica" ]]; then
    echo "expected binary at $tmp/install-bin/multica" >&2
    cat "$out" >&2 || true
    cat "$err" >&2 || true
    return 1
  fi

  if ! grep -q "Multica CLI installed to $tmp/install-bin/multica" "$out"; then
    echo "expected install success message" >&2
    cat "$out" >&2 || true
    return 1
  fi
}

test_install_failure_does_not_claim_success() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  _setup_sandbox "$tmp"
  local fail_bin="$tmp/readonly-parent/bin"
  mkdir -p "$tmp/readonly-parent"
  chmod 555 "$tmp/readonly-parent"

  cat >"$tmp/stub-bin/sudo" <<'STUB'
#!/usr/bin/env bash
echo "simulated sudo install failure" >&2
exit 1
STUB
  chmod +x "$tmp/stub-bin/sudo"

  local out="$tmp/install.out"
  local err="$tmp/install.err"
  if PATH="$tmp/stub-bin:/usr/bin:/bin" \
    MULTICA_BIN_DIR="$fail_bin" \
    MULTICA_TEST_ARCHIVE="$tmp/multica.tar.gz" \
    MULTICA_SKIP_BREW=1 \
    bash "$ROOT_DIR/scripts/install.sh" >"$out" 2>"$err"; then
    echo "expected install.sh to fail when sudo install fails" >&2
    cat "$out" >&2 || true
    cat "$err" >&2 || true
    return 1
  fi

  if grep -q "Multica CLI installed to" "$out"; then
    echo "installer claimed CLI install success after failure" >&2
    cat "$out" >&2 || true
    return 1
  fi

  if grep -q "Multica CLI is ready" "$out"; then
    echo "installer claimed final success after failure" >&2
    cat "$out" >&2 || true
    return 1
  fi

  if [[ -x "$fail_bin/multica" ]]; then
    echo "binary should not exist after failed install" >&2
    return 1
  fi
}

test_brew_install_failure_falls_back_to_release_binary
test_brew_tap_failure_falls_back_to_release_binary
test_skip_brew_uses_release_binary
test_fork_repo_skips_homebrew
test_source_build_fallback_when_release_missing
test_tar_extract_failure_falls_back_to_source_build
test_install_creates_missing_bin_dir
test_install_failure_does_not_claim_success
echo "install.sh tests passed"
