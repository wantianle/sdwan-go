#!/usr/bin/env bash
set -euo pipefail

# ────────────────────────────────────────────────────────────
# sdwan installer — auto-detect OS/arch, download binary,
# interactive server selection, systemd/launchd setup.
#
# One-liner:
#   curl -fsSL https://raw.githubusercontent.com/USER/REPO/main/scripts/install.sh | sudo bash
# Specific version:
#   curl -fsSL https://raw.githubusercontent.com/USER/REPO/main/scripts/install.sh | sudo bash -s -- 1.0.29
# ────────────────────────────────────────────────────────────

REPO_OWNER="wantianle"
REPO_NAME="sdwan-go"
REPO_BRANCH="master"
GH_PROXIES=("https://gh.ddlc.top/" "https://gh-proxy.com/" "https://gh.idayer.com/")  # GitHub mirrors (verified working 2025-06-29)
INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/etc/sdwan"
CONFIG_FILE="$CONFIG_DIR/iwan.conf"
VERSION="latest"
TEST_HOST="${TEST_HOST:-hfs.minieye.tech}"

G='\033[0;32m'; R='\033[0;31m'; Y='\033[0;33m'; B='\033[0;34m'; NC='\033[0m'

# ────────────────────────────────────────────────────────────
usage() {
    cat <<EOF
Usage: sudo bash install.sh [options]

Options:
  -v, --version VERSION   Install a specific GitHub release tag (default: latest)
  -h, --help              Show this help

Examples:
  curl -fsSL https://raw.githubusercontent.com/${REPO_OWNER}/${REPO_NAME}/${REPO_BRANCH}/scripts/install.sh | sudo bash
  curl -fsSL https://raw.githubusercontent.com/${REPO_OWNER}/${REPO_NAME}/${REPO_BRANCH}/scripts/install.sh | sudo bash -s -- 1.0.29
  curl -fsSL https://raw.githubusercontent.com/${REPO_OWNER}/${REPO_NAME}/${REPO_BRANCH}/scripts/install.sh | sudo bash -s -- -v v1.0.29
EOF
}

parse_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            -v|--version)
                [[ $# -ge 2 ]] || { echo "Missing value for $1" >&2; exit 1; }
                VERSION="$2"
                shift 2
                ;;
            -h|--help)
                usage
                exit 0
                ;;
            latest|LATEST)
                VERSION="latest"
                shift
                ;;
            -*)
                echo "Unknown option: $1" >&2
                usage >&2
                exit 1
                ;;
            *)
                VERSION="$1"
                shift
                ;;
        esac
    done

    local version_lower
    version_lower=$(printf '%s' "$VERSION" | tr '[:upper:]' '[:lower:]')
    if [[ "$version_lower" == "latest" ]]; then
        VERSION="latest"
    else
        VERSION="v${VERSION#[vV]}"
    fi
}

# ────────────────────────────────────────────────────────────
check_root() {
    if [[ $EUID -ne 0 ]]; then
        echo -e "${R}❌ 需要 root 权限，请用 sudo 运行${NC}"
        exit 1
    fi
}

# ────────────────────────────────────────────────────────────
detect_platform() {
    OS=$(uname -s | tr '[:upper:]' '[:lower:]')
    ARCH=$(uname -m)

    case "$ARCH" in
        x86_64|amd64) ARCH="amd64" ;;
        arm64|aarch64) ARCH="arm64" ;;
        *) echo -e "${R}❌ 不支持的架构: $ARCH${NC}"; exit 1 ;;
    esac

    case "$OS" in
        linux)   BINARY="sdwan-linux-${ARCH}" ;;
        darwin)  BINARY="sdwan-macos-${ARCH}" ;;
        *) echo -e "${R}❌ 不支持的系统: $OS${NC}"; exit 1 ;;
    esac

    echo -e "${B}📋 检测到系统: $OS / $ARCH${NC}"
}

