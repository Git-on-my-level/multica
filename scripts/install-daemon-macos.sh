#!/usr/bin/env bash
# Install and activate a Multica CLI daemon as a per-user macOS launch agent.
#
# This script owns only the launchd boundary. The Multica daemon owns task
# execution, task recovery, and its local /health endpoint. In particular, the
# script never kills an agent process or tries to infer task state from PIDs.
#
# The default action is --render. Activation is an explicit mutation so a
# copied or reviewed command cannot unexpectedly reload a worker.
set -euo pipefail

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

info() {
  printf '%s\n' "$*"
}

usage() {
  cat <<'EOF'
Install or activate a Multica daemon as a per-user macOS launch agent.

Usage:
  install-daemon-macos.sh [options]

Actions:
  --activate       Write the plist and load/reload it through launchctl.
                   A running daemon must be idle.
  --render         Print the generated plist and do not touch launchd.
                   (default)
  --deactivate     Unload the launch agent. The daemon must be idle.

Options:
  --binary PATH    Absolute path to the multica executable
  --profile NAME   Multica CLI profile (default: default profile)
  --label NAME     launchd label (default: com.multica.daemon)
  --plist PATH     Plist destination (default: ~/Library/LaunchAgents/<label>.plist)
  --path PATH      PATH exported to launchd (default: current PATH)
  --no-auto-update Pass --no-auto-update to the daemon
  -h, --help       Show this help

The generated launch agent uses ProcessType=Interactive. This is intended for
dedicated worker Macs where prompt task pickup matters more than background
power throttling. It does not grant permissions and it does not manage the
machine's other services.
EOF
}

require_darwin() {
  [ "$(uname -s)" = "Darwin" ] || die "this installer supports macOS only"
}

