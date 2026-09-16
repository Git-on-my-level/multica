#!/usr/bin/env bash
# Regression tests for install-daemon-macos.sh. These tests exercise the
# launchd and /health boundaries with small command stubs; they never load a
# real launch agent or invoke a real daemon.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INSTALLER="$ROOT_DIR/scripts/install-daemon-macos.sh"
TEST_PYTHON="$(command -v python3)"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

assert_contains() {
  local needle=$1 file=$2
  grep -Fq -- "$needle" "$file" || fail "expected $file to contain: $needle"
}

assert_not_contains() {
  local needle=$1 file=$2
  if grep -Fq -- "$needle" "$file"; then
    fail "expected $file not to contain: $needle"
  fi
}

setup_case() {
  TEST_TMP="$(mktemp -d)"
  TEST_HOME="$TEST_TMP/home"
  TEST_STUBS="$TEST_TMP/stubs"
  TEST_LOG="$TEST_TMP/launchctl.log"
  mkdir -p "$TEST_HOME" "$TEST_STUBS"
  touch "$TEST_TMP/multica"
  touch "$TEST_TMP/multica & <worker>"
  chmod +x "$TEST_TMP/multica"
  chmod +x "$TEST_TMP/multica & <worker>"

  cat >"$TEST_STUBS/launchctl" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$MULTICA_TEST_LAUNCHCTL_LOG"
case "${1:-}" in
  print)
    if [ "${MULTICA_TEST_LAUNCHCTL_STATE:-absent}" = loaded ]; then
      printf 'pid = 4242\n'
      exit 0
    fi
    exit 1
    ;;
  bootstrap|bootout)
    exit 0
    ;;
  *)
    exit 2
    ;;
esac
STUB
  chmod +x "$TEST_STUBS/launchctl"

  cat >"$TEST_STUBS/curl" <<'STUB'
#!/usr/bin/env bash
if [ "${MULTICA_TEST_CURL_STATE:-unreachable}" != healthy ]; then
  exit 7
fi
printf '%s' "$MULTICA_TEST_HEALTH"
STUB
  chmod +x "$TEST_STUBS/curl"
}

run_installer() {
  local state=${1:-absent} curl_state=${2:-unreachable}
  if [ "$#" -ge 2 ]; then
    shift 2
  else
    set --
  fi
  set +e
  HOME="$TEST_HOME" \
    PATH="/usr/bin:/bin" \
    MULTICA_DAEMON_BIN="$TEST_TMP/multica" \
    MULTICA_LAUNCHD_LABEL="com.test.multica" \
    MULTICA_DAEMON_PROFILE="worker" \
    MULTICA_LAUNCHD_PLIST="$TEST_HOME/Library/LaunchAgents/com.test.multica.plist" \
    MULTICA_DAEMON_PATH="/opt/homebrew/bin:/Users/test/bin with spaces" \
    MULTICA_LAUNCHCTL_BIN="$TEST_STUBS/launchctl" \
    MULTICA_CURL_BIN="$TEST_STUBS/curl" \
    MULTICA_PYTHON_BIN="$TEST_PYTHON" \
    MULTICA_TEST_LAUNCHCTL_STATE="$state" \
    MULTICA_TEST_CURL_STATE="$curl_state" \
    MULTICA_TEST_HEALTH="${MULTICA_TEST_HEALTH:-}" \
    MULTICA_TEST_LAUNCHCTL_LOG="$TEST_LOG" \
    "$INSTALLER" "$@" >"$TEST_TMP/out" 2>"$TEST_TMP/err"
  TEST_STATUS=$?
  set -e
}

