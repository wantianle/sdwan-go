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
write_config() {
    local server="$1"
    local username="$2"
    local password="$3"

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
mtu=1436
encrypt=0
tunname=iwan1
routenet=192.168.0.0/16
EOF

    chmod 600 "$CONFIG_FILE"
    echo -e "${G}✅ 配置文件已保存: $CONFIG_FILE${NC}"
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
        urls+=("${mirror}${release_url} (${name})" )
    done
    urls+=("$release_url (direct)")

    local total=${#urls[@]}
    local i=0
    for entry in "${urls[@]}"; do
        ((i++)) || true
        local url="${entry% (*}"
        local label="${entry##*(}"
        label="${label%)}"
        printf "  [%d/%d] %-20s ... " "$i" "$total" "$label"
        if curl -fsSL --connect-timeout 5 --max-time 30 "$url" -o "$dest" 2>/dev/null; then
            echo -e "${G}✅ $(du -h "$dest" | cut -f1)${NC}"
            chmod +x "$dest"
            return 0
        else
            echo -e "${Y}超时${NC}"
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

    launchctl bootstrap system "$plist" 2>/dev/null || launchctl load "$plist"
    echo -e "${G}✅ LaunchDaemon 已安装并启动${NC}"
}

# ────────────────────────────────────────────────────────────
verify() {
    echo ""
    echo "────────────── 状态检查 ──────────────"
    echo -e "  等待隧道建立..."
    sleep 6

    local logs=""
    if [[ "$OS" == "linux" ]]; then
        logs=$(journalctl -u sdwan -n 120 --no-pager 2>/dev/null || true)
    else
        logs=$(tail -n 120 /var/log/sdwan.log 2>/dev/null || true)
    fi

    if [[ "$OS" == "linux" ]]; then
        if ip link show iwan1 &>/dev/null; then
            echo -e "  虚拟网卡: ${G}iwan1 已创建${NC}"
            ip -4 addr show iwan1 2>/dev/null | grep "inet " | awk '{print "  └─ IP: " $2}' || true
        else
            echo -e "  虚拟网卡: ${Y}iwan1 尚未出现 (请稍候)${NC}"
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
            echo -e "  虚拟网卡: ${Y}utun IP 尚未出现 (请稍候)${NC}"
        fi

        if route -n get 192.168.0.0 2>/dev/null | grep -q 'interface: utun'; then
            echo -e "  路由:     ${G}192.168.0.0/16 → utun${NC}"
        else
            echo -e "  路由:     ${Y}未确认 192.168.0.0/16 → utun${NC}"
        fi
    fi

    if printf '%s\n' "$logs" | grep -qi "AUTH REJECTED"; then
        echo -e "  认证:     ${R}失败，用户名或密码错误 (AUTH REJECTED)${NC}"
    elif printf '%s\n' "$logs" | grep -Eqi "Authenticated successfully|Tunnel established|OPENACK received"; then
        echo -e "  认证/隧道:${G} 已建立${NC}"
    else
        echo -e "  认证/隧道:${Y} 状态未知，请稍后查看日志${NC}"
    fi

    if [[ "$OS" == "linux" ]]; then
        if ping -c 1 -W 2 "$TEST_HOST" >/dev/null 2>&1; then
            echo -e "  内网测试: ${G}$TEST_HOST 可达${NC}"
        else
            echo -e "  内网测试: ${Y}$TEST_HOST 暂不可达 (警告，不影响安装)${NC}"
        fi
    else
        if ping -c 1 -W 2000 "$TEST_HOST" >/dev/null 2>&1; then
            echo -e "  内网测试: ${G}$TEST_HOST 可达${NC}"
        else
            echo -e "  内网测试: ${Y}$TEST_HOST 暂不可达 (警告，不影响安装)${NC}"
        fi
    fi

    echo "──────────────────────────────────────"
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
    echo -e "${Y}💡 修改配置: sudo nano /etc/sdwan/iwan.conf && sudo systemctl restart sdwan${NC}"
}

# ────────────────────────────────────────────────────────────
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

    write_config "$SERVER" "$USERNAME" "$PASSWORD"

    case "$OS" in
        linux)  install_linux_service ;;
        darwin) install_macos_service ;;
    esac

    verify
}

main "$@"
