#!/usr/bin/env bash
set -euo pipefail

# ────────────────────────────────────────────────────────────
# sdwan uninstaller — stop service, remove binary + config
#
# One-liner (via GitHub proxy):
#   curl -fsSL https://ghproxy.com/https://raw.githubusercontent.com/wantianle/sdwan-go/master/scripts/uninstall.sh | sudo bash
# ────────────────────────────────────────────────────────────

G='\033[0;32m'; R='\033[0;31m'; Y='\033[0;33m'; NC='\033[0m'

if [[ $EUID -ne 0 ]]; then
    echo -e "${R}❌ 需要 root 权限，请用 sudo 运行${NC}"
    exit 1
fi

echo -e "${Y}🗑️  正在卸载 SDWAN...${NC}"

OS=$(uname -s | tr '[:upper:]' '[:lower:]')

case "$OS" in
    linux)
        systemctl stop sdwan 2>/dev/null || true
        systemctl disable sdwan 2>/dev/null || true
        rm -f /etc/systemd/system/sdwan.service
        systemctl daemon-reload 2>/dev/null || true
        echo -e "${G}✅ systemd 服务已移除${NC}"
        ;;
    darwin)
        launchctl bootout system /Library/LaunchDaemons/com.minieye.sdwan.plist 2>/dev/null || \
            launchctl unload /Library/LaunchDaemons/com.minieye.sdwan.plist 2>/dev/null || true
        rm -f /Library/LaunchDaemons/com.minieye.sdwan.plist
        echo -e "${G}✅ LaunchDaemon 已移除${NC}"
        ;;
esac

rm -f /usr/local/bin/sdwan /usr/local/bin/sdwan_helper.sh
rm -rf /etc/sdwan
echo -e "${G}✅ 二进制和配置已删除${NC}"

echo ""
echo -e "${G}🗑️  卸载完成${NC}"