test_render_escapes_xml_and_keeps_arguments_separate() {
  setup_case
  HOME="$TEST_HOME" PATH="/usr/bin:/bin" "$INSTALLER" \
    --render \
    --binary "$TEST_TMP/multica & <worker>" \
    --profile 'worker/profile & one' \
    --label com.test.render \
    --path '/opt/homebrew/bin:/Users/test/bin & one' \
    --no-auto-update >"$TEST_TMP/rendered"

  assert_contains '<string>/var/' "$TEST_TMP/rendered"
  assert_contains '&amp; &lt;worker&gt;' "$TEST_TMP/rendered"
  assert_contains 'ProcessType</key>' "$TEST_TMP/rendered"
  assert_contains '<string>Interactive</string>' "$TEST_TMP/rendered"
  assert_contains '<string>--no-auto-update</string>' "$TEST_TMP/rendered"
  assert_contains '<string>worker/profile &amp; one</string>' "$TEST_TMP/rendered"
  assert_contains '/opt/homebrew/bin:/Users/test/bin &amp; one' "$TEST_TMP/rendered"
  assert_not_contains 'MULTICA_TOKEN' "$TEST_TMP/rendered"
}

test_default_action_only_renders() {
  setup_case
  run_installer
  [ "$TEST_STATUS" -eq 0 ] || fail "default render failed: $(cat "$TEST_TMP/err")"
  assert_contains '<plist version="1.0">' "$TEST_TMP/out"
  [ ! -f "$TEST_HOME/Library/LaunchAgents/com.test.multica.plist" ] || fail "default action installed a plist"
  [ ! -s "$TEST_LOG" ] || fail "default action contacted launchctl"
}

test_activate_new_service_bootstraps_without_health_endpoint() {
  setup_case
  run_installer absent unreachable --activate
  [ "$TEST_STATUS" -eq 0 ] || fail "fresh activation failed: $(cat "$TEST_TMP/err")"
  [ -f "$TEST_HOME/Library/LaunchAgents/com.test.multica.plist" ] || fail "plist was not installed"
  assert_contains "bootstrap gui/$(id -u) $TEST_HOME/Library/LaunchAgents/com.test.multica.plist" "$TEST_LOG"
  assert_not_contains 'bootout' "$TEST_LOG"
}

test_busy_service_refuses_reload() {
  setup_case
  MULTICA_TEST_HEALTH='{"status":"running","pid":4242,"profile":"worker","active_task_count":1,"running_task_count":1,"resource_wait_task_count":0}' run_installer loaded healthy --activate
  [ "$TEST_STATUS" -ne 0 ] || fail "busy activation unexpectedly succeeded"
  assert_contains 'active=1 running=1 resource_wait=0' "$TEST_TMP/err"
  assert_not_contains 'bootout' "$TEST_LOG"
  assert_not_contains 'bootstrap' "$TEST_LOG"
}

test_loaded_service_with_unknown_health_refuses_reload() {
  setup_case
  run_installer loaded unreachable --activate
  [ "$TEST_STATUS" -ne 0 ] || fail "unknown-health activation unexpectedly succeeded"
  assert_contains 'is unreachable' "$TEST_TMP/err"
  assert_not_contains 'bootout' "$TEST_LOG"
  assert_not_contains 'bootstrap' "$TEST_LOG"
}

test_loaded_idle_service_reloads() {
  setup_case
  MULTICA_TEST_HEALTH='{"status":"running","pid":4242,"profile":"worker","active_task_count":0,"running_task_count":0,"resource_wait_task_count":0}' run_installer loaded healthy --activate
  [ "$TEST_STATUS" -eq 0 ] || fail "idle reload failed: $(cat "$TEST_TMP/err")"
  assert_contains "bootout gui/$(id -u)/com.test.multica" "$TEST_LOG"
  assert_contains "bootstrap gui/$(id -u) $TEST_HOME/Library/LaunchAgents/com.test.multica.plist" "$TEST_LOG"
}

test_loaded_same_configuration_is_idempotent() {
  setup_case
  run_installer absent unreachable --activate
  [ "$TEST_STATUS" -eq 0 ] || fail "initial activation failed: $(cat "$TEST_TMP/err")"
  MULTICA_TEST_HEALTH='{"status":"running","pid":4242,"profile":"worker","active_task_count":0,"running_task_count":0,"resource_wait_task_count":0}' run_installer loaded healthy --activate
  [ "$TEST_STATUS" -eq 0 ] || fail "idempotent activation failed: $(cat "$TEST_TMP/err")"
  [ "$(grep -c '^bootstrap ' "$TEST_LOG")" -eq 1 ] || fail "same configuration was bootstrapped again"
  assert_not_contains 'bootout' "$TEST_LOG"
}

