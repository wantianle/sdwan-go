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
ROUTENET="192.168.0.0/16"
DOWNLOAD_TMP=""
MANIFEST_TMP=""
SERVICE_JOURNAL_CURSOR=""
MACOS_LOG_OFFSET=0

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

check_controlling_tty() {
    if ! { exec 3<>/dev/tty; } 2>/dev/null; then
        echo -e "${R}❌ 此安装程序需要可用的控制终端以读取交互式配置${NC}" >&2
        exit 1
    fi
    exec 3>&-
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
        darwin)  BINARY="sdwan-darwin-${ARCH}" ;;
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
        local -a ping_args=(-c 2)
        case "$OS" in
            linux)  ping_args+=(-W 2) ;;    # seconds
            darwin) ping_args+=(-W 2000) ;; # milliseconds
        esac
        lat=$(ping "${ping_args[@]}" "$addr" 2>/dev/null | awk -F '/' '/^rtt |^round-trip / {printf "%.0f", $5}')
        local display color suffix="ms"
        if [[ -z "$lat" ]]; then
            display="超时"; color="$R"; suffix=""
        else
            display="$lat"; color="$G"
        fi
        if [[ -n "$lat" ]] && (( lat > 300 )); then
            color="$R"
        elif [[ -n "$lat" ]] && (( lat > 100 )); then
            color="$Y"
        fi
        cache="${cache}${id}) | ${desc} | ${B}${addr}${NC} | ${color}${display}${suffix}${NC}\\n"
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
            darwin) _ping_args=(-D -s "$mid" -c 1 -W 1000 "$server") ;;
        esac
        local attempt=0 passed=0
        while [[ $attempt -lt 3 ]]; do
            attempt=$(( attempt + 1 ))
            if ping "${_ping_args[@]}" >/dev/null 2>&1; then
                passed=1
                break
            fi
        done
        if [[ $passed -eq 1 ]]; then
            best=$mid
            low=$(( mid + 1 ))
        else
            high=$(( mid - 1 ))
        fi
    done

    if [[ $best -eq 0 ]]; then
        echo -e "${Y}⚠️  MTU 探测未成功 — 使用默认值 1436${NC}" >&2
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
    case "$OS" in
        darwin) stat -f%z "$f" 2>/dev/null || { wc -c < "$f" | tr -d ' '; } ;;
        linux)  stat -c%s "$f" 2>/dev/null || { wc -c < "$f" | tr -d ' '; } ;;
    esac
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

cleanup_download_temp() {
    if [[ -n "$DOWNLOAD_TMP" ]]; then
        rm -f "$DOWNLOAD_TMP"
    fi
    if [[ -n "$MANIFEST_TMP" ]]; then
        rm -f "$MANIFEST_TMP"
    fi
}

trap cleanup_download_temp EXIT

# ────────────────────────────────────────────────────────────
sha256_file() {
    local file="$1"
    case "$OS" in
        darwin) shasum -a 256 "$file" | awk '{print $1}' ;;
        linux)  sha256sum "$file" | awk '{print $1}' ;;
    esac
}

checksum_from_manifest() {
    local manifest="$1"
    local binary="$2"
    awk -v binary="$binary" '
        length($1) == 64 && $1 ~ /^[0-9a-fA-F]+$/ && \
            ($2 == binary || $2 == "*" binary || $2 == "dist/" binary || $2 == "*dist/" binary) {
            print tolower($1)
            exit
        }
    ' "$manifest"
}

