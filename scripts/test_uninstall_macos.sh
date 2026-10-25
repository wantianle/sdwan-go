#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck disable=SC1091 # Path is resolved dynamically from this test's directory.
source "$script_dir/uninstall.sh"

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

assert() {
    "$@" || { printf 'assertion failed: %s\n' "$*" >&2; exit 1; }
}

assert_equal() {
    [[ $1 == "$2" ]] || { printf 'expected %s, got %s\n' "$1" "$2" >&2; exit 1; }
}

make_mock_commands() {
    cat >"$tmpdir/launchctl" <<'EOF'
#!/usr/bin/env bash
set -eu
printf '%s\n' "$*" >>"$MOCK_CALL_LOG"
if [[ $1 == print ]]; then
    count=0
    [[ -f $MOCK_PRINT_COUNT ]] && count=$(<"$MOCK_PRINT_COUNT")
    count=$((count + 1))
    printf '%s\n' "$count" >"$MOCK_PRINT_COUNT"
    line='__ABSENT__'
    current=0
    while IFS= read -r candidate; do
        current=$((current + 1))
        [[ $current == "$count" ]] && { line=$candidate; break; }
    done <"$MOCK_PRINT_SEQUENCE"
    [[ $line == '__ABSENT__' ]] && exit 1
    printf '%b\n' "$line"
    exit 0
fi
if [[ $1 == bootout && $2 == system/com.minieye.sdwan ]]; then
    exit "${MOCK_LABEL_BOOTOUT_STATUS:-0}"
fi
if [[ $1 == bootout && $2 == system && $3 == "$MOCK_PLIST_PATH" ]]; then
    exit "${MOCK_PLIST_BOOTOUT_STATUS:-0}"
fi
exit 97
EOF
    cat >"$tmpdir/pid-check" <<'EOF'
#!/usr/bin/env bash
set -eu
printf '%s\n' "$*" >>"$MOCK_CALL_LOG"
count=0
[[ -f $MOCK_PID_COUNT ]] && count=$(<"$MOCK_PID_COUNT")
count=$((count + 1))
printf '%s\n' "$count" >"$MOCK_PID_COUNT"
[[ $count -le ${MOCK_PID_ALIVE_CHECKS:-0} ]]
EOF
    cat >"$tmpdir/sleep" <<'EOF'
#!/usr/bin/env bash
printf 'sleep %s\n' "$*" >>"$MOCK_CALL_LOG"
EOF
    cat >"$tmpdir/ps" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "${MOCK_PS_OUTPUT:-}"
exit "${MOCK_PS_STATUS:-0}"
EOF
    cat >"$tmpdir/rm" <<'EOF'
#!/usr/bin/env bash
printf 'rm %s\n' "$*" >>"$MOCK_CALL_LOG"
/bin/rm "$@"
EOF
    chmod +x "$tmpdir/launchctl" "$tmpdir/pid-check" "$tmpdir/sleep" "$tmpdir/ps" "$tmpdir/rm"
}

prepare_case() {
    local case_dir="$tmpdir/$1"
    mkdir -p "$case_dir/etc-sdwan"
    SDWAN_PLIST="$case_dir/com.minieye.sdwan.plist"
    SDWAN_BINARY="$case_dir/sdwan"
    # shellcheck disable=SC2034 # Used by remove_managed_files from sourced script.
    SDWAN_HELPER="$case_dir/sdwan_helper.sh"
    SDWAN_CONFIG_DIR="$case_dir/etc-sdwan"
    SDWAN_LOG="$case_dir/sdwan.log"
    : >"$SDWAN_PLIST"; : >"$SDWAN_BINARY"; : >"$SDWAN_CONFIG_DIR/control.token"
    : >"$SDWAN_LOG"; : >"$SDWAN_LOG.1"
    MOCK_CALL_LOG="$case_dir/calls"
    MOCK_PRINT_SEQUENCE="$case_dir/prints"
    MOCK_PRINT_COUNT="$case_dir/print-count"
    MOCK_PID_COUNT="$case_dir/pid-count"
    MOCK_PLIST_PATH="$SDWAN_PLIST"
    export MOCK_CALL_LOG MOCK_PRINT_SEQUENCE MOCK_PRINT_COUNT MOCK_PID_COUNT MOCK_PLIST_PATH
    : >"$MOCK_CALL_LOG"
}

set_prints() { printf '%b\n' "$@" >"$MOCK_PRINT_SEQUENCE"; }
assert_removed() { [[ ! -e $SDWAN_PLIST && ! -e $SDWAN_BINARY && ! -e $SDWAN_CONFIG_DIR && ! -e $SDWAN_LOG && ! -e $SDWAN_LOG.1 ]]; }
assert_preserved() { [[ -e $SDWAN_PLIST && -e $SDWAN_BINARY && -e $SDWAN_CONFIG_DIR/control.token && -e $SDWAN_LOG ]]; }
expect_uninstall_failure() {
    if uninstall_macos; then
        printf 'expected uninstall_macos to fail\n' >&2
        exit 1
    fi
}

