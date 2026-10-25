# sdwan-go

Go 实现的 SD-WAN VPN 隧道客户端，跨平台支持。

| 平台 | 控制方式 |
|------|---------|
| Windows | 托盘面板（Wails v2 WebView2） |
| Linux | systemd 服务 + CLI 子命令 |
| macOS | LaunchDaemon 服务 + CLI 子命令 |

## 架构

```
Windows panel  ──HTTP──▶  sdwan daemon  ──UDP──▶  SD-WAN server
Linux/macOS service ───▶  sdwan foreground ──UDP──▶ SD-WAN server
                                  │
                                  ├─ TUN/Wintun/utun
                                  └─ IP + 路由
```

Windows 面板通过 localhost 控制 API 管理 daemon。Linux 和 macOS 由 systemd/launchd
以前台模式运行 `sdwan -f /etc/sdwan/iwan.conf`，由服务管理器负责重启。核心进程拥有
TUN 网卡、IP 和路由的生命周期；启动失败时会后台自动重连（指数退避 500ms~8s），auth
拒绝时永久停止。

## 目录结构

```
sdwan-go/
├── cmd/sdwan/main.go              ← 入口（daemon / CLI）
├── internal/core/                 ← 隧道核心
│   ├── client.go                  ← UDP 会话管理、重连、暂停
│   ├── config.go                  ← iwan.conf 解析
│   ├── control.go                 ← HTTP 控制 API 服务端
│   ├── protocol.go                ← SD-WAN 协议（OPEN, ECHOREQ, AES/MD5）
│   ├── runner.go                  ← daemon / one-shot 启动编排
│   ├── route_conflict.go          ← 路由重叠检测
│   ├── protocol_test.go
│   └── tunnel_*.go                ← 平台 TUN 实现
├── pkg/                           ← 共享包（core & panel 共用）
│   ├── protocol/                  ← 协议常量、构建解析、加密
│   └── controlapi/                ← 控制 API 类型与 HTTP 客户端
├── panel/                         ← Windows 托盘面板（独立 Go module）
│   ├── main.go / app.go / systray.go
│   ├── core/manager.go / control.go
│   └── frontend/
├── scripts/
│   ├── install.sh / uninstall.sh
│   └── install.ps1 / uninstall.ps1
├── iwan.conf                      ← 配置模板（仅参考）
├── Makefile / go.mod / go.sum
└── README.md
```

## 一键安装

**Linux / macOS：**
```bash
curl --connect-timeout 10 --max-time 60 -fsSL https://gh.ddlc.top/https://raw.githubusercontent.com/wantianle/sdwan-go/master/scripts/install.sh | sudo bash
```

**Windows（管理员 PowerShell）：**
```powershell
iwr -useb https://raw.githubusercontent.com/wantianle/sdwan-go/master/scripts/install.ps1 | iex
```

> 上面的镜像只用于拉取安装脚本；脚本启动后会自动切换下载镜像（gh.ddlc.top → gh-proxy.com → gh.idayer.com）。若该镜像不可用，可把 `https://gh.ddlc.top/` 改为 `https://gh-proxy.com/` 或 `https://gh.idayer.com/`。`--connect-timeout 10 --max-time 60` 会避免外层 curl 无限等待。

**从本地构建产物安装（不走网络拉二进制）：**

已经 `make` 出 `dist/` 时，不需要再从 Release 下载：

```bash
# Linux / macOS
sudo bash scripts/install.sh --local-dir ./dist
```

```powershell
# Windows（管理员）
.\scripts\install.ps1 -LocalDir .\dist
```

- Linux/macOS 从 `--local-dir` 取 `sdwan-linux-<arch>` / `sdwan-darwin-<arch>`（也兼容 `make` 产出的 `sdwan-macos-<arch>` 拼写）
- Windows 从 `-LocalDir` 取 `panel.exe`、`sdwan-windows-amd64.exe`、`wintun.dll`
- 目录里有 `SHA256SUMS` 时按它严格校验（缺记录/重复/格式非法一律中止）；没有该文件则打印一次提示后按可信产物安装
- 只跳过「下载二进制」这一步；服务器测速、MTU 探测、安装后隧道验证仍需要网络

**安装指定版本：**
```bash
curl -fsSL .../install.sh | sudo bash -s -- 1.2.3
```

```powershell
iwr -useb .../install.ps1 | iex
# 然后输入版本号提示时填 1.2.3，或用安装目录里的 install.ps1 -Version 1.2.3
```

## 配置文件 (iwan.conf)

| 路径 | 平台 |
|------|------|
| `/etc/sdwan/iwan.conf` | Linux / macOS |
| `C:\ProgramData\sdwan\iwan.conf` | Windows |

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
| `port` | | UDP 端口，默认 10010 |
| `mtu` | | 最大传输单元，默认 1436 |
| `encrypt` | | 0=明文，1=AES 加密 |
| `tunname` | | TUN 网卡名，默认 iwan1 |
| `routenet` | | 内网路由网段，CIDR 格式 |