# ────────────────────────────────────────────────────────────
select_server() {
    echo "" >&2
    echo "---------- ⚡ SD-WAN 服务器延迟检测 ----------" >&2
    local nodes=(
        "1|电信专线 [1000M] (推荐)|minieye.9966.org"
        "2|电信普宽 [3×100M]|dwan.minieye.tech"
        "3|移动专线 [500M]|minieye.8866.org"
        "4|联通普宽 [200M]|minieye.2288.org"
        "5|电信专线 [50M] (财务)|youjia.8866.org"
    )

    local cache=""
    for node in "${nodes[@]}"; do
        IFS="|" read -r id desc addr <<< "$node"
        local lat
        lat=$(ping -c 2 -W 2 "$addr" 2>/dev/null | awk -F '/' 'END {printf "%.0f", $5}')
        local display="$lat" color="$G"
        if [[ -z "$lat" ]]; then
            display="超时"; color="$R"
        elif (( lat > 300 )); then
            color="$R"
        elif (( lat > 100 )); then
            color="$Y"
        fi
        cache="${cache}${id}) | ${desc} | ${B}${addr}${NC} | ${color}${display}ms${NC}\\n"
    done
    echo -e "$cache" | column -t -s "|" >&2
    echo "----------------------------------------------" >&2

    read -rp "选择服务器 (直接回车=1): " choice </dev/tty
    case $choice in
        2) echo "dwan.minieye.tech" ;;
        3) echo "minieye.8866.org" ;;
        4) echo "minieye.2288.org" ;;
        5) echo "youjia.8866.org" ;;
        *) echo "minieye.9966.org" ;;
    esac
}

# ────────────────────────────────────────────────────────────
# probe_mtu uses binary search (548..1472) to find the largest
# IPv4 ICMP echo payload that passes with the DF bit set.
# Returns the tunnel MTU (payload + 28 - 64, clamped [1200,1436]).
# All output goes to stderr; only the MTU integer goes to stdout.
probe_mtu() {
    local server="$1"
    local low=548 high=1472 best=0 mid

    echo -e "${B}⏳ 自动探测 MTU (${server})...${NC}" >&2

    # choose ping flags per OS
    case "$OS" in
        linux)   local do_flag="do" ;;
        darwin)  local do_flag="" ;;
    esac

    # binary search — at most ceil(log2(1472-548+1)) ≃ 10 rounds
    while [[ $low -le $high ]]; do
        mid=$(( (low + high) / 2 ))
        local -a _ping_args=()
        case "$OS" in
            linux)  _ping_args=(-4 -M "$do_flag" -s "$mid" -c 1 -W 1 "$server") ;;
            darwin) _ping_args=(-4 -D -s "$mid" -c 1 -W 1000 "$server") ;;
        esac
        if ping "${_ping_args[@]}" >/dev/null 2>&1; then
            best=$mid
            low=$(( mid + 1 ))
        else
            high=$(( mid - 1 ))
        fi
    done

    if [[ $best -eq 0 ]]; then
        echo -e "${Y}⚠️  MTU 探测未成功 — 使用默认值 1436（非致命警告）${NC}" >&2
        echo "1436"
        return
    fi

    local candidate=$(( best + 28 - 64 ))
    [[ $candidate -lt 1200 ]] && candidate=1200
    [[ $candidate -gt 1436 ]] && candidate=1436

    echo -e "${G}✅ 探测到最佳 MTU: ${candidate}（最大有效载荷=${best} bytes）${NC}" >&2
    echo "$candidate"
}

# ────────────────────────────────────────────────────────────
write_config() {
    local server="$1"
    local username="$2"
    local password="$3"
    local mtu="${4:-1436}"

    mkdir -p "$CONFIG_DIR"

    if [[ -f "$CONFIG_FILE" ]]; then
        read -rp "配置已存在，覆盖? (y/n): " overwrite </dev/tty
        [[ "$overwrite" != "y" && "$overwrite" != "Y" ]] && {
            echo -e "${G}✅ 保留现有配置${NC}"
            return
        }
    fi

    cat > "$CONFIG_FILE" <<EOF
server=$server
username=$username
password=$password
port=10010
mtu=$mtu
encrypt=0
tunname=iwan1
routenet=192.168.0.0/16
EOF

    chmod 600 "$CONFIG_FILE"
    echo -e "${G}✅ 配置文件已保存: $CONFIG_FILE${NC}"
}