require_absolute() {
  case "$1" in
    /*) ;;
    *) die "$2 must be an absolute path: $1" ;;
  esac
}

# XML text escaping is kept here instead of using a shell quoting convention:
# launchd reads plist XML, not a shell command line. The ProgramArguments are
# emitted as separate <string> entries, so spaces and shell metacharacters in a
# path remain data.
xml_escape() {
  local value=$1
  value=${value//&/&amp;}
  value=${value//</&lt;}
  value=${value//>/&gt;}
  value=${value//\"/&quot;}
  value=${value//\'/&apos;}
  printf '%s' "$value"
}

xml_string() {
  printf '    <string>%s</string>\n' "$(xml_escape "$1")"
}

xml_key_string() {
  printf '    <key>%s</key>\n    <string>%s</string>\n' "$(xml_escape "$1")" "$(xml_escape "$2")"
}

xml_top_key_string() {
  printf '  <key>%s</key>\n  <string>%s</string>\n' "$(xml_escape "$1")" "$(xml_escape "$2")"
}

# Keep this calculation in lockstep with server/cmd/multica/cmd_daemon.go's
# healthPortForProfile. `od` is part of macOS and avoids requiring jq/python.
profile_health_port() {
  local profile=$1
  if [ -z "$profile" ]; then
    printf '19514'
    return
  fi

  local byte_sum
  byte_sum="$(printf '%s' "$profile" | LC_ALL=C od -An -tu1 | awk '{ for (i = 1; i <= NF; i++) total += $i } END { print total + 0 }')"
  printf '%s' "$((19514 + 1 + byte_sum % 1000))"
}

health_is_idle() {
  local body=$1 port=$2 profile=$3 launchctl_pid=$4 result

  # Do not use grep/sed to interpret JSON. Health is a safety boundary: a
  # malformed, negative, fractional, boolean, or missing count must never be
  # collapsed into zero. Python 3 is a deliberate activation prerequisite on
  # macOS so this check uses a real JSON parser with strict integer semantics.
  result="$(printf '%s' "$body" | "$PYTHON_BIN" -c '
import json
import sys

expected_profile = sys.argv[1]
expected_pid = int(sys.argv[2])

try:
    payload = json.load(sys.stdin)
    if not isinstance(payload, dict):
        raise ValueError("health response is not a JSON object")
    if payload.get("status") != "running":
        raise ValueError("health status is not running")
    if "profile" not in payload or type(payload["profile"]) is not str:
        raise ValueError("health response has no readable profile identity")
    if payload["profile"] != expected_profile:
        got = payload["profile"] or "<default>"
        want = expected_profile or "<default>"
        raise ValueError(f"health profile {got!r} does not match {want!r}")
    if "pid" not in payload or type(payload["pid"]) is not int or payload["pid"] <= 0:
        raise ValueError("health response has no valid PID")
    actual_pid = payload["pid"]
    if actual_pid != expected_pid:
        raise ValueError(f"health PID {actual_pid} does not match launchd PID {expected_pid}")

    counts = {}
    for name in ("active_task_count", "running_task_count", "resource_wait_task_count"):
        value = payload.get(name)
        if type(value) is not int or value < 0:
            raise ValueError(f"health field {name} is not a non-negative integer")
        counts[name] = value
    if any(counts.values()):
        raise ValueError(
            "daemon is busy: active={active_task_count} running={running_task_count} "
            "resource_wait={resource_wait_task_count}".format(**counts)
        )
except (ValueError, TypeError, json.JSONDecodeError) as error:
    print(f"{error}")
    raise SystemExit(1)
print("idle")
' "$profile" "$launchctl_pid")" || die "daemon health on port $port is unsafe: $result"
  [ "$result" = idle ] || die "daemon health on port $port is unsafe: $result"
}

launchctl_loaded() {
  LAUNCHCTL_INFO=''
  if ! LAUNCHCTL_INFO="$("$LAUNCHCTL_BIN" print "$LAUNCH_DOMAIN/$LABEL" 2>/dev/null)"; then
    return 1
  fi
  LAUNCHCTL_PID="$(printf '%s\n' "$LAUNCHCTL_INFO" | sed -n 's/^[[:space:]]*pid = \([0-9][0-9]*\).*$/\1/p')"
  return 0
}

# Return one of: absent, healthy, unknown. `curl` failure is unknown because
# the endpoint may be between restarts. Callers combine this with launchctl's
# service state; an already-loaded service with unknown health always fails.
read_health() {
  local body
  HEALTH_BODY=''
  if ! body="$("$CURL_BIN" --silent --show-error --fail --max-time 2 --max-filesize 65536 "http://127.0.0.1:$HEALTH_PORT/health" 2>/dev/null)"; then
    return 1
  fi
  HEALTH_BODY=$body
  return 0
}

check_idle_before_change() {
  local loaded=$1
  if read_health; then
    if [ "$loaded" != true ]; then
      # A response on the daemon's port while our label is not loaded is
      # evidence of a foreign/manual daemon. Leave it alone and fail closed.
      die "health port $HEALTH_PORT is already served while $LAUNCH_DOMAIN/$LABEL is not loaded; refusing to manage an unknown daemon"
    fi
    case "$LAUNCHCTL_PID" in
      ''|*[!0-9]*|0) die "launchd service $LAUNCH_DOMAIN/$LABEL has no unambiguous running PID; refusing lifecycle change" ;;
    esac
    health_is_idle "$HEALTH_BODY" "$HEALTH_PORT" "$PROFILE" "$LAUNCHCTL_PID"
    return
  fi

  if [ "$loaded" = true ]; then
    die "launchd service $LAUNCH_DOMAIN/$LABEL is loaded but /health on port $HEALTH_PORT is unreachable; refusing to stop or restart an unknown daemon"
  fi
}

render_plist() {
  local out=$1
  {
    printf '<?xml version="1.0" encoding="UTF-8"?>\n'
    printf '<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">\n'
    printf '<plist version="1.0">\n<dict>\n'
    printf '  <key>Label</key>\n  <string>%s</string>\n' "$(xml_escape "$LABEL")"
    printf '  <key>ProgramArguments</key>\n  <array>\n'
    xml_string "$BINARY"
    xml_string daemon
    xml_string start
    xml_string --foreground
    if [ -n "$PROFILE" ]; then
      xml_string --profile
      xml_string "$PROFILE"
    fi
    if [ "$NO_AUTO_UPDATE" = true ]; then
      xml_string --no-auto-update
    fi
    printf '  </array>\n'
    printf '  <key>ProcessType</key>\n  <string>Interactive</string>\n'
    printf '  <key>RunAtLoad</key>\n  <true/>\n'
    printf '  <key>KeepAlive</key>\n  <true/>\n'
    printf '  <key>EnvironmentVariables</key>\n  <dict>\n'
    xml_key_string HOME "$HOME"
    xml_key_string PATH "$DAEMON_PATH"
    printf '  </dict>\n'
    xml_top_key_string StandardOutPath "$STDOUT_PATH"
    xml_top_key_string StandardErrorPath "$STDERR_PATH"
    printf '</dict>\n</plist>\n'
  } >"$out"
}

activate() {
  local loaded=$1 temp_plist=$2

  check_idle_before_change "$loaded"
  mkdir -p "$(dirname "$PLIST_PATH")"
  if [ "$loaded" = true ] && [ -f "$PLIST_PATH" ] && cmp -s "$temp_plist" "$PLIST_PATH"; then
    info "launchd service $LAUNCH_DOMAIN/$LABEL plist is unchanged; leaving the existing loaded process running"
    return
  fi

  if [ "$loaded" = true ]; then
    # Refresh launchd's PID and the daemon's health immediately before the
    # destructive half of a reload. A daemon can claim work between the first
    # preflight and this point; there is no atomic drain primitive at this
    # boundary, so a changed state aborts before bootout.
    if ! launchctl_loaded; then
      die "launchd service $LAUNCH_DOMAIN/$LABEL changed state during activation; refusing to boot it out"
    fi
    check_idle_before_change true
    # Keep the currently loaded plist intact until the final health guard
    # passes. If the daemon claims work during the second check, a retry must
    # still compare against the plist that is actually installed.
    mv "$temp_plist" "$PLIST_PATH"
    "$LAUNCHCTL_BIN" bootout "$LAUNCH_DOMAIN/$LABEL"
  else
    mv "$temp_plist" "$PLIST_PATH"
  fi
  "$LAUNCHCTL_BIN" bootstrap "$LAUNCH_DOMAIN" "$PLIST_PATH"
  info "activated $LAUNCH_DOMAIN/$LABEL from $PLIST_PATH"
}

deactivate() {
  local loaded=$1
  if [ "$loaded" != true ]; then
    # Do not mistake a manually started daemon for an absent launch agent.
    # An unreachable endpoint is safe here because launchctl already proved
    # that this label is not loaded; a responding endpoint is treated as an
    # unknown service and fails closed in check_idle_before_change.
    check_idle_before_change false
    info "launchd service $LAUNCH_DOMAIN/$LABEL is not loaded"
    return
  fi
  check_idle_before_change true
  if ! launchctl_loaded; then
    info "launchd service $LAUNCH_DOMAIN/$LABEL exited while it was being deactivated"
    return
  fi
  check_idle_before_change true
  "$LAUNCHCTL_BIN" bootout "$LAUNCH_DOMAIN/$LABEL"
  info "deactivated $LAUNCH_DOMAIN/$LABEL (plist retained at $PLIST_PATH)"
}

require_darwin

ACTION=render
BINARY="${MULTICA_DAEMON_BIN:-}"
PROFILE="${MULTICA_DAEMON_PROFILE:-}"
LABEL="${MULTICA_LAUNCHD_LABEL:-com.multica.daemon}"
DAEMON_PATH="${MULTICA_DAEMON_PATH:-${PATH:-/usr/bin:/bin:/usr/sbin:/sbin}}"
PLIST_PATH="${MULTICA_LAUNCHD_PLIST:-}"
NO_AUTO_UPDATE=false
LAUNCHCTL_BIN="${MULTICA_LAUNCHCTL_BIN:-launchctl}"
CURL_BIN="${MULTICA_CURL_BIN:-curl}"
PYTHON_BIN="${MULTICA_PYTHON_BIN:-python3}"

while [ "$#" -gt 0 ]; do
  case "$1" in
    --activate) ACTION=activate ;;
    --render) ACTION=render ;;
    --deactivate) ACTION=deactivate ;;
    --binary)
      [ "$#" -ge 2 ] || die "--binary requires a value"
      BINARY=$2
      shift
      ;;
    --profile)
      [ "$#" -ge 2 ] || die "--profile requires a value"
      PROFILE=$2
      shift
      ;;
    --label)
      [ "$#" -ge 2 ] || die "--label requires a value"
      LABEL=$2
      shift
      ;;
    --plist)
      [ "$#" -ge 2 ] || die "--plist requires a value"
      PLIST_PATH=$2
      shift
      ;;
    --path)
      [ "$#" -ge 2 ] || die "--path requires a value"
      DAEMON_PATH=$2
      shift
      ;;
    --no-auto-update) NO_AUTO_UPDATE=true ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown option: $1 (use --help)" ;;
  esac
  shift
done

[ -n "$HOME" ] || die "HOME is required"
require_absolute "$HOME" HOME

if [ -z "$BINARY" ]; then
  BINARY="$(command -v multica || true)"
fi
[ -n "$BINARY" ] || die "multica executable not found; pass --binary /absolute/path/to/multica"
require_absolute "$BINARY" binary
[ -x "$BINARY" ] || die "binary is not executable: $BINARY"

case "$LABEL" in
  ''|*[!A-Za-z0-9._-]*) die "label may contain only letters, numbers, dot, underscore, and hyphen: $LABEL" ;;
esac

HEALTH_PORT="$(profile_health_port "$PROFILE")"

if [ -z "$PLIST_PATH" ]; then
  PLIST_PATH="$HOME/Library/LaunchAgents/$LABEL.plist"
fi
require_absolute "$PLIST_PATH" plist

LAUNCH_DOMAIN="gui/$(id -u)"
STDOUT_PATH="$HOME/.multica/launchd-$LABEL.stdout.log"
STDERR_PATH="$HOME/.multica/launchd-$LABEL.stderr.log"

if [ "$ACTION" = render ]; then
  TEMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/multica-launchd.XXXXXX")"
  trap 'rm -rf "$TEMP_DIR"' EXIT
  render_plist "$TEMP_DIR/daemon.plist"
  cat "$TEMP_DIR/daemon.plist"
  exit 0
fi

command -v "$LAUNCHCTL_BIN" >/dev/null 2>&1 || die "launchctl not found"
command -v "$CURL_BIN" >/dev/null 2>&1 || die "curl not found"
command -v "$PYTHON_BIN" >/dev/null 2>&1 || die "python3 is required for a fail-closed health check"

LOADED=false
if launchctl_loaded; then
  LOADED=true
fi

if [ "$ACTION" = deactivate ]; then
  deactivate "$LOADED"
  exit 0
fi

TEMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/multica-launchd.XXXXXX")"
trap 'rm -rf "$TEMP_DIR"' EXIT
render_plist "$TEMP_DIR/daemon.plist"
activate "$LOADED" "$TEMP_DIR/daemon.plist"
