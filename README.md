# sdwan-go

Go 实现的 SD-WAN VPN 隧道客户端，跨平台支持。

| 平台 | 控制方式 |
|------|---------|
| Windows | 托盘面板（Wails v2） |
| Linux | systemd 服务 |
| macOS | LaunchDaemon 服务 |

## 目录结构

```
sdwan-go/
├── cmd/sdwan/main.go
├── internal/core/
│   ├── client.go / config.go / protocol.go
│   ├── protocol_test.go
│   └── tunnel_linux.go / tunnel_darwin.go / tunnel_windows.go
├── panel/                   ← Windows 托盘面板（独立 Go module）
│   ├── main.go / app.go / systray.go
│   ├── core/manager.go
│   └── frontend/
├── scripts/
│   ├── install.sh
│   └── install.ps1
├── iwan.conf
├── Makefile / go.mod / go.sum
└── README.md
```

## 一键安装

**Linux / macOS：**
```bash
curl -fsSL https://raw.githubusercontent.com/wantianle/sdwan-go/master/scripts/install.sh | sudo bash
```

**Windows（管理员 PowerShell）：**
```powershell
iwr -useb https://raw.githubusercontent.com/wantianle/sdwan-go/master/scripts/install.ps1 | iex
```

## 验证隧道

```bash
ping 10.10.10.1
ping hfs.minieye.tech    # 应解析到 192.168.x.x
```

## 停止 / 卸载

**停止服务：**
```bash
# Linux
sudo systemctl stop sdwan

# macOS
sudo launchctl stop com.minieye.sdwan

# Windows
右键托盘图标 → 退出
```

**卸载：**
```bash
# Linux
sudo systemctl disable --now sdwan
sudo rm /etc/systemd/system/sdwan.service /usr/local/bin/sdwan
sudo rm -rf /etc/sdwan

# macOS
sudo launchctl bootout system /Library/LaunchDaemons/com.minieye.sdwan.plist
sudo rm /Library/LaunchDaemons/com.minieye.sdwan.plist /usr/local/bin/sdwan
sudo rm -rf /etc/sdwan

# Windows
schtasks /delete /tn "SDWAN Panel" /f
rmdir /s C:\ProgramData\sdwan
```

## 配置文件 (iwan.conf)

```ini
server=minieye.9966.org
username=your_username
password=your_password
port=10010
mtu=1436
encrypt=0
tunname=iwan1
routenet=192.168.0.0/16
```

| 字段 | 必填 | 说明 |
|------|:--:|------|
| `server` | ✅ | SDWAN 服务器地址 |
| `username` | ✅ | 工号 |
| `password` | ✅ | 密码 |
| `port` | | 端口，默认 10010 |
| `mtu` | | MTU，默认 1436 |
| `encrypt` | | 0=明文，1=AES 加密 |
| `tunname` | | TUN 网卡名，默认 iwan1 |
| `routenet` | | 内网路由网段 |

**路径：** 默认 exe 同目录 `iwan.conf`，可用 `-f` 指定。

## 多平台兼容性

| 平台 | TUN 驱动 | 权限 |
|------|---------|------|
| Linux | `/dev/net/tun`（内核自带） | root |
| macOS | `utun`（内核自带） | root |
| Windows | `wintun.dll` | 管理员 |