download_binary() {
    local binary="$1"
    local release_base
    if [[ "$VERSION" == "latest" ]]; then
        release_base="https://github.com/${REPO_OWNER}/${REPO_NAME}/releases/latest/download"
    else
        release_base="https://github.com/${REPO_OWNER}/${REPO_NAME}/releases/download/${VERSION}"
    fi
    local release_url="${release_base}/${binary}"
    local manifest_url="${release_base}/SHA256SUMS"
    local dest="$INSTALL_DIR/sdwan"

    mkdir -p "$INSTALL_DIR"

    echo -e "${B}📥 下载 $binary (version: $VERSION) ...${NC}"

    # Build ordered URL lists: each proxy mirror then direct.
    local -a urls=()
    local -a manifest_urls=()
    for mirror in "${GH_PROXIES[@]}"; do
        local name="${mirror#*://}"; name="${name%/}"  # "ghproxy.com"
        urls+=("${mirror}${release_url} (${name})")
        manifest_urls+=("${mirror}${manifest_url} (${name})")
    done
    urls+=("$release_url (direct)")
    manifest_urls+=("$manifest_url (direct)")

    local expected_checksum="" manifest_entry manifest_candidate manifest_label
    echo -e "${B}🔐 获取 SHA256SUMS ...${NC}"
    for manifest_entry in "${manifest_urls[@]}"; do
        manifest_candidate="${manifest_entry% (*}"
        manifest_label="${manifest_entry##*(}"
        manifest_label="${manifest_label%)}"
        MANIFEST_TMP=$(mktemp "$INSTALL_DIR/.sdwan-sha256sums.XXXXXX")
        if curl -fsSL --connect-timeout 5 --max-time 60 "$manifest_candidate" -o "$MANIFEST_TMP" 2>/dev/null; then
            expected_checksum=$(checksum_from_manifest "$MANIFEST_TMP" "$binary")
            rm -f "$MANIFEST_TMP"
            MANIFEST_TMP=""
            if [[ "$expected_checksum" =~ ^[0-9a-f]{64}$ ]]; then
                echo -e "  ${G}✅ SHA256SUMS: ${manifest_label}${NC}"
                break
            fi
            echo -e "  ${Y}⚠️  SHA256SUMS 无效: ${manifest_label}${NC}"
        else
            rm -f "$MANIFEST_TMP"
            MANIFEST_TMP=""
            echo -e "  ${Y}⚠️  SHA256SUMS 下载失败: ${manifest_label}${NC}"
        fi
    done

    if [[ ! "$expected_checksum" =~ ^[0-9a-f]{64}$ ]]; then
        echo -e "${R}❌ 无法获取或解析 ${binary} 的 SHA256SUMS，已停止安装以保护完整性${NC}"
        exit 1
    fi

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
        # A proxy/redirect may omit Content-Length. With pipefail enabled,
        # grep then returns 1; treat that as "no progress size available",
        # not as a fatal installer error.
        total_size=$(printf '%s\n' "$headers" | awk 'tolower($1) == "content-length:" { size=$2 } END { gsub("\\r", "", size); print size }')
        if [[ -z "$total_size" ]] || ! [[ "$total_size" =~ ^[0-9]+$ ]] || (( total_size <= 0 )); then
            total_size=0
        fi

        if (( total_size > 0 )); then
            # ── progress-mode download ──
            local human_total
            human_total=$(bytes_human "$total_size")

            # Start download silently in background
            DOWNLOAD_TMP=$(mktemp "$INSTALL_DIR/.sdwan.XXXXXX")
            curl -fsSL --connect-timeout 5 --max-time 60 "$url" -o "$DOWNLOAD_TMP" &>/dev/null &
            local curl_pid=$!

            # Poll destination file size; render single-line progress
            local cur=0 pct=0 human_cur=""
            while kill -0 "$curl_pid" 2>/dev/null; do
                cur=$(file_size "$DOWNLOAD_TMP" 2>/dev/null || echo 0)
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
                final_sz=$(file_size "$DOWNLOAD_TMP" 2>/dev/null || echo 0)
                human_cur=$(bytes_human "$final_sz")
                clear_download_line
                local actual_checksum
                actual_checksum=$(sha256_file "$DOWNLOAD_TMP" 2>/dev/null || true)
                if [[ "$actual_checksum" == "$expected_checksum" ]]; then
                    printf "  [%d/%d] %-20s ${G}✅ %s${NC}\n" \
                        "$i" "$total" "$label" "$human_cur"
                    chmod 755 "$DOWNLOAD_TMP"
                    mv -f "$DOWNLOAD_TMP" "$dest"
                    DOWNLOAD_TMP=""
                    return 0
                fi
                rm -f "$DOWNLOAD_TMP"
                DOWNLOAD_TMP=""
                printf "  [%d/%d] %-20s ${R}校验和不匹配${NC}\n" \
                    "$i" "$total" "$label"
            else
                rm -f "$DOWNLOAD_TMP"
                DOWNLOAD_TMP=""
                clear_download_line
                printf "  [%d/%d] %-20s ${Y}下载失败 (curl %d)${NC}\n" \
                    "$i" "$total" "$label" "$curl_rc"
            fi
        else
            # ── fallback: no Content-Length available ──
            printf "  [%d/%d] %-20s ... " "$i" "$total" "$label"
            DOWNLOAD_TMP=$(mktemp "$INSTALL_DIR/.sdwan.XXXXXX")
            if curl -fsSL --connect-timeout 5 --max-time 60 "$url" -o "$DOWNLOAD_TMP" 2>/dev/null; then
                local final_sz
                final_sz=$(file_size "$DOWNLOAD_TMP" 2>/dev/null || echo 0)
                local human_size
                human_size=$(bytes_human "$final_sz")
                local actual_checksum
                actual_checksum=$(sha256_file "$DOWNLOAD_TMP" 2>/dev/null || true)
                if [[ "$actual_checksum" == "$expected_checksum" ]]; then
                    echo -e "${G}✅ ${human_size}${NC}"
                    chmod 755 "$DOWNLOAD_TMP"
                    mv -f "$DOWNLOAD_TMP" "$dest"
                    DOWNLOAD_TMP=""
                    return 0
                fi
                rm -f "$DOWNLOAD_TMP"
                DOWNLOAD_TMP=""
                echo -e "${R}校验和不匹配${NC}"
            else
                local curl_rc=$?
                rm -f "$DOWNLOAD_TMP"
                DOWNLOAD_TMP=""
                echo -e "${Y}下载失败 (curl ${curl_rc})${NC}"
            fi
        fi
    done

    cleanup_download_temp

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

    systemctl daemon-reload
    systemctl enable sdwan
    # Capture the journal boundary immediately before this restart. Records
    # queried after this cursor belong to the current service attempt only.
    SERVICE_JOURNAL_CURSOR=$(journalctl -u sdwan -n 0 --show-cursor --no-pager 2>/dev/null \
        | awk '/^-- cursor: / { sub(/^-- cursor: /, ""); print; exit }' || true)
    systemctl restart sdwan
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
    local label="com.minieye.sdwan"
    local plist_tmp

    bootout_macos_job

    # The daemon creates its control token here. Keep the containing directory
    # root-owned before launch so token creation cannot inherit unsafe access.
    mkdir -p "$CONFIG_DIR"
    chown root:wheel "$CONFIG_DIR"
    chmod 700 "$CONFIG_DIR"

    # Preserve prior records and inspect only bytes appended after this point.
    touch /var/log/sdwan.log
    MACOS_LOG_OFFSET=$(file_size /var/log/sdwan.log)

    plist_tmp=$(mktemp "/Library/LaunchDaemons/.${label}.XXXXXX")
    write_macos_plist "$plist_tmp"

    chown root:wheel "$plist_tmp"
    chmod 644 "$plist_tmp"
    plutil -lint "$plist_tmp" >/dev/null
    mv -f "$plist_tmp" "$plist"

    launchctl bootstrap system "$plist"
    launchctl print "system/$label" >/dev/null
    echo -e "${G}✅ LaunchDaemon 已安装并启动${NC}"
}