## 日常管理

安装脚本会把 Linux/macOS 配置成系统服务；Windows 由 `panel.exe`、托盘图标和后台 daemon 共同管理。修改配置后需要重启对应服务或面板才会生效。

### Linux（systemd）

```bash
# 服务状态 / 启动 / 停止 / 重启
sudo systemctl status sdwan
sudo systemctl start sdwan
sudo systemctl stop sdwan
sudo systemctl restart sdwan

# 设置或取消开机自启
sudo systemctl enable sdwan
sudo systemctl disable sdwan

# 编辑配置并重启
sudo vi /etc/sdwan/iwan.conf
sudo systemctl restart sdwan

# 实时日志 / 最近日志
sudo journalctl -u sdwan -f
sudo journalctl -u sdwan -n 50 --no-pager

# 检查虚拟网卡、分配 IP 与内网路由
ip -4 addr show iwan1
ip route show 192.168.0.0/16
```

### macOS（launchd）

macOS 会动态分配本次服务进程所拥有的 `utunN`（不应假定固定网卡名）。LaunchDaemon
以 `sdwan -f /etc/sdwan/iwan.conf` 前台运行，并由 `KeepAlive` 负责重启。要持续停止服务，
请使用 `launchctl bootout`。服务 stderr 日志为 `/var/log/sdwan.log`。

```bash
# 服务状态
sudo launchctl list | grep com.minieye.sdwan

# 重启服务（修改配置后使用）
sudo launchctl kickstart -k system/com.minieye.sdwan

# 持续停止/移除当前已加载的服务
sudo launchctl bootout system/com.minieye.sdwan

# 重新加载服务（bootout 后或排错时使用）
sudo launchctl bootstrap system /Library/LaunchDaemons/com.minieye.sdwan.plist

# 编辑配置并重启
sudo vi /etc/sdwan/iwan.conf
sudo launchctl kickstart -k system/com.minieye.sdwan

# 实时日志 / 检查 utun 网卡与路由
tail -f /var/log/sdwan.log
ifconfig | grep -A4 '^utun'
route -n get 192.168.0.0
```

### Windows（Panel / 托盘）

在开始菜单运行 **SDWAN Panel**，或双击安装目录中的 `panel.exe`。托盘图标左键打开面板，右键可退出；退出会断开 SD-WAN 并清理虚拟网卡。

```powershell
# 配置和日志位置
notepad C:\ProgramData\sdwan\iwan.conf
Get-Content C:\ProgramData\sdwan\sdwan.log -Wait -Tail 30
Get-Content C:\ProgramData\sdwan\panel.log -Tail 80

# 配置修改后：完全退出旧进程，再启动面板
taskkill /f /im panel.exe
taskkill /f /im sdwan-windows-amd64.exe
Start-Process C:\ProgramData\sdwan\panel.exe

# 检查虚拟网卡、IP 与内网路由
Get-NetIPAddress -InterfaceAlias iwan1 -AddressFamily IPv4
Get-NetRoute -DestinationPrefix 192.168.0.0/16 | Where-Object InterfaceAlias -eq iwan1
```

> Windows 安装脚本会尝试授予当前用户 `C:\ProgramData\sdwan\iwan.conf` 的修改权限；若仍被公司策略拦截，请以管理员身份打开记事本后编辑该文件。

## CLI 子命令

Windows 面板启动后台 daemon 后，可用 CLI 子命令通过控制 API 查询状态或切换服务器。
Linux/macOS 服务以前台模式运行，不提供 daemon 控制 API。

```bash
# 查看当前状态（服务器、路由、session、冲突等）
sudo sdwan status -f /etc/sdwan/iwan.conf

# 切换服务器
sudo sdwan switch minieye.8866.org -f /etc/sdwan/iwan.conf
```

Windows 子命令通过读取同目录下的 `control.token` 与 daemon 的 HTTP API 通信。

## 控制 API (HTTP)

