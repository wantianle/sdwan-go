#!/usr/bin/env bash
set -euo pipefail

# ────────────────────────────────────────────────────────────
# sdwan uninstaller — stop service, remove binary + config
# ────────────────────────────────────────────────────────────

G='\033[0;32m'; R='\033[0;31m'; Y='\033[0;33m'; NC='\033[0m'

SDWAN_BINARY=${SDWAN_BINARY:-/usr/local/bin/sdwan}
SDWAN_HELPER=${SDWAN_HELPER:-/usr/local/bin/sdwan_helper.sh}
SDWAN_CONFIG_DIR=${SDWAN_CONFIG_DIR:-/etc/sdwan}
SDWAN_PLIST=${SDWAN_PLIST:-/Library/LaunchDaemons/com.minieye.sdwan.plist}
SDWAN_LOG=${SDWAN_LOG:-/var/log/sdwan.log}
LAUNCHCTL_BIN=${LAUNCHCTL_BIN:-launchctl}
SYSTEMCTL_BIN=${SYSTEMCTL_BIN:-systemctl}
PS_BIN=${PS_BIN:-ps}
PID_CHECK_BIN=${PID_CHECK_BIN:-kill}
SLEEP_BIN=${SLEEP_BIN:-sleep}
RM_BIN=${RM_BIN:-rm}
UNINSTALL_POLL_SECONDS=${UNINSTALL_POLL_SECONDS:-10}
SYSTEMD_SERVICE_FILE=${SYSTEMD_SERVICE_FILE:-/etc/systemd/system/sdwan.service}
readonly LAUNCHD_JOB='system/com.minieye.sdwan'
readonly SYSTEMD_SERVICE='sdwan'

check_root() {
    if [[ ${UNINSTALL_SKIP_ROOT_CHECK:-0} != 1 && $EUID -ne 0 ]]; then
        echo -e "${R}❌ 需要 root 权限，请用 sudo 运行${NC}"
        exit 1
    fi
}

launchd_job_output() {
    "$LAUNCHCTL_BIN" print "$LAUNCHD_JOB" 2>/dev/null
}

launchd_job_pid() {
    local line pid
    while IFS= read -r line; do
        # launchctl print emits the managed process as a standalone `pid = N`
        # property. Anchoring the whole line avoids nested/free-form text.
        if [[ $line =~ ^[[:space:]]*pid[[:space:]]*=[[:space:]]*([1-9][0-9]*)[[:space:]]*$ ]]; then
            pid=${BASH_REMATCH[1]}
            printf '%s\n' "$pid"
            return 0
        fi
    done <<< "$1"
    return 0
}

pid_is_running() {
    "$PID_CHECK_BIN" -0 "$1" 2>/dev/null
}

wait_for_launchd_shutdown() {
    local managed_pid=$1
    local attempt=0
    local job_output

    while (( attempt <= UNINSTALL_POLL_SECONDS )); do
        if ! job_output=$(launchd_job_output) && { [[ -z $managed_pid ]] || ! pid_is_running "$managed_pid"; }; then
            return 0
        fi
        (( attempt == UNINSTALL_POLL_SECONDS )) && break
        "$SLEEP_BIN" 1
        ((attempt += 1))
    done

    echo -e "${R}❌ LaunchDaemon 未能停止；为保护现有文件，取消卸载${NC}" >&2
    if [[ -n $job_output ]]; then
        printf 'launchctl print %s still reports:\n%s\n' "$LAUNCHD_JOB" "$job_output" >&2
    fi
    if [[ -n $managed_pid ]] && pid_is_running "$managed_pid"; then
        printf 'managed PID is still running: %s\n' "$managed_pid" >&2
    fi
    return 1
}

exact_sdwan_processes() {
    local line pid command process_list

    if ! process_list=$("$PS_BIN" -axo pid=,command=); then
        printf 'unable to enumerate processes while checking %s\n' "$SDWAN_BINARY" >&2
        return 1
    fi
    while IFS= read -r line; do
        [[ $line =~ ^[[:space:]]*([0-9]+)[[:space:]]+(.*)$ ]] || continue
        pid=${BASH_REMATCH[1]}
        command=${BASH_REMATCH[2]}
        # Match argv[0] only: similarly named binaries and arbitrary command text
        # must not block (or be killed by) this uninstaller.
        if [[ $command == "$SDWAN_BINARY" || $command == "$SDWAN_BINARY "* ]]; then
            printf '%s %s\n' "$pid" "$command"
        fi
    done <<< "$process_list"
}

systemd_main_pid() {
    local pid
    if ! pid=$("$SYSTEMCTL_BIN" show --property=MainPID --value "$SYSTEMD_SERVICE" 2>/dev/null); then
        printf 'unable to query systemd MainPID for %s\n' "$SYSTEMD_SERVICE" >&2
        return 1
    fi
    if [[ ! $pid =~ ^(0|[1-9][0-9]*)$ ]]; then
        printf 'unexpected systemd MainPID for %s: %s\n' "$SYSTEMD_SERVICE" "$pid" >&2
        return 1
    fi
    if [[ $pid != 0 ]]; then
        printf '%s\n' "$pid"
    fi
    return 0
}