# A loaded old job must be gone before replacing its executable or plist.
bootout_macos_job() {
    local label="com.minieye.sdwan"
    launchctl bootout "system/$label" 2>/dev/null || true
    if launchctl print "system/$label" >/dev/null 2>&1; then
        echo -e "${R}❌ 旧 LaunchDaemon 仍在运行；拒绝替换二进制或 plist${NC}" >&2
        return 1
    fi
}

write_macos_plist() {
    local destination="$1"
    cat > "$destination" <<EOF
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
    <key>StandardErrorPath</key>
    <string>/var/log/sdwan.log</string>
</dict>
</plist>
EOF
}

# ────────────────────────────────────────────────────────────
# auth_rejected_current_attempt checks the service logs for an
# AUTH REJECTED marker from the current attempt. Linux queries only records
# after the pre-restart journal cursor; macOS reads only bytes appended after
# the pre-bootstrap log boundary.
# Returns 0 if found, 1 otherwise.
auth_rejected_current_attempt() {
    local logs=""
    if [[ "$OS" == "linux" ]]; then
        # Without a cursor, do not inspect historical logs and risk reporting
        # a stale authentication error as belonging to this installation.
        [[ -n "$SERVICE_JOURNAL_CURSOR" ]] || return 1
        logs=$(journalctl -u sdwan --after-cursor "$SERVICE_JOURNAL_CURSOR" --no-pager 2>/dev/null || true)
    else
        logs=$(tail -c "+$((MACOS_LOG_OFFSET + 1))" /var/log/sdwan.log 2>/dev/null || true)
    fi
    printf '%s\n' "$logs" | awk 'BEGIN { found=0 } tolower($0) ~ /auth rejected/ { found=1 } END { exit !found }'
}

tunnel_routenet() {
    local configured=""
    if [[ -f "$CONFIG_FILE" ]]; then
        configured=$(awk -F= '$1 == "routenet" { print $2; exit }' "$CONFIG_FILE")
    fi
    printf '%s\n' "${configured:-$ROUTENET}"
}