Windows daemon 在 `127.0.0.1:17890` 提供 REST API，Bearer token 鉴权（token 文件与 daemon 同目录的 `control.token`）。

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/v1/status` | 状态：running/reconnecting/paused/disconnected，路由冲突 |
| `POST` | `/v1/switch` | `{"server":"host"}` 切换服务器 |
| `POST` | `/v1/pause` | `{"pause":true/false}` 暂停/恢复 |
| `POST` | `/v1/shutdown` | 优雅退出 |

## 多平台兼容性

| 平台 | TUN 驱动 | 权限 |
|------|---------|------|
| Linux | `/dev/net/tun`（内核自带） | root |
| macOS | `utun`（内核自带） | root |
| Windows | `wintun.dll` | 管理员 |

## 验证隧道

```bash
ping 10.10.10.1
ping hfs.minieye.tech    # 应解析到 192.168.x.x
```

## 卸载

**Linux / macOS：**
```bash
curl -fsSL https://raw.githubusercontent.com/wantianle/sdwan-go/master/scripts/uninstall.sh | sudo bash
```

**Windows（管理员 PowerShell）：**
```powershell
iwr -useb https://raw.githubusercontent.com/wantianle/sdwan-go/master/scripts/uninstall.ps1 | iex
```

## 开发

```bash
make          # 构建、vet、test
make test     # go test -race
make vet      # go vet
make tidy     # go mod tidy
```

跨平台构建：
```bash
GOOS=windows GOARCH=amd64 go build ./cmd/sdwan
GOOS=linux   GOARCH=amd64 go build ./cmd/sdwan
GOOS=darwin  GOARCH=amd64 go build ./cmd/sdwan
```

## 工具仓视角（MDRIVE_WS/tools）

> 所属域：system　·　最后核对：2026-10-25

- **宿主路径**：`~/dev/mdrive4/MDRIVE_WS/tools/system/sdwan-go`
- **容器路径**：`/mdrive/tools/system/sdwan-go`
- **仓库性质**：**独立 git 仓库**（remote `git@github.com:wantianle/sdwan-go.git`），由 `MDRIVE_WS/tools/.gitignore` 整目录排除，**不并入 tools 仓历史**。版本操作请用 `git -C .../system/sdwan-go`，不要在 tools 仓里 `git add` 本目录。
- **为什么不合并**：两个 install 脚本的下载链路直接依赖该仓库的 GitHub Release CI；合并进本地-only 的 tools 仓会让发布链断掉。

### 与 sdwan_nas 的关系

| 目录 | 方案 | 说明 |
|---|---|---|
| `../sdwan_nas/` | 旧版 shell（`sdwand`） | 安装脚本 + `sdwan_helper.sh` + `sdwan.service` + NAS CIFS 挂载 |
| `./`（本目录） | Go 版（`sdwan`） | 常驻 daemon、进程内自愈、DNS 缓存、Windows panel、跨平台安装器 |

两者**互斥**：同一台机器不要让两套同时接管 `sdwan.service` / `iwan1`。

### 依赖

| 项 | 要求 |
|---|---|
| 构建 | Go 1.23+（当前用 go1.26 构建亦可）；`make` 需要 `CGO_ENABLED=0` 交叉编译 |
| 运行（Linux） | systemd、`iproute2`、root 权限改动网卡/路由 |
| 运行（macOS） | launchd、需 root 创建 utun（动态 `utunN`） |
| 运行（Windows） | Wintun 驱动 + `wintun.dll`；daemon 由 `panel.exe` 拉起（control API `127.0.0.1:17890`） |
| 产物 | `dist/` 下的平台二进制 + `wintun.dll`（不入库，`make` 复制） |

### 已知坑

- **macOS 网卡名**：不要写 `iwan1`，`water` 在 Darwin 只接受空名（内核动态分配 `utunN`）；路由按解析到的实际 `utunN` 添加。
- **Windows 路由索引**：`route.exe IF` 用的是 IPv4 接口索引，`wmic` 的 Index 与之不一致；只认 `netsh interface ipv4 show interfaces` 的 `Idx`。
- **Windows 默认路由/DNS**：`iwan1` 建好后必须压制 metric 并清掉其默认路由与 DNS，否则会抢走外网流量（表现为必应/网页变慢）。
- **切换服务器会换 tunnel IP**：服务器每次分配不同 `10.100.100.x`，切换后客户端会重配 TUN IP；长连接（SSH/网页）会断。
- **断网重连**：靠进程内自愈（`Client.Start()` + reconnectLoop）。已加 3s DNS 上限与解析缓存（失败不清缓存，5 分钟 TTL 适应换 IP），因此断网/睡眠唤醒后应几秒内自愈；`MainPID` 不变、`NRestarts=0` 是正常表现。
- **`/etc/hosts` 固定服务器 IP 不可靠**：会被系统重写覆盖，别再依赖；用配置里的域名 + 客户端缓存。
- **远程模式**：安装脚本需要能访问 GitHub Release；网络受限时用 `--local-dir`。

### 验证

```bash
# 单元测试与静态检查
go test -race ./...
go vet ./...
gofmt -l . | grep -v '^$' && echo "需要 gofmt"

# 安装脚本（本地模式，无需 root/联网）
bash scripts/test_install_local.sh
shellcheck scripts/install.sh scripts/test_install_local.sh

# 运行态
systemctl status sdwan --no-pager          # Linux
ip -br addr show iwan1; ip route | grep 192.168.0.0/16
journalctl -u sdwan -n 50 --no-pager
```

### 相关

- 旧版客户端：`../sdwan_nas/`
- 车端连接/部署：skill `mdrive-device`（`device/ssh_tool`、`device/md_tool`）
- 容器与库路径：skill `mdrive-container`
- 逆向资料（握手协议）：`~/dev/myScript/ai/research/sdwan_reverse_engineering.md`
