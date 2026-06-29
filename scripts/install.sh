#!/usr/bin/env bash
set -euo pipefail

# ────────────────────────────────────────────────────────────
# sdwan installer — auto-detect OS/arch, download binary,
# interactive server selection, systemd/launchd setup.
#
# One-liner:
#   curl -fsSL https://raw.githubusercontent.com/USER/REPO/main/scripts/install.sh | sudo bash
# ────────────────────────────────────────────────────────────

REPO_OWNER="wantianle"
REPO_NAME="sdwan-go"
REPO_BRANCH="master"
GH_PROXY="https://ghproxy.com/"  # GitHub mirror for users who cannot access GitHub directly
INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/etc/sdwan"
CONFIG_FILE="$CONFIG_DIR/iwan.conf"

G='\033[0;32m'; R='\033[0;31m'; Y='\033[0;33m'; B='\e[0;34m'; NC='\033[0m'

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
    echo ""
    echo "---------- ⚡ SD-WAN 服务器延迟检测 ----------"
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
    echo -e "$cache" | column -t -s "|"
    echo "----------------------------------------------"

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
    local direct_url="https://raw.githubusercontent.com/${REPO_OWNER}/${REPO_NAME}/${REPO_BRANCH}/dist/${binary}"
    local proxy_url="${GH_PROXY}${direct_url}"
    local release_url="https://github.com/${REPO_OWNER}/${REPO_NAME}/releases/latest/download/${binary}"

    local dest="$INSTALL_DIR/sdwan"
    echo -e "${B}📥 下载 $binary ...${NC}"

    if command -v curl >/dev/null 2>&1; then
        # Try proxy first, then direct, then releases
        curl -fsSL "$proxy_url" -o "$dest" 2>/dev/null || \
        curl -fsSL "$direct_url" -o "$dest" 2>/dev/null || \
        curl -fsSL "${GH_PROXY}${release_url}" -o "$dest" 2>/dev/null || \
        curl -fsSL "$release_url" -o "$dest" 2>/dev/null || {
            echo -e "${R}❌ 所有下载方式均失败，请检查网络${NC}"
            exit 1
        }
    elif command -v wget >/dev/null 2>&1; then
        wget -q "$proxy_url" -O "$dest" 2>/dev/null || \
        wget -q "$direct_url" -O "$dest" 2>/dev/null || \
        wget -q "${GH_PROXY}${release_url}" -O "$dest" 2>/dev/null || \
        wget -q "$release_url" -O "$dest" 2>/dev/null || {
            echo -e "${R}❌ 所有下载方式均失败，请检查网络${NC}"
            exit 1
        }
    else
        echo -e "${R}❌ 需要 curl 或 wget${NC}"
        exit 1
    fi

    chmod +x "$dest"
    echo -e "${G}✅ 已安装到 $dest${NC}"
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
    sleep 2

    if ip link show iwan1 &>/dev/null; then
        echo -e "  虚拟网卡: ${G}iwan1 已创建${NC}"
        ip addr show iwan1 2>/dev/null | grep "inet " | awk '{print "  └─ IP: " $2}'
    else
        echo -e "  虚拟网卡: ${Y}iwan1 尚未出现 (请稍候)${NC}"
    fi

    if ip route 2>/dev/null | grep -q iwan1 || route -n get 192.168.0.0 2>/dev/null | grep -q iwan1; then
        echo -e "  路由:     ${G}192.168.0.0/16 → iwan1${NC}"
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
    check_root
    detect_platform

    SERVER=$(select_server)
    echo -e "${G}✅ 服务器: $SERVER${NC}"

    read -rp "👤 工号 (username): " USERNAME </dev/tty
    read -rsp "🔑 密码 (password): " PASSWORD </dev/tty
    echo

    write_config "$SERVER" "$USERNAME" "$PASSWORD"
    download_binary "$BINARY"

    case "$OS" in
        linux)  install_linux_service ;;
        darwin) install_macos_service ;;
    esac

    verify
}

main