# ────────────────────────────────────────────────────────────
# linux_tunnel_ready checks real network state for a ready
# Linux tunnel. Returns 0 if iwan1 exists, has IPv4, and
# route 192.168.0.0/16 points to dev iwan1.
linux_tunnel_ready() {
    local routenet
    routenet=$(tunnel_routenet)
    ip link show iwan1 &>/dev/null || return 1
    ip -4 addr show iwan1 2>/dev/null | awk 'BEGIN { found=0 } /inet / { found=1 } END { exit !found }' || return 1
    ip route 2>/dev/null | awk -v routenet="$routenet" 'BEGIN { found=0 } $1 == routenet && $0 ~ /dev iwan1/ { found=1 } END { exit !found }' || return 1
}

# ────────────────────────────────────────────────────────────
# darwin_tunnel_ready checks real network state for a ready
# macOS tunnel. Returns 0 if a utun* interface has inet IPv4
# and route to 192.168.0.0 uses utun.
darwin_route_interface() {
    local routenet
    routenet=$(tunnel_routenet)
    route -n get "${routenet%/*}" 2>/dev/null | awk '/interface:/{print $2; exit}'
}

darwin_interface_ipv4() {
    local interface="$1"
    ifconfig "$interface" 2>/dev/null | awk '/inet / { print $2; exit }'
}

darwin_tunnel_ready() {
    local interface darwin_ip
    interface=$(darwin_route_interface)
    [[ "$interface" =~ ^utun[0-9]+$ ]] || return 1
    darwin_ip=$(darwin_interface_ipv4 "$interface")
    [[ -n "$darwin_ip" ]]
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
    local routenet
    routenet=$(tunnel_routenet)
    if [[ "$OS" == "linux" ]]; then
        if ip link show iwan1 &>/dev/null; then
            echo -e "  虚拟网卡: ${G}iwan1 已创建${NC}"
            ip -4 addr show iwan1 2>/dev/null | grep "inet " | awk '{print "  └─ IP: " $2}' || true
        else
            echo -e "  虚拟网卡: ${Y}iwan1 未出现${NC}"
        fi
        if ip route 2>/dev/null | awk -v routenet="$routenet" 'BEGIN { found=0 } $1 == routenet && $0 ~ /dev iwan1/ { found=1 } END { exit !found }'; then
            echo -e "  路由:     ${G}${routenet} → iwan1${NC}"
        else
            echo -e "  路由:     ${Y}未确认 ${routenet} → iwan1${NC}"
        fi
    else
        local interface darwin_ip
        interface=$(darwin_route_interface)
        darwin_ip=""
        if [[ "$interface" =~ ^utun[0-9]+$ ]]; then
            darwin_ip=$(darwin_interface_ipv4 "$interface")
        fi
        if [[ -n "$darwin_ip" ]]; then
            echo -e "  虚拟网卡: ${G}${interface} 已创建${NC}"
            echo "  └─ IP: $darwin_ip"
        else
            echo -e "  虚拟网卡: ${Y}路由对应 utun IP 未出现${NC}"
        fi
        if [[ "$interface" =~ ^utun[0-9]+$ ]]; then
            echo -e "  路由:     ${G}${routenet} → ${interface}${NC}"
        else
            echo -e "  路由:     ${Y}未确认 ${routenet} → utun${NC}"
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
        local -a ping_args=(-c 1)
        case "$OS" in
            linux) ping_args+=(-W 3) ;;
            darwin) ping_args+=(-W 3000) ;;
        esac
        if ping "${ping_args[@]}" "$TEST_HOST" >/dev/null 2>&1; then
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
    if [[ "$OS" == "linux" ]]; then
        echo -e "${Y}💡 修改配置: sudo vi /etc/sdwan/iwan.conf && sudo systemctl restart sdwan${NC}"
    else
        echo -e "${Y}💡 修改配置: sudo vi /etc/sdwan/iwan.conf && sudo launchctl kickstart -k system/com.minieye.sdwan${NC}"
    fi
}

main() {
    parse_args "$@"
    check_root
    check_controlling_tty
    detect_platform

    # Prove the old service is gone before download_binary atomically replaces
    # the installed executable. install_macos_service repeats this before the
    # plist is replaced and bootstrapped.
    if [[ "$OS" == "darwin" ]]; then
        bootout_macos_job
    fi

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
    local vresult=0
    if verify_tunnel; then
        vresult=0
    else
        vresult=$?
    fi

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
        exit 1
    elif [[ $vresult -eq 0 ]]; then
        show_postinstall_help
    else
        echo ""
        echo -e "${Y}⚠️  隧道未能在 15s 内建立，请检查网络后手动重启:${NC}"
        case "$OS" in
            linux)  echo "   sudo systemctl restart sdwan" ;;
            darwin) echo "   sudo launchctl kickstart -k system/com.minieye.sdwan" ;;
        esac
        exit 1
    fi
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
    main "$@"
fi