systemd_unit_is_stopped() {
    local state load_state status

    if state=$("$SYSTEMCTL_BIN" is-active "$SYSTEMD_SERVICE" 2>/dev/null); then
        status=0
    else
        status=$?
    fi

    case "$state" in
        inactive|failed)
            return 0
            ;;
        active|activating|deactivating)
            return 1
            ;;
        unknown)
            # `is-active` reports unknown for an absent unit. Accept that only
            # after an independent query confirms systemd's LoadState is
            # `not-found`; command/transport failures are never treated as off.
            if ! load_state=$("$SYSTEMCTL_BIN" show --property=LoadState --value "$SYSTEMD_SERVICE" 2>/dev/null); then
                printf 'unable to confirm whether systemd unit %s is absent\n' "$SYSTEMD_SERVICE" >&2
                return 2
            fi
            if [[ $load_state == not-found ]]; then
                return 0
            fi
            printf 'unexpected systemd LoadState for %s: %s\n' "$SYSTEMD_SERVICE" "$load_state" >&2
            return 2
            ;;
        *)
            printf 'unable to verify systemd state for %s (is-active status %s, output: %s)\n' "$SYSTEMD_SERVICE" "$status" "$state" >&2
            return 2
            ;;
    esac
}

wait_for_systemd_shutdown() {
    local managed_pid=$1
    local attempt=0
    local unit_status

    while (( attempt <= UNINSTALL_POLL_SECONDS )); do
        if systemd_unit_is_stopped; then
            unit_status=0
        else
            unit_status=$?
        fi
        if (( unit_status == 2 )); then
            echo -e "${R}❌ 无法确认 systemd 服务状态；为保护现有文件，取消卸载${NC}" >&2
            return 1
        fi
        if (( unit_status == 0 )) && { [[ -z $managed_pid ]] || ! pid_is_running "$managed_pid"; }; then
            return 0
        fi
        (( attempt == UNINSTALL_POLL_SECONDS )) && break
        "$SLEEP_BIN" 1
        ((attempt += 1))
    done

    echo -e "${R}❌ systemd 服务未能停止；为保护现有文件，取消卸载${NC}" >&2
    if ! systemd_unit_is_stopped; then
        printf 'systemctl is-active %s still reports active\n' "$SYSTEMD_SERVICE" >&2
    fi
    if [[ -n $managed_pid ]] && pid_is_running "$managed_pid"; then
        printf 'managed PID is still running: %s\n' "$managed_pid" >&2
    fi
    return 1
}

remove_managed_files() {
    local log_file

    "$RM_BIN" -f "$SDWAN_PLIST"
    "$RM_BIN" -f "$SDWAN_BINARY" "$SDWAN_HELPER"
    "$RM_BIN" -rf "$SDWAN_CONFIG_DIR"
    "$RM_BIN" -f "$SDWAN_LOG"
    for log_file in "$SDWAN_LOG".*; do
        [[ -e $log_file || -L $log_file ]] || continue
        [[ -f $log_file || -L $log_file ]] && "$RM_BIN" -f "$log_file"
    done
}

remove_linux_managed_files() {
    "$RM_BIN" -f "$SYSTEMD_SERVICE_FILE"
    "$RM_BIN" -f "$SDWAN_BINARY" "$SDWAN_HELPER"
    "$RM_BIN" -rf "$SDWAN_CONFIG_DIR"
}

uninstall_linux() {
    local managed_pid='' residual_processes=''

    # Capture this before stop: systemd clears MainPID once the unit exits.
    if ! managed_pid=$(systemd_main_pid); then
        echo -e "${R}❌ 无法确认 systemd 主进程；为保护现有文件，取消卸载${NC}" >&2
        return 1
    fi
    "$SYSTEMCTL_BIN" stop "$SYSTEMD_SERVICE" 2>/dev/null || true
    "$SYSTEMCTL_BIN" disable "$SYSTEMD_SERVICE" 2>/dev/null || true

    wait_for_systemd_shutdown "$managed_pid" || return 1

    if ! residual_processes=$(exact_sdwan_processes); then
        echo -e "${R}❌ 无法确认已安装 SDWAN 进程；为保护现有文件，取消卸载${NC}" >&2
        return 1
    fi
    if [[ -n $residual_processes ]]; then
        echo -e "${R}❌ 检测到仍在运行的已安装 SDWAN 进程；为保护现有文件，取消卸载${NC}" >&2
        printf '%s\n' "$residual_processes" >&2
        return 1
    fi

    remove_linux_managed_files
    "$SYSTEMCTL_BIN" daemon-reload 2>/dev/null || true
    echo -e "${G}✅ systemd 服务、二进制和配置已移除${NC}"
}

uninstall_macos() {
    local job_output='' managed_pid='' manual_processes=''

    if job_output=$(launchd_job_output); then
        managed_pid=$(launchd_job_pid "$job_output")
        if ! "$LAUNCHCTL_BIN" bootout "$LAUNCHD_JOB"; then
            "$LAUNCHCTL_BIN" bootout system "$SDWAN_PLIST" || true
        fi
    else
        echo 'LaunchDaemon is already absent.'
    fi

    wait_for_launchd_shutdown "$managed_pid" || return 1

    if ! manual_processes=$(exact_sdwan_processes); then
        echo -e "${R}❌ 无法确认手动启动的 SDWAN 进程；为保护现有文件，取消卸载${NC}" >&2
        return 1
    fi
    if [[ -n $manual_processes ]]; then
        echo -e "${R}❌ 检测到手动启动的 SDWAN 进程；为保护现有文件，取消卸载${NC}" >&2
        printf '%s\n' "$manual_processes" >&2
        return 1
    fi

    remove_managed_files
    echo -e "${G}✅ LaunchDaemon 已移除${NC}"
}

main() {
    local os
    check_root
    echo -e "${Y}🗑️  正在卸载 SDWAN...${NC}"
    os=$(uname -s | tr '[:upper:]' '[:lower:]')

    case "$os" in
        linux)
            uninstall_linux
            ;;
        darwin)
            uninstall_macos
            ;;
    esac

    echo ''
    echo -e "${G}🗑️  卸载完成${NC}"
}

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then
    main "$@"
fi