# ────────────────────────────────────────────────────────────
# Helper: portable file size in bytes (macOS → BSD stat, Linux → GNU stat, fallback → wc)
file_size() {
    local f="$1"
    [[ -f "$f" ]] || { echo 0; return; }
    stat -f%z "$f" 2>/dev/null || stat -c%s "$f" 2>/dev/null || { wc -c < "$f" | tr -d ' '; }
}

# Helper: human-readable byte count
bytes_human() {
    local b=$1
    (( b > 0 )) || { echo "0B"; return; }
    if (( b >= 1073741824 )); then
        awk "BEGIN {printf \"%.1fG\", $b/1073741824}"
    elif (( b >= 1048576 )); then
        awk "BEGIN {printf \"%.1fM\", $b/1048576}"
    elif (( b >= 1024 )); then
        awk "BEGIN {printf \"%.1fK\", $b/1024}"
    else
        echo "${b}B"
    fi
}

clear_download_line() {
    # Return to the beginning of the current line and clear to end-of-line.
    # This avoids leftovers whenever a refreshed progress frame is shorter
    # than the previous one.
    printf "\r\033[K"
}

# ────────────────────────────────────────────────────────────
download_binary() {
    local binary="$1"
    local release_url
    if [[ "$VERSION" == "latest" ]]; then
        release_url="https://github.com/${REPO_OWNER}/${REPO_NAME}/releases/latest/download/${binary}"
    else
        release_url="https://github.com/${REPO_OWNER}/${REPO_NAME}/releases/download/${VERSION}/${binary}"
    fi
    local dest="$INSTALL_DIR/sdwan"

    echo -e "${B}📥 下载 $binary (version: $VERSION) ...${NC}"

    # Build ordered URL list: each proxy mirror then direct
    local -a urls=()
    for mirror in "${GH_PROXIES[@]}"; do
        local name="${mirror#*://}"; name="${name%/}"  # "ghproxy.com"
        urls+=("${mirror}${release_url} (${name})")
    done
    urls+=("$release_url (direct)")

    local total=${#urls[@]}
    local i=0
    for entry in "${urls[@]}"; do
        ((i++)) || true
        local url="${entry% (*}"
        local label="${entry##*(}"
        label="${label%)}"

        # ── discover total size via HEAD (follow redirects) ──
        local total_size=0 headers
        headers=$(curl -fsSLI --connect-timeout 5 --max-time 10 "$url" 2>/dev/null || true)
        total_size=$(printf '%s\n' "$headers" | grep -i '^Content-Length:' | tail -1 | awk '{print $2}' | tr -d '\r')
        if [[ -z "$total_size" ]] || ! [[ "$total_size" =~ ^[0-9]+$ ]] || (( total_size <= 0 )); then
            total_size=0
        fi

        if (( total_size > 0 )); then
            # ── progress-mode download ──
            local human_total
            human_total=$(bytes_human "$total_size")

            # Start download silently in background
            curl -fsSL --connect-timeout 5 --max-time 60 "$url" -o "$dest" &>/dev/null &
            local curl_pid=$!

            # Poll destination file size; render single-line progress
            local cur=0 pct=0 human_cur=""
            while kill -0 "$curl_pid" 2>/dev/null; do
                cur=$(file_size "$dest" 2>/dev/null || echo 0)
                pct=$(( cur * 100 / total_size ))
                [[ $pct -gt 100 ]] && pct=100
                human_cur=$(bytes_human "$cur")
                clear_download_line
                printf "  [%d/%d] %-20s %d%% %s/%s" \
                    "$i" "$total" "$label" "$pct" "$human_cur" "$human_total"
                sleep 0.3
            done

            # Collect exit code
            local curl_rc=0
            wait "$curl_pid" || curl_rc=$?

            if [[ $curl_rc -eq 0 ]]; then
                local final_sz
                final_sz=$(file_size "$dest" 2>/dev/null || echo 0)
                human_cur=$(bytes_human "$final_sz")
                clear_download_line
                printf "  [%d/%d] %-20s ${G}✅ %s${NC}\n" \
                    "$i" "$total" "$label" "$human_cur"
                chmod +x "$dest"
                return 0
            else
                rm -f "$dest"
                clear_download_line
                printf "  [%d/%d] %-20s ${Y}超时${NC}\n" \
                    "$i" "$total" "$label"
            fi
        else
            # ── fallback: no Content-Length available ──
            printf "  [%d/%d] %-20s ... " "$i" "$total" "$label"
            if curl -fsSL --connect-timeout 5 --max-time 60 "$url" -o "$dest" 2>/dev/null; then
                local final_sz
                final_sz=$(file_size "$dest" 2>/dev/null || echo 0)
                local human_size
                human_size=$(bytes_human "$final_sz")
                echo -e "${G}✅ ${human_size}${NC}"
                chmod +x "$dest"
                return 0
            else
                rm -f "$dest"
                echo -e "${Y}超时${NC}"
            fi
        fi
    done

    echo -e "${R}❌ 所有下载方式均失败，请检查网络${NC}"
    exit 1
}

# ────────────────────────────────────────────────────────────
install_linux_service() {
    local service_file="/etc/systemd/system/sdwan.service"

    cat > "$service_file" <<EOF
[Unit]
Description=SD-WAN VPN Tunnel Service
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/sdwan -f /etc/sdwan/iwan.conf
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF

    # Capture timestamp before starting so verify_tunnel can filter current-attempt logs
    SERVICE_START_TS=$(date +'%Y-%m-%d %H:%M:%S')
    systemctl daemon-reload
    systemctl enable --now sdwan
    sleep 2

    if systemctl is-active --quiet sdwan; then
        echo -e "${G}✅ 服务已在运行${NC}"
    else
        echo -e "${Y}⚠️  服务未启动，检查日志: journalctl -u sdwan -f -n 20${NC}"
    fi
}

# ────────────────────────────────────────────────────────────
install_macos_service() {
    local plist="/Library/LaunchDaemons/com.minieye.sdwan.plist"

    cat > "$plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.minieye.sdwan</string>
    <key>ProgramArguments</key>
    <array>
        <string>/usr/local/bin/sdwan</string>
        <string>-f</string>
        <string>/etc/sdwan/iwan.conf</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/var/log/sdwan.log</string>
    <key>StandardErrorPath</key>
    <string>/var/log/sdwan.log</string>
</dict>
</plist>
EOF

    # Truncate log before starting to avoid stale-log poisoning
    : > /var/log/sdwan.log 2>/dev/null || true
    launchctl bootstrap system "$plist" 2>/dev/null || launchctl load "$plist"
    echo -e "${G}✅ LaunchDaemon 已安装并启动${NC}"
}

# ────────────────────────────────────────────────────────────
# auth_rejected_current_attempt checks the service logs for an
# AUTH REJECTED marker that appeared after the current attempt
# started. Returns 0 if found, 1 otherwise.
auth_rejected_current_attempt() {
    local logs=""
    if [[ "$OS" == "linux" ]]; then
        logs=$(journalctl -u sdwan --since "$SERVICE_START_TS" --no-pager 2>/dev/null || true)
    else
        logs=$(tail -n 80 /var/log/sdwan.log 2>/dev/null || true)
    fi
    if printf '%s\n' "$logs" | grep -qi "AUTH REJECTED"; then
        return 0
    fi
    return 1
}

# ────────────────────────────────────────────────────────────
# linux_tunnel_ready checks real network state for a ready
# Linux tunnel. Returns 0 if iwan1 exists, has IPv4, and
# route 192.168.0.0/16 points to dev iwan1.
linux_tunnel_ready() {
    ip link show iwan1 &>/dev/null || return 1
    ip -4 addr show iwan1 2>/dev/null | grep -q "inet " || return 1
    ip route 2>/dev/null | grep -q "192.168.0.0/16.*iwan1" || return 1
}

# ────────────────────────────────────────────────────────────
# darwin_tunnel_ready checks real network state for a ready
# macOS tunnel. Returns 0 if a utun* interface has inet IPv4
# and route to 192.168.0.0 uses utun.
darwin_tunnel_ready() {
    local darwin_ip=""
    darwin_ip=$(ifconfig 2>/dev/null | awk '/^utun[0-9]+:/{found=1; next} /^[a-z]+[0-9]+:/{found=0} found && /inet /{print $2; exit}' || true)
    [[ -n "$darwin_ip" ]] || return 1
    route -n get 192.168.0.0 2>/dev/null | grep -q 'interface: utun' || return 1
}

# ────────────────────────────────────────────────────────────
# verify_tunnel polls network state for up to 15s. Success is
# determined by real network state only (interface, IP, route).
# Logs are scanned ONLY for AUTH REJECTED markers for the
# current attempt.
# Returns: 0 = ready, 1 = auth rejected, 2 = timeout/incomplete.
verify_tunnel() {
    echo "────────────── 状态检查 ──────────────"
    echo -e "  等待隧道建立 (最多 15s)..."

    local ready=0
    local auth_rejected=0

    for ((sec=1; sec<=15; sec++)); do
        sleep 1

        # Check for AUTH REJECTED in current-attempt logs only
        if [[ $auth_rejected -eq 0 ]] && auth_rejected_current_attempt; then
            auth_rejected=1
            break
        fi

        # Check real network state
        if [[ "$OS" == "linux" ]]; then
            linux_tunnel_ready && { ready=1; break; }
        else
            darwin_tunnel_ready && { ready=1; break; }
        fi
    done

    # ── report results ──
    if [[ "$OS" == "linux" ]]; then
        if ip link show iwan1 &>/dev/null; then
            echo -e "  虚拟网卡: ${G}iwan1 已创建${NC}"
            ip -4 addr show iwan1 2>/dev/null | grep "inet " | awk '{print "  └─ IP: " $2}' || true
        else
            echo -e "  虚拟网卡: ${Y}iwan1 未出现${NC}"
        fi
        if ip route 2>/dev/null | grep -q "192.168.0.0/16.*iwan1"; then
            echo -e "  路由:     ${G}192.168.0.0/16 → iwan1${NC}"
        else
            echo -e "  路由:     ${Y}未确认 192.168.0.0/16 → iwan1${NC}"
        fi
    else
        local darwin_ip=""
        darwin_ip=$(ifconfig 2>/dev/null | awk '/^utun[0-9]+:/{found=1; next} /^[a-z]+[0-9]+:/{found=0} found && /inet /{print $2; exit}' || true)
        if [[ -n "$darwin_ip" ]]; then
            echo -e "  虚拟网卡: ${G}utun 已创建${NC}"
            echo "  └─ IP: $darwin_ip"
        else
            echo -e "  虚拟网卡: ${Y}utun IP 未出现${NC}"
        fi
        if route -n get 192.168.0.0 2>/dev/null | grep -q 'interface: utun'; then
            echo -e "  路由:     ${G}192.168.0.0/16 → utun${NC}"
        else
            echo -e "  路由:     ${Y}未确认 192.168.0.0/16 → utun${NC}"
        fi
    fi

    # Auth
    if [[ $auth_rejected -eq 1 ]]; then
        echo ""
        echo -e "  ${Y}━━━ AUTH REJECTED ━━━${NC}"
        echo -e "  ${R}认证失败：用户名或密码错误，服务器拒绝了连接${NC}"
        echo "──────────────────────────────────────"
        return 1
    fi

    if [[ $ready -eq 1 ]]; then
        echo -e "  认证/隧道:${G} 已建立${NC}"
    else
        echo -e "  认证/隧道:${Y} 超时未建立，请检查网络和配置${NC}"
        echo "──────────────────────────────────────"
        return 2
    fi

    # Ping 3 times with 1s gaps — warning-only, must not decide success
    local ping_ok=0
    for _ in 1 2 3; do
        sleep 1
        if ping -c 1 -W 3 "$TEST_HOST" >/dev/null 2>&1; then
            ping_ok=1
            break
        fi
    done
    if [ "$ping_ok" -eq 1 ]; then
        echo -e "  内网测试: ${G}$TEST_HOST 可达${NC}"
    else
        echo -e "  内网测试: ${Y}$TEST_HOST 暂不可达 (警告，不影响安装)${NC}"
    fi

    echo "──────────────────────────────────────"
    return 0
}

# show_postinstall_help prints the post-install management commands.
show_postinstall_help() {
    echo ""
    echo -e "${G}✅ 安装完成！${NC}"
    echo ""
    echo -e "${B}💡 管理命令:${NC}"
    echo "   sudo systemctl status sdwan       # 查看状态 (Linux)"
    echo "   sudo launchctl list | grep sdwan  # 查看状态 (macOS)"
    echo "   ping hfs.minieye.tech            # 测试连通"
    echo "   sudo journalctl -u sdwan -f       # 实时日志 (Linux)"
    echo "   tail -f /var/log/sdwan.log        # 实时日志 (macOS)"
    echo ""
    echo -e "${Y}💡 修改配置: sudo vi /etc/sdwan/iwan.conf && sudo systemctl restart sdwan${NC}"
}

main() {
    parse_args "$@"
    check_root
    detect_platform

    # Download binary first — if this fails, no point configuring
    download_binary "$BINARY"

    SERVER=$(select_server)
    echo -e "${G}✅ 服务器: $SERVER${NC}"

    while true; do
        read -rp "👤 工号 (username): " USERNAME </dev/tty
        [[ -n "${USERNAME//[[:space:]]/}" ]] && break
        echo -e "${Y}用户名不能为空，请重新输入${NC}"
    done
    while true; do
        read -rsp "🔑 密码 (password): " PASSWORD </dev/tty
        echo
        [[ -n "$PASSWORD" ]] && break
        echo -e "${Y}密码不能为空，请重新输入${NC}"
    done

    # auto-probe the tunnel MTU before writing config
    PROBED_MTU=$(probe_mtu "$SERVER")

    write_config "$SERVER" "$USERNAME" "$PASSWORD" "$PROBED_MTU"

    case "$OS" in
        linux)  install_linux_service ;;
        darwin) install_macos_service ;;
    esac

    # Verify tunnel; report results.
    # set -e would kill the script when verify_tunnel returns non-zero,
    # so we temporarily disable it to capture the result.
    set +e
    verify_tunnel
    local vresult=$?
    set -e

    if [[ $vresult -eq 1 ]]; then
        echo ""
        echo -e "${R}━━━ AUTH REJECTED ━━━${NC}"
        echo -e "${Y}认证被拒绝：请按以下步骤修改配置后重新启动服务：${NC}"
        echo ""
        if [[ "$OS" == "linux" ]]; then
            echo "  sudo vi $CONFIG_FILE"
            echo "  sudo systemctl restart sdwan"
            echo "  sudo journalctl -u sdwan -f -n 20"
        else
            echo "  sudo vi $CONFIG_FILE"
            echo "  sudo launchctl kickstart -k system/com.minieye.sdwan"
            echo "  tail -f /var/log/sdwan.log"
        fi
    elif [[ $vresult -eq 0 ]]; then
        show_postinstall_help
    else
        echo ""
        echo -e "${Y}⚠️  隧道未能在 15s 内建立，请检查网络后手动重启:${NC}"
        case "$OS" in
            linux)  echo "   sudo systemctl restart sdwan" ;;
            darwin) echo "   sudo launchctl kickstart -k system/com.minieye.sdwan" ;;
        esac
    fi
}

main "$@"
