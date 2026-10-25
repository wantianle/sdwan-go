#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck disable=SC1091 # Path is resolved dynamically from this test's directory.
source "$script_dir/uninstall.sh"

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

assert() { "$@" || { printf 'assertion failed: %s\n' "$*" >&2; exit 1; }; }
assert_equal() { [[ $1 == "$2" ]] || { printf 'expected %s, got %s\n' "$1" "$2" >&2; exit 1; }; }

make_mock_commands() {
    cat >"$tmpdir/systemctl" <<'EOF'
#!/usr/bin/env bash
set -eu
printf '%s\n' "$*" >>"$MOCK_CALL_LOG"
case "$1" in
    show)
        if [[ $2 == --property=MainPID ]]; then
            printf '%s\n' "${MOCK_MAIN_PID:-0}"
            exit "${MOCK_MAIN_PID_STATUS:-0}"
        fi
        printf '%s\n' "${MOCK_LOAD_STATE:-loaded}"
        exit "${MOCK_LOAD_STATE_STATUS:-0}"
        ;;
    is-active)
        count=0; [[ -f $MOCK_ACTIVE_COUNT ]] && count=$(<"$MOCK_ACTIVE_COUNT")
        count=$((count + 1)); printf '%s\n' "$count" >"$MOCK_ACTIVE_COUNT"
        if [[ -n ${MOCK_ACTIVE_OUTPUT:-} ]]; then
            printf '%s\n' "$MOCK_ACTIVE_OUTPUT"
            exit "${MOCK_ACTIVE_STATUS:-0}"
        fi
        if [[ $count -le ${MOCK_ACTIVE_CHECKS:-0} ]]; then
            printf 'active\n'
            exit 0
        fi
        printf 'inactive\n'
        exit 3
        ;;
esac
EOF
    cat >"$tmpdir/pid-check" <<'EOF'
#!/usr/bin/env bash
set -eu
printf '%s\n' "$*" >>"$MOCK_CALL_LOG"
count=0; [[ -f $MOCK_PID_COUNT ]] && count=$(<"$MOCK_PID_COUNT")
count=$((count + 1)); printf '%s\n' "$count" >"$MOCK_PID_COUNT"
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
    chmod +x "$tmpdir/systemctl" "$tmpdir/pid-check" "$tmpdir/sleep" "$tmpdir/ps" "$tmpdir/rm"
}

prepare_case() {
    local case_dir="$tmpdir/$1"
    mkdir -p "$case_dir/etc-sdwan"
    SDWAN_BINARY="$case_dir/sdwan"; SDWAN_HELPER="$case_dir/sdwan_helper.sh"
    SDWAN_CONFIG_DIR="$case_dir/etc-sdwan"; SYSTEMD_SERVICE_FILE="$case_dir/sdwan.service"
    : >"$SDWAN_BINARY"; : >"$SDWAN_HELPER"; : >"$SDWAN_CONFIG_DIR/iwan.conf"; : >"$SYSTEMD_SERVICE_FILE"
    MOCK_CALL_LOG="$case_dir/calls"; MOCK_ACTIVE_COUNT="$case_dir/active-count"; MOCK_PID_COUNT="$case_dir/pid-count"
    export MOCK_CALL_LOG MOCK_ACTIVE_COUNT MOCK_PID_COUNT
    : >"$MOCK_CALL_LOG"
}

assert_removed() { [[ ! -e $SDWAN_BINARY && ! -e $SDWAN_HELPER && ! -e $SDWAN_CONFIG_DIR && ! -e $SYSTEMD_SERVICE_FILE ]]; }
assert_preserved() { [[ -e $SDWAN_BINARY && -e $SDWAN_HELPER && -e $SDWAN_CONFIG_DIR/iwan.conf && -e $SYSTEMD_SERVICE_FILE ]]; }
expect_failure() { if uninstall_linux; then printf 'expected uninstall_linux to fail\n' >&2; exit 1; fi; }

make_mock_commands
# shellcheck disable=SC2034 # Commands are consumed by sourced uninstaller functions.
SYSTEMCTL_BIN="$tmpdir/systemctl"
# shellcheck disable=SC2034 # Commands are consumed by sourced uninstaller functions.
PID_CHECK_BIN="$tmpdir/pid-check"
# shellcheck disable=SC2034 # Commands are consumed by sourced uninstaller functions.
SLEEP_BIN="$tmpdir/sleep"
# shellcheck disable=SC2034 # Commands are consumed by sourced uninstaller functions.
PS_BIN="$tmpdir/ps"
# shellcheck disable=SC2034 # Commands are consumed by sourced uninstaller functions.
RM_BIN="$tmpdir/rm"