test_foreign_health_endpoint_refuses_when_label_not_loaded() {
  setup_case
  MULTICA_TEST_HEALTH='{"status":"running","pid":4242,"profile":"worker","active_task_count":0,"running_task_count":0,"resource_wait_task_count":0}' run_installer absent healthy --activate
  [ "$TEST_STATUS" -ne 0 ] || fail "foreign endpoint activation unexpectedly succeeded"
  assert_contains 'already served while' "$TEST_TMP/err"
  assert_not_contains 'bootstrap' "$TEST_LOG"
}

test_malformed_health_fields_fail_closed() {
  local health
  for health in \
    '{"status":"starting","pid":4242,"profile":"worker","active_task_count":0,"running_task_count":0,"resource_wait_task_count":0}' \
    '{"status":"running","profile":"worker","active_task_count":0,"running_task_count":0,"resource_wait_task_count":0}' \
    '{"status":"running","pid":4243,"profile":"worker","active_task_count":0,"running_task_count":0,"resource_wait_task_count":0}' \
    '{"status":"running","pid":4242,"profile":"other","active_task_count":0,"running_task_count":0,"resource_wait_task_count":0}' \
    '{"status":"running","pid":4242,"profile":"worker","active_task_count":"0","running_task_count":0,"resource_wait_task_count":0}' \
    '{"status":"running","pid":4242,"profile":"worker","active_task_count":-1,"running_task_count":0,"resource_wait_task_count":0}' \
    '{"status":"running","pid":4242,"profile":"worker","active_task_count":0.5,"running_task_count":0,"resource_wait_task_count":0}' \
    '{"status":"running","pid":4242,"profile":"worker","active_task_count":false,"running_task_count":0,"resource_wait_task_count":0}'
  do
    setup_case
    MULTICA_TEST_HEALTH="$health" run_installer loaded healthy --activate
    [ "$TEST_STATUS" -ne 0 ] || fail "malformed health unexpectedly succeeded: $health"
    assert_contains 'unsafe' "$TEST_TMP/err"
    assert_not_contains 'bootout' "$TEST_LOG"
  done
}

test_deactivate_refuses_busy_service() {
  setup_case
  MULTICA_TEST_HEALTH='{"status":"running","pid":4242,"profile":"worker","active_task_count":0,"running_task_count":1,"resource_wait_task_count":0}' run_installer loaded healthy --deactivate
  [ "$TEST_STATUS" -ne 0 ] || fail "busy deactivation unexpectedly succeeded"
  assert_contains 'running=1' "$TEST_TMP/err"
  assert_not_contains 'bootout' "$TEST_LOG"
}

test_deactivate_refuses_foreign_health_service() {
  setup_case
  MULTICA_TEST_HEALTH='{"status":"running","pid":4242,"profile":"worker","active_task_count":0,"running_task_count":0,"resource_wait_task_count":0}' run_installer absent healthy --deactivate
  [ "$TEST_STATUS" -ne 0 ] || fail "foreign endpoint deactivation unexpectedly succeeded"
  assert_contains 'already served while' "$TEST_TMP/err"
  assert_not_contains 'bootout' "$TEST_LOG"
}

test_render_escapes_xml_and_keeps_arguments_separate
test_default_action_only_renders
test_activate_new_service_bootstraps_without_health_endpoint
test_busy_service_refuses_reload
test_loaded_service_with_unknown_health_refuses_reload
test_loaded_idle_service_reloads
test_loaded_same_configuration_is_idempotent
test_foreign_health_endpoint_refuses_when_label_not_loaded
test_malformed_health_fields_fail_closed
test_deactivate_refuses_busy_service
test_deactivate_refuses_foreign_health_service
printf 'install-daemon-macos tests passed\n'