make_mock_commands
# shellcheck disable=SC2034 # Commands are consumed by sourced uninstaller functions.
LAUNCHCTL_BIN="$tmpdir/launchctl"
# shellcheck disable=SC2034 # Commands are consumed by sourced uninstaller functions.
PID_CHECK_BIN="$tmpdir/pid-check"
# shellcheck disable=SC2034 # Commands are consumed by sourced uninstaller functions.
SLEEP_BIN="$tmpdir/sleep"
# shellcheck disable=SC2034 # Commands are consumed by sourced uninstaller functions.
PS_BIN="$tmpdir/ps"
# shellcheck disable=SC2034 # Commands are consumed by sourced uninstaller functions.
RM_BIN="$tmpdir/rm"

# Bash 3.2-compatible PID parsing accepts standalone top-level or indented fields,
# while rejecting zero and arbitrary/nested-looking text.
assert_equal 42 "$(launchd_job_pid $'pid = 42')"
assert_equal 73 "$(launchd_job_pid $'\tpid = 73')"
assert_equal '' "$(launchd_job_pid $'pid = 0\nlast exit code = 42\nworker pid = 77')"

# An absent job exits promptly and removes only scoped files in required order.
prepare_case absent
set_prints '__ABSENT__'
UNINSTALL_POLL_SECONDS=5 MOCK_PS_OUTPUT='' uninstall_macos
assert_removed
assert_equal 2 "$(<"$MOCK_PRINT_COUNT")"
expected_calls=$(printf 'print system/com.minieye.sdwan\nprint system/com.minieye.sdwan\nrm -f %s\nrm -f %s %s\nrm -rf %s\nrm -f %s\nrm -f %s\n' "$SDWAN_PLIST" "$SDWAN_BINARY" "$SDWAN_HELPER" "$SDWAN_CONFIG_DIR" "$SDWAN_LOG" "$SDWAN_LOG.1")
assert_equal "$expected_calls" "$(<"$MOCK_CALL_LOG")"

# A recorded PID keeps cleanup blocked even after the job itself disappears.
prepare_case pid-alive
set_prints 'pid = 4242' '__ABSENT__'
MOCK_PID_ALIVE_CHECKS=9 MOCK_PS_OUTPUT='' UNINSTALL_POLL_SECONDS=0 expect_uninstall_failure
assert_preserved

# Delayed PID disappearance is polled and succeeds within the configured bound.
prepare_case pid-delayed
set_prints $'\tpid = 4242' '__ABSENT__' '__ABSENT__'
MOCK_PID_ALIVE_CHECKS=1 MOCK_PS_OUTPUT='' UNINSTALL_POLL_SECONDS=2 uninstall_macos
assert_removed
assert_equal 2 "$(<"$MOCK_PID_COUNT")"
assert_equal 1 "$(grep -c '^sleep 1$' "$MOCK_CALL_LOG")"

# A PID that remains alive through the bound preserves every file.
prepare_case pid-remains
set_prints 'pid = 4242' '__ABSENT__' '__ABSENT__'
MOCK_PID_ALIVE_CHECKS=9 MOCK_PS_OUTPUT='' UNINSTALL_POLL_SECONDS=1 expect_uninstall_failure
assert_preserved

# A failed label bootout tries the scoped plist fallback, then verifies absence.
prepare_case fallback
set_prints 'pid = 4242' '__ABSENT__'
MOCK_LABEL_BOOTOUT_STATUS=1 MOCK_PLIST_BOOTOUT_STATUS=0 MOCK_PID_ALIVE_CHECKS=0 MOCK_PS_OUTPUT='' UNINSTALL_POLL_SECONDS=0 uninstall_macos
assert_removed
assert grep -Fx "bootout system/com.minieye.sdwan" "$MOCK_CALL_LOG"
assert grep -Fx "bootout system $SDWAN_PLIST" "$MOCK_CALL_LOG"

# Failed label and plist bootouts do not hide a job that remains present.
prepare_case both-bootouts-fail
set_prints 'pid = 4242' 'pid = 4242'
MOCK_LABEL_BOOTOUT_STATUS=1 MOCK_PLIST_BOOTOUT_STATUS=1 MOCK_PID_ALIVE_CHECKS=9 MOCK_PS_OUTPUT='' UNINSTALL_POLL_SECONDS=0 expect_uninstall_failure
assert_preserved

# A direct exact-path manual process blocks removal; similarly named processes do not.
prepare_case manual
set_prints '__ABSENT__'
MOCK_PS_OUTPUT="123 $SDWAN_BINARY -f /tmp/iwan.conf" UNINSTALL_POLL_SECONDS=0 expect_uninstall_failure
assert_preserved

# An enumeration failure is indeterminate and keeps all managed files intact.
prepare_case ps-query-failure
set_prints '__ABSENT__'
MOCK_PS_STATUS=1 MOCK_PS_OUTPUT='' UNINSTALL_POLL_SECONDS=0 expect_uninstall_failure
assert_preserved

prepare_case unrelated
set_prints '__ABSENT__'
MOCK_PS_OUTPUT=$'124 /usr/local/bin/sdwan-helper\n125 /tmp/sdwan' UNINSTALL_POLL_SECONDS=0 uninstall_macos
assert_removed

# Phase 3 must never manipulate interfaces or broadly terminate processes.
if grep -Eq 'ifconfig[[:space:]].*utun.*destroy|\b(pkill|killall)\b' "$script_dir/uninstall.sh"; then
    printf 'uninstaller contains prohibited broad interface/process handling\n' >&2
    exit 1
fi