# A known inactive state is safe and removes scoped files without waiting for a PID.
prepare_case inactive
MOCK_MAIN_PID=0 MOCK_ACTIVE_CHECKS=0 MOCK_PID_ALIVE_CHECKS=0 MOCK_PS_OUTPUT='' UNINSTALL_POLL_SECONDS=2 uninstall_linux
assert_removed
assert grep -Fx 'show --property=MainPID --value sdwan' "$MOCK_CALL_LOG"
assert grep -Fx 'stop sdwan' "$MOCK_CALL_LOG"
assert grep -Fx 'disable sdwan' "$MOCK_CALL_LOG"

# An absent unit is safe only when an independent LoadState query confirms it.
prepare_case absent
MOCK_MAIN_PID=0 MOCK_ACTIVE_OUTPUT=unknown MOCK_ACTIVE_STATUS=4 MOCK_LOAD_STATE=not-found MOCK_LOAD_STATE_STATUS=0 MOCK_PID_ALIVE_CHECKS=0 MOCK_PS_OUTPUT='' UNINSTALL_POLL_SECONDS=2 uninstall_linux
assert_removed
assert grep -Fx 'show --property=LoadState --value sdwan' "$MOCK_CALL_LOG"

# A normally managed process is captured before stop and polled until it exits.
prepare_case managed-stop
MOCK_MAIN_PID=4242 MOCK_ACTIVE_CHECKS=1 MOCK_PID_ALIVE_CHECKS=1 MOCK_PS_OUTPUT='' UNINSTALL_POLL_SECONDS=2 uninstall_linux
assert_removed
assert_equal 2 "$(<"$MOCK_PID_COUNT")"
assert_equal 2 "$(grep -c '^sleep 1$' "$MOCK_CALL_LOG")"

# A captured PID remaining alive prevents every deletion.
prepare_case pid-remains
MOCK_MAIN_PID=4242 MOCK_ACTIVE_CHECKS=0 MOCK_PID_ALIVE_CHECKS=9 MOCK_PS_OUTPUT='' UNINSTALL_POLL_SECONDS=1 expect_failure
assert_preserved

# An active unit through the bound prevents every deletion.
prepare_case unit-active
MOCK_MAIN_PID=0 MOCK_ACTIVE_CHECKS=9 MOCK_PID_ALIVE_CHECKS=0 MOCK_PS_OUTPUT='' UNINSTALL_POLL_SECONDS=1 expect_failure
assert_preserved

# A MainPID query error is indeterminate and must not issue stop or delete files.
prepare_case main-pid-query-failure
MOCK_MAIN_PID_STATUS=1 MOCK_ACTIVE_CHECKS=0 MOCK_PID_ALIVE_CHECKS=0 MOCK_PS_OUTPUT='' UNINSTALL_POLL_SECONDS=1 expect_failure
assert_preserved
if grep -Fx 'stop sdwan' "$MOCK_CALL_LOG"; then
    printf 'stop ran after MainPID query failure\n' >&2
    exit 1
fi

# A failed is-active invocation is not evidence that the unit is inactive.
prepare_case status-query-failure
MOCK_MAIN_PID=0 MOCK_ACTIVE_OUTPUT='Failed to connect to bus' MOCK_ACTIVE_STATUS=1 MOCK_PID_ALIVE_CHECKS=0 MOCK_PS_OUTPUT='' UNINSTALL_POLL_SECONDS=1 expect_failure
assert_preserved

# An exact installed binary launched manually blocks deletion after service stop.
prepare_case manual
MOCK_MAIN_PID=0 MOCK_ACTIVE_CHECKS=0 MOCK_PID_ALIVE_CHECKS=0 MOCK_PS_OUTPUT="123 $SDWAN_BINARY -f /tmp/iwan.conf" UNINSTALL_POLL_SECONDS=0 expect_failure
assert_preserved

# A failed process enumeration cannot establish safety and preserves every file.
prepare_case ps-query-failure
MOCK_MAIN_PID=0 MOCK_ACTIVE_CHECKS=0 MOCK_PID_ALIVE_CHECKS=0 MOCK_PS_STATUS=1 MOCK_PS_OUTPUT='' UNINSTALL_POLL_SECONDS=0 expect_failure
assert_preserved

# Similar names and other paths must not block this exact-path check.
prepare_case unrelated
MOCK_MAIN_PID=0 MOCK_ACTIVE_CHECKS=0 MOCK_PID_ALIVE_CHECKS=0 MOCK_PS_OUTPUT=$'124 /usr/local/bin/sdwan-helper\n125 /tmp/sdwan' UNINSTALL_POLL_SECONDS=0 uninstall_linux
assert_removed

# Never introduce broad process termination to this verified cleanup path.
if grep -Eq '\b(pkill|killall)\b' "$script_dir/uninstall.sh"; then
    printf 'uninstaller contains prohibited broad process handling\n' >&2
    exit 1
fi
