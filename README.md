# sdwan

Go 实现的 SDWAN VPN 隧道客户端（公司 `sdwand` 替换品）。

支持 Linux / macOS (Intel + Apple Silicon) / Windows 三个平台。

## 目录结构

```
sdwan-go/
├── main.go
├── client.go           ← UDP, 握手, 数据转发
├── config.go           ← iwan.conf 解析, 服务器列表
├── protocol.go         ← 包头, MD5签名, AES加密, TLV
├── tunnel_linux.go     ← Linux: /dev/net/tun + ip命令
├── tunnel_darwin.go    ← macOS: utun + ifconfig
├── tunnel_windows.go   ← Windows: Wintun + netsh
├── install.sh           ← 一键部署 (自动识别架构)
├── panel/               ← Windows 托盘面板 (Wails v2 + WebView2)
│   ├── main.go / app.go / systray.go
│   ├── core/manager.go   ← 驱动真实 sdwan.exe 进程
│   └── frontend/         ← 苹果风毛玻璃 HTML 面板
├── Makefile
├── go.mod / go.sum
├── README.md
└── dist/                 ← 编译产物
    ├── sdwan-linux-amd64
    ├── sdwan-macos-amd64
    ├── sdwan-macos-arm64
    ├── sdwan-windows-amd64.exe
    └── sdwan-panel.exe
```

## 编译

```bash
make              # 全平台编译
make linux        # 只编 Linux
make macos        # 只编 macOS
make windows      # 只编 Windows
make VERSION=2.0  # 指定版本号
```

## 配置文件 (iwan.conf)

INI 格式，`#` 开头为注释。**配置文件必须放在 exe 同目录**（默认读取当前目录的 `iwan.conf`），跨平台统一。

```ini
server=minieye.9966.org    # 可不填，启动时会交互选择
username=your_username
password=your_password
port=10010
mtu=1436
encrypt=0
pipeid=0
pipeidx=0
tunname=iwan1              # TUN 网卡名称（可选，默认 iwan1）
routenet=192.168.0.0/16    # 内网路由网段（可选，默认 192.168.0.0/16）
```

`server` 为空时启动会弹出交互选单。可选服务器：

| 序号 | 地址 |
|------|------|
| 1 | minieye.9966.org |
| 2 | dwan.minieye.tech |
| 3 | minieye.8866.org |
| 4 | minieye.2288.org |
| 5 | youjia.8866.org |

## 命令行用法

```bash
./sdwan -version                  # 查看版本信息
./sdwan -f /path/to/iwan.conf    # 指定配置文件
```

服务器通过配置文件指定（`server=minieye.9966.org`），Windows 用托盘面板切换，无需命令行参数。

## 一键安装

**Linux / macOS：**
```bash
curl -fsSL https://raw.githubusercontent.com/wantianle/sdwan-go/main/scripts/install.sh | sudo bash
```

**Windows（管理员 PowerShell）：**
```powershell
iwr -useb https://raw.githubusercontent.com/wantianle/sdwan-go/main/scripts/install.ps1 | iex
```

脚本会自动：识别 OS 和架构 → ping 测速选服务器 → 输入工号密码 → 下载二进制 → 配置 systemd/LaunchDaemon/开机自启 → 启动服务。

## 查看日志

| 平台 | 日志路径 |
|------|---------|
| Linux | `journalctl -u sdwan -f` |
| macOS | `tail -f /var/log/sdwan.log` |
| Windows | `Get-Content C:\ProgramData\sdwan\sdwan.log -Wait -Tail 20` |

## 多平台兼容性说明

| 平台 | TUN 驱动 | 配置文件默认路径 |
|------|---------|----------------|
| Linux | `/dev/net/tun`（内核自带） | `./iwan.conf` |
| macOS | `utun`（内核自带） | `./iwan.conf` |
| Windows | `wintun.dll`（需放 exe 同目录） | `./iwan.conf` |

所有平台均使用当前目录的 `iwan.conf` 作为默认配置，可用 `-f` 指定其他路径。

## 验证隧道

```bash
# 成功连接后应能 ping 通内网
ping 10.10.10.1
ping hfs.minieye.tech    # 应解析到 192.168.x.x
```

## 停止隧道

```bash
# 前台运行时直接 Ctrl+C
# systemd 服务：
sudo systemctl stop sdwan
```
